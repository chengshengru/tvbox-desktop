// Package home 首页内容位：配置 sections + 默认生成、10 分钟缓存、单行失败不影响其它行。
// 翻译自 src/main/services/home.ts。
//
// 作者：Shiro
package home

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"tvbox-desktop/internal/cms"
	"tvbox-desktop/internal/httputil"
	"tvbox-desktop/internal/search"
	"tvbox-desktop/internal/settings"
	"tvbox-desktop/internal/sites"
	"tvbox-desktop/internal/sources"
	"tvbox-desktop/models"
)

// homeLimit 每个内容位最多放多少条。首页是横向滑动的，太多没必要，也省流量。
const homeLimit = 24

// 首页内容位缓存多久。首页是每次启动都看的第一个页面，不缓存等于每次启动都打一遍所有源。
const (
	ttlMs     = 10 * 60 * 1000
	errorTtlM = 60 * 1000
)

// errorTtlM 失败的那几条只缓存 1 分钟。
//
// 「fetch failed」很多时候是一次网络的抖动（实测素博第一次拉就失败、紧接着重试就好了），
// 按 10 分钟缓存下来会让用户盯着一条错误的横条看很久。

type homeCacheEntry struct {
	sig      string
	at       int64
	sections []models.HomeSection
}

var (
	homeMu         sync.Mutex
	homeCacheState *homeCacheEntry
)

// ClearCache 站点列表变了（换源、启用开关）就得让缓存失效。
func ClearCache() {
	homeMu.Lock()
	defer homeMu.Unlock()
	homeCacheState = nil
}

// plan 一个内容位的拉取计划。
type plan struct {
	key    string
	title  string
	origin string // "config" | "default"
	site   models.Site
	typeID *string
}

// Sections 首页内容位。配置里有 `home.sections` 就按配置来，没有就每个可用源给一条「最近更新」。
//
// 拉取是**失败不抛**的：某个源挂了只让那一条内容位自己显示错误，
// 不能让整个首页白屏。force 为 true 时忽略缓存重新拉。
func Sections(ctx context.Context, force bool) ([]models.HomeSection, error) {
	plans, missing := buildPlan()
	keys := make([]string, 0, len(plans))
	for _, p := range plans {
		keys = append(keys, p.key)
	}
	sig := strings.Join(keys, "|")

	now := time.Now().UnixMilli()
	homeMu.Lock()
	if !force && homeCacheState != nil && homeCacheState.sig == sig {
		age := now - homeCacheState.at
		// 上一次有拉失败的内容位时只用短 TTL，让抖动过的源很快有机会翻身
		limit := int64(ttlMs)
		for _, s := range homeCacheState.sections {
			if s.Error != "" {
				limit = errorTtlM
				break
			}
		}
		if age < limit {
			out := make([]models.HomeSection, 0, len(homeCacheState.sections)+len(missing))
			out = append(out, homeCacheState.sections...)
			out = append(out, missing...)
			homeMu.Unlock()
			return out, nil
		}
	}
	homeMu.Unlock()

	st := settings.Get()
	concurrency := clampConcurrency(st.Concurrency)
	sections := make([]models.HomeSection, len(plans))
	// 单行失败不影响其它行：loadSection 自己收敛错误，worker 池只管拉平铺任务
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i := range plans {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			sections[i] = loadSection(ctx, plans[i])
		}(i)
	}
	wg.Wait()

	filled := sections
	if filled == nil {
		filled = []models.HomeSection{}
	}
	homeMu.Lock()
	homeCacheState = &homeCacheEntry{sig: sig, at: time.Now().UnixMilli(), sections: filled}
	homeMu.Unlock()

	out := make([]models.HomeSection, 0, len(filled)+len(missing))
	out = append(out, filled...)
	out = append(out, missing...)
	return out, nil
}

