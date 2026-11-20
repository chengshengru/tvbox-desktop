// engine.go：Engine 接口 + worker 池 Manager（设计文档 §5/§6.2/§9）。
// 池键 = kind::siteKey::group::extHash，同脚本不同 ext 视为不同 worker；
// 空闲 60s 回收、总数上限 8、QuickJS 上下文非并发安全所以按 worker 串行调用。
//
// 作者：Shiro
package spider

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"tvbox-desktop/internal/settings"
	"tvbox-desktop/models"
)

// Engine 一个脚本运行时。实现：js.Worker（P1）/ python.Worker（P4）。
type Engine interface {
	// Init 执行脚本 init(ext, group)；同 worker 只跑一次（由池保证）。
	Init(ctx context.Context, ext, group string) error
	// Invoke 调用一个协议方法，params 为 TVBox input 的字段集（含 type）。
	Invoke(ctx context.Context, method string, params map[string]any) (string, error)
	// Close 释放上下文/进程。
	Close() error
}

// ScriptSource 脚本产物来源：缓存文件路径或内联代码体（§6.3）。
type ScriptSource struct {
	// Kind 脚本形态："js" / "python"。
	Kind string
	// Path 下载缓存文件路径（http(s) 来源时非空）。
	Path string
	// Inline 内联代码体（api 字段直接写脚本时非空）。
	Inline string
}

// EngineFactory 为一个脚本创建引擎实例；js / python 包在 P1+ 阶段注册。
type EngineFactory func(ctx context.Context, src ScriptSource) (Engine, error)

const (
	// maxWorkers worker 池上限（§9 资源限额）。
	maxWorkers = 8
	// idleTTL 空闲回收阈值。
	idleTTL = 60 * time.Second
	// janitorTick 回收巡检周期。
	janitorTick = 10 * time.Second
)

// entry 池内一个 worker 的包装：串行锁 + 空闲时间戳 + init 一次性。
type entry struct {
	engine   Engine
	mu       sync.Mutex // 同一上下文/进程禁止并发调用
	lastUsed time.Time
	initOnce sync.Once
	initErr  error
}

// poolT worker 池。
type poolT struct {
	mu        sync.Mutex
	factories map[string]EngineFactory
	entries   map[string]*entry
	janitorOn bool
}

var pool = &poolT{
	factories: map[string]EngineFactory{},
	entries:   map[string]*entry{},
}

// RegisterFactory 注册一种形态的引擎工厂（js / python 包初始化时调用）。
func RegisterFactory(kind string, f EngineFactory) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	pool.factories[kind] = f
}

// registered 报告某形态是否已有引擎实现。
func registered(kind string) bool {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return pool.factories[kind] != nil
}

// workerKey 池键：形态 + 站点 + 分组（订阅源隔离）+ ext 哈希。
func workerKey(kind string, site models.Site) string {
	return kind + "::" + site.Key + "::" + site.SourceID + "::" + sha16(site.Ext)
}

// poolInvoke 取（或建）worker → 保证 init 一次 → 串行调用协议方法。
func poolInvoke(ctx context.Context, kind string, site models.Site, method string, params map[string]any) (string, error) {
	key := workerKey(kind, site)
	e, err := acquire(ctx, kind, site, key)
	if err != nil {
		return "", err
	}

	timeout := invokeTimeout(site)
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 同一 worker 串行调用：QuickJS 上下文 / Python 子进程均非并发安全；
	// init 也放在锁内，防止回收器误关正在初始化的 worker。
	e.mu.Lock()
	defer e.mu.Unlock()
	// init 同组只跑一次；失败即出池，下次调用重建（§5 失败即降级）
	e.initOnce.Do(func() {
		e.initErr = e.engine.Init(cctx, site.Ext, site.SourceID)
	})
	if e.initErr != nil {
		pool.discard(key, e)
		return "", fmt.Errorf("脚本 init 失败：%w", e.initErr)
	}

	pool.touch(key)
	res, err := e.engine.Invoke(cctx, method, BuildInput(method, params))
	pool.touch(key)
	if err != nil {
		// 超时打断后脚本状态不再可信：出池重建，防脏上下文传染后续调用（§11.2 池自愈）
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			pool.discard(key, e)
		}
		return res, fmt.Errorf("Spider[%s] %s：%w", site.Name, method, err)
	}
	return res, nil
}

