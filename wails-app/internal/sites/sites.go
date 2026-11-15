// Package sites 站点聚合层：汇总所有启用订阅源的站点、本地启停覆盖、连通性测试。
// 翻译自 src/main/services/sites.ts。
//
// 作者：Shiro
package sites

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/text/collate"
	"golang.org/x/text/language"

	"tvbox-desktop/internal/cms"
	"tvbox-desktop/internal/httputil"
	"tvbox-desktop/internal/settings"
	"tvbox-desktop/internal/sources"
	"tvbox-desktop/internal/store"
	"tvbox-desktop/models"
)

// storeName 本地覆盖项的持久化 store 名。
const storeName = "sites"

// SiteOverride 站点的本地覆盖项（启停/排序/健康度/请求头/超时/改名）。
type SiteOverride struct {
	// Enabled 本地启停开关；nil 表示未覆盖。
	Enabled *bool `json:"enabled,omitempty"`
	// Order 手动排序位；nil 表示未覆盖。
	Order *int `json:"order,omitempty"`
	// Health 最近一次测活耗时（ms），-1 表示失败；nil 表示未覆盖。
	Health *float64 `json:"health,omitempty"`
	// Header 追加/覆盖的请求头。
	Header map[string]string `json:"header,omitempty"`
	// Timeout 覆盖站点超时（秒）。
	Timeout *float64 `json:"timeout,omitempty"`
	// Name 本地改名。
	Name string `json:"name,omitempty"`
}

// readOverrides 读本地覆盖表。
func readOverrides() map[string]SiteOverride {
	data := store.ReadJSON[map[string]SiteOverride](storeName, nil)
	if data == nil {
		return map[string]SiteOverride{}
	}
	return data
}

// writeOverrides 写本地覆盖表。
func writeOverrides(data map[string]SiteOverride) {
	store.WriteStore(storeName, data)
}

// GetSites 汇总所有启用配置源的站点。
// 去重规则：同名 key 以「配置源列表顺序在前」的为准（后面的源不覆盖前面的）。
func GetSites() []models.Site {
	srcList := sources.List()
	overrides := readOverrides()
	seen := map[string]bool{}
	out := []models.Site{}

	for _, source := range srcList {
		if !source.Enabled {
			continue
		}
		parsed, ok := sources.GetParsed(source.ID)
		if !ok {
			continue
		}
		for _, site := range parsed.Sites {
			if seen[site.Key] {
				continue
			}
			seen[site.Key] = true
			merged := site
			if ov, has := overrides[site.Key]; has {
				if ov.Enabled != nil {
					merged.Enabled = *ov.Enabled
				}
				if ov.Name != "" {
					merged.Name = ov.Name
				}
				if ov.Timeout != nil && *ov.Timeout != 0 {
					merged.Timeout = ov.Timeout
				}
				if ov.Header != nil {
					h := map[string]string{}
					for k, v := range merged.Header {
						h[k] = v
					}
					for k, v := range ov.Header {
						h[k] = v
					}
					merged.Header = h
				}
				if ov.Health != nil {
					merged.Health = ov.Health
				}
			}
			out = append(out, merged)
		}
	}

	// 排序：手动 order 优先，其余按中文名拼音（对应 localeCompare 'zh-Hans-CN'）
	coll := collate.New(language.Make("zh-Hans-CN"))
	orderOf := func(key string) int64 {
		if ov, has := overrides[key]; has && ov.Order != nil {
			return int64(*ov.Order)
		}
		return math.MaxInt64
	}
	sort.SliceStable(out, func(i, j int) bool {
		oa, ob := orderOf(out[i].Key), orderOf(out[j].Key)
		if oa != ob {
			return oa < ob
		}
		return coll.CompareString(out[i].Name, out[j].Name) < 0
	})
	return out
}

// GetSite 按 key 取一个站点。
func GetSite(key string) (models.Site, bool) {
	for _, s := range GetSites() {
		if s.Key == key {
			return s, true
		}
	}
	return models.Site{}, false
}

// SetEnabled 覆盖单个站点的启停。
func SetEnabled(key string, enabled bool) {
	data := readOverrides()
	ov := data[key]
	ov.Enabled = &enabled
	data[key] = ov
	writeOverrides(data)
}

// SetAllEnabled 批量覆盖站点启停。
func SetAllEnabled(keys []string, enabled bool) {
	data := readOverrides()
	for _, key := range keys {
		ov := data[key]
		ov.Enabled = &enabled
		data[key] = ov
	}
	writeOverrides(data)
}

// ResolveHeader 合并全局 headers 规则与站点自身请求头。
func ResolveHeader(site models.Site, rawURL string) map[string]string {
	var rules []models.HeaderRule
	for _, source := range sources.List() {
		if !source.Enabled {
			continue
		}
		if parsed, ok := sources.GetParsed(source.ID); ok {
			rules = append(rules, parsed.Headers...)
		}
	}
	out := map[string]string{"User-Agent": settings.Get().UserAgent}
	for k, v := range httputil.MatchHeaderRules(rawURL, rules) {
		out[k] = v
	}
	for k, v := range site.Header {
		out[k] = v
	}
	return out
}

