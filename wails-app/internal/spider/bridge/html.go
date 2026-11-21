// html.go：猫协议 pdfh/pd 全局函数，委托 htmlrules（goquery）选择器引擎。
//
// 作者：Shiro
package bridge

import (
	"strings"
	"sync"

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

// HTMLStore htmlParse 的元素句柄表（§6.6）：脚本侧只持有数字句柄，
// goquery 对象全部留在宿主进程；worker 销毁时整表释放，不存在跨调用悬挂。
type HTMLStore struct {
	mu   sync.Mutex
	next int
	docs map[int]*goquery.Document
	sels map[int]*goquery.Selection
}

// NewHTMLStore 建空句柄表。
func NewHTMLStore() *HTMLStore {
	return &HTMLStore{
		docs: map[int]*goquery.Document{},
		sels: map[int]*goquery.Selection{},
	}
}

// Open 解析 HTML 并登记根句柄；解析失败返回 0（host.js 侧表现为空集合）。
func (s *HTMLStore) Open(htmlBody string) int {
	doc := parseDoc(htmlBody)
	if doc == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	id := s.next
	s.docs[id] = doc
	s.sels[id] = doc.Selection
	return id
}

// Find 在每个句柄的集合内按 CSS 选择器找子元素，返回新句柄列表。
func (s *HTMLStore) Find(ids []int, selector string) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		sel, ok := s.sels[id]
		if !ok {
			continue
		}
		sel.Find(selector).Each(func(_ int, e *goquery.Selection) {
			s.next++
			h := s.next
			s.sels[h] = e
			out = append(out, h)
		})
	}
	return out
}

// sel 取句柄对应集合；不存在返回空集合。
func (s *HTMLStore) sel(id int) *goquery.Selection {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sel, ok := s.sels[id]; ok {
		return sel
	}
	return &goquery.Selection{}
}

// Text 集合全部文本（cheerio .text() 对集合的拼接语义）。
func (s *HTMLStore) Text(id int) string {
	return s.sel(id).Text()
}

// HTML 首个元素 inner HTML。
func (s *HTMLStore) HTML(id int) string {
	inner, err := s.sel(id).First().Html()
	if err != nil {
		return ""
	}
	return inner
}

// Attr 首个元素指定属性；不存在返回空串。
func (s *HTMLStore) Attr(id int, key string) string {
	v, _ := s.sel(id).First().Attr(key)
	return v
}

// Close 清空整表（worker 销毁时调用）。
func (s *HTMLStore) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs = map[int]*goquery.Document{}
	s.sels = map[int]*goquery.Selection{}
	s.next = 0
}