// acquire 命中池则复用，否则经 fetcher 取脚本 + 工厂建新 worker。
func acquire(ctx context.Context, kind string, site models.Site, key string) (*entry, error) {
	pool.mu.Lock()
	if e, ok := pool.entries[key]; ok {
		e.lastUsed = time.Now()
		pool.mu.Unlock()
		return e, nil
	}
	f := pool.factories[kind]
	pool.mu.Unlock()
	if f == nil {
		return nil, ErrNoEngine
	}

	// 下载/建引擎在池锁外做，避免阻塞其它站点的取用
	src, err := resolveScript(ctx, kind, site)
	if err != nil {
		return nil, err
	}
	engine, err := f(ctx, src)
	if err != nil {
		return nil, err
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()
	if e, ok := pool.entries[key]; ok { // 并发下他人已建好，丢弃自己这份
		e.lastUsed = time.Now()
		_ = engine.Close()
		return e, nil
	}
	pool.evictOverflowLocked()
	e := &entry{engine: engine, lastUsed: time.Now()}
	pool.entries[key] = e
	if !pool.janitorOn {
		pool.janitorOn = true
		go janitorLoop()
	}
	return e, nil
}

// evictOverflowLocked 池满时按 LRU 淘汰空闲 worker（持池锁调用）。
// 全忙则临时超限——宁可不淘汰也不杀正在跑的调用（杀在跑引擎会崩）。
func (p *poolT) evictOverflowLocked() {
	for len(p.entries) >= maxWorkers {
		var victimKey string
		var victim *entry
		for k, e := range p.entries {
			if !e.mu.TryLock() {
				continue // 忙
			}
			if victim == nil || e.lastUsed.Before(victim.lastUsed) {
				if victim != nil {
					victim.mu.Unlock()
				}
				victimKey, victim = k, e
			} else {
				e.mu.Unlock()
			}
		}
		if victim == nil {
			return // 全忙，临时超限
		}
		delete(p.entries, victimKey)
		victim.mu.Unlock()
		go func(en Engine) { _ = en.Close() }(victim.engine)
	}
}

// touch 刷新空闲时间戳（调用前后各一次）。
func (p *poolT) touch(key string) {
	p.mu.Lock()
	if e, ok := p.entries[key]; ok {
		e.lastUsed = time.Now()
	}
	p.mu.Unlock()
}

// discard 把 init 失败的 worker 出池并关闭。
func (p *poolT) discard(key string, want *entry) {
	p.mu.Lock()
	if cur, ok := p.entries[key]; ok && cur == want {
		delete(p.entries, key)
	}
	p.mu.Unlock()
	_ = want.engine.Close()
}

// janitorLoop 周期回收空闲超时的 worker（§5 idle TTL 60s 自动销毁）。
func janitorLoop() {
	for {
		time.Sleep(janitorTick)
		pool.reapIdle()
	}
}

// reapIdle 关闭空闲超时且不在调用中的 worker。
func (p *poolT) reapIdle() {
	p.mu.Lock()
	now := time.Now()
	var victims []*entry
	for k, e := range p.entries {
		if now.Sub(e.lastUsed) <= idleTTL {
			continue
		}
		if !e.mu.TryLock() {
			continue // 长调用在跑，本轮跳过
		}
		delete(p.entries, k)
		victims = append(victims, e)
	}
	p.mu.Unlock()
	for _, e := range victims {
		e.mu.Unlock()
		_ = e.engine.Close()
	}
}

// invokeTimeout 单方法超时：设置值（默认 25s，上限 60s）；站点自身 timeout 更小时尊重站点。
func invokeTimeout(site models.Site) time.Duration {
	sec := settings.Get().Spider.Timeout
	if sec <= 0 {
		sec = 25
	}
	if sec > 60 {
		sec = 60
	}
	if site.Timeout != nil && *site.Timeout > 0 && *site.Timeout < sec {
		sec = *site.Timeout
	}
	return time.Duration(sec * float64(time.Second))
}
