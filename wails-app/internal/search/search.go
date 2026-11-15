// Package search 点播聚合搜索：多站点并发（CMS + 网页规则双路）、
// 逐站进度回报、可取消。
// 翻译自 src/main/services/search.ts。
//
// 作者：Shiro
package search

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"

	"tvbox-desktop/internal/cms"
	"tvbox-desktop/internal/htmlrules"
	"tvbox-desktop/internal/httputil"
	"tvbox-desktop/internal/settings"
	"tvbox-desktop/internal/sites"
	"tvbox-desktop/models"
)

// defaultLimit 单站默认返回上限（TS DEFAULT_LIMIT）。
const defaultLimit = 30

// 当前这一轮搜索的可取消 context：发起新一轮 Run 时覆盖，Cancel 取消它。
// 语义等价 TS 里的 currentToken 令牌：旧的那一批跑完当前站点后自行停下。
// CancelFunc 本身不可比较，用指针包一层做身份标记。
type activeRun struct {
	cancel context.CancelFunc
}

var (
	runMu     sync.Mutex
	runActive *activeRun
)

var (
	acPlaceholderRe = regexp.MustCompile(`(?i)\{ac\}`)
	// wdPlaceholderRe 与 TS /gi 一致，大小写都要认。
	wdPlaceholderRe = regexp.MustCompile(`(?i)\{(wd|kw|key|keyword)\}`)
	httpSchemeRe    = regexp.MustCompile(`(?i)^https?://`)
)

// Cancel 中止正在进行中的聚合搜索（等价 TS cancelSearch）。
// 没有搜索在跑时调用无副作用。
func Cancel() {
	runMu.Lock()
	defer runMu.Unlock()
	if runActive != nil {
		runActive.cancel()
	}
}

// Run 聚合搜索：按并发上限把所有可搜站点跑一遍。
// 每完成一个站点就同步回调一次 emit（界面可以边搜边出结果）。
// 被取消（Cancel 或 ctx done）时返回已收集到的部分结果且不报错——与 TS 的
// token 语义一致：worker 发现令牌变了就停下，searchAll 照样 resolve。
func Run(ctx context.Context, wd string, opts models.SearchOptions, emit func(models.SearchProgress)) (models.SearchResponse, error) {
	keyword := strings.TrimSpace(wd)
	if keyword == "" {
		return models.SearchResponse{Wd: keyword, Ms: 0, Total: 0, Results: []models.SiteSearchResult{}}, nil
	}

	// 新一轮搜索开始时把上一轮停掉（等价 token++）
	runCtx, cancel := context.WithCancel(ctx)
	active := &activeRun{cancel: cancel}
	runMu.Lock()
	if runActive != nil {
		runActive.cancel()
	}
	runActive = active
	runMu.Unlock()
	defer cancel()
	defer func() {
		runMu.Lock()
		if runActive == active {
			runActive = nil
		}
		runMu.Unlock()
	}()

	all := searchableSites()
	targets := all
	if len(opts.Sites) > 0 {
		want := make(map[string]bool, len(opts.Sites))
		for _, k := range opts.Sites {
			want[k] = true
		}
		targets = make([]models.Site, 0, len(want))
		for _, s := range all {
			if want[s.Key] {
				targets = append(targets, s)
			}
		}
	}
	if len(targets) == 0 {
		return models.SearchResponse{Wd: keyword, Ms: 0, Total: 0, Results: []models.SiteSearchResult{}}, nil
	}

	st := settings.Get()
	concurrency := st.Concurrency
	if opts.Concurrency != nil {
		concurrency = *opts.Concurrency
	}
	concurrency = clampConcurrency(concurrency)
	limit := defaultLimit
	if opts.LimitPerSite != nil {
		limit = *opts.LimitPerSite
	}

	started := time.Now()
	var (
		resultsMu sync.Mutex
		emitMu    sync.Mutex
		results   = []models.SiteSearchResult{}
		done      int
	)
	// 每站点一个 goroutine，信号量限并发（等价 TS 的 worker 池）。
	// emit 用独立锁串行化：TS 的 onResult 跑在单线程事件循环上，Go 侧必须保证回调不并发。
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, site := range targets {
		wg.Add(1)
		go func(site models.Site) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-runCtx.Done():
				return
			}
			defer func() { <-sem }()
			if runCtx.Err() != nil {
				return
			}
			result := searchSite(runCtx, site, keyword, limit)
			// 取消后丢弃在飞的结果（TS 在 push 前检查 token）
			if runCtx.Err() != nil {
				return
			}
			resultsMu.Lock()
			results = append(results, result)
			done++
			cur := done
			resultsMu.Unlock()
			if emit != nil {
				emitMu.Lock()
				emit(models.SearchProgress{Wd: keyword, Done: cur, Total: len(targets), Result: result})
				emitMu.Unlock()
			}
		}(site)
	}
	wg.Wait()

	return models.SearchResponse{
		Wd:      keyword,
		Ms:      time.Since(started).Milliseconds(),
		Total:   len(targets),
		Results: results,
	}, nil
}

