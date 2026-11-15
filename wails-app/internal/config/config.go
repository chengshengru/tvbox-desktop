// Package config TVBox 配置 JSON 解析层：同时吃 TVBox 兼容方言与本项目简洁方言。
// 设计原则是「只做覆盖不做新增语义」——扩展字段对安卓 TVBox 无副作用。
// 翻译自 src/main/services/config.ts。
//
// 作者：Shiro
package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"

	"tvbox-desktop/internal/httputil"
	"tvbox-desktop/models"
)

// stringTypeMap 字符串类型别名 → 数字类型（与 TS STRING_TYPE_MAP 一致）。
var stringTypeMap = map[string]int{
	"cms":       3,
	"maccms":    3,
	"apple":     3,
	"apples":    3,
	"苹果cms":     3,
	"api":       4,
	"json":      4,
	"html":      1,
	"web":       1,
	"rule":      1,
	"rules":     1,
	"page":      1,
	"spider":    0,
	"jar":       0,
	"aggregate": 0,
	"other":     2,
}

var (
	httpsRe = regexp.MustCompile(`(?i)^https?://`)
	// schemeRe 忠实保留 TS 原文笔误：`(proxy|file|assets|clan|to)`? 里的反引号+? 使反引号可选。
	schemeRe      = regexp.MustCompile("(?i)^(proxy|file|assets|clan|to)`?://")
	bareDomainRe  = regexp.MustCompile(`(?i)^[\w.-]+\.[a-z]{2,}`)
	cmsAPIRe      = regexp.MustCompile(`(?i)provide/vod|api\.php|/vod/`)
	unsupportedRe = regexp.MustCompile(`(?i)^(proxy|assets|file|clan|to):`)
)

// Defaults 站点归一化时的全局默认值（超时、请求头）。
type Defaults struct {
	// Timeout 全局默认超时（秒），0 表示未提供。
	Timeout float64
	// Header 全局默认请求头。
	Header map[string]string
}

// NormalizeContext 站点归一化上下文：来源标识 + 全局默认值。
type NormalizeContext struct {
	// SourceID 订阅源 id，写进每个站点的 SourceID。
	SourceID string
	// SourceName 订阅源名，写进每个站点的 SourceName，也是配置名兜底。
	SourceName string
	// Defaults 全局默认值，可为 nil。
	Defaults *Defaults
}

// defaults 取上下文里的默认值（nil 安全）。
func (c NormalizeContext) defaults() Defaults {
	if c.Defaults == nil {
		return Defaults{}
	}
	return *c.Defaults
}

// shortHash 稳定短哈希，用于在没有 key 时生成站点标识。
// 按 UTF-16 码元迭代并做 uint32 溢出乘法，与 JS 的 charCodeAt + Math.imul 逐位等价。
func shortHash(input string) string {
	units := utf16.Encode([]rune(input))
	var h uint32 = 0x811c9dc5
	for _, u := range units {
		h ^= uint32(u)
		h *= 0x01000193
	}
	return strconv.FormatUint(uint64(h), 36)
}

// normalizeApi 把站点接口地址补全成可请求形态。
func normalizeApi(raw string) string {
	api := strings.TrimSpace(raw)
	if api == "" {
		return ""
	}
	if httpsRe.MatchString(api) {
		return api
	}
	if schemeRe.MatchString(api) {
		return api
	}
	// 形如 cms1.example.com/api.php/provide/vod/ 的裸域名
	if bareDomainRe.MatchString(api) {
		return "https://" + api
	}
	return api
}

// toNumber 判断 JSON 值是否「数字」并取值（float64/int/json.Number；字符串不算，对齐 typeof number）。
func toNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

// normalizeType 归一化站点类型：数字 0-4 直用；字符串查别名表；完全没有类型信息时才按内容猜测。
func normalizeType(raw any, api string, rules *models.HtmlRules) int {
	if n, ok := toNumber(raw); ok {
		if n == math.Trunc(n) && n >= 0 && n <= 4 {
			return int(n)
		}
	}
	if s, ok := raw.(string); ok {
		key := strings.ToLower(strings.TrimSpace(s))
		if key != "" {
			if mapped, ok := stringTypeMap[key]; ok {
				return mapped
			}
			if f, err := strconv.ParseFloat(key, 64); err == nil && f == math.Trunc(f) && f >= 0 && f <= 4 {
				return int(f)
			}
			// 配置里明确写了类型但我们不认识：如实标成「未知」，
			// 不要偷偷降级成 type=4（CMS 变体），否则 UI 会给出错误的可用性承诺。
			return 2
		}
	}
	// 完全没有类型信息时才按内容猜测
	if rules != nil && (rules.List != "" || rules.Search != "") {
		return 1
	}
	if cmsAPIRe.MatchString(api) {
		return 3
	}
	if api != "" {
		return 4
	}
	return 2
}

