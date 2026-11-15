// Package live 电视直播源的解析。
// 翻译自 src/main/services/live.ts。
//
// 配置里的 lives 只有元信息（name / type / url / epg），真正的内容要另外拉一次。
// 两种格式都要吃：
//
//	· m3u —— 标准的 #EXTM3U / #EXTINF:-1 tvg-name="..." group-title="...",显示名
//	· txt —— 国内常见的「分组名,#genre#」开头，随后每行「频道名,地址」
//
// type 字段只能当提示，实际以内容判断（配置写错了也能用）。
//
// 作者：Shiro
package live

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"

	"tvbox-desktop/internal/httputil"
	"tvbox-desktop/internal/settings"
	"tvbox-desktop/internal/sources"
	"tvbox-desktop/models"
)

// rawChannel 一条原始频道。
type rawChannel struct {
	Name  string
	URL   string
	Group string
	Logo  string
}

// channelWithSource 带来源名的原始频道（合并线路时用）。
type channelWithSource struct {
	rawChannel
	Source string
}

// liveSourceRef 一个待载入的直播源。
type liveSourceRef struct {
	Name string
	URL  string
}

// addressRe 能直接当播放地址的写法（huya:// 与 douyin:// 是房间号，播放前才换成真地址）。
var addressRe = regexp.MustCompile(`(?i)^(https?|rtmp|rtsp|rtp|udp|file|huya|douyin)://`)

var (
	httpRe    = regexp.MustCompile(`(?i)^https?://`)
	fileURLRe = regexp.MustCompile(`(?i)^file:///?`)
	extinfRe  = regexp.MustCompile(`(?i)^#EXTINF`)
	genreRe   = regexp.MustCompile(`(?i)#genre#?`)
	m3uHeadRe = regexp.MustCompile(`(?i)#EXTM3U|#EXTINF`)
	bracketRe = regexp.MustCompile(`[「」【】《》\[\]（）()]`)
	otherRe   = regexp.MustCompile(`^(未分组|其他|其它|默认|default|undefined|null)$`)
	// 分组名归一用的前导装饰符：\s 之外再补上 TS \s 覆盖的常见 Unicode 空白
	leadingDecoratorRe = regexp.MustCompile(`^[\s\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000\ufeff•·・*\-—]+`)
)

// groupAlias 分组别名归并规则（对应 TS GROUP_ALIAS）。
var groupAlias = []struct {
	re   *regexp.Regexp
	name string
}{
	{re: regexp.MustCompile(`^游戏`), name: "游戏赛事"},
	{re: regexp.MustCompile(`^影视`), name: "影视轮播"},
	{re: regexp.MustCompile(`^咪咕`), name: "咪咕移动"},
}

// zhCollator 中文排序器，等价 JS 的 localeCompare(x, 'zh-CN')。
var zhCollator = collate.New(language.MustParse("zh-CN"))

// stripDecorator 去掉分组名里的装修符。
func stripDecorator(raw string) string {
	s := leadingDecoratorRe.ReplaceAllString(raw, "")
	s = bracketRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// normalizeGroup 分组名归一。
//
// 公开源里的分组名带装修符，同一类内容在不同源里写法不一样：
// 「•游戏「赛事」」去掉符号后是「游戏赛事」，跟本地内置的「游戏赛事」要落到同一组，
// 否则用户会看到两个意思一样的分组。
func normalizeGroup(raw string) string {
	name := stripDecorator(raw)
	if name == "" {
		return "其它"
	}
	if otherRe.MatchString(strings.ToLower(name)) {
		return "其它"
	}
	for _, alias := range groupAlias {
		if alias.re.MatchString(name) {
			return alias.name
		}
	}
	return name
}

// pickAttr 从 #EXTINF 的属性段里取 key="value"。
func pickAttr(attrs, key string) string {
	re := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(key) + `="([^"]*)"`)
	m := re.FindStringSubmatch(attrs)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// parseM3u 解析标准 m3u 播放列表。
func parseM3u(text string) []rawChannel {
	out := []rawChannel{}
	var pending *rawChannel

	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}

		if extinfRe.MatchString(line) {
			// 属性段在冒号之后、最后一个逗号之前；逗号之后的才是显示名
			body := line
			if i := strings.Index(line, ":"); i >= 0 {
				body = line[i+1:]
			}
			attrs, label := body, ""
			if comma := strings.LastIndex(body, ","); comma >= 0 {
				attrs = body[:comma]
				label = strings.TrimSpace(body[comma+1:])
			}
			name := pickAttr(attrs, "tvg-name")
			if name == "" {
				name = label
			}
			pending = &rawChannel{
				Name:  name,
				Group: pickAttr(attrs, "group-title"),
				Logo:  pickAttr(attrs, "tvg-logo"),
			}
			continue
		}

		if strings.HasPrefix(line, "#") {
			continue
		}

		if pending != nil {
			ch := rawChannel{
				Name:  pending.Name,
				URL:   line,
				Group: normalizeGroup(pending.Group),
				Logo:  pending.Logo,
			}
			if ch.Name == "" {
				ch.Name = line
			}
			out = append(out, ch)
			pending = nil
		}
	}

	filtered := out[:0]
	for _, c := range out {
		if c.URL != "" && c.Name != "" {
			filtered = append(filtered, c)
		}
	}
	return filtered
}

