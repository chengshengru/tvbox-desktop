// Package livehealth 直播线路体检。
// 翻译自 src/main/services/liveHealth.ts。
//
// 公开 IPTV 源的真实可用率很低（实测某条主力源 27% 的线路是死链，另一条 88%），
// 而一个频道往往挂着好几条线路 —— 只要知道「哪几条是活的」，点击体验就从
// 「随缘播放 + 手动换线」变成「直接播通的那条」。
//
// 体检刻意做得很轻：只读响应体的前几 KB，够看到 #EXTM3U 就够。
// 绝不能把整条流拉下来 —— 那是几百 MB 的事，而且直播流根本不会结束。
//
// 作者：Shiro
package livehealth

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"tvbox-desktop/internal/httputil"
	"tvbox-desktop/internal/settings"
	"tvbox-desktop/internal/sources"
	"tvbox-desktop/internal/store"
	"tvbox-desktop/models"
)

const storeName = "live-health"

const (
	// maxBytes 只读这么多字节就够判断了：master/media 播放列表都在最前面
	maxBytes = 4096
	// okTTL 探测通的线路 30 分钟内不重复探；failTTL 不通的 5 分钟后重试（公开源恢复得很快）
	okTTL   = 30 * 60 * 1000
	failTTL = 5 * 60 * 1000
	// defaultTimeout 设置里没有超时值时的兜底（秒）
	defaultTimeout = 6
	// maxConcurrency 并发池上限：直播源同一台 CDN 上可能挂着几十条线路，一次性并发会被限速
	maxConcurrency = 8
	// maxURLs 一次最多探这么多条，防止界面手滑传进来 800 条把网络打满
	maxURLs = 60
)

var (
	httpRe       = regexp.MustCompile(`(?i)^https?://`)
	extm3uRe     = regexp.MustCompile(`(?i)#EXTM3U`)
	bandwidthRe  = regexp.MustCompile(`(?i)BANDWIDTH=(\d+)`)
	resolutionRe = regexp.MustCompile(`(?i)RESOLUTION=\d+x(\d+)`)
)

// ProbeOptions 体检选项，字段语义与 TS 的 { timeout, force } 对齐。
type ProbeOptions struct {
	// Timeout 单条探测超时（秒），<=0 表示跟随全局设置。
	Timeout float64
	// Force 忽略新鲜度缓存，全部重探。
	Force bool
}

// probeTransport 与全局网络层同构的 Transport：连接复用 + 忽略非常规证书（采集站常见自签）。
var probeTransport = &http.Transport{
	MaxIdleConns:        64,
	MaxIdleConnsPerHost: 8,
	IdleConnTimeout:     60 * time.Second,
	TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
	Proxy:               http.ProxyFromEnvironment,
}

// probeClient 探测用客户端：不设整体 Timeout（直播流不会结束），超时全靠 ctx 控制。
var probeClient = &http.Client{Transport: probeTransport}

// isFresh 体检结果是否还在有效期内。
func isFresh(h *models.LiveHealth, now int64) bool {
	if h == nil || h.At == 0 {
		return false
	}
	ttl := int64(failTTL)
	if h.OK {
		ttl = int64(okTTL)
	}
	return now-h.At < ttl
}

// probeOutcome 单条探测的原始结论（落库前再补 at）。
type probeOutcome struct {
	ok        bool
	ms        int64
	height    int
	bandwidth float64
	errMsg    string
}

// probeOne 探一条地址：GET 后最多读 maxBytes 字节就主动断开，够判断 m3u8 即可。
func probeOne(ctx context.Context, rawURL string, timeout time.Duration, header map[string]string) probeOutcome {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now().UnixMilli()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return probeOutcome{ms: time.Now().UnixMilli() - started, errMsg: "地址非法"}
	}
	req.Header.Set("User-Agent", httputil.DefaultUA)
	req.Header.Set("Accept", "*/*")
	for k, v := range header {
		req.Header.Set(k, v)
	}

	res, err := probeClient.Do(req)
	if err != nil {
		ms := time.Now().UnixMilli() - started
		why := "网络错误"
		if reqCtx.Err() == context.DeadlineExceeded {
			why = fmt.Sprintf("超时（%gs）", timeout.Seconds())
		} else if msg := err.Error(); msg != "" {
			why = msg
		}
		return probeOutcome{ms: ms, errMsg: why}
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// 403/404 是公开源最常见的死法；把状态码原样带出来，用户一眼能看懂
		return probeOutcome{ms: time.Now().UnixMilli() - started, errMsg: fmt.Sprintf("HTTP %d", res.StatusCode)}
	}

	// 只读到够判断为止，然后主动断开 —— 否则会把整条直播流拉完
	buf := make([]byte, maxBytes)
	n, _ := io.ReadFull(res.Body, buf)
	text := string(buf[:n])
	ms := time.Now().UnixMilli() - started

	if !extm3uRe.MatchString(text) {
		ctype := res.Header.Get("Content-Type")
		msg := "返回的不是 m3u8"
		if ctype != "" {
			msg = fmt.Sprintf("不是 m3u8（%s）", strings.Split(ctype, ";")[0])
		}
		return probeOutcome{ms: ms, errMsg: msg}
	}

	// master 播放列表：顺手记下最高一档，挑线路时能偏向高画质的那条
	out := probeOutcome{ok: true, ms: ms}
	for _, m := range bandwidthRe.FindAllStringSubmatch(text, -1) {
		if v, perr := strconv.ParseFloat(m[1], 64); perr == nil && v > out.bandwidth {
			out.bandwidth = v
		}
	}
	for _, m := range resolutionRe.FindAllStringSubmatch(text, -1) {
		if v, perr := strconv.Atoi(m[1]); perr == nil && v > out.height {
			out.height = v
		}
	}
	return out
}

