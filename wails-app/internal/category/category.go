// Package category 点播分类浏览：分类树聚合归一化（大类/子分类合并）+ 分页浏览。
// 翻译自 src/main/services/category.ts。
//
// 分类浏览。
//
// 每个采集站都有自己的分类树（`?ac=list` 返回的 `class` 数组，元素形如
// {type_id, type_name, type_pid}）。同一个大类在各站的叫法和 id 都不一样：
//
//	非凡/电影天堂：电影片#1 连续剧#2 综艺片#3 动漫片#4
//	量子资源：    电影片#1 连续剧#2 综艺片#3 动漫片#4 电影解说#35 体育#36 …
//	素博资源：    电影#1 电视剧#2 动漫#3 综艺#4 纪录片#5 体育赛事#23 …
//
// 所以这里把各站的分类归并到一组固定的中文大类（见 buckets），
// 大类下再按子分类（动作片 / 韩剧 / 大陆综艺 …）细分（见 subs）。
//
// 三个实测结论决定了查询方式：
//  1. 父分类 id 查不出东西（`t=1` 返回 0 条），必须展开到叶子分类；
//  2. 重复的 t 参数不能指望按 OR 处理（实测「最后一个生效」），
//     所以每个子分类单独发一次请求，拿回来自己合并（见 buildBrowseURL 注释）；
//  3. **归并必须按叶子分类名来判**，不能只看顶层分类：天堂／量子的「记录片」
//     挂在「电影片」下面，非凡／天堂的「短剧」挂在「连续剧」下面，量子／天堂的
//     「动画片」也挂在「电影片」下面——只看顶层就会把这些全算进电影或电视剧。
//
// 作者：Shiro
package category

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"

	"tvbox-desktop/internal/cms"
	"tvbox-desktop/internal/httputil"
	"tvbox-desktop/internal/search"
	"tvbox-desktop/internal/settings"
	"tvbox-desktop/internal/sites"
	"tvbox-desktop/models"
)

// skipRe 「作品类型」之外的分类，连同它整棵子树一起丢掉。
var skipRe = regexp.MustCompile(`(解说|演员|明星|资讯|新闻|预告|花絮|伦理|福利|写真|片花|榜单)`)

type bucketRule struct {
	name string
	re   *regexp.Regexp
}

// buckets 归并规则：从上往下第一个命中的就是结果，所以顺序有讲究
// （「短剧」必须排在「电视剧」前面，否则会被「剧」吃掉）。
// 「电影」里也不能写 `片$`：那会把「综艺片」「动漫片」「纪录片」全吃成电影。
var buckets = []bucketRule{
	{"电影", regexp.MustCompile(`电影|影院`)},
	{"短剧", regexp.MustCompile(`短剧|微剧|竖屏`)},
	{"纪录片", regexp.MustCompile(`纪录|记录|纪实`)},
	{"动漫", regexp.MustCompile(`动漫|动画|番剧|漫画|漫剧`)},
	{"综艺", regexp.MustCompile(`综艺|真人秀|脱口秀`)},
	{"体育", regexp.MustCompile(`体育|赛事|足球|篮球|台球`)},
	{"少儿", regexp.MustCompile(`少儿|儿童|亲子|幼教|益智`)},
	{"电视剧", regexp.MustCompile(`电视剧|连续剧|电视|剧集|国产剧|美剧|韩剧|日剧|港剧|台剧|泰剧`)},
}

// orderList 目录里大类的展示顺序。
var orderList = []string{"电影", "电视剧", "动漫", "综艺", "纪录片", "短剧", "体育", "少儿"}