// parseTxt 解析国内常见的「分组名,#genre#」txt 源。
func parseTxt(text string) []rawChannel {
	out := []rawChannel{}
	group := "其它"

	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}

		// 「央视频道,#genre#」——#genre# 也可能写成 #genre 或 #类型#
		if genreRe.MatchString(line) {
			group = normalizeGroup(strings.SplitN(line, ",", 2)[0])
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}

		idx := strings.Index(line, ",")
		if idx <= 0 {
			continue
		}
		name := strings.TrimSpace(line[:idx])
		addr := strings.TrimSpace(line[idx+1:])
		if addr == "" {
			continue
		}
		if addressRe.MatchString(addr) {
			out = append(out, rawChannel{Name: name, URL: addr, Group: group})
			continue
		}
		// 有些 txt 会把一堆地址塞在同一行，用 # 分隔
		for _, piece := range strings.Split(addr, "#") {
			one := strings.TrimSpace(piece)
			if one != "" {
				out = append(out, rawChannel{Name: name, URL: one, Group: group})
			}
		}
	}
	return out
}

// parseLiveContent 按内容判断格式：有 #EXTM3U / #EXTINF 就是 m3u，其余当 txt。
func parseLiveContent(text string) []rawChannel {
	if m3uHeadRe.MatchString(text) {
		return parseM3u(text)
	}
	return parseTxt(text)
}

// readSource 拉取（或本地读取）一个直播源内容。
func readSource(ctx context.Context, rawURL string, timeout float64) (string, error) {
	if httpRe.MatchString(strings.TrimSpace(rawURL)) {
		return httputil.FetchText(ctx, rawURL, httputil.Options{Timeout: timeout})
	}
	// 本地文件：支持 file:/// 与 Windows 盘符路径
	path := fileURLRe.ReplaceAllString(rawURL, "")
	path, err := url.PathUnescape(path)
	if err != nil {
		return "", fmt.Errorf("本地直播源地址非法：%s", rawURL)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读取本地直播源失败：%s", rawURL)
	}
	return string(data), nil
}

// mergeChannels 合并同一频道的多条线路：同名频道收成一张卡，卡上列出来自哪些源。
func mergeChannels(raw []channelWithSource) []models.LiveGroupEntry {
	// 同名频道合并（JS Map 保插入序，Go 用「切片 + 下标表」等价实现）
	byName := map[string]int{}
	entries := []models.LiveChannelEntry{}
	for _, ch := range raw {
		if i, ok := byName[ch.Name]; ok {
			hit := &entries[i]
			exists := false
			for _, u := range hit.URLs {
				if u.URL == ch.URL {
					exists = true
					break
				}
			}
			if !exists {
				hit.URLs = append(hit.URLs, models.LiveURL{URL: ch.URL, Source: ch.Source})
			}
			if hit.Logo == "" && ch.Logo != "" {
				hit.Logo = ch.Logo
			}
			continue
		}
		byName[ch.Name] = len(entries)
		entries = append(entries, models.LiveChannelEntry{
			Name:  ch.Name,
			Group: ch.Group,
			Logo:  ch.Logo,
			URLs:  []models.LiveURL{{URL: ch.URL, Source: ch.Source}},
		})
	}

	// 按分组归并（同样保插入序）
	groupIdx := map[string]int{}
	groups := []models.LiveGroupEntry{}
	for _, ch := range entries {
		i, ok := groupIdx[ch.Group]
		if !ok {
			i = len(groups)
			groupIdx[ch.Group] = i
			groups = append(groups, models.LiveGroupEntry{Name: ch.Group})
		}
		groups[i].Channels = append(groups[i].Channels, ch)
	}

	for i := range groups {
		chs := groups[i].Channels
		sort.SliceStable(chs, func(a, b int) bool {
			return zhCollator.CompareString(chs[a].Name, chs[b].Name) < 0
		})
	}

	// 「其它」永远排最后，其余按频道数从多到少
	sort.SliceStable(groups, func(a, b int) bool {
		ga, gb := groups[a], groups[b]
		if ga.Name == "其它" {
			return false
		}
		if gb.Name == "其它" {
			return true
		}
		if len(ga.Channels) != len(gb.Channels) {
			return len(ga.Channels) > len(gb.Channels)
		}
		return zhCollator.CompareString(ga.Name, gb.Name) < 0
	})
	return groups
}