// buildPlan 汇总配置源里的内容位；一条都没配时给默认值。
func buildPlan() ([]plan, []models.HomeSection) {
	enabled := []models.Site{}
	for _, s := range sites.GetSites() {
		if s.Enabled {
			enabled = append(enabled, s)
		}
	}
	plans := []plan{}
	missing := []models.HomeSection{}
	seen := map[string]bool{}

	for _, source := range sources.List() {
		if !source.Enabled {
			continue
		}
		parsed, ok := sources.GetParsed(source.ID)
		if !ok || parsed.Home == nil {
			continue
		}
		for _, raw := range parsed.Home.Sections {
			title := strings.TrimSpace(raw.Title)
			ref := strings.TrimSpace(raw.Site)
			if title == "" && ref == "" {
				continue
			}
			site, ok := matchSite(enabled, ref)
			if !ok {
				missing = append(missing, models.HomeSection{
					Key:    "missing:" + source.ID + ":" + ref,
					Title:  firstNonEmpty(title, ref),
					Origin: "config",
					Items:  []models.Vod{},
					Error:  fmt.Sprintf("配置里写的影视源「%s」不在当前已启用的源里", ref),
				})
				continue
			}
			if !isCMS(site) {
				missing = append(missing, models.HomeSection{
					Key:      "unsupported:" + source.ID + ":" + site.Key,
					Title:    firstNonEmpty(title, site.Name),
					Origin:   "config",
					SiteKey:  site.Key,
					SiteName: site.Name,
					Items:    []models.Vod{},
					Error:    fmt.Sprintf("%s 不是苹果CMS 类型，首页内容位暂时只支持这类源", site.Name),
				})
				continue
			}
			var typeID *string
			if raw.TypeID != nil {
				if s := models.Str(raw.TypeID); s != "" {
					typeID = &s
				}
			}
			key := site.Key + "::" + typeIDOrLatest(typeID)
			if seen[key] {
				continue
			}
			seen[key] = true
			p := plan{key: key, origin: "config", site: site, typeID: typeID}
			switch {
			case title != "":
				p.title = title
			case typeID != nil:
				p.title = site.Name + " · " + *typeID
			default:
				p.title = site.Name + " · 最近更新"
			}
			plans = append(plans, p)
		}
	}

	// 一条内容位都没配时给默认值：每个可用源一条「最近更新」。
	// 这样第一次用的用户打开首页就有东西看，而不是一片空白。
	if len(plans) == 0 {
		for _, site := range enabled {
			if !isCMS(site) {
				continue
			}
			plans = append(plans, plan{
				key:    site.Key + "::latest",
				title:  site.Name + " · 最近更新",
				origin: "default",
				site:   site,
			})
		}
	}
	return plans, missing
}

// loadSection 拉一个内容位；任何异常都收敛成该行的 error 字段。
func loadSection(ctx context.Context, p plan) models.HomeSection {
	base := models.HomeSection{
		Key:      p.key,
		Title:    p.title,
		Origin:   p.origin,
		SiteKey:  p.site.Key,
		SiteName: p.site.Name,
		Items:    []models.Vod{},
	}
	// ac=detail 会连 vod_play_url 一起带回来，点进详情就不用再请求一次
	var requestURL string
	if p.typeID != nil {
		// pg=1：基座 BuildBrowseURL 会把 int pg 拼成空值（models.Str 不认 int），
		// 服务端取默认第 1 页，行为不受影响。
		requestURL = cms.BuildBrowseURL(p.site, "detail", []string{*p.typeID}, 1)
	} else {
		requestURL = cms.BuildURL(p.site, cms.Query{AC: "detail", PG: float64(1)})
	}
	// TS 不传 timeout，走统一网络层默认 20s
	text, err := httputil.FetchText(ctx, requestURL, httputil.Options{Header: sites.ResolveHeader(p.site, requestURL)})
	if err != nil {
		base.Error = err.Error()
		return base
	}
	payload, err := httputil.ParseLooseJSON(text)
	if err != nil {
		base.Error = err.Error()
		return base
	}
	m, _ := payload.(map[string]any)
	if !cms.IsOK(payload) {
		msg := models.Str(models.Get(m, "msg"))
		if msg == "" {
			msg = "code 非成功"
		}
		base.Error = fmt.Sprintf("接口返回异常：%s", msg)
		return base
	}
	raw := models.AsArray(models.Get(m, "list"))
	if len(raw) > homeLimit {
		raw = raw[:homeLimit]
	}
	items := make([]models.Vod, 0, len(raw))
	for _, it := range raw {
		item, _ := it.(map[string]any)
		items = append(items, search.CmsItemToVod(item, p.site))
	}
	if len(items) == 0 {
		base.Error = "这一个内容位没有返回数据"
		return base
	}
	base.Items = items
	return base
}

// matchSite 从配置源的 `home.sections` 里找站点。
//
// `site` 这一项各家配置写得不一样：有的写站点 key，有的写中文名。
// 所以按「key 精确 → 名字精确 → 名字互相包含」三级去找，都找不到就跳过。
func matchSite(list []models.Site, ref string) (models.Site, bool) {
	want := strings.TrimSpace(ref)
	if want == "" {
		return models.Site{}, false
	}
	lower := strings.ToLower(want)
	for _, s := range list {
		if s.Key == want {
			return s, true
		}
	}
	for _, s := range list {
		if s.Name == want {
			return s, true
		}
	}
	for _, s := range list {
		if strings.ToLower(s.Key) == lower || strings.ToLower(s.Name) == lower {
			return s, true
		}
	}
	for _, s := range list {
		if strings.Contains(s.Name, want) || strings.Contains(want, s.Name) {
			return s, true
		}
	}
	return models.Site{}, false
}

// isCMS 只对苹果CMS 类型的站点拉内容：网页规则站的「最近更新」得靠 HTML 规则，暂时不做。
func isCMS(site models.Site) bool {
	return (site.Type == 3 || site.Type == 4) && site.Enabled && site.Unsupported == "" && site.API != ""
}

// typeIDOrLatest 内容位 key 后缀：没配 typeId 时用 latest。
func typeIDOrLatest(typeID *string) string {
	if typeID != nil {
		return *typeID
	}
	return "latest"
}

// firstNonEmpty 返回第一个非空串。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// clampConcurrency 并发数夹在 [1, 32]。
func clampConcurrency(n int) int {
	if n < 1 {
		return 1
	}
	if n > 32 {
		return 32
	}
	return n
}