// flagVal 宽松布尔位：undefined/null 用 fallback；布尔直用；数字按 !=0；其余按 true/yes/1 字符串。
func flagVal(m map[string]any, key string, fallback bool) bool {
	v := m[key]
	if v == nil {
		return fallback
	}
	if b, ok := v.(bool); ok {
		return b
	}
	if n, ok := toNumber(v); ok {
		return n != 0
	}
	// JS 的 Number("") === 0、Number("  ") === 0：空串按数字 0 处理（结果为 false），
	// 与 ParseFloat 失败后落入字符串比较殊途同归，这里显式对齐。
	s := strings.TrimSpace(models.Str(v))
	if s == "" {
		return false
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return n != 0
	}
	lower := strings.ToLower(s)
	return lower == "true" || lower == "yes" || lower == "1"
}

// normalizeRules 归一化手写网页规则；全空时返回 nil。
func normalizeRules(raw any) *models.HtmlRules {
	r, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if s, ok := r[k].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
		return ""
	}
	rules := &models.HtmlRules{
		List:          pick("list", "vodList", "listRule"),
		Name:          pick("name", "vodName", "title"),
		Link:          pick("link", "vodLink", "href"),
		Pic:           pick("pic", "vodPic", "image"),
		Remarks:       pick("remarks", "vodRemarks", "note"),
		Search:        pick("search", "searchUrl"),
		DetailName:    pick("detailName"),
		DetailContent: pick("detailContent", "content"),
		PlayList:      pick("playList", "playFrom"),
		PlayItem:      pick("playItem", "playUrl"),
	}
	// 只要有一条非空就保留，否则视为无规则
	if rules.List == "" && rules.Name == "" && rules.Link == "" && rules.Pic == "" &&
		rules.Remarks == "" && rules.Search == "" && rules.DetailName == "" &&
		rules.DetailContent == "" && rules.PlayList == "" && rules.PlayItem == "" {
		return nil
	}
	return rules
}

// nullish 模拟 JS 的 ?? 链：返回第一个非 nil 值。
func nullish(vals ...any) any {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

// jsString 模拟 `x ? String(x) : undefined`：假值（null/false/0/NaN/空串）返回空串。
func jsString(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case bool:
		if !x {
			return ""
		}
		return "true"
	case float64:
		if x == 0 || math.IsNaN(x) {
			return ""
		}
	case json.Number:
		if f, err := x.Float64(); err == nil && (f == 0 || math.IsNaN(f)) {
			return ""
		}
	case string:
		return x
	}
	return models.Str(v)
}

// jsonString 等价 JSON.stringify：紧凑输出且不转义 HTML（json.Encoder 默认会转义 <>&）。
func jsonString(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimRight(buf.String(), "\n")
}

// mergeHeader 合并默认请求头与对象里的 header 字段（值按 Str 宽松转字符串）。
func mergeHeader(base map[string]string, raw any) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	if m, ok := raw.(map[string]any); ok {
		for k, v := range m {
			if v == nil {
				continue
			}
			out[k] = models.Str(v)
		}
	}
	return out
}

// extValue 取 raw.ext（对象形态）里的字段；ext 是字符串/缺失时返回 nil。
func extValue(raw map[string]any, key string) any {
	if em, ok := raw["ext"].(map[string]any); ok {
		return em[key]
	}
	return nil
}

