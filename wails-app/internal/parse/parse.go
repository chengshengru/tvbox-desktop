// Package parse TVBox 解析接口（parses）：把「分享页地址」换成真正能播的地址。
//
// 为什么需要它：不少采集站的线路给的不是直链，而是一个**分享页地址**
// （实测素博的 subyun 线路就是 https://play.xluuss.com/play/nelOoxJd），
// 这种地址交给播放器是播不出来的，得先拿它去问「解析接口」换一个真地址。
//
// 协议（TVBox 的约定）：把分享页地址做 URL 编码后接到接口后面，
//
//	https://okjx.cc/?url=https%3A%2F%2Fplay.xluuss.com%2Fplay%2FnelOoxJd
//
// 响应可能是三种东西，都要认：
//   - 直接 302 到 m3u8（看最终 URL）；
//   - JSON，地址在 url / data.url / result.url 之类的键里；
//   - 一段网页，地址藏在 var main = "..." 或 player_aaaa={"url":"..."} 里。
//
// 只做 HTTP 那一类（type=1/2）。type=0 是要真实浏览器嗅探的，先不碰。
// 翻译自 src/main/services/parse.ts。
//
// 作者：Shiro
package parse

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"tvbox-desktop/internal/httputil"
	"tvbox-desktop/internal/sources"
	"tvbox-desktop/internal/store"
	"tvbox-desktop/models"
)

// store 名：战绩与自定义解析，与 Electron 版 data/*.json 完全互认。
const (
	statsStoreName  = "parse-stats"
	customStoreName = "parses"
)

const (
	// parseTimeout 单条解析的超时（秒）。解析接口本身要替我们去第三方站点抓地址，给宽一点。
	parseTimeout = 8
	// parseBudget 一轮解析最多花多久。轮询完所有接口不能无限等下去。
	parseBudget = 24 * time.Second
	// maxHops 最多跟几跳。实测常见的是一跳 meta refresh，给 3 跳足够，也防止绕圈。
	maxHops = 3
)

// SampleTarget 设置页「测一下」默认拿这个网页地址当素材 —— 一个公开的视频页。
const SampleTarget = "https://v.qq.com/x/cover/mzc00200mp8vo9b.html"

// directLabel 这一步不走解析接口，只把「读出来」记在展示里，名字要让人看得懂。
const directLabel = "直接从播放页里取"

/* ---------- 地址判定 ---------- */

var (
	mediaRe  = regexp.MustCompile(`(?i)\.(m3u8|mp4|flv|mkv|ts|mov|avi|webm|m4v|mpd|mp3|m4a)(\?|#|$)`)
	directRe = regexp.MustCompile(`(?i)^(https?|rtmp|rtsp|file)://`)
	rtspRe   = regexp.MustCompile(`(?i)^(rtmp|rtsp|file):`)
	// nonMediaRe 一眼就能看出不是播放地址的后缀。
	//
	// 实测踩过两次：腾讯的剧集页里扫出 xxx_avatar.png；某个解析接口的跳板页里
	// 扫出 wasm_exec.js，还被当成「解析成功了」。这两种都要挡掉。
	// 反过来，**没有后缀**的地址要放行 —— 不少接口给的就是 …/live/abc?token=…。
	nonMediaRe = regexp.MustCompile(`(?i)\.(js|mjs|cjs|css|json|xml|html?|png|jpe?g|gif|webp|svg|ico|woff2?|ttf|eot|map|txt|apk|zip)(\?|#|$)`)
	// httpRe 用于校验解析接口地址前缀与跳板地址。
	httpRe  = regexp.MustCompile(`(?i)^https?://`)
	spaceRe = regexp.MustCompile(`\s`)
)

// isDirectMedia 这个地址能不能直接交给播放器。
//
// 判据只有「看起来是不是媒体文件」这一条 —— 不做域名白名单，因为采集站的 CDN
// 域名每天都在换，白名单一定会误判。反过来，把网页地址当直链播放的表现是
// 播放器一直转圈、不报错（实测过），所以宁可多走一次解析也不要把网页直接丢进去。
func isDirectMedia(raw string) bool {
	u := strings.TrimSpace(raw)
	if u == "" {
		return false
	}
	if rtspRe.MatchString(u) {
		return true
	}
	return mediaRe.MatchString(u)
}

