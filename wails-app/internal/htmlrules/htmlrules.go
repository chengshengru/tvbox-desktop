// Package htmlrules 网页规则站的取值语法（goquery 版），刻意与 TVBox 的 html 规则保持一致：
//
//	".title a&&href"       取 .title a 的 href
//	".title&&Text"         取文本
//	".pic&&data-src"       取 data-src 属性
//	".a&&href||.b&&Text"   前者取不到时用后者
//
// `&&` 后面用逗号分隔（如 "Text,href"）表示取不到文本就退到 href，
// 因为网上流传的规则两种写法都有。
// 翻译自 src/main/services/htmlRules.ts。
//
// 作者：Shiro
package htmlrules

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
)

// RulePart 一条规则拆出来的候选分支。
type RulePart struct {
	// Selector CSS 选择器部分。
	Selector string `json:"selector"`
	// Args 要尝试的属性，按顺序取第一个非空值；Text/Html 是伪属性。
	Args []string `json:"args"`
}

var (
	wsRe   = regexp.MustCompile(`\s+`)
	httpRe = regexp.MustCompile(`(?i)^https?://`)
	dataRe = regexp.MustCompile(`(?i)^data:`)
)

// ParseRule 把一条规则串拆成若干候选分支。
func ParseRule(rule string) []RulePart {
	if strings.TrimSpace(rule) == "" {
		return nil
	}
	var out []RulePart
	for _, part := range strings.Split(rule, "||") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var segs []string
		for _, seg := range strings.Split(part, "&&") {
			seg = strings.TrimSpace(seg)
			if seg != "" {
				segs = append(segs, seg)
			}
		}
		rp := RulePart{}
		if len(segs) > 0 {
			rp.Selector = segs[0]
		}
		if len(segs) > 1 {
			for _, seg := range segs[1:] {
				for _, a := range strings.Split(seg, ",") {
					t := strings.TrimSpace(a)
					if t != "" {
						rp.Args = append(rp.Args, t)
					}
				}
			}
		}
		out = append(out, rp)
	}
	return out
}

// RuleSelector 只取选择器部分，用于「先选出元素再逐个取值」的场景（剧集列表）。
func RuleSelector(rule string) string {
	parts := ParseRule(rule)
	if len(parts) == 0 {
		return ""
	}
	return parts[0].Selector
}

// Absolutize 把可能是相对路径的地址补全成绝对地址。
func Absolutize(rawURL string, base string) string {
	value := strings.TrimSpace(rawURL)
	if value == "" {
		return ""
	}
	if httpRe.MatchString(value) || dataRe.MatchString(value) {
		return value
	}
	if strings.HasPrefix(value, "//") {
		return "https:" + value
	}
	u, err := url.Parse(value)
	if err != nil {
		return value
	}
	b, err := url.Parse(base)
	if err != nil || !b.IsAbs() {
		// JS 的 new URL(value, base) 在 base 非法时会抛错并退回原值
		return value
	}
	return b.ResolveReference(u).String()
}

// readArg 从元素上按单个伪属性/属性名取值。
func readArg(doc *goquery.Document, el *goquery.Selection, arg string, base string) string {
	key := strings.TrimSpace(arg)
	lower := strings.ToLower(key)
	switch lower {
	case "text", "textnodes":
		return strings.TrimSpace(wsRe.ReplaceAllString(el.Text(), " "))
	case "html", "innerhtml":
		h, _ := el.Html()
		return strings.TrimSpace(h)
	case "outerhtml":
		return strings.TrimSpace(outerHTML(el))
	}
	// 属性名大小写敏感的场景极少，两种都试一遍
	raw, ok := el.Attr(key)
	if !ok {
		raw, _ = el.Attr(lower)
	}
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if lower == "href" || lower == "src" || strings.HasSuffix(lower, "src") || strings.HasSuffix(lower, "-url") {
		return Absolutize(value, base)
	}
	return value
}

// outerHTML 序列化选择器命中的全部节点（等价 cheerio 的 $.html(el)）。
func outerHTML(el *goquery.Selection) string {
	var b strings.Builder
	el.Each(func(_ int, s *goquery.Selection) {
		if len(s.Nodes) > 0 {
			_ = html.Render(&b, s.Nodes[0])
		}
	})
	return b.String()
}

// Extract 从一个元素上按规则取值。
// fallback 为规则里没写属性时的默认候选（例如链接默认取 href），不传时等价 TS 默认值 ["Text"]。
func Extract(doc *goquery.Document, el *goquery.Selection, rule string, base string, fallback ...string) string {
	def := []string{"Text"}
	if len(fallback) > 0 {
		def = fallback
	}
	branches := ParseRule(rule)
	if len(branches) == 0 {
		branches = []RulePart{{Selector: "", Args: def}}
	}
	for _, part := range branches {
		var target *goquery.Selection
		if part.Selector != "" {
			target = el.Find(part.Selector).First()
		} else {
			target = el
		}
		if target.Length() == 0 {
			continue
		}
		args := part.Args
		if len(args) == 0 {
			args = def
		}
		for _, arg := range args {
			if value := readArg(doc, target, arg, base); value != "" {
				return value
			}
		}
	}
	return ""
}

// SelectAll 按规则选出所有元素（剧集、线路容器等）；规则为空时返回空集合。
func SelectAll(doc *goquery.Document, scope *goquery.Selection, rule string) *goquery.Selection {
	selector := RuleSelector(rule)
	if selector == "" {
		// 与 TS 版一致：用一个永不存在的类名得到空集合
		return doc.Find(".__tvbox_none__")
	}
	return scope.Find(selector)
}