// CmsItemToVod 把苹果CMS 返回的一条记录归一化成 Vod（detail/category 复用）。
// 字段名兼容两套写法（vod_id/id、vod_pic/pic …），与 TS 的 ?? 兜底链一致。
func CmsItemToVod(item map[string]any, site models.Site) models.Vod {
	return models.Vod{
		VodID:       trimStr(pick(item, "vod_id", "id")),
		VodName:     trimStr(pick(item, "vod_name", "name")),
		VodPic:      htmlrules.Absolutize(trimStr(pick(item, "vod_pic", "pic")), site.API),
		VodRemarks:  trimStr(pick(item, "vod_remarks", "remarks")),
		VodYear:     trimStr(pick(item, "vod_year", "year")),
		VodArea:     trimStr(pick(item, "vod_area", "area")),
		VodActor:    trimStr(pick(item, "vod_actor", "actor")),
		VodDirector: trimStr(pick(item, "vod_director", "director")),
		TypeName:    trimStr(pick(item, "type_name", "class")),
		VodScore:    trimStr(pick(item, "vod_score", "score")),
		SiteKey:     site.Key,
		SiteName:    site.Name,
		SourceID:    site.SourceID,
	}
}

// searchSite 搜索单个站点；任何异常都收敛成一个 ok:false 的结果，不影响其它站点。
func searchSite(ctx context.Context, site models.Site, wd string, limit int) models.SiteSearchResult {
	start := time.Now()
	elapsed := func() int64 { return time.Since(start).Milliseconds() }

	if site.Unsupported != "" {
		return failResult(site, site.Unsupported, 0)
	}
	if !site.Searchable {
		return failResult(site, "该站点已关闭搜索", 0)
	}
	if site.API == "" {
		return failResult(site, "站点未配置接口地址", 0)
	}

	st := settings.Get()
	timeout := resolveTimeout(site, st)

	var (
		list []models.Vod
		err  error
	)
	switch site.Type {
	case 3, 4:
		list, err = searchCms(ctx, site, wd, limit, timeout)
	case 1:
		list, err = searchHtml(ctx, site, wd, limit, timeout)
	default:
		return failResult(site, "该类型暂不支持搜索", 0)
	}
	if err != nil {
		return failResult(site, err.Error(), elapsed())
	}
	if list == nil {
		list = []models.Vod{}
	}
	message := "没有匹配结果"
	if len(list) > 0 {
		message = fmt.Sprintf("找到 %d 条", len(list))
	}
	return models.SiteSearchResult{
		SiteKey:  site.Key,
		SiteName: site.Name,
		OK:       true,
		Ms:       elapsed(),
		Message:  message,
		List:     list,
	}
}

// searchableSites 列出当前可以参与聚合搜索的站点。
func searchableSites() []models.Site {
	out := []models.Site{}
	for _, s := range sites.GetSites() {
		if s.Enabled && s.Searchable && s.Unsupported == "" {
			out = append(out, s)
		}
	}
	return out
}

// searchCms CMS 型站点搜索。
// 搜索用 ac=detail：MacCMS V10 的 detail 同时支持 wd 关键词，且一次就带回
// vod_play_url，省掉一次详情请求。只有配置作者在模板里写了 {ac} 时，才按
// TVBox 惯例填 videolist（部分老版本 CMS 只认这个写法）。
func searchCms(ctx context.Context, site models.Site, wd string, limit int, timeout float64) ([]models.Vod, error) {
	ac := "detail"
	if acPlaceholderRe.MatchString(site.API) {
		ac = "videolist"
	}

	// 基座 cms.BuildURL 在模板模式下不解引用 *string，{wd} 会被替换成空串
	// （models.Str 不认指针类型）。这里把关键词按 encodeURIComponent 语义预先
	// 填进地址，再交给 BuildURL 填其余占位符；基地址模式照常传 WD（那条路会解引用）。
	// 注意：只含 {wd} 的模板替换后不再有占位符，BuildURL 会走基地址分支追加
	// ac/pg 参数，采集站对多余参数无感，行为不受影响。
	query := cms.Query{AC: ac, PG: float64(1)}
	targetSite := site
	if wdPlaceholderRe.MatchString(site.API) {
		enc := encodeComponent(wd)
		targetSite.API = wdPlaceholderRe.ReplaceAllStringFunc(site.API, func(string) string { return enc })
	} else {
		query.WD = cms.Str(wd)
	}

	requestURL := cms.BuildURL(targetSite, query)
	header := sites.ResolveHeader(site, requestURL)
	text, err := httputil.FetchText(ctx, requestURL, httputil.Options{Header: header, Timeout: timeout})
	if err != nil {
		return nil, err
	}
	payload, err := httputil.ParseLooseJSON(text)
	if err != nil {
		return nil, err
	}
	m, _ := payload.(map[string]any)
	if !cms.IsOK(payload) {
		// TS：String(payload?.msg ?? 'code 非成功')——msg 缺失才用默认文案，空串照拼
		msg := "code 非成功"
		if v, ok := m["msg"]; ok && v != nil {
			msg = models.Str(v)
		}
		return nil, fmt.Errorf("接口返回异常：%s", msg)
	}

	raw := models.AsArray(models.Get(m, "list"))
	if len(raw) > limit {
		raw = raw[:limit]
	}
	out := []models.Vod{}
	for _, it := range raw {
		item, _ := it.(map[string]any)
		vod := CmsItemToVod(item, site)
		if vod.VodName == "" || vod.VodID == "" {
			continue
		}
		out = append(out, vod)
	}
	return out, nil
}