var (
	searchPlaceholderRe = regexp.MustCompile(`(?i)\{(wd|kw|key|keyword)\}`)
	httpTargetRe        = regexp.MustCompile(`(?i)^https?://`)
)

// Test 站点连通性测试：发一次最小请求，能解析出结果就算通。
func Test(ctx context.Context, key string) (models.TestResult, error) {
	site, found := GetSite(key)
	if !found {
		return models.TestResult{OK: false, Ms: 0, Message: "站点不存在"}, nil
	}
	if site.Unsupported != "" {
		return models.TestResult{OK: false, Ms: 0, Message: site.Unsupported}, nil
	}
	if site.API == "" {
		return models.TestResult{OK: false, Ms: 0, Message: "站点未配置接口地址"}, nil
	}

	st := settings.Get()
	timeout := st.Timeout
	if site.Timeout != nil && *site.Timeout != 0 {
		timeout = *site.Timeout
	}
	start := time.Now()
	elapsed := func() int64 { return time.Since(start).Milliseconds() }
	fail := func(ms int64, msg string) (models.TestResult, error) {
		saveHealth(key, -1)
		return models.TestResult{OK: false, Ms: ms, Message: msg}, nil
	}

	switch site.Type {
	case 3, 4:
		u := cms.BuildURL(site, cms.Query{AC: "detail", PG: 1})
		header := ResolveHeader(site, u)
		text, err := httputil.FetchText(ctx, u, httputil.Options{Header: header, Timeout: timeout})
		if err != nil {
			return fail(elapsed(), err.Error())
		}
		payload, err := httputil.ParseLooseJSON(text)
		if err != nil {
			return fail(elapsed(), err.Error())
		}
		if !cms.IsOK(payload) {
			msg := "code 非成功"
			if m, ok := payload.(map[string]any); ok && m["msg"] != nil {
				if s := models.Str(m["msg"]); s != "" {
					msg = s
				}
			}
			return models.TestResult{OK: false, Ms: elapsed(), Message: fmt.Sprintf("接口返回异常：%s", msg)}, nil
		}
		var list []any
		if m, ok := payload.(map[string]any); ok {
			list = models.AsArray(m["list"])
		}
		if len(list) == 0 {
			return models.TestResult{OK: false, Ms: elapsed(), Message: "接口连通但未返回数据"}, nil
		}
		sample := ""
		if first, ok := list[0].(map[string]any); ok {
			sample = models.Str(first["vod_name"])
		}
		saveHealth(key, elapsed())
		return models.TestResult{OK: true, Ms: elapsed(), Message: fmt.Sprintf("正常，返回 %d 条", len(list)), Sample: sample}, nil

	case 1:
		// 网页规则站：请求搜索地址，再用 rules.list 选择器验证页面结构是否还认得出来。
		// 注意 rules.list 是 CSS 选择器而不是 URL，不能拿它去发请求。
		selector := ""
		template := strings.TrimSpace(site.API)
		if site.Rules != nil {
			selector = strings.TrimSpace(site.Rules.List)
			if s := strings.TrimSpace(site.Rules.Search); s != "" {
				template = s
			}
		}
		target := searchPlaceholderRe.ReplaceAllString(template, encodeComponent("测试"))
		if !httpTargetRe.MatchString(target) {
			return models.TestResult{OK: false, Ms: 0, Message: "网页规则站需要一个 http(s) 地址，当前地址不可请求"}, nil
		}
		header := ResolveHeader(site, target)
		text, err := httputil.FetchText(ctx, target, httputil.Options{Header: header, Timeout: timeout})
		if err != nil {
			return fail(elapsed(), err.Error())
		}
		if utf8.RuneCountInString(text) < 50 {
			return models.TestResult{OK: false, Ms: elapsed(), Message: "页面内容过短，可能被拦截"}, nil
		}
		if selector != "" {
			doc, err := goquery.NewDocumentFromReader(strings.NewReader(text))
			if err != nil {
				return fail(elapsed(), err.Error())
			}
			count := doc.Find(selector).Length()
			if count == 0 {
				return models.TestResult{
					OK:      false,
					Ms:      elapsed(),
					Message: fmt.Sprintf("页面可访问，但列表选择器「%s」没有匹配到节点，规则可能已失效", selector),
				}, nil
			}
			saveHealth(key, elapsed())
			return models.TestResult{OK: true, Ms: elapsed(), Message: fmt.Sprintf("正常，匹配到 %d 个列表节点", count)}, nil
		}
		saveHealth(key, elapsed())
		return models.TestResult{OK: true, Ms: elapsed(), Message: fmt.Sprintf("正常，页面 %d 字节", len(text))}, nil
	}

	return models.TestResult{OK: false, Ms: 0, Message: "该类型暂不支持测试"}, nil
}

// saveHealth 记录测活耗时到本地覆盖表。
func saveHealth(key string, ms int64) {
	data := readOverrides()
	ov := data[key]
	h := float64(ms)
	ov.Health = &h
	data[key] = ov
	writeOverrides(data)
}

// encodeComponent encodeURIComponent 语义（空格转 %20 而非 +）。
func encodeComponent(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// SetOrder 覆盖站点的手动排序位。
func SetOrder(key string, order int) {
	data := readOverrides()
	ov := data[key]
	ov.Order = &order
	data[key] = ov
	writeOverrides(data)
}