// encodeComponent encodeURIComponent 语义（空格转 %20 而非 +；个别 !'()~* 的转义
// 差异服务端解码结果相同，不影响解析接口的使用）。
func encodeComponent(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// buildParseUrl 把分享页地址接成解析接口的请求地址。
//
// 三种写法都要认：
//   - ...?url=（TVBox 配置里最常见，末尾已经带等号，直接接）
//   - ...?a=1（已经有别的参数，用 & 接）
//   - .../jx/（什么都没有，用 ? 接）
func buildParseUrl(template string, target string) (string, error) {
	tpl := strings.TrimSpace(template)
	if tpl == "" {
		return "", errors.New("解析接口地址为空")
	}
	enc := encodeComponent(target)
	if urlPlaceholderRe.MatchString(tpl) {
		return urlPlaceholderRe.ReplaceAllLiteralString(tpl, enc), nil
	}
	if trailingEmptyParamRe.MatchString(tpl) {
		return tpl + enc, nil
	}
	sep := "?"
	if strings.Contains(tpl, "?") {
		sep = "&"
	}
	return tpl + sep + "url=" + enc, nil
}

var (
	urlPlaceholderRe     = regexp.MustCompile(`(?i)\{url\}`)
	trailingEmptyParamRe = regexp.MustCompile(`[?&][^=&]*=$`)
)

/* ---------- 从响应里挖地址 ---------- */

var (
	escapedSlashRe = regexp.MustCompile(`\\u002[fF]`)
	escapedAmpRe   = regexp.MustCompile(`(?i)\\u0026`)
)

func unescapeURL(raw string) string {
	out := strings.ReplaceAll(raw, `\/`, "/")
	out = escapedSlashRe.ReplaceAllString(out, "/")
	out = escapedAmpRe.ReplaceAllString(out, "&")
	out = strings.ReplaceAll(out, "&amp;", "&")
	return strings.TrimSpace(out)
}

var urlKeys = []string{"url", "playurl", "play_url", "m3u8", "src", "video", "link", "address"}

func looksLikeStream(u string) bool {
	if isDirectMedia(u) {
		return true
	}
	return !nonMediaRe.MatchString(u)
}

// asPlayable 宽松值 → 完整可播地址；不是字符串 / 不是完整地址 / 中间有空格都返回空串。
// 有空格多半是从一段 JS 里截出来的残片。
func asPlayable(value any) string {
	s, ok := value.(string)
	if !ok {
		return ""
	}
	u := unescapeURL(s)
	if !directRe.MatchString(u) || spaceRe.MatchString(u) {
		return ""
	}
	return u
}

var (
	hlsRe   = regexp.MustCompile(`(?i)https?://[^\s'"<>()]+\.m3u8(?:\?[^\s'"<>()]*)?`)
	otherRe = regexp.MustCompile(`(?i)https?://[^\s'"<>()]+\.(?:mp4|flv|mkv|mov|webm|m4v|mpd)(?:\?[^\s'"<>()]*)?`)
)

// scanMediaUrl 直接在整段响应里扫一个媒体文件地址。
//
// 这条比「猜变量名」有用得多：实测素博的分享页就是个 DPlayer 壳，地址写成
//
//	const vid = 'https://play.xluuss.com/play/nelOoxJd/index.m3u8';
//
// 变量名千奇百怪（vid / main / playerUrl…），但**后缀是骗不了人的**。
// 优先 m3u8，其次其它容器格式。
func scanMediaUrl(text string) string {
	flat := unescapeURL(text)
	if hit := hlsRe.FindString(flat); hit != "" {
		return hit
	}
	return otherRe.FindString(flat)
}

func deepFindUrl(node any, depth int) string {
	if node == nil || depth > 4 {
		return ""
	}
	if direct := asPlayable(node); direct != "" {
		return direct
	}
	if arr, ok := node.([]any); ok {
		for _, item := range arr {
			if hit := deepFindUrl(item, depth+1); hit != "" {
				return hit
			}
		}
		return ""
	}
	obj, ok := node.(map[string]any)
	if !ok {
		return ""
	}
	// 先按已知的键名找（Go map 无序，这里按 URL_KEYS 的优先级顺序找，结果更稳定）
	for _, want := range urlKeys {
		for k, v := range obj {
			if strings.EqualFold(k, want) {
				if hit := deepFindUrl(v, depth+1); hit != "" {
					return hit
				}
			}
		}
	}
	// 再进常见的容器键
	for _, key := range []string{"data", "result", "info", "content"} {
		if v, ok := obj[key]; ok {
			if hit := deepFindUrl(v, depth+1); hit != "" {
				return hit
			}
		}
	}
	return ""
}

var extractPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bvar\s+main\s*=\s*['"]([^'"]+)['"]`),
	regexp.MustCompile(`(?i)\bvar\s+url\s*=\s*['"]([^'"]+)['"]`),
	regexp.MustCompile(`(?i)"url"\s*:\s*"([^"]+)"`),
	regexp.MustCompile(`(?i)'url'\s*:\s*'([^']+)'`),
	regexp.MustCompile(`(?i)"playUrl"\s*:\s*"([^"]+)"`),
	regexp.MustCompile(`(?i)<video[^>]+src=["']([^"']+)["']`),
	regexp.MustCompile(`(?i)<source[^>]+src=["']([^"']+)["']`),
	regexp.MustCompile(`(?i)(?:src|url)\s*=\s*["'](https?:\\?/\\?/[^"']+)["']`),
}

