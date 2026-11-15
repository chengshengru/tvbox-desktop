// Package detail 点播详情：CMS 型与网页规则型双路，线路/剧集归一化。
// 翻译自 src/main/services/detail.ts。
//
// 作者：Shiro
package detail

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"tvbox-desktop/internal/cms"
	"tvbox-desktop/internal/htmlrules"
	"tvbox-desktop/internal/httputil"
	"tvbox-desktop/internal/search"
	"tvbox-desktop/internal/settings"
	"tvbox-desktop/internal/sites"
	"tvbox-desktop/models"
)

var (
	tagRe    = regexp.MustCompile(`<[^>]+>`)
	spaceRe  = regexp.MustCompile(`\s+`)
	httpRe   = regexp.MustCompile(`(?i)^https?://`)
	h1Title  = "h1"
	titleSel = "title"
)

// Get 取一部影片的详情（含全部播放线路）。
func Get(ctx context.Context, siteKey, vodID string) (models.VodDetail, error) {
	site, ok := sites.GetSite(siteKey)
	if !ok {
		return models.VodDetail{}, errors.New("站点不存在或已被禁用")
	}
	if site.Unsupported != "" {
		return models.VodDetail{}, errors.New(site.Unsupported)
	}
	if vodID == "" {
		return models.VodDetail{}, errors.New("缺少影片 id")
	}

	switch site.Type {
	case 3, 4:
		return detailCms(ctx, site, vodID)
	case 1:
		return detailHtml(ctx, site, vodID)
	default:
		return models.VodDetail{}, errors.New("该类型暂不支持获取详情")
	}
}

// detailCms CMS 型站点详情：一次 ac=detail 拿全字段。
func detailCms(ctx context.Context, site models.Site, vodID string) (models.VodDetail, error) {
	timeout := resolveTimeout(site)
	requestURL := cms.BuildURL(site, cms.Query{AC: "detail", ID: vodID})
	header := sites.ResolveHeader(site, requestURL)
	text, err := httputil.FetchText(ctx, requestURL, httputil.Options{Header: header, Timeout: timeout})
	if err != nil {
		return models.VodDetail{}, err
	}
	payload, err := httputil.ParseLooseJSON(text)
	if err != nil {
		return models.VodDetail{}, err
	}
	m, _ := payload.(map[string]any)
	if !cms.IsOK(payload) {
		msg := "code 非成功"
		if v, ok := m["msg"]; ok && v != nil {
			msg = models.Str(v)
		}
		return models.VodDetail{}, fmt.Errorf("接口返回异常：%s", msg)
	}
	rawList := models.AsArray(models.Get(m, "list"))
	if len(rawList) == 0 {
		return models.VodDetail{}, errors.New("接口没有返回该影片的详情")
	}
	raw, _ := rawList[0].(map[string]any)

	base := search.CmsItemToVod(raw, site)
	// 注意：TS 这里用的 s() 不做 trim（与 search 里的版本不同），保持一致
	s := func(keys ...string) string { return models.Str(pick(raw, keys...)) }
	content := strings.TrimSpace(tagRe.ReplaceAllString(s("vod_content", "vod_blurb"), ""))
	lines := parseLines(s("vod_play_from"), s("vod_play_url"), s("vod_play_note"))
	return models.VodDetail{Vod: base, VodContent: content, Lines: lines}, nil
}