// leafFollowsParent 叶子名自己也命中某个大类、但它挂在另一个作品大类底下的例外。
//
// 归并的主规则是「叶子名优先，认不出才往上找父分类」，这是为了救
// `记录片`（挂在电影片下、该进纪录片）和 `短剧`（挂在连续剧下）。
// 但同一条规则会把「电影片 → 动画片」判成动漫，而它放的其实是动画电影
// （跟光速 / 素博的「动漫电影」是同一类东西），于是同一类内容裂成
// 「电影 → 动画」和「动漫 → 动画片」两个区。
//
// 所以这里精确列出要跟着父分类走的叶子：父正则只对着祖先链匹配，
// 别的站点真把「动画片」挂在「动漫片」下时不受影响。
var leafFollowsParent = []struct {
	leaf   *regexp.Regexp
	parent *regexp.Regexp
	bucket string
}{
	{regexp.MustCompile(`^动画片$`), regexp.MustCompile(`电影`), "电影"},
}

// subs 子分类归一表。各站点对同一个子分类的叫法差别很大
// （韩剧 / 韩国剧、美剧 / 欧美剧、港澳剧 / 香港剧），
// 不归一的话目录里会并排出现「韩剧」和「韩国剧」两个标签。
// 没命中的就用叶子自己的名字，所以没见过的站点也能正常显示。
var subs = map[string][]bucketRule{
	"电影": {
		{"动作", regexp.MustCompile(`动作`)},
		{"喜剧", regexp.MustCompile(`喜剧`)},
		{"爱情", regexp.MustCompile(`爱情`)},
		{"科幻", regexp.MustCompile(`科幻`)},
		{"恐怖", regexp.MustCompile(`恐怖|惊悚`)},
		{"剧情", regexp.MustCompile(`剧情`)},
		{"战争", regexp.MustCompile(`战争`)},
		{"动画", regexp.MustCompile(`动画|动漫`)},
	},
	"电视剧": {
		{"国产", regexp.MustCompile(`国产|大陆`)},
		{"香港", regexp.MustCompile(`香港|港澳|港剧`)},
		{"台湾", regexp.MustCompile(`台湾|台剧`)},
		{"韩国", regexp.MustCompile(`韩国|韩剧`)},
		{"日本", regexp.MustCompile(`日本|日剧`)},
		{"欧美", regexp.MustCompile(`欧美|美剧`)},
		{"泰国", regexp.MustCompile(`泰国|泰剧`)},
		{"海外", regexp.MustCompile(`海外|其他`)},
	},
	"动漫": {
		{"国产", regexp.MustCompile(`国产|中国`)},
		{"日本", regexp.MustCompile(`日本|日韩`)},
		{"欧美", regexp.MustCompile(`欧美`)},
		{"港台", regexp.MustCompile(`港台`)},
		{"海外", regexp.MustCompile(`海外`)},
		{"动画电影", regexp.MustCompile(`电影`)},
		{"动画片", regexp.MustCompile(`动画片`)},
	},
	"综艺": {
		{"大陆", regexp.MustCompile(`大陆|国产`)},
		{"港台", regexp.MustCompile(`港台`)},
		{"日韩", regexp.MustCompile(`日韩`)},
		{"欧美", regexp.MustCompile(`欧美`)},
	},
	"体育": {
		{"足球", regexp.MustCompile(`足球`)},
		{"篮球", regexp.MustCompile(`篮球`)},
		{"网球", regexp.MustCompile(`网球`)},
		{"台球", regexp.MustCompile(`台球|斯诺克`)},
		{"其他", regexp.MustCompile(`其他|综合`)},
	},
	"短剧": {
		{"擦边短剧", regexp.MustCompile(`擦边`)},
		{"古装仙侠", regexp.MustCompile(`古装|仙侠`)},
		{"现代都市", regexp.MustCompile(`现代|都市`)},
		{"穿越年代", regexp.MustCompile(`穿越|年代`)},
		{"言情总裁", regexp.MustCompile(`言情|总裁`)},
		{"重生民国", regexp.MustCompile(`重生|民国`)},
		{"反转爽剧", regexp.MustCompile(`反转|爽剧`)},
		{"脑洞悬疑", regexp.MustCompile(`脑洞|悬疑`)},
		{"短剧", regexp.MustCompile(`短剧|微剧|竖屏`)},
	},
	"纪录片": {
		{"纪录片", regexp.MustCompile(`纪录|记录|纪实`)},
	},
}