// collectLiveSources 从所有启用配置源里收集 lives，并去重。
func collectLiveSources(parsed []*models.ParsedConfig) []liveSourceRef {
	seen := map[string]bool{}
	out := []liveSourceRef{}
	for _, cfg := range parsed {
		for _, live := range cfg.Lives {
			u := strings.TrimSpace(live.URL)
			if u == "" || seen[u] {
				continue
			}
			seen[u] = true
			name := live.Name
			if name == "" {
				name = u
			}
			out = append(out, liveSourceRef{Name: name, URL: u})
		}
	}
	return out
}

// builtinLive 内置的自定义频道。
//
// 这些不是任何 IPTV 订阅能提供的东西 —— 它们是某个主播的房间，地址要现解析
// （见 internal/stream）。写死在代码里，跟着应用走：换订阅、跑自检清空配置源都不会把它们弄丢。
//
// 分组名「游戏赛事」会被 normalizeGroup 的别名规则并到公开源的「•游戏「赛事」」里。
const builtinLive = `游戏赛事,#genre#
danking直播,huya://10188
`

// 内置频道始终算一个直播源。
const (
	builtinSourceName = "内置自定义频道"
	builtinSourceURL  = "builtin://custom"
)

// liveCacheEntry 载入缓存项。
type liveCacheEntry struct {
	at     int64
	result models.LiveResult
}

const cacheTTL = 10 * 60 * 1000

var (
	cacheMu    sync.Mutex
	cache      *liveCacheEntry
	generation int
)

// ClearCache 清空直播载入缓存。
//
// 光清 cache 不够：很可能正好有一次 Load 还在飞，它跑完会把**上一个源**
// 的结果写回 cache。表现成「同步完新订阅再进电视直播，看到的还是旧分组」，
// 而且只在换源和首次加载撞上时才复现（自检第一次跑必挂就是这么来的）。
func ClearCache() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cache = nil
	generation++
}

// Load 载入全部直播源并合并成分组。force 时跳过 TTL 缓存。
func Load(ctx context.Context, force bool) (models.LiveResult, error) {
	now := time.Now().UnixMilli()
	cacheMu.Lock()
	if !force && cache != nil && now-cache.at < cacheTTL {
		result := cache.result
		cacheMu.Unlock()
		return result, nil
	}
	gen := generation
	cacheMu.Unlock()

	list := sources.List()
	parsed := []*models.ParsedConfig{}
	for _, s := range list {
		if !s.Enabled {
			continue
		}
		if p, ok := sources.GetParsed(s.ID); ok && p != nil {
			parsed = append(parsed, p)
		}
	}
	srcs := collectLiveSources(parsed)
	st := settings.Get()

	all := []channelWithSource{}
	stats := []models.LiveSourceStat{}

	// 内置频道先塞进去，这样即使一个配置源都没有，电视直播页也不是空的
	builtin := parseLiveContent(builtinLive)
	for _, ch := range builtin {
		all = append(all, channelWithSource{rawChannel: ch, Source: builtinSourceName})
	}
	stats = append(stats, models.LiveSourceStat{Name: builtinSourceName, URL: builtinSourceURL, Channels: len(builtin)})

	// 并发载入各源；合并时按源顺序落位（TS 用 Promise.all 完成序不确定，
	// 这里改为固定顺序，保证同名频道的分组/logo 归属可复现）
	type sourceLoad struct {
		channels []rawChannel
		err      string
	}
	loads := make([]sourceLoad, len(srcs))
	var wg sync.WaitGroup
	for i, src := range srcs {
		wg.Add(1)
		go func(i int, src liveSourceRef) {
			defer wg.Done()
			text, err := readSource(ctx, src.URL, st.Timeout)
			if err != nil {
				loads[i].err = err.Error()
				return
			}
			loads[i].channels = parseLiveContent(text)
		}(i, src)
	}
	wg.Wait()

	for i, src := range srcs {
		loaded := loads[i]
		if loaded.err != "" {
			stats = append(stats, models.LiveSourceStat{Name: src.Name, URL: src.URL, Channels: 0, Error: loaded.err})
			continue
		}
		if len(loaded.channels) == 0 {
			stats = append(stats, models.LiveSourceStat{Name: src.Name, URL: src.URL, Channels: 0, Error: "没有解析出任何频道，格式可能不兼容"})
			continue
		}
		for _, ch := range loaded.channels {
			all = append(all, channelWithSource{rawChannel: ch, Source: src.Name})
		}
		stats = append(stats, models.LiveSourceStat{Name: src.Name, URL: src.URL, Channels: len(loaded.channels)})
	}

	result := models.LiveResult{Groups: mergeChannels(all), Total: len(all), Sources: stats}
	// 期间换过配置源就说明这份结果已经过期了，别写回缓存 —— 照旧把本次结果
	// 返回给调用方（它要的就是当下这一次），但不让它污染下一次。
	cacheMu.Lock()
	if gen == generation {
		cache = &liveCacheEntry{at: time.Now().UnixMilli(), result: result}
	}
	cacheMu.Unlock()
	return result, nil
}
