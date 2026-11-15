// Package sources 订阅源管理：CRUD + 同步 + 已解析配置的内存缓存。
// 翻译自 src/main/services/sources.ts。
//
// 作者：Shiro
package sources

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"tvbox-desktop/internal/config"
	"tvbox-desktop/internal/settings"
	"tvbox-desktop/internal/store"
	"tvbox-desktop/models"
)

// storeName 持久化用的 store 名。
const storeName = "sources"

var (
	mu          sync.Mutex
	parsedCache = map[string]*models.ParsedConfig{}
	hydrated    = false
)

var (
	httpURLRe = regexp.MustCompile(`(?i)^https?://`)
	pathSepRe = regexp.MustCompile(`[\\/]`)
	jsonExtRe = regexp.MustCompile(`(?i)\.json$`)
)

// AddInput 新增订阅源的入参。
type AddInput struct {
	// Name 展示名；留空时按地址猜一个。
	Name string
	// URL 订阅地址（远程 URL 或本地文件路径）。
	URL string
	// Kind "url" | "file"；留空时按 URL 前缀自动判定。
	Kind string
}

// List 列出全部订阅源。
func List() []models.ConfigSource {
	list := store.ReadJSON[[]models.ConfigSource](storeName, nil)
	if list == nil {
		return []models.ConfigSource{}
	}
	return list
}

// saveSources 全量落库（防抖写盘由 store 负责）。
func saveSources(list []models.ConfigSource) {
	store.WriteStore(storeName, list)
}

// newID 生成 UUID v4（等价 node 的 randomUUID）。
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 极端兜底：用时间戳凑一个可辨识的 id
		return fmt.Sprintf("src-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// guessName 没填名字时按地址猜一个展示名。
func guessName(rawURL string, kind string) string {
	if kind == "file" {
		parts := pathSepRe.Split(rawURL, -1)
		base := parts[len(parts)-1]
		return jsonExtRe.ReplaceAllString(base, "")
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return "远程配置"
	}
	return u.Hostname()
}

// Hydrate 启动时用上次成功的原始配置回填缓存，做到「首屏不等网络」。
func Hydrate(ctx context.Context) error {
	mu.Lock()
	if hydrated {
		mu.Unlock()
		return nil
	}
	hydrated = true
	mu.Unlock()

	for _, source := range List() {
		if !source.Enabled || source.Raw == nil {
			continue
		}
		parsed, err := config.ParseConfigWith(source.Raw, config.NormalizeContext{
			SourceID:   source.ID,
			SourceName: source.Name,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "[sources] 回填 %s 失败：%v\n", source.Name, err)
			continue
		}
		mu.Lock()
		parsedCache[source.ID] = parsed
		mu.Unlock()
	}
	return nil
}

// GetParsed 取内存里已解析的配置。
func GetParsed(sourceID string) (*models.ParsedConfig, bool) {
	mu.Lock()
	defer mu.Unlock()
	p, ok := parsedCache[sourceID]
	return p, ok
}

// Add 新增订阅源：查重、猜名、自动判定 kind。
func Add(ctx context.Context, input AddInput) (models.ConfigSource, error) {
	rawURL := strings.TrimSpace(input.URL)
	if rawURL == "" {
		return models.ConfigSource{}, errors.New("地址不能为空")
	}
	kind := input.Kind
	if kind == "" {
		if httpURLRe.MatchString(rawURL) {
			kind = "url"
		} else {
			kind = "file"
		}
	}
	list := List()
	for _, s := range list {
		if s.URL == rawURL {
			return models.ConfigSource{}, errors.New("该配置源已存在")
		}
	}

	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = guessName(rawURL, kind)
	}
	source := models.ConfigSource{
		ID:       newID(),
		Name:     name,
		URL:      rawURL,
		Kind:     kind,
		Enabled:  true,
		LastSync: 0,
	}
	list = append(list, source)
	saveSources(list)
	return source, nil
}

// Update 增量更新订阅源（仅 name/url/enabled 生效，与 TS 的 patch 类型一致）。
func Update(ctx context.Context, id string, patch map[string]any) (models.ConfigSource, error) {
	list := List()
	idx := -1
	for i := range list {
		if list[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return models.ConfigSource{}, errors.New("配置源不存在")
	}
	next := list[idx]
	if v, ok := patch["name"]; ok {
		next.Name = models.Str(v)
	}
	if v, ok := patch["url"]; ok {
		u := models.Str(v)
		next.URL = u
		if u != "" {
			if httpURLRe.MatchString(u) {
				next.Kind = "url"
			} else {
				next.Kind = "file"
			}
		}
	}
	if v, ok := patch["enabled"]; ok {
		next.Enabled = models.Bool(v)
	}
	list[idx] = next
	saveSources(list)
	// 停用源时清掉解析缓存，避免聚合层继续吃到旧站点
	if v, ok := patch["enabled"]; ok {
		if b, isBool := v.(bool); isBool && !b {
			mu.Lock()
			delete(parsedCache, id)
			mu.Unlock()
		}
	}
	return next, nil
}

// Remove 删除订阅源并清缓存。
func Remove(id string) error {
	list := List()
	out := make([]models.ConfigSource, 0, len(list))
	for _, s := range list {
		if s.ID != id {
			out = append(out, s)
		}
	}
	saveSources(out)
	mu.Lock()
	delete(parsedCache, id)
	mu.Unlock()
	return nil
}

// Sync 同步单个订阅源：拉取→解析→回填缓存→更新统计。
// 同步失败但本地有缓存时保留可用数据，界面只提示不中断。
func Sync(ctx context.Context, id string) (models.ConfigSource, error) {
	list := List()
	idx := -1
	for i := range list {
		if list[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return models.ConfigSource{}, errors.New("配置源不存在")
	}
	source := list[idx]
	st := settings.Get()

	next := source
	raw, err := config.LoadConfigSourceWithTimeout(ctx, source.URL, source.Kind, st.Timeout)
	var parsed *models.ParsedConfig
	if err == nil {
		parsed, err = config.ParseConfigWith(raw, config.NormalizeContext{
			SourceID:   source.ID,
			SourceName: source.Name,
			Defaults:   &config.Defaults{Timeout: st.Timeout},
		})
	}
	if err != nil {
		next.Error = err.Error()
	} else {
		mu.Lock()
		parsedCache[source.ID] = parsed
		mu.Unlock()
		next.Raw = parsed.Raw
		next.LastSync = time.Now().UnixMilli()
		next.Error = ""
		if next.Name == "" {
			next.Name = parsed.Name
		}
		next.Notice = parsed.Notice
		next.SiteCount = len(parsed.Sites)
		next.LiveCount = len(parsed.Lives)
		next.ParseCount = len(parsed.Parses)
	}
	list[idx] = next
	saveSources(list)
	return next, nil
}

// SyncAll 顺序同步全部启用源：站点数量有限，避免同时打太多源触发风控。
func SyncAll(ctx context.Context) ([]models.ConfigSource, error) {
	list := List()
	results := []models.ConfigSource{}
	for _, source := range list {
		if !source.Enabled {
			continue
		}
		r, err := Sync(ctx, source.ID)
		if err != nil {
			continue
		}
		results = append(results, r)
	}
	return results, nil
}