// classTTL 分类树半天不会变，缓存 10 分钟，免得每次进目录页都打一轮请求（毫秒）。
const classTTL = 10 * 60 * 1000

// browseLimit 分类浏览单页条数上限（TS 固定 30）。
const browseLimit = 30

type classCacheEntry struct {
	at      int64
	classes []models.SiteClass
}

// pendingClasses 同一站点并发请求去重（等价 TS 的 inflight Map）。
type pendingClasses struct {
	done    chan struct{}
	classes []models.SiteClass
}

var (
	classesMu  sync.Mutex
	classCache = map[string]classCacheEntry{}
	inflight   = map[string]*pendingClasses{}
	// generation 换一次配置源就 +1，用来认出「还在飞的那次请求」属于上一套源。
	generation int
)

// ClearCache 配置源一变就把分类树缓存清掉。
// 缓存按站点 key 存，换一整套订阅后 key 不同本来也会自然失效，
// 但「原地改掉某个源的地址」时 key 可能没变，而它的分类树已经换了，
// 所以由上层在增删改/同步配置源时主动作废。
func ClearCache() {
	classesMu.Lock()
	defer classesMu.Unlock()
	classCache = map[string]classCacheEntry{}
	inflight = map[string]*pendingClasses{}
	// 清空 inflight 只是让后来者重新发请求，**已经在飞的那次**跑完照样会
	// 把结果写进 classCache——于是上一套源的分类树又盖回来了。用代次号挡住。
	generation++
}

// List 汇总各站点的分类，按「大类 → 子分类」归并后返回目录。
func List(ctx context.Context) ([]models.CategoryBucket, error) {
	all := sites.GetSites()
	targets := []models.Site{}
	for _, s := range all {
		if s.Enabled && (s.Type == 3 || s.Type == 4) {
			targets = append(targets, s)
		}
	}

	// 各站分类树并发拉取（等价 TS 的 Promise.all；loadClasses 自带缓存与去重）
	type siteTree struct {
		site    models.Site
		classes []models.SiteClass
	}
	trees := make([]siteTree, len(targets))
	var wg sync.WaitGroup
	for i := range targets {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			trees[i] = siteTree{site: targets[i], classes: loadClasses(ctx, targets[i])}
		}(i)
	}
	wg.Wait()

	// 大类 → 子分类 → 站点 → 叶子 id；站点列表要保留首次出现顺序（等价 TS 的 Map）
	type subSiteEntry struct {
		name  string
		ids   []string
		seen  map[string]bool
		order int
	}
	type subData struct {
		siteOrder []string
		sites     map[string]*subSiteEntry
	}
	type bucketData struct {
		subs map[string]*subData
	}
	grouped := map[string]*bucketData{}

	for _, t := range trees {
		for _, lf := range collectLeaves(t.classes) {
			bd := grouped[lf.bucket]
			if bd == nil {
				bd = &bucketData{subs: map[string]*subData{}}
				grouped[lf.bucket] = bd
			}
			sd := bd.subs[lf.sub]
			if sd == nil {
				sd = &subData{sites: map[string]*subSiteEntry{}}
				bd.subs[lf.sub] = sd
			}
			entry := sd.sites[t.site.Key]
			if entry == nil {
				entry = &subSiteEntry{name: t.site.Name, seen: map[string]bool{}, order: lf.order}
				sd.sites[t.site.Key] = entry
				sd.siteOrder = append(sd.siteOrder, t.site.Key)
			}
			for _, id := range leafIDs(t.classes, lf.id) {
				if !entry.seen[id] {
					entry.seen[id] = true
					entry.ids = append(entry.ids, id)
				}
			}
		}
	}

	coll := collate.New(language.Make("zh-Hans-CN"))
	byName := map[string]models.CategoryBucket{}
	extra := []models.CategoryBucket{}
	for bucketName, bd := range grouped {
		type subSort struct {
			name  string
			sites []models.CategorySiteIds
			order int
		}
		list := []subSort{}
		for subName, sd := range bd.subs {
			sitesOf := []models.CategorySiteIds{}
			minOrder := math.MaxInt32
			for _, key := range sd.siteOrder {
				v := sd.sites[key]
				if v.order < minOrder {
					minOrder = v.order
				}
				if len(v.ids) == 0 {
					continue
				}
				ids := make([]string, len(v.ids))
				copy(ids, v.ids)
				sitesOf = append(sitesOf, models.CategorySiteIds{SiteKey: key, SiteName: v.name, TypeIDs: ids})
			}
			if len(sitesOf) == 0 {
				continue
			}
			list = append(list, subSort{name: subName, sites: sitesOf, order: minOrder})
		}
		// 归一表里靠前的子分类排前面，表里没有的（站点自造的名字）按中文排到最后
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].order != list[j].order {
				return list[i].order < list[j].order
			}
			return coll.CompareString(list[i].name, list[j].name) < 0
		})
		if len(list) == 0 {
			continue
		}
		bucket := models.CategoryBucket{Name: bucketName, Subs: make([]models.CategorySub, 0, len(list))}
		for _, s := range list {
			bucket.Subs = append(bucket.Subs, models.CategorySub{Name: s.name, Sites: s.sites})
		}
		byName[bucketName] = bucket
		if !containsName(orderList, bucketName) {
			extra = append(extra, bucket)
		}
	}

	out := []models.CategoryBucket{}
	for _, n := range orderList {
		if b, ok := byName[n]; ok {
			out = append(out, b)
		}
	}
	sort.SliceStable(extra, func(i, j int) bool {
		return coll.CompareString(extra[i].Name, extra[j].Name) < 0
	})
	out = append(out, extra...)
	return out, nil
}