// detailHtml 网页规则站详情：vod_id 本身就是详情页地址。
func detailHtml(ctx context.Context, site models.Site, vodID string) (models.VodDetail, error) {
	if !httpRe.MatchString(vodID) {
		return models.VodDetail{}, errors.New("该网页规则站没有解析出可用的详情页地址")
	}
	timeout := resolveTimeout(site)
	header := sites.ResolveHeader(site, vodID)
	text, err := httputil.FetchText(ctx, vodID, httputil.Options{Header: header, Timeout: timeout})
	if err != nil {
		return models.VodDetail{}, err
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(text))
	if err != nil {
		return models.VodDetail{}, err
	}
	rules := models.HtmlRules{}
	if site.Rules != nil {
		rules = *site.Rules
	}
	scope := doc.Find("body")
	if scope.Length() == 0 {
		scope = doc.Selection
	}

	name := htmlrules.Extract(doc, scope, rules.DetailName, vodID, "Text")
	if name == "" {
		name = strings.TrimSpace(doc.Find(h1Title).First().Text())
	}
	if name == "" {
		name = strings.TrimSpace(doc.Find(titleSel).First().Text())
	}
	// 注意：JS 的 \s 还匹配 NBSP 等 Unicode 空白，Go 的 \s 只匹配 ASCII 空白；
	// 与基座 htmlrules 的 wsRe 保持同一口径，实际采集页差异可忽略。
	content := strings.TrimSpace(spaceRe.ReplaceAllString(
		htmlrules.Extract(doc, scope, rules.DetailContent, vodID, "Text"), " "))
	pic := htmlrules.Extract(doc, scope, rules.Pic, vodID, "data-src", "data-original", "src")

	lines := []models.Line{}
	containers := htmlrules.SelectAll(doc, scope, rules.PlayList)
	if containers.Length() > 0 && rules.PlayItem != "" {
		containers.Each(func(i int, node *goquery.Selection) {
			episodes := collectEpisodes(doc, node, rules.PlayItem)
			if len(episodes) == 0 {
				return
			}
			label := strings.TrimSpace(attrOr(node, "data-name", ""))
			if label == "" {
				label = strings.TrimSpace(node.Find("h3,h2,.title,.name").First().Text())
			}
			if label == "" {
				label = fmt.Sprintf("线路%d", i+1)
			}
			lines = append(lines, models.Line{Name: label, Episodes: episodes})
		})
	}
	if len(lines) == 0 && rules.PlayItem != "" {
		// 没有线路容器时退化成单线路
		episodes := collectEpisodes(doc, scope, rules.PlayItem)
		if len(episodes) > 0 {
			lines = append(lines, models.Line{Name: "默认线路", Episodes: episodes})
		}
	}

	if name == "" && len(lines) == 0 {
		return models.VodDetail{}, errors.New("详情页没有解析出内容，规则可能已失效")
	}

	return models.VodDetail{
		Vod: models.Vod{
			VodID:    vodID,
			VodName:  firstNonEmpty(name, vodID),
			VodPic:   pic,
			SiteKey:  site.Key,
			SiteName: site.Name,
			SourceID: site.SourceID,
		},
		VodContent: content,
		Lines:      lines,
	}, nil
}

// collectEpisodes 在容器内按 playItem 规则收集剧集（锚点 href 为播放地址）。
func collectEpisodes(doc *goquery.Document, scope *goquery.Selection, playItem string) []models.Episode {
	episodes := []models.Episode{}
	htmlrules.SelectAll(doc, scope, playItem).Each(func(_ int, anchor *goquery.Selection) {
		epURL := strings.TrimSpace(attrOr(anchor, "href", ""))
		if epURL == "" {
			return
		}
		label := strings.TrimSpace(spaceRe.ReplaceAllString(anchor.Text(), " "))
		episodes = append(episodes, models.Episode{
			Name:  firstNonEmpty(label, fmt.Sprintf("第%d集", len(episodes)+1)),
			URL:   epURL,
			Index: len(episodes),
		})
	})
	return episodes
}

// parseLines 把苹果CMS 的 vod_play_from / vod_play_url 拆成线路与剧集。
// 约定格式：线路之间用 $$$ 分隔，剧集之间用 # 分隔，剧集名与地址用 $ 分隔。
func parseLines(from string, url string, note string) []models.Line {
	froms := strings.Split(from, "$$$")
	groups := strings.Split(url, "$$$")
	notes := strings.Split(note, "$$$")
	lines := []models.Line{}

	for gi, group := range groups {
		name := strings.TrimSpace(firstNonEmpty(at(froms, gi), at(notes, gi), fmt.Sprintf("线路%d", gi+1)))
		episodes := []models.Episode{}
		for _, segment := range strings.Split(group, "#") {
			seg := strings.TrimSpace(segment)
			if seg == "" {
				continue
			}
			split := strings.Index(seg, "$")
			epName := ""
			epURL := seg
			if split >= 0 {
				epName = strings.TrimSpace(seg[:split])
				epURL = strings.TrimSpace(seg[split+1:])
			}
			if epURL == "" {
				continue
			}
			episodes = append(episodes, models.Episode{
				Name:  firstNonEmpty(epName, fmt.Sprintf("第%d集", len(episodes)+1)),
				URL:   epURL,
				Index: len(episodes),
			})
		}
		if len(episodes) > 0 {
			lines = append(lines, models.Line{Name: name, Episodes: episodes})
		}
	}
	return lines
}

// pick 按 ?? 语义取第一个存在且非 nil 的键。
func pick(item map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := item[k]; ok && v != nil {
			return v
		}
	}
	return nil
}

// at 越界安全的下标取值。
func at(list []string, i int) string {
	if i < len(list) {
		return list[i]
	}
	return ""
}

// attrOr 取属性，缺失时返回 def。
func attrOr(sel *goquery.Selection, key string, def string) string {
	if v, ok := sel.Attr(key); ok {
		return v
	}
	return def
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

// resolveTimeout 站点自身超时优先，0/未设置时退回全局设置。
func resolveTimeout(site models.Site) float64 {
	st := settings.Get()
	if site.Timeout != nil && *site.Timeout != 0 {
		return *site.Timeout
	}
	return st.Timeout
}