// playerAaaaRe 爱奇艺那套播放器把结果塞在 player_aaaa 里。
var playerAaaaRe = regexp.MustCompile(`(?i)player_aaaa\s*=\s*(\{[\s\S]*?\})\s*;?\s*</script>`)

// ExtractUrl 从解析接口的响应里挖出真实播放地址。
//
// requireMedia 是给「自己读播放页」那条路用的：读的是**任意一个网页**，
// 里面的 src= / url= 可能是头像、广告、预告片，所以必须看得见媒体后缀才认
// （实测踩过：腾讯的剧集页里扫出个 xxx_avatar.png）。而解析接口是专门干这个的，
// 它返回什么就信什么 —— 有些接口给的是不带后缀的直链。
// 返回空串表示没挖到。
func ExtractUrl(body string, requireMedia bool) string {
	text := strings.TrimSpace(body)
	if text == "" {
		return ""
	}
	accept := func(value string) string {
		if value == "" {
			return ""
		}
		// 自己读网页时必须有媒体后缀；解析接口只要求「不像个静态资源」
		if requireMedia {
			if isDirectMedia(value) {
				return value
			}
			return ""
		}
		if looksLikeStream(value) {
			return value
		}
		return ""
	}

	if bare := accept(asPlayable(text)); bare != "" {
		return bare
	}

	if parsed, err := httputil.ParseLooseJSON(text); err == nil {
		if hit := accept(deepFindUrl(parsed, 0)); hit != "" {
			return hit
		}
	}
	// 不是 JSON 就继续按网页找

	// 整段扫一遍媒体地址 —— 比下面那串变量名模式管用得多
	if scanned := scanMediaUrl(text); scanned != "" {
		return scanned
	}

	for _, re := range extractPatterns {
		// 要遍历所有匹配：第一个往往是个图片或脚本地址，直接取第一个就会误判
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			if u := accept(asPlayable(m[1])); u != "" {
				return u
			}
		}
	}

	// 爱奇艺那套播放器把结果塞在 player_aaaa 里
	if pm := playerAaaaRe.FindStringSubmatch(text); pm != nil {
		if data, err := httputil.ParseLooseJSON(pm[1]); err == nil {
			if hit := accept(deepFindUrl(data, 0)); hit != "" {
				return hit
			}
		}
	}
	return ""
}

var redirectPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)<meta[^>]+http-equiv=["']?refresh["']?[^>]*content=["'][^"']*?url=([^"';\s>]+)`),
	regexp.MustCompile(`(?i)content=["'][^"']*?url=([^"';\s>]+)["'][^>]*http-equiv=["']?refresh`),
	regexp.MustCompile(`(?i)(?:window\.)?location\.href\s*=\s*['"]([^'"]+)['"]`),
	regexp.MustCompile(`(?i)(?:window\.)?location\.replace\(\s*['"]([^'"]+)['"]`),
	regexp.MustCompile(`(?i)(?:window\.)?location\s*=\s*['"]([^'"]+)['"]`),
}