// Browse 打开某个大类（可选某个子分类）的一页。
// `sub` 传空串表示「全部」，这时会把该大类下**每个**子分类都查一遍再合并。
func Browse(ctx context.Context, bucket, sub string, page int) (models.CategoryPage, error) {
	categories, err := List(ctx)
	if err != nil {
		return models.CategoryPage{}, err
	}
	var target *models.CategoryBucket
	for i := range categories {
		if categories[i].Name == bucket {
			target = &categories[i]
			break
		}
	}
	if target == nil {
		return models.CategoryPage{}, fmt.Errorf("没有找到「%s」这个分类", bucket)
	}

	picked := []models.CategorySub{}
	for _, s := range target.Subs {
		if sub == "" || s.Name == sub {
			picked = append(picked, s)
		}
	}
	if len(picked) == 0 {
		return models.CategoryPage{}, fmt.Errorf("「%s」下没有「%s」这个子分类", bucket, sub)
	}

	limit := browseLimit
	all := sites.GetSites()
	byKey := map[string]models.Site{}
	for _, s := range all {
		byKey[s.Key] = s
	}

	// 一个子分类一个请求。**不能把多个叶子 id 塞进同一次请求**：重复的 t 参数
	// 服务端只认最后一个（实测 t=6&t=7 的 total 等于 t=7，返回的也全是喜剧片），
	// 所以「全部」只能分开查、拿回结果以后自己合并。
	type job struct {
		siteKey  string
		siteName string
		site     models.Site
		ids      []string
	}
	jobs := []job{}
	for _, s := range picked {
		for _, entry := range s.Sites {
			if site, ok := byKey[entry.SiteKey]; ok {
				jobs = append(jobs, job{siteKey: entry.SiteKey, siteName: entry.SiteName, site: site, ids: entry.TypeIDs})
			}
		}
	}

	st := settings.Get()
	concurrency := clampConcurrency(st.Concurrency)
	pages := make([]sitePage, len(jobs))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i := range jobs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			pages[i] = browseSite(ctx, jobs[i].site, jobs[i].ids, page, limit)
		}(i)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return models.CategoryPage{}, ctx.Err()
	}

	// 按站点归拢，同一站点的多个子分类合并成一条结果（界面上一站一个 chip）
	type siteSlot struct {
		siteName string
		results  []models.SiteSearchResult
	}
	slotOrder := []string{}
	slots := map[string]*siteSlot{}
	for i, j := range jobs {
		slot, ok := slots[j.siteKey]
		if !ok {
			slot = &siteSlot{siteName: j.siteName}
			slots[j.siteKey] = slot
			slotOrder = append(slotOrder, j.siteKey)
		}
		slot.results = append(slot.results, pages[i].result)
	}

	results := []models.SiteSearchResult{}
	// 一个站点能翻多深，取决于它**最浅**的那个子分类——只有每个子分类都还有第 N 页，
	// 合并出来的第 N 页才是完整的
	siteDepth := []int{}
	for _, siteKey := range slotOrder {
		slot := slots[siteKey]
		lists := [][]models.Vod{}
		for _, r := range slot.results {
			if r.OK {
				lists = append(lists, r.List)
			}
		}
		list := interleave(lists)
		if len(list) > limit {
			list = list[:limit]
		}
		if len(list) == 0 {
			found := false
			for _, r := range slot.results {
				if !r.OK {
					results = append(results, r)
					found = true
					break
				}
			}
			if !found {
				results = append(results, models.SiteSearchResult{
					SiteKey:  siteKey,
					SiteName: slot.siteName,
					OK:       false,
					Ms:       0,
					Message:  "这个分类下没有返回内容",
					List:     []models.Vod{},
				})
			}
			continue
		}
		depths := []int{}
		for i, j := range jobs {
			if j.siteKey == siteKey && pages[i].pageCount > 0 {
				depths = append(depths, pages[i].pageCount)
			}
		}
		if len(depths) > 0 {
			siteDepth = append(siteDepth, minInt(depths))
		}
		ms := int64(0)
		for _, r := range slot.results {
			if r.Ms > ms {
				ms = r.Ms
			}
		}
		results = append(results, models.SiteSearchResult{
			SiteKey:  siteKey,
			SiteName: slot.siteName,
			OK:       true,
			Ms:       ms,
			Message:  fmt.Sprintf("返回 %d 条", len(list)),
			List:     list,
		})
	}

	// 各站点能翻到的深度不一样，取最深的那个，界面上至少能一直翻到某一站到底
	pageCount := page
	for _, d := range siteDepth {
		if d > pageCount {
			pageCount = d
		}
	}
	return models.CategoryPage{Page: page, PageCount: pageCount, Bucket: bucket, Sub: sub, Results: results}, nil
}