// NormalizeSite 把一条站点配置归一化成内部结构；无法识别时返回 nil。
func NormalizeSite(input any, ctx NormalizeContext) *models.Site {
	if input == nil {
		return nil
	}
	raw, ok := input.(map[string]any)
	if !ok {
		return nil
	}
	// TS: String(raw.name ?? raw.title ?? '').trim()——?? 只跳过 null/undefined，空串保留
	name := strings.TrimSpace(models.Str(nullish(raw["name"], raw["title"])))
	api := normalizeApi(models.Str(nullish(raw["api"], raw["url"], extValue(raw, "api"))))
	if name == "" && api == "" {
		return nil
	}

	rules := normalizeRules(nullish(raw["rules"], extValue(raw, "rules")))
	typ := normalizeType(raw["type"], api, rules)
	key := strings.TrimSpace(models.Str(nullish(raw["key"], raw["id"], "csp_"+shortHash(name+"|"+api))))

	searchable := flagVal(raw, "searchable", true)
	// ext：字符串原样，对象转 JSON 串，其余（含 nil/假值）不保留
	var extStr string
	switch e := raw["ext"].(type) {
	case string:
		extStr = e
	case nil:
	default:
		if truthyExt(raw["ext"]) {
			extStr = jsonString(raw["ext"])
		}
	}
	var jarStr string
	if j, ok := raw["jar"].(string); ok {
		jarStr = j
	}

	categories := []string(nil)
	if arr, ok := raw["categories"].([]any); ok {
		categories = models.StrSlice(arr)
	}

	var style *models.SiteStyle
	if sm, ok := raw["style"].(map[string]any); ok {
		style = &models.SiteStyle{Type: models.Str(sm["type"])}
		if f, ok := toNumber(sm["ratio"]); ok && f != 0 {
			ratio := f
			style.Ratio = &ratio
		} else if s, ok := sm["ratio"].(string); ok {
			if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil && f != 0 {
				ratio := f
				style.Ratio = &ratio
			}
		}
	}

	header := mergeHeader(ctx.defaults().Header, raw["header"])
	if len(header) == 0 {
		header = nil
	}

	// timeout：Number(raw.timeout) || ctx.defaults.timeout
	var timeout *float64
	if f, ok := numberLoose(raw["timeout"]); ok && f != 0 {
		timeout = &f
	} else if d := ctx.defaults().Timeout; d != 0 {
		timeout = &d
	}

	playerType := 1
	if f, ok := numberLoose(nullish(raw["playerType"], 1)); ok && int(f) != 0 {
		playerType = int(f)
	}

	site := &models.Site{
		Key:         key,
		Name:        firstNonEmpty(name, key),
		Type:        typ,
		API:         api,
		Ext:         extStr,
		Jar:         jarStr,
		Searchable:  searchable,
		QuickSearch: flagVal(raw, "quickSearch", searchable),
		Filterable:  flagVal(raw, "filterable", typ == 3 || typ == 4),
		PlayerType:  playerType,
		Categories:  categories,
		Style:       style,
		Header:      header,
		Timeout:     timeout,
		SourceID:    ctx.SourceID,
		SourceName:  ctx.SourceName,
		Enabled:     flagVal(raw, "enabled", true),
		Rules:       rules,
	}

	// 桌面端不支持的能力，显式标注原因，UI 上灰显而不是静默丢弃
	if site.API == "" || unsupportedRe.MatchString(site.API) {
		site.Unsupported = "该站点依赖内置 Spider 或本地资源，桌面端无法直接请求"
		site.Searchable = false
		site.QuickSearch = false
	} else if typ == 0 {
		site.Unsupported = "需要配套 Spider（jar/dex），桌面端无法运行"
		site.Searchable = false
		site.QuickSearch = false
	} else if typ == 2 {
		site.Unsupported = "未知站点类型，暂不支持"
		site.Searchable = false
		site.QuickSearch = false
	}
	return site
}

// truthyExt 判断 ext 对象是否 JS 真值（空对象 {} 在 JS 里为真，照旧序列化）。
func truthyExt(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x != 0 && !math.IsNaN(x)
	case json.Number:
		f, err := x.Float64()
		return err == nil && f != 0
	case string:
		return x != ""
	}
	return v != nil
}

// numberLoose 宽松取数字：数字直取，字符串按 ParseFloat（对齐 JS Number(string)）。
func numberLoose(v any) (float64, bool) {
	if n, ok := toNumber(v); ok {
		return n, true
	}
	if s, ok := v.(string); ok {
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		return f, err == nil
	}
	return 0, false
}

// firstNonEmpty 模拟 a || b。
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// normalizeParses 归一化解析接口列表。
func normalizeParses(raw any) []models.ParseItem {
	out := []models.ParseItem{}
	arr, ok := raw.([]any)
	if !ok {
		return out
	}
	for _, item := range arr {
		p, ok := item.(map[string]any)
		if !ok {
			continue
		}
		url := strings.TrimSpace(models.Str(p["url"]))
		if url == "" {
			continue
		}
		name := "未命名解析"
		if p["name"] != nil {
			name = models.Str(p["name"])
		}
		typ := 1
		if f, ok := numberLoose(nullish(p["type"], 1)); ok && int(f) != 0 {
			typ = int(f)
		}
		out = append(out, models.ParseItem{Name: name, Type: typ, URL: url, Ext: p["ext"]})
	}
	return out
}

// normalizeLives 归一化直播源列表：字符串项直接当地址；本地协议前缀的项丢弃。
func normalizeLives(raw any) []models.LiveSource {
	out := []models.LiveSource{}
	arr, ok := raw.([]any)
	if !ok {
		return out
	}
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, models.LiveSource{Name: "直播源", Type: 0, URL: s})
			continue
		}
		l, ok := item.(map[string]any)
		if !ok {
			continue
		}
		url := strings.TrimSpace(models.Str(l["url"]))
		if url == "" || unsupportedRe.MatchString(url) {
			continue
		}
		name := "直播源"
		if l["name"] != nil {
			name = models.Str(l["name"])
		}
		typ := l["type"]
		if typ == nil {
			typ = 0
		}
		out = append(out, models.LiveSource{Name: name, Type: typ, URL: url, EPG: jsString(l["epg"])})
	}
	return out
}