// searchHtml 网页规则站搜索：CSS 选择器 + 规则串取值。
func searchHtml(ctx context.Context, site models.Site, wd string, limit int, timeout float64) ([]models.Vod, error) {
	rules := models.HtmlRules{}
	if site.Rules != nil {
		rules = *site.Rules
	}
	// TS：(site.rules?.search || site.api).trim()——空串才退到 api
	template := rules.Search
	if template == "" {
		template = site.API
	}
	target := strings.TrimSpace(template)
	target = wdPlaceholderRe.ReplaceAllStringFunc(target, func(string) string { return encodeComponent(wd) })
	if !httpSchemeRe.MatchString(target) {
		return nil, errors.New("网页规则站缺少可请求的搜索地址")
	}

	header := sites.ResolveHeader(site, target)
	text, err := httputil.FetchText(ctx, target, httputil.Options{Header: header, Timeout: timeout})
	if err != nil {
		return nil, err
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(text))
	if err != nil {
		return nil, err
	}
	nodes := doc.Find(strings.TrimSpace(rules.List))
	if nodes.Length() == 0 {
		return nil, errors.New("列表选择器没有匹配到节点，规则可能已失效")
	}

	out := []models.Vod{}
	nodes.Slice(0, limit).Each(func(_ int, node *goquery.Selection) {
		link := htmlrules.Extract(doc, node, rules.Link, target, "href", "Text")
		name := htmlrules.Extract(doc, node, rules.Name, target, "Text")
		if name == "" {
			return
		}
		out = append(out, models.Vod{
			// 网页规则站没有稳定的数字 id，直接把详情页地址当作 id。
			// 这样详情页不需要再猜一次地址，也不会因为站点改版而失效。
			VodID:      firstNonEmpty(link, name),
			VodName:    name,
			VodPic:     htmlrules.Extract(doc, node, rules.Pic, target, "data-src", "data-original", "src"),
			VodRemarks: htmlrules.Extract(doc, node, rules.Remarks, target, "Text"),
			SiteKey:    site.Key,
			SiteName:   site.Name,
			SourceID:   site.SourceID,
		})
	})
	return out, nil
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

// trimStr 宽松转字符串并 trim（等价 TS 的 String(v).trim()）。
// 注意：Go 的 TrimSpace 按 Unicode 空白裁剪，比 JS 的 trim() 少裁 \uFEFF 之外
// 的差异极小，实际数据里两者等价。
func trimStr(v any) string {
	return strings.TrimSpace(models.Str(v))
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

// failResult 组装一个失败站点结果。
func failResult(site models.Site, message string, ms int64) models.SiteSearchResult {
	return models.SiteSearchResult{
		SiteKey:  site.Key,
		SiteName: site.Name,
		OK:       false,
		Ms:       ms,
		Message:  message,
		List:     []models.Vod{},
	}
}

// resolveTimeout 站点自身超时优先，0/未设置时退回全局设置。
func resolveTimeout(site models.Site, st models.AppSettings) float64 {
	if site.Timeout != nil && *site.Timeout != 0 {
		return *site.Timeout
	}
	return st.Timeout
}

// clampConcurrency 并发数夹在 [1, 32]（与 TS Math.max(1, Math.min(32, x)) 一致）。
func clampConcurrency(n int) int {
	if n < 1 {
		return 1
	}
	if n > 32 {
		return 32
	}
	return n
}

// encodeComponent encodeURIComponent 语义（空格转 %20 而非 +）。
func encodeComponent(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}