// sitePage 单站点一次分页浏览的结果 + 站点自己报的总页数。
type sitePage struct {
	result models.SiteSearchResult
	// pageCount 拿不到时退回「这一页满不满」的猜测
	pageCount int
}

// browseSite 拉某个站点在若干叶子分类下的一页。
func browseSite(ctx context.Context, site models.Site, typeIDs []string, page, limit int) sitePage {
	started := time.Now()
	elapsed := func() int64 { return time.Since(started).Milliseconds() }
	failPage := func(message string, ms int64, pageCount int) sitePage {
		return sitePage{result: failResult(site, message, ms), pageCount: pageCount}
	}

	// ac=detail 会把 vod_play_url 一起带回来，详情页就不用再请求一次
	requestURL := buildBrowseURL(site, "detail", typeIDs, page)
	text, err := httputil.FetchText(ctx, requestURL, httputil.Options{Header: sites.ResolveHeader(site, requestURL)})
	if err != nil {
		return failPage(err.Error(), elapsed(), 0)
	}
	ms := elapsed()
	payload, err := httputil.ParseLooseJSON(text)
	if err != nil {
		return failPage(err.Error(), ms, 0)
	}
	m, _ := payload.(map[string]any)
	if !cms.IsOK(payload) {
		msg := trimStr(models.Get(m, "msg"))
		if msg == "" {
			msg = "接口返回异常"
		}
		return failPage(fmt.Sprintf("接口返回异常：%s", msg), ms, 0)
	}
	raw := models.AsArray(models.Get(m, "list"))
	if len(raw) > limit {
		raw = raw[:limit]
	}
	list := make([]models.Vod, 0, len(raw))
	for _, it := range raw {
		item, _ := it.(map[string]any)
		list = append(list, search.CmsItemToVod(item, site))
	}
	// 多数采集站每页固定 20 条，和我们要的 limit 不一定一致，
	// 所以页数优先读接口自己报的 pagecount，读不到才按「这一页满没满」猜
	pageCount := page
	if len(list) > 0 {
		pageCount = page + 1
	}
	if v := models.Get(m, "pagecount"); v != nil {
		reported := models.Num(v, math.NaN())
		if !math.IsNaN(reported) && reported > 0 {
			pageCount = int(reported)
		}
	}
	if len(list) == 0 {
		return failPage("这个分类下没有返回内容", ms, pageCount)
	}
	return sitePage{
		result: models.SiteSearchResult{
			SiteKey:  site.Key,
			SiteName: site.Name,
			OK:       true,
			Ms:       ms,
			Message:  fmt.Sprintf("返回 %d 条", len(list)),
			List:     list,
		},
		pageCount: pageCount,
	}
}