// ExtractRedirect 这个响应本身是不是一个「去别处」的跳板。
//
// 实测公开解析接口很爱这么干：jx.aidouer.net 什么都不解析，只回一个
//
//	<meta http-equiv="refresh" content="3;url=https://jx.77flv.cc/?url=…">
//
// 真正干活的在下一跳。不跟过去就永远只得到「没有可识别的播放地址」。
// 返回空串表示不是跳板。
func ExtractRedirect(body string, base string) string {
	text := strings.TrimSpace(body)
	if text == "" {
		return ""
	}
	for _, re := range redirectPatterns {
		m := re.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		raw := unescapeURL(m[1])
		if !httpRe.MatchString(raw) {
			continue
		}
		ref, err := url.Parse(base)
		if err != nil {
			continue
		}
		next, err := url.Parse(raw)
		if err != nil {
			// 地址不合法就换下一个模式
			continue
		}
		// 等价 new URL(raw, base)：raw 为绝对地址时忽略 base
		resolved := ref.ResolveReference(next).String()
		if resolved != base {
			return resolved
		}
	}
	return ""
}

/* ---------- 解析列表 ---------- */

// flagsOf 取解析接口声明的线路标记（ext.flag 可能是数组也可能是逗号/竖线分隔的串）。
func flagsOf(item models.ParseItem) []string {
	ext, ok := item.Ext.(map[string]any)
	if !ok {
		return []string{}
	}
	raw := ext["flag"]
	switch v := raw.(type) {
	case []any:
		out := []string{}
		for _, it := range v {
			if s := strings.TrimSpace(models.Str(it)); s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		out := []string{}
		for _, part := range flagSplitRe.Split(v, -1) {
			if s := strings.TrimSpace(part); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return []string{}
}

var flagSplitRe = regexp.MustCompile(`[,|]`)

// configParses 配置里带的所有解析接口（只看已启用的配置源）。
func configParses() []models.ParseItem {
	out := []models.ParseItem{}
	for _, source := range sources.List() {
		if !source.Enabled {
			continue
		}
		parsed, ok := sources.GetParsed(source.ID)
		if ok && len(parsed.Parses) > 0 {
			out = append(out, parsed.Parses...)
		}
	}
	return out
}

func readStats() map[string]models.ParseStat {
	return store.ReadJSON[map[string]models.ParseStat](statsStoreName, map[string]models.ParseStat{})
}

// ResolveOptions 解析换址的选项。
type ResolveOptions struct {
	// Flag 线路名（详情里的 line.name），用来挑专门解析这条线路的接口。
	Flag string
	// Prefer 用户在界面上手动指定的一条解析（按名字或地址）。
	Prefer string
}

// AddInput 手动新增解析接口的入参。
type AddInput struct {
	Name  string
	Type  int
	URL   string
	Flags []string
}

// List 配置里带的 + 手动加的，去重后带上战绩。
func List() []models.ParseInfo {
	stats := readStats()
	custom := store.ReadJSON[[]models.ParseItem](customStoreName, nil)
	out := []models.ParseInfo{}
	seen := map[string]bool{}

	push := func(item models.ParseItem, origin string) {
		u := strings.TrimSpace(item.URL)
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		name := strings.TrimSpace(item.Name)
		if name == "" {
			name = u
		}
		// Go 的 int 区分不了「没填」和显式 0，与 config.normalizeParses 同一约定：0 视为 1
		typ := item.Type
		if typ == 0 {
			typ = 1
		}
		out = append(out, models.ParseInfo{
			ParseItem: models.ParseItem{Name: name, Type: typ, URL: u, Ext: item.Ext},
			Origin:    origin,
			Flags:     flagsOf(item),
			Stat:      stats[u],
		})
	}

	for _, item := range configParses() {
		push(item, "config")
	}
	for _, item := range custom {
		push(item, "custom")
	}
	return out
}

// Add 手动新增一条解析接口，返回操作后的全量列表。
func Add(input AddInput) ([]models.ParseInfo, error) {
	u := strings.TrimSpace(input.URL)
	if !httpRe.MatchString(u) {
		return nil, errors.New("解析接口地址要以 http(s):// 开头")
	}
	all := List()
	for _, p := range all {
		if p.URL == u {
			return nil, errors.New("这条解析接口已经有了")
		}
	}

	custom := store.ReadJSON[[]models.ParseItem](customStoreName, nil)
	flags := []string{}
	for _, f := range input.Flags {
		if s := strings.TrimSpace(f); s != "" {
			flags = append(flags, s)
		}
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = u
	}
	// 与 TS 的 `type ?? 1` 对齐：Go 里 0 视为未填
	typ := input.Type
	if typ == 0 {
		typ = 1
	}
	item := models.ParseItem{Name: name, Type: typ, URL: u}
	if len(flags) > 0 {
		item.Ext = map[string]any{"flag": flags}
	}
	custom = append(custom, item)
	store.WriteStore(customStoreName, custom)
	return List(), nil
}

// Remove 删掉手动加的解析接口（配置里带的删不掉），返回操作后的全量列表。
func Remove(rawURL string) ([]models.ParseInfo, error) {
	target := strings.TrimSpace(rawURL)
	custom := store.ReadJSON[[]models.ParseItem](customStoreName, nil)
	out := []models.ParseItem{}
	for _, item := range custom {
		if strings.TrimSpace(item.URL) != target {
			out = append(out, item)
		}
	}
	store.WriteStore(customStoreName, out)
	return List(), nil
}

// ResetStats 清空可用性排行（换了网络环境、或者想重新比一遍时用）。
func ResetStats() []models.ParseInfo {
	store.WriteStore(statsStoreName, map[string]models.ParseStat{})
	return List()
}

/* ---------- 排行 ---------- */

// scoreOf 战绩打分。
func scoreOf(stat models.ParseStat) float64 {
	// 拉普拉斯平滑：没测过的排在 1 胜 0 负和 0 胜 1 负之间，不会因为「没数据」被压到最后
	rate := float64(stat.OK+1) / float64(stat.OK+stat.Fail+2)
	speed := 0.5
	if stat.LastMs > 0 {
		speed = math.Max(0, 1-math.Min(float64(stat.LastMs), 8000)/8000)
	}
	return rate*2 + speed
}

// orderParses 决定先试哪条解析。
//
// 分档比按分数排更重要：声明了「我专门解析 subyun 这条线路」的接口，
// 一定要排在通配的那批前面 —— 否则它可能被一堆更快的通配接口挤到后面，
// 而通配接口对这条线路其实是解析不了的。
func orderParses(list []models.ParseInfo, flag string, prefer string) []models.ParseInfo {
	tier := func(p models.ParseInfo) int {
		if prefer != "" && (p.Name == prefer || p.URL == prefer) {
			return 0
		}
		if len(p.Flags) > 0 && flag != "" {
			for _, f := range p.Flags {
				if strings.EqualFold(f, flag) {
					return 1
				}
			}
		}
		if len(p.Flags) == 0 {
			return 2
		}
		return 3
	}
	// type=0 是要真实浏览器嗅探的，纯 HTTP 多半换不出东西，放最后
	kind := func(p models.ParseInfo) int {
		if p.Type == 0 {
			return 1
		}
		return 0
	}

	out := make([]models.ParseInfo, len(list))
	copy(out, list)
	// sort.SliceStable 对齐 JS 稳定排序（ES2019 起 Array.sort 保证稳定）
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if t := tier(a) - tier(b); t != 0 {
			return t < 0
		}
		if k := kind(a) - kind(b); k != 0 {
			return k < 0
		}
		return scoreOf(a.Stat) > scoreOf(b.Stat)
	})
	return out
}

/* ---------- 单条解析 ---------- */

// headersOf 解析接口自带的请求头；解析接口基本都校验来源，不带就是 403。
func headersOf(item models.ParseInfo) map[string]string {
	out := map[string]string{}
	if ext, ok := item.Ext.(map[string]any); ok {
		if header, ok := ext["header"].(map[string]any); ok {
			for k, v := range header {
				if s, ok := v.(string); ok {
					out[k] = s
				}
			}
		}
	}
	if _, has := out["Referer"]; !has {
		out["Referer"] = item.URL
	}
	return out
}

// walk 从一个地址出发，跟着跳转、逐跳找播放地址。
// 「读播放页本身」和「问解析接口」走的是同一套逻辑，只是起点和请求头不同。
func walk(ctx context.Context, start string, headers map[string]string, label string, strict bool) models.ParseAttempt {
	started := time.Now()
	current := start
	for hop := 0; hop < maxHops; hop++ {
		api := current
		res, err := httputil.Request(ctx, api, httputil.Options{Timeout: parseTimeout, Header: headers})
		if err != nil {
			return models.ParseAttempt{Name: label, OK: false, Ms: time.Since(started).Milliseconds(), Error: err.Error()}
		}
		ms := time.Since(started).Milliseconds()

		// 一：自己 302 到了媒体文件
		if res.FinalURL != "" && res.FinalURL != api && isDirectMedia(res.FinalURL) {
			return models.ParseAttempt{Name: label, OK: true, Ms: ms, URL: res.FinalURL}
		}
		// 二：从响应体里挖
		text := httputil.DecodeResponse(res, "")
		found := ExtractUrl(text, strict)
		if found != "" && found != api {
			return models.ParseAttempt{Name: label, OK: true, Ms: ms, URL: found}
		}
		// 三：这是个跳板，跟过去接着找
		base := res.FinalURL
		if base == "" {
			base = api
		}
		next := ExtractRedirect(text, base)
		if next == "" || next == api {
			break
		}
		current = next
	}
	errMsg := "跟着跳转找了几跳，还是没拿到播放地址"
	if current == start {
		errMsg = "没有拿到可识别的播放地址"
	}
	return models.ParseAttempt{Name: label, OK: false, Ms: time.Since(started).Milliseconds(), Error: errMsg}
}

// tryDirect 先自己读一遍播放页。
//
// 很多采集站的「分享页」就是个 DPlayer 壳，地址明文写在脚本里
// （实测素博的 subyun 线路就是），一次 GET 就能拿到，不必麻烦任何第三方解析。
// 读不出来才去问解析接口 —— 顺序反过来的话，明明能自己解决的地址也要绕一圈外网。
func tryDirect(ctx context.Context, target string) models.ParseAttempt {
	headers := map[string]string{}
	if u, err := url.Parse(target); err == nil && u.Scheme != "" && u.Host != "" {
		headers["Referer"] = u.Scheme + "://" + u.Host + "/"
	}
	// 相对地址之类，不补来源
	return walk(ctx, target, headers, directLabel, true)
}

func tryParse(ctx context.Context, item models.ParseInfo, target string) models.ParseAttempt {
	api, err := buildParseUrl(item.URL, target)
	if err != nil {
		return models.ParseAttempt{Name: item.Name, OK: false, Ms: 0, Error: err.Error()}
	}
	return walk(ctx, api, headersOf(item), item.Name, false)
}

// statsMu 战绩读改写互斥：轮询并发时防止丢更新。
var statsMu sync.Mutex

// recordStat 记一次战绩（成功加 ok、失败加 fail 并留错误原因）。
func recordStat(rawURL string, attempt models.ParseAttempt) {
	statsMu.Lock()
	defer statsMu.Unlock()
	stats := readStats()
	cur := stats[rawURL]
	if attempt.OK {
		stats[rawURL] = models.ParseStat{OK: cur.OK + 1, Fail: cur.Fail, LastOK: time.Now().UnixMilli(), LastMs: attempt.Ms}
	} else {
		stats[rawURL] = models.ParseStat{OK: cur.OK, Fail: cur.Fail + 1, LastOK: cur.LastOK, LastMs: cur.LastMs, LastError: attempt.Error}
	}
	store.WriteStore(statsStoreName, stats)
}

/* ---------- 对外入口 ---------- */

// Resolve 把一个播放地址变成「真的能播的地址」。
//
// 直链直接放行；网页地址则先自己读一遍播放页，再并发轮询所有解析接口，
// 第一个成功的就返回（先到先得），其余随预算 context 一起取消。
// 失败**不返回异常语义的残缺结果**，而是把每条接口的失败原因一起带回来 ——
// 界面上要能列出来让用户知道是「全都挂了」还是「某一条超时」。
func Resolve(ctx context.Context, rawURL string, opts ResolveOptions) (models.ParseResult, error) {
	u := strings.TrimSpace(rawURL)
	if u == "" {
		return models.ParseResult{}, errors.New("没有播放地址")
	}

	if isDirectMedia(u) {
		return models.ParseResult{OK: true, URL: u, Direct: true, Ms: 0, Attempts: []models.ParseAttempt{}}, nil
	}

	list := List()
	started := time.Now()
	attempts := []models.ParseAttempt{}

	// 第一步：先自己把播放页读一遍。很多分享页地址就明文写在里面。
	direct := tryDirect(ctx, u)
	attempts = append(attempts, direct)
	if direct.OK && direct.URL != "" {
		return models.ParseResult{
			OK: true, URL: direct.URL, Direct: false, ParseName: direct.Name,
			Ms: time.Since(started).Milliseconds(), Attempts: attempts,
		}, nil
	}

	if len(list) == 0 {
		return models.ParseResult{
			OK: false, URL: "", Direct: false,
			Ms:       time.Since(started).Milliseconds(),
			Attempts: attempts,
			Error:    "这个地址是网页而不是直链，播放页里也没找到播放地址，而配置里一条解析接口都没有",
		}, nil
	}

	ordered := orderParses(list, opts.Flag, opts.Prefer)

	// 轮询：全部接口并发发出去，单接口 8s、整体 24s 都由 context 预算兜底；
	// 谁先解析成功谁先回，其余随 budgetCtx 取消自动收尾。
	budgetCtx, cancelBudget := context.WithTimeout(ctx, parseBudget)
	defer cancelBudget()
	type polled struct {
		item    models.ParseInfo
		attempt models.ParseAttempt
	}
	ch := make(chan polled, len(ordered))
	for _, item := range ordered {
		go func(p models.ParseInfo) {
			attemptCtx, cancel := context.WithTimeout(budgetCtx, parseTimeout*time.Second)
			defer cancel()
			ch <- polled{item: p, attempt: tryParse(attemptCtx, p, u)}
		}(item)
	}

	done := 0
	for done < len(ordered) {
		select {
		case got := <-ch:
			done++
			attempt := got.attempt
			attempts = append(attempts, attempt)
			recordStat(got.item.URL, attempt)
			if attempt.OK && attempt.URL != "" {
				return models.ParseResult{
					OK: true, URL: attempt.URL, Direct: false, ParseName: attempt.Name,
					Ms: time.Since(started).Milliseconds(), Attempts: attempts,
				}, nil
			}
		case <-budgetCtx.Done():
			// 预算耗尽：还没回来的按「没轮到」记进尝试列表
			for i := done; i < len(ordered); i++ {
				attempts = append(attempts, models.ParseAttempt{Name: ordered[i].Name, OK: false, Ms: 0, Error: "整体超时，没轮到这条"})
			}
			done = len(ordered)
		}
	}

	reason := "未知原因"
	for _, a := range attempts {
		if a.Error != "" {
			reason = a.Error
			break
		}
	}
	return models.ParseResult{
		OK: false, URL: "", Direct: false,
		Ms:       time.Since(started).Milliseconds(),
		Attempts: attempts,
		Error:    fmt.Sprintf("%d 条路子都没能换出地址（%s）", len(attempts), reason),
	}, nil
}

// Test 单独测一条解析接口。target 留空时用默认素材。
func Test(ctx context.Context, url string, target string) (models.ParseTestResult, error) {
	use := strings.TrimSpace(target)
	if use == "" {
		use = SampleTarget
	}
	list := List()
	var item *models.ParseInfo
	for i := range list {
		if list[i].URL == url {
			item = &list[i]
			break
		}
	}
	if item == nil {
		return models.ParseTestResult{}, errors.New("没有这条解析接口")
	}
	attemptCtx, cancel := context.WithTimeout(ctx, parseTimeout*time.Second)
	defer cancel()
	attempt := tryParse(attemptCtx, *item, use)
	recordStat(item.URL, attempt)
	return models.ParseTestResult{
		OK:     attempt.OK,
		Ms:     attempt.Ms,
		Target: use,
		URL:    attempt.URL,
		Error:  attempt.Error,
	}, nil
}