// normalizeHeaderRules 归一化全局请求头规则。
func normalizeHeaderRules(raw any) []models.HeaderRule {
	out := []models.HeaderRule{}
	arr, ok := raw.([]any)
	if !ok {
		return out
	}
	for _, item := range arr {
		h, ok := item.(map[string]any)
		if !ok {
			continue
		}
		host := jsString(h["host"])
		hm, ok := h["header"].(map[string]any)
		if host == "" || !ok || len(hm) == 0 {
			continue
		}
		header := map[string]string{}
		for k, v := range hm {
			header[k] = models.Str(v)
		}
		out = append(out, models.HeaderRule{Host: models.Str(h["host"]), Header: header})
	}
	return out
}

// asMap 宽松取 map（nil 安全）。
func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

// decodeInto JSON 往返转结构体（等价 TS 的类型断言 + 后续按字段读取）。
func decodeInto[T any](v any) *T {
	if v == nil {
		return nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var t T
	if err := json.Unmarshal(data, &t); err != nil {
		return nil
	}
	return &t
}

// ParseConfigWith 把任意一份配置 JSON 归一化成内部结构（带完整归一化上下文）。
func ParseConfigWith(rawInput any, ctx NormalizeContext) (*models.ParsedConfig, error) {
	raw := asMap(rawInput)
	ext := asMap(raw["tvboxDesktop"])
	settingsRaw := asMap(raw["settings"])

	defaults := &Defaults{}
	// timeout: Number(ext.timeout ?? settings.timeout ?? ctx.defaults.timeout) || undefined
	t := nullish(ext["timeout"], settingsRaw["timeout"], ctx.defaults().Timeout)
	if n, ok := numberLoose(t); ok && n != 0 {
		defaults.Timeout = n
	}
	defaults.Header = mergeHeader(ctx.defaults().Header, raw["header"])

	sitesRaw, _ := raw["sites"].([]any)
	seen := map[string]bool{}
	sites := []models.Site{}
	for _, item := range sitesRaw {
		site := NormalizeSite(item, NormalizeContext{
			SourceID:   ctx.SourceID,
			SourceName: ctx.SourceName,
			Defaults:   defaults,
		})
		if site == nil {
			continue
		}
		if seen[site.Key] {
			continue
		}
		seen[site.Key] = true
		sites = append(sites, *site)
	}

	name := "未命名配置"
	if v := nullish(raw["name"], ext["name"], ctx.SourceName); v != nil {
		name = models.Str(v)
	}

	flags := []string{}
	if arr, ok := raw["flags"].([]any); ok {
		flags = models.StrSlice(arr)
		if flags == nil {
			flags = []string{}
		}
	}

	notice := ""
	if v := raw["notice"]; v != nil {
		notice = jsString(v)
	}

	return &models.ParsedConfig{
		Name:           name,
		Notice:         notice,
		Sites:          sites,
		Parses:         normalizeParses(raw["parses"]),
		Lives:          normalizeLives(raw["lives"]),
		Flags:          flags,
		Headers:        normalizeHeaderRules(nullish(raw["headers"], settingsRaw["headers"])),
		Theme:          decodeInto[models.ThemeConfig](nullish(raw["theme"], ext["theme"])),
		ExternalPlayer: decodeInto[models.ExternalPlayerConfig](nullish(raw["externalPlayer"], ext["externalPlayer"])),
		Home:           decodeInto[models.HomeConfig](nullish(raw["home"], ext["home"])),
		Raw:            rawInput,
	}, nil
}

// LoadConfigSourceWithTimeout 拉取（或读取本地）一份配置源，返回原始 JSON。
// kind: "file" 读本地文件（剥 BOM 后宽松解析），其余按远程 URL 拉取。
func LoadConfigSourceWithTimeout(ctx context.Context, rawURL string, kind string, timeout float64) (any, error) {
	if kind == "file" {
		data, err := os.ReadFile(rawURL)
		if err != nil {
			return nil, fmt.Errorf("读取本地配置文件失败：%s", rawURL)
		}
		text := strings.TrimPrefix(string(data), "\ufeff")
		return httputil.ParseLooseJSON(text)
	}
	return httputil.FetchJSON(ctx, rawURL, httputil.Options{Timeout: timeout})
}

// ParseConfig 便捷入口：仅按配置名解析（订阅源场景请用 ParseConfigWith 以带上 sourceId）。
func ParseConfig(raw any, name string) (*models.ParsedConfig, error) {
	return ParseConfigWith(raw, NormalizeContext{SourceName: name})
}

// LoadConfigSource 便捷入口：默认 20s 超时。
func LoadConfigSource(ctx context.Context, rawURL string, kind string) (any, error) {
	return LoadConfigSourceWithTimeout(ctx, rawURL, kind, 20)
}
