// Package cms 苹果CMS(MacCMS V10) 接口地址构造与响应判活。
// 同时支持「基地址 + 参数」和「模板地址（含 {ac} {wd} {pg} {t} {ids} 占位符）」两种写法。
// 翻译自 src/main/services/cms.ts。
//
// 作者：Shiro
package cms

import (
	"net/url"
	"regexp"
	"strings"

	"tvbox-desktop/models"
)

// Query 一次 CMS 请求的参数。nil 表示未提供（对齐 TS 的 undefined 语义）。
type Query struct {
	// AC: list / detail / videolist
	AC string
	WD *string
	PG any
	T  any
	ID any // 对应 TS 的 ids
	H  string
}

type placeholder struct {
	re  *regexp.Regexp
	get func(q Query) any
}

var placeholders = []placeholder{
	{regexp.MustCompile(`\{ac\}`), func(q Query) any { return q.AC }},
	{regexp.MustCompile(`\{(wd|kw|key|keyword)\}`), func(q Query) any { return q.WD }},
	{regexp.MustCompile(`\{(pg|page)\}`), func(q Query) any { return q.PG }},
	{regexp.MustCompile(`\{(t|type|typeid|class)\}`), func(q Query) any { return q.T }},
	{regexp.MustCompile(`\{(ids|id)\}`), func(q Query) any { return q.ID }},
}

// encodeVal encodeURIComponent 语义（空格转 %20 而非 +）。
func encodeVal(v any) string {
	s := models.Str(v)
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

var idsParamRe = regexp.MustCompile(`[?&]ids=`)

// BuildURL 按站点 api 模板/基地址两种模式拼出请求地址。
func BuildURL(site models.Site, q Query) string {
	api := site.API
	templated := false
	u := api

	for _, p := range placeholders {
		if p.re.MatchString(u) {
			templated = true
			value := deref(p.get(q))
			repl := ""
			if value != nil && !isNilPtr(value) {
				repl = encodeVal(value)
			}
			// 用 Expand 语义的空替换函数避免 $ 被当组引用；repl 已 URL 编码不含 $，直接 ReplaceAllLiteralString 最稳
			u = p.re.ReplaceAllLiteralString(u, repl)
		}
	}

	if templated {
		out := strings.ReplaceAll(u, "{h}", q.H)
		// 模板里没有 {ids} 槽位时，详情请求还是要能带上 ids：
		// 只替换不追加的话，`?ac={ac}&t={t}&pg={pg}&wd={wd}` 这种模板
		// 永远取不到详情页（实测桩上就是这个现象——点进去一直解析不出剧集）。
		if q.ID != nil && !isNilPtr(q.ID) && !idsParamRe.MatchString(out) {
			sep := "?"
			if strings.Contains(out, "?") {
				sep = "&"
				if strings.HasSuffix(out, "?") || strings.HasSuffix(out, "&") {
					sep = ""
				}
			}
			out = out + sep + "ids=" + encodeVal(q.ID)
		}
		return out
	}

	// 基地址模式：安全拼接查询串
	var parts []string
	if q.AC != "" {
		parts = append(parts, "ac="+encodeVal(q.AC))
	}
	if q.WD != nil {
		parts = append(parts, "wd="+encodeVal(*q.WD))
	}
	if q.PG != nil && !isNilPtr(q.PG) {
		parts = append(parts, "pg="+encodeVal(q.PG))
	}
	if q.T != nil && !isNilPtr(q.T) && models.Str(q.T) != "" {
		parts = append(parts, "t="+encodeVal(q.T))
	}
	if q.ID != nil && !isNilPtr(q.ID) {
		parts = append(parts, "ids="+encodeVal(q.ID))
	}
	qs := strings.Join(parts, "&")
	if qs == "" {
		return u
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
		if strings.HasSuffix(u, "?") || strings.HasSuffix(u, "&") {
			sep = ""
		}
	}
	return u + sep + qs
}

// isNilPtr any 里装的是 nil 指针时判空（简化版：只处理常见类型）。
func isNilPtr(v any) bool {
	switch x := v.(type) {
	case *string:
		return x == nil
	case *float64:
		return x == nil
	case *int:
		return x == nil
	}
	return false
}

// deref 解引用 *string（{wd} 槽位传的是 Query.WD *string，不解引用会替换成空串）。
func deref(v any) any {
	if p, ok := v.(*string); ok {
		if p == nil {
			return nil
		}
		return *p
	}
	return v
}

// Str 便捷构造 *string（查询参数用）。
func Str(s string) *string { return &s }

var browseTemplatedRe = regexp.MustCompile(`\{(ac|wd|kw|key|keyword|pg|page|t|type|typeid|class|ids|id|h)\}`)

// BuildBrowseURL 分类浏览用的地址，和 BuildURL 的区别是 t 可以传多个。
//
// 苹果CMS 把同一个大类拆成一堆叶子分类（电影 → 动作片/喜剧片/爱情片…），
// 而父分类 id 查不出任何东西（实测 t=1 返回 0 条），只能落到叶子上。
// 注意：不要指望把多个叶子 id 并列进同一次请求——实测重复的 t 参数是
// 「最后一个生效」而不是 OR。所以调用方（category 包）对每个子分类单独发一次
// 请求，拿回来自己合并；这里传多个 id 只是给模板式站点留个兜底。
// 模板式站点只有一个 {t} 槽位，塞不下多个值，只取第一个叶子。
func BuildBrowseURL(site models.Site, ac string, types []string, pg int) string {
	api := site.API
	// 只要地址里有任何占位符就算模板式。不能只认 {t}：像
	// `...?ac={ac}&pg={pg}&wd={wd}` 这种没有 {t} 的模板，如果走下面的「追加参数」
	// 分支，模板里的字面量会和新参数同时留在 URL 上，服务端读到哪一个完全看它先取哪个，等于随机。
	if browseTemplatedRe.MatchString(api) {
		t := ""
		if len(types) > 0 {
			t = types[0]
		}
		return BuildURL(site, Query{AC: ac, T: t, PG: pg})
	}
	parts := []string{"ac=" + encodeVal(ac)}
	for _, t := range types {
		if t != "" {
			parts = append(parts, "t="+encodeVal(t))
		}
	}
	parts = append(parts, "pg="+encodeVal(pg))
	qs := strings.Join(parts, "&")
	sep := "?"
	if strings.Contains(api, "?") {
		sep = "&"
		if strings.HasSuffix(api, "?") || strings.HasSuffix(api, "&") {
			sep = ""
		}
	}
	return api + sep + qs
}

// IsOK 归一化苹果CMS 返回，容忍 code 为 1 / "1" / "ok" 等写法。
func IsOK(payload any) bool {
	m, ok := payload.(map[string]any)
	if !ok {
		return false
	}
	code, exists := m["code"]
	if !exists || code == nil {
		return true
	}
	s := strings.ToLower(models.Str(code))
	if s == "" {
		return true
	}
	switch s {
	case "1", "true", "ok", "success", "200":
		return true
	}
	return false
}