// Get 读全部体检缓存（store 里的整张表）。
func Get() map[string]models.LiveHealth {
	m := store.ReadJSON[map[string]models.LiveHealth](storeName, nil)
	if m == nil {
		return map[string]models.LiveHealth{}
	}
	return m
}

// Clear 清空体检缓存。
func Clear() map[string]models.LiveHealth {
	empty := map[string]models.LiveHealth{}
	store.WriteStore(storeName, empty)
	return empty
}

// Probe 体检一批地址，结果并进缓存后返回**整张表**。
// 传进来的地址无论新旧都会被重探（调用方只在「需要知道当下结果」时才调）。
// 探测本身不中断流程，error 恒为 nil（单条失败记录在该条的 Error 字段里）。
func Probe(ctx context.Context, urls []string, opts ProbeOptions) (map[string]models.LiveHealth, error) {
	st := settings.Get()
	table := Get()
	now := time.Now().UnixMilli()

	// 去重、去空、只留 http(s)
	seen := map[string]bool{}
	unique := make([]string, 0, len(urls))
	for _, u := range urls {
		t := strings.TrimSpace(u)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		unique = append(unique, t)
	}
	targets := make([]string, 0, len(unique))
	for _, u := range unique {
		if httpRe.MatchString(u) {
			targets = append(targets, u)
		}
	}

	todo := targets
	if !opts.Force {
		todo = make([]string, 0, len(targets))
		for _, u := range targets {
			h := table[u]
			if !isFresh(&h, now) {
				todo = append(todo, u)
			}
		}
		if len(todo) > maxURLs {
			todo = todo[:maxURLs]
		}
	}

	if len(todo) > 0 {
		timeout := opts.Timeout
		if timeout <= 0 {
			timeout = st.Timeout
			if timeout > defaultTimeout*2 {
				timeout = defaultTimeout * 2
			}
			if timeout <= 0 {
				timeout = defaultTimeout
			}
		}

		// 和站点请求一样走「全局 headers 规则」：不少直播网关校验 Referer / UA，
		// 不带就一律 403，会把好线路误判成死的
		rules := []models.HeaderRule{}
		for _, s := range sources.List() {
			if !s.Enabled {
				continue
			}
			if p, ok := sources.GetParsed(s.ID); ok && p != nil {
				rules = append(rules, p.Headers...)
			}
		}

		// 并发池：标准库 WaitGroup + 信号量通道（等价 TS 的 pool，项目不引 x/sync）
		outcomes := make([]probeOutcome, len(todo))
		sem := make(chan struct{}, maxConcurrency)
		var wg sync.WaitGroup
		for i, u := range todo {
			wg.Add(1)
			go func(i int, u string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				ua := st.UserAgent
				if ua == "" {
					ua = httputil.DefaultUA
				}
				header := map[string]string{"User-Agent": ua}
				for k, v := range httputil.MatchHeaderRules(u, rules) {
					header[k] = v
				}
				outcomes[i] = probeOne(ctx, u, time.Duration(timeout*float64(time.Second)), header)
			}(i, u)
		}
		wg.Wait()

		for i, u := range todo {
			o := outcomes[i]
			h := models.LiveHealth{OK: o.ok, Ms: o.ms, At: time.Now().UnixMilli(), Error: o.errMsg}
			if o.height > 0 {
				height := o.height
				h.Height = &height
			}
			if o.bandwidth > 0 {
				bw := o.bandwidth
				h.Bandwidth = &bw
			}
			table[u] = h
		}
		store.WriteStore(storeName, table)
	}

	return table, nil
}