// interleave 把同一个站点上多个子分类的结果交替穿插成一条列表。
// 直接首尾相接的话，第一屏会全是某个子分类的片子；交替取能让第一屏就有各种类型。
// 顺带按 vod_id 去重（同一个片子在站点的多个分类下都可能出现）。
func interleave(lists [][]models.Vod) []models.Vod {
	out := []models.Vod{}
	seen := map[string]bool{}
	depth := 0
	for _, l := range lists {
		if len(l) > depth {
			depth = len(l)
		}
	}
	for i := 0; i < depth; i++ {
		for _, l := range lists {
			if i >= len(l) {
				continue
			}
			vod := l[i]
			if seen[vod.VodID] {
				continue
			}
			seen[vod.VodID] = true
			out = append(out, vod)
		}
	}
	return out
}

// normalizeClasses 把 `class` 数组归一化成 {id,name,pid}，缺字段的条目直接丢掉。
func normalizeClasses(raw any) []models.SiteClass {
	out := []models.SiteClass{}
	for _, it := range models.AsArray(raw) {
		item, _ := it.(map[string]any)
		id := trimStr(pick(item, "type_id", "id", "tid"))
		name := trimStr(pick(item, "type_name", "name", "title"))
		pid := trimStr(pick(item, "type_pid", "pid"))
		if pid == "" {
			pid = "0"
		}
		if id == "" || name == "" {
			continue
		}
		out = append(out, models.SiteClass{ID: id, Name: name, PID: pid})
	}
	return out
}

// leafIDs 收集某个分类子树下的**叶子** id。
// 有子分类就往下一路展开（父分类 id 查不出东西），没有子分类就用自己。
func leafIDs(classes []models.SiteClass, rootID string) []string {
	children := []string{}
	for _, c := range classes {
		if c.PID == rootID {
			children = append(children, c.ID)
		}
	}
	if len(children) == 0 {
		return []string{rootID}
	}
	out := []string{}
	for _, id := range children {
		out = append(out, leafIDs(classes, id)...)
	}
	return out
}

// bucketOf 把站点自己的分类名归到固定大类；认不出返回空串。
func bucketOf(name string) string {
	if name == "" || skipRe.MatchString(name) {
		return ""
	}
	for _, b := range buckets {
		if b.re.MatchString(name) {
			return b.name
		}
	}
	return ""
}

