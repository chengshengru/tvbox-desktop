// html.go：猫协议 pdfh/pd 全局函数，委托 htmlrules（goquery）选择器引擎。
//
// 作者：Shiro
package bridge

import (
	"strings"

	"github.com/PuerkitoBio/goquery"

	"tvbox-desktop/internal/htmlrules"
)

// Pdfh 对应猫协议 pdfh(html, rule)：按规则取文档里第一个命中值。
func Pdfh(htmlBody, rule string) string {
	doc := parseDoc(htmlBody)
	if doc == nil {
		return ""
	}
	return htmlrules.Extract(doc, doc.Selection, rule, "")
}

// Pd 对应猫协议 pd(html, rule)：取全部命中值并以 "*a*" 连接（等价 pdfa(...).join("*a*")）。
func Pd(htmlBody, rule string) string {
	doc := parseDoc(htmlBody)
	if doc == nil {
		return ""
	}
	var out []string
	for _, part := range htmlrules.ParseRule(rule) {
		if part.Selector == "" {
			continue
		}
		doc.Find(part.Selector).Each(func(_ int, s *goquery.Selection) {
			if v := htmlrules.ExtractArgs(doc, s, part.Args, ""); v != "" {
				out = append(out, v)
			}
		})
		if len(out) > 0 {
			break // || 分支回退语义：前一分支有值即止
		}
	}
	return strings.Join(out, "*a*")
}

// parseDoc 解析 HTML 片段；失败返回 nil。
func parseDoc(htmlBody string) *goquery.Document {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlBody))
	if err != nil {
		return nil
	}
	return doc
}