// subOf 把叶子分类名归到某个大类下的子分类；没命中的就用它自己的名字。
func subOf(bucket string, leafName string) string {
	for _, r := range subs[bucket] {
		if r.re.MatchString(leafName) {
			return r.name
		}
	}
	return leafName
}

// ancestry 从叶子往上找它的祖先链（叶子自己排第一）。
// 走到顶级（pid === '0'）或者父节点不存在就停，顺便用 depth 兜住脏数据造成的环。
func ancestry(leaf models.SiteClass, byID map[string]models.SiteClass) []models.SiteClass {
	chain := []models.SiteClass{}
	cur := leaf
	for depth := 0; depth < 8; depth++ {
		chain = append(chain, cur)
		if cur.PID == "0" {
			break
		}
		next, ok := byID[cur.PID]
		if !ok {
			break
		}
		cur = next
	}
	return chain
}

// leafInfo 归一化后的叶子：位于哪个大类、哪个子分类。
type leafInfo struct {
	id     string
	bucket string
	sub    string
	// order 归一表里的位置，用来给子分类排稳定顺序
	order int
}

// collectLeaves 把一棵分类树归一成「大类/子分类 → 叶子」列表。
func collectLeaves(classes []models.SiteClass) []leafInfo {
	byID := map[string]models.SiteClass{}
	// 有子节点的都不是叶子
	parents := map[string]bool{}
	for _, c := range classes {
		byID[c.ID] = c
		if c.PID != "0" {
			parents[c.PID] = true
		}
	}
	leaves := []leafInfo{}

	for _, lf := range classes {
		if parents[lf.ID] {
			continue
		}

		chain := ancestry(lf, byID)
		// 祖先链上只要有「解说 / 伦理 / 预告」这类非作品分类，整棵子树都丢掉
		skipped := false
		for _, c := range chain {
			if skipRe.MatchString(c.Name) {
				skipped = true
				break
			}
		}
		if skipped {
			continue
		}

		// 叶子自己的名字优先，认不出来才往上找父分类（「动作片」→「电影片」）
		// bucketOf 对 SKIP 名返回空串，所以这一步天然会跳过被挡的祖先
		bucket := ""
		for _, c := range chain {
			if b := bucketOf(c.Name); b != "" {
				bucket = b
				break
			}
		}

		for _, r := range leafFollowsParent {
			if !r.leaf.MatchString(lf.Name) {
				continue
			}
			hit := false
			for _, c := range chain {
				if r.parent.MatchString(c.Name) {
					hit = true
					break
				}
			}
			if hit {
				bucket = r.bucket
				break
			}
		}
		if bucket == "" {
			continue
		}

		sub := subOf(bucket, lf.Name)
		rules := subs[bucket]
		order := len(rules)
		for i, r := range rules {
			if r.name == sub {
				order = i
				break
			}
		}
		leaves = append(leaves, leafInfo{id: lf.ID, bucket: bucket, sub: sub, order: order})
	}
	return leaves
}

// loadClasses 带缓存与并发去重地拉取某站点的分类树。
func loadClasses(ctx context.Context, site models.Site) []models.SiteClass {
	classesMu.Lock()
	if e, ok := classCache[site.Key]; ok && nowMs()-e.at < classTTL {
		classesMu.Unlock()
		return e.classes
	}
	if p, ok := inflight[site.Key]; ok {
		classesMu.Unlock()
		select {
		case <-p.done:
			return p.classes
		case <-ctx.Done():
			return []models.SiteClass{}
		}
	}
	p := &pendingClasses{done: make(chan struct{})}
	inflight[site.Key] = p
	gen := generation
	classesMu.Unlock()

	classes, ok := fetchClasses(ctx, site)

	classesMu.Lock()
	// 期间换过配置源就别写回缓存了，否则上一套源的分类树会盖住新的；
	// 拉取失败（含取消）也不写，避免一次抖动把空目录缓存 10 分钟。
	if ok && gen == generation {
		classCache[site.Key] = classCacheEntry{at: nowMs(), classes: classes}
	}
	// 与 TS 一致：无条件删除。若清缓存后同 key 又注册了新任务，这里误删只会
	// 导致重复请求，不会污染缓存。
	delete(inflight, site.Key)
	classesMu.Unlock()
	p.classes = classes
	close(p.done)
	return classes
}

// fetchClasses 拉一次 `?ac=list`；分类拉不到不该让整个目录页崩掉，
// 失败时当作这个站点没有目录（ok=false 表示不缓存）。
func fetchClasses(ctx context.Context, site models.Site) ([]models.SiteClass, bool) {
	requestURL := cms.BuildURL(site, cms.Query{AC: "list"})
	// TS 不传 timeout，走统一网络层默认 20s
	text, err := httputil.FetchText(ctx, requestURL, httputil.Options{Header: sites.ResolveHeader(site, requestURL)})
	if err != nil {
		return []models.SiteClass{}, false
	}
	payload, err := httputil.ParseLooseJSON(text)
	if err != nil {
		return []models.SiteClass{}, false
	}
	if !cms.IsOK(payload) {
		return []models.SiteClass{}, false
	}
	m, _ := payload.(map[string]any)
	return normalizeClasses(models.Get(m, "class")), true
}

// buildBrowseURL 与 cms.BuildBrowseURL 等价，唯一区别是 pg 以 float64 传给 BuildURL：
// models.Str 不认 int（返回空串），基座 BuildBrowseURL 的签名把 pg 定成了 int，
// 拼出来会是 `pg=` 空值、翻页永远停在第一页（实测确认）。基座注释里的坑位说明原样保留：
// 模板式站点只有一个 {t} 槽位塞不下多个值，只取第一个叶子；多个叶子 id 不能并列进
// 同一次请求（重复 t 参数是「最后一个生效」而不是 OR），所以调用方对每个子分类
// 单独发一次请求再自己合并。基座修掉 Str/BuildBrowseURL 后本函数可以删掉。
func buildBrowseURL(site models.Site, ac string, types []string, pg int) string {
	api := site.API
	// 只要地址里有任何占位符就算模板式。不能只认 {t}：像
	// `...?ac={ac}&pg={pg}&wd={wd}` 这种没有 {t} 的模板，如果走下面的「追加参数」
	// 分支，模板里的字面量会和新参数同时留在 URL 上，服务端读到哪一个完全看它先取哪个，等于随机。
	if browseTemplatedRe.MatchString(api) {
		t := ""
		if len(types) > 0 {
			t = types[0]
		}
		return cms.BuildURL(site, cms.Query{AC: ac, T: t, PG: float64(pg)})
	}
	parts := []string{"ac=" + encodeVal(ac)}
	for _, t := range types {
		if t != "" {
			parts = append(parts, "t="+encodeVal(t))
		}
	}
	parts = append(parts, "pg="+encodeVal(float64(pg)))
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

// browseTemplatedRe 与基座 cms 内部的模板判定保持同一正则。
var browseTemplatedRe = regexp.MustCompile(`\{(ac|wd|kw|key|keyword|pg|page|t|type|typeid|class|ids|id|h)\}`)

// encodeVal encodeURIComponent 语义（空格转 %20 而非 +）。
func encodeVal(v any) string {
	return strings.ReplaceAll(url.QueryEscape(models.Str(v)), "+", "%20")
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
func trimStr(v any) string {
	return strings.TrimSpace(models.Str(v))
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

// containsName 字符串切片包含判定。
func containsName(list []string, name string) bool {
	for _, n := range list {
		if n == name {
			return true
		}
	}
	return false
}

// minInt 切片最小值。
func minInt(list []int) int {
	m := list[0]
	for _, n := range list[1:] {
		if n < m {
			m = n
		}
	}
	return m
}

// nowMs 当前毫秒时间戳（等价 Date.now()）。
func nowMs() int64 {
	return time.Now().UnixMilli()
}
