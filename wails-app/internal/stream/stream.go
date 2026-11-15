// Package stream 房间号频道（虎牙 / 抖音）的地址解析。
// 翻译自 src/main/services/streamResolve.ts。
//
// 为什么不能在直播源里直接写 m3u8：
//
//	· 虎牙 `data.stream.hls.multiLine[].url` 带 wsSecret / wsTime / fm，几小时就失效；
//	· 抖音 `hls_pull_url_map` 每次进房都不一样。
//
// 所以直播源里只写 `huya://<房间号>` 或 `douyin://<web_rid>`，用户点开频道时才换真地址。
//
// 两个平台都校验 Referer，而且必须带浏览器 UA，缺任一个都是 403。
//
// 作者：Shiro
package stream

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"tvbox-desktop/internal/httputil"
	"tvbox-desktop/models"
)

// 解析时不能设太短：抖音要先取一次首页拿 ttwid，再进房。
const (
	resolveTimeout = 12 * time.Second
	probeTimeout   = 8 * time.Second
)

// roomURLRe 房间号频道写法（与 shared/liveUrl.ts 的 ROOM_URL_RE 一致）。
var roomURLRe = regexp.MustCompile(`(?i)^(huya|douyin)://(\d+)$`)

// huyaM3u8Re 虎牙线路过滤：只认地址里带 .m3u8 的。
var huyaM3u8Re = regexp.MustCompile(`(?i)\.m3u8`)

// fetchTransport 与全局网络层同构的 Transport：连接复用 + 忽略非常规证书（采集站常见自签）。
var fetchTransport = &http.Transport{
	MaxIdleConns:        64,
	MaxIdleConnsPerHost: 8,
	IdleConnTimeout:     60 * time.Second,
	TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
	Proxy:               http.ProxyFromEnvironment,
}

// fetchClient 需要拿原始响应头（多值 Set-Cookie），不能走整体读进内存的 httputil.Request。
var fetchClient = &http.Client{Transport: fetchTransport}

// fetchRaw 拉一个地址，返回正文与响应里的 Set-Cookie 裸值（name=value）。
// 对应 TS 的 fetchRaw：fetch 对非 2xx 不抛错，正文照读；
// 多值 cookie 用 res.Header["Set-Cookie"] 收集（对应 getSetCookie），ttwid 等缺一不可。
func fetchRaw(ctx context.Context, rawURL string, headers map[string]string, timeout time.Duration) (string, []string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", nil, fmt.Errorf("解析失败：非法地址 %s", rawURL)
	}
	req.Header.Set("User-Agent", httputil.DefaultUA)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := fetchClient.Do(req)
	if err != nil {
		if reqCtx.Err() == context.DeadlineExceeded {
			return "", nil, fmt.Errorf("解析超时（%ds）", int(timeout.Seconds()))
		}
		return "", nil, fmt.Errorf("解析失败：%s", trimErr(err))
	}
	defer res.Body.Close()

	data, err := io.ReadAll(res.Body)
	if err != nil {
		if reqCtx.Err() == context.DeadlineExceeded {
			return "", nil, fmt.Errorf("解析超时（%ds）", int(timeout.Seconds()))
		}
		return "", nil, fmt.Errorf("解析失败：%s", trimErr(err))
	}

	// 每条 Set-Cookie 只取 name=value 段（对应 c.split(';')[0]）
	bare := make([]string, 0, len(res.Header["Set-Cookie"]))
	for _, c := range res.Header["Set-Cookie"] {
		if i := strings.Index(c, ";"); i >= 0 {
			c = c[:i]
		}
		bare = append(bare, strings.TrimSpace(c))
	}
	return string(data), bare, nil
}

// probePlaylist 拉一个地址看它到底是哪种 m3u8。
//
// 这里必须区分「媒体播放列表」和「主播放列表」，否则会踩一个大坑：
// 虎牙 TX/AL 两条线给的是**主播放列表**（只有 `#EXT-X-STREAM-INF`），
// 里面那一条变体地址被改写成 `http://<边缘节点IP>/tx.hls.huya.com/src/...`，
// 浏览器带着 `Host: <IP>` 去拉一律 403（实测过还原主机名、加 Origin、加 Referer 都还是 403）。
// 而 HS 那条直接给**媒体播放列表**（有 `#EXTINF`），分片是相对路径，实测 200 + `video/MP2T`。
// 所以优先要 media，master 只当兜底。
func probePlaylist(ctx context.Context, rawURL, referer string) (ok bool, media bool) {
	text, _, err := fetchRaw(ctx, rawURL, map[string]string{"Referer": referer, "Accept": "*/*"}, probeTimeout)
	if err != nil {
		return false, false
	}
	if !strings.Contains(text, "#EXTM3U") {
		return false, false
	}
	return true, strings.Contains(text, "#EXTINF")
}

/* ---------- 会话层补 Referer ---------- */

// refererRules 命中平台规则时要补的请求头。
//
// hls.js 用 XHR 拉播放列表和分片，而 `Referer` 是浏览器的 forbidden header ——
// `xhr.setRequestHeader('Referer', …)` 会被**静默忽略**。实测发出去的是页面自己的来源
// （`http://localhost:5173/`），虎牙边缘节点因此对分片回 403，播放器停在 readyState 0
// 且不报错（`video.error` 一直是 null），非常难查。
//
// Electron 版用 `webRequest.onBeforeSendHeaders` 在会话层补写；Go 版由 mediamw
// 同源中间件转发前查这张表注入（见改造方案 §5.1/§5.2）。
// 只按域名后缀判断，因为虎牙会把分片地址改写成 `http://<边缘节点IP>/tx.hls.huya.com/src/…`，
// 主机名跑到了 path 里，按 host 匹配会漏。
var refererRules = []struct {
	re      *regexp.Regexp
	referer string
	origin  string
}{
	{re: regexp.MustCompile(`(?i)huya\.com`), referer: "https://www.huya.com/", origin: "https://www.huya.com"},
	{
		re:      regexp.MustCompile(`(?i)(douyin|douyincdn|bytefcdn|volccdn|pstatp|ixigua|bytedance|byteimg|snssdk)\.com`),
		referer: "https://live.douyin.com/",
		origin:  "https://live.douyin.com",
	},
}

// HeadersFor 命中平台规则时返回要补的头部；https 才补 Origin（http 下实测不需要，多补反而可能被拒）。
// 未命中返回 nil，供 mediamw 查表。
func HeadersFor(rawURL string) map[string]string {
	for _, rule := range refererRules {
		if rule.re.MatchString(rawURL) {
			headers := map[string]string{"Referer": rule.referer}
			if strings.HasPrefix(rawURL, "https:") {
				headers["Origin"] = rule.origin
			}
			return headers
		}
	}
	return nil
}

/* ---------- 虎牙 ---------- */

// huyaLine 虎牙的一条 HLS 线路。
type huyaLine struct {
	URL     string
	CDNType string
}

func resolveHuya(ctx context.Context, roomID string) (models.LiveResolved, error) {
	referer := "https://www.huya.com/"
	text, _, err := fetchRaw(ctx,
		"https://mp.huya.com/cache.php?m=Live&do=profileRoom&roomid="+roomID,
		map[string]string{"Referer": referer, "Accept": "application/json, text/plain, */*"},
		resolveTimeout)
	if err != nil {
		return models.LiveResolved{}, err
	}

	v, perr := httputil.ParseLooseJSON(text)
	if perr != nil {
		return models.LiveResolved{}, fmt.Errorf("虎牙返回的不是 JSON，接口可能改了")
	}
	jsonObj, _ := v.(map[string]any)
	data, _ := jsonObj["data"].(map[string]any)
	if data == nil {
		msg := models.Str(jsonObj["message"])
		if msg == "" {
			msg = "虎牙没有返回房间数据"
		}
		return models.LiveResolved{}, fmt.Errorf("%s", msg)
	}

	status := strings.ToUpper(models.Str(data["liveStatus"]))
	if status == "OFF" || status == "" {
		return models.LiveResolved{}, fmt.Errorf("主播当前没有开播")
	}
	if status == "REPLAY" {
		return models.LiveResolved{}, fmt.Errorf("主播当前没有开播（房间在放录像）")
	}

	streamMap, _ := data["stream"].(map[string]any)
	hlsMap, _ := streamMap["hls"].(map[string]any)
	lines := []huyaLine{}
	for _, item := range models.AsArray(hlsMap["multiLine"]) {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		u := models.Str(m["url"])
		if !huyaM3u8Re.MatchString(u) {
			continue
		}
		lines = append(lines, huyaLine{URL: u, CDNType: models.Str(m["cdnType"])})
	}
	if len(lines) == 0 {
		return models.LiveResolved{}, fmt.Errorf("虎牙没有给出 HLS 线路")
	}

	// AL（阿里云）实测常年 403，排到最后；HS 优先，因为它直接给媒体播放列表
	rank := func(t string) int {
		switch t {
		case "HS":
			return 0
		case "AL":
			return 2
		default:
			return 1
		}
	}
	sort.SliceStable(lines, func(i, j int) bool { return rank(lines[i].CDNType) < rank(lines[j].CDNType) })

	masterFallback := -1
	for i, line := range lines {
		ok, media := probePlaylist(ctx, line.URL, referer)
		if !ok {
			continue
		}
		if media {
			return models.LiveResolved{URL: line.URL, Referer: referer, Line: line.CDNType}, nil
		}
		if masterFallback < 0 {
			masterFallback = i
		}
	}
	// 退而求其次：主播放列表里的变体地址多数播不了，但万一某天能播也不至于直接报错
	if masterFallback >= 0 {
		line := lines[masterFallback]
		return models.LiveResolved{URL: line.URL, Referer: referer, Line: line.CDNType}, nil
	}
	return models.LiveResolved{}, fmt.Errorf("虎牙给了 %d 条线路，但没有一条能拉下来", len(lines))
}

/* ---------- 抖音 ---------- */

func resolveDouyin(ctx context.Context, webRid string) (models.LiveResolved, error) {
	referer := "https://live.douyin.com/"
	// 先进一次首页拿 ttwid：没有它进房接口会返回空数据
	_, homeCookies, err := fetchRaw(ctx, "https://live.douyin.com/", map[string]string{"Accept": "text/html"}, resolveTimeout)
	if err != nil {
		return models.LiveResolved{}, err
	}
	cookie := strings.Join(homeCookies, "; ")

	query := "aid=6383&app_name=douyin_web&live_id=1&device_platform=web&language=zh-CN" +
		"&enter_from=web_live&cookie_enabled=true&screen_width=1920&screen_height=1080" +
		"&browser_language=zh-CN&browser_platform=Win32&browser_name=Chrome&browser_version=126.0.0.0"
	text, _, err := fetchRaw(ctx,
		"https://live.douyin.com/webcast/room/web/enter/?"+query+"&web_rid="+webRid,
		map[string]string{
			"Referer": referer + webRid,
			"Cookie":  cookie,
			"Accept":  "application/json, text/plain, */*",
		},
		resolveTimeout)
	if err != nil {
		return models.LiveResolved{}, err
	}

	v, perr := httputil.ParseLooseJSON(text)
	if perr != nil {
		return models.LiveResolved{}, fmt.Errorf("抖音返回的不是 JSON，可能需要签名参数了")
	}
	jsonObj, _ := v.(map[string]any)
	dataWrap, _ := jsonObj["data"].(map[string]any)
	rooms := models.AsArray(dataWrap["data"])
	if len(rooms) == 0 {
		return models.LiveResolved{}, fmt.Errorf("抖音没有返回房间数据，房间号可能不对")
	}
	room, _ := rooms[0].(map[string]any)

	status := int(models.Num(room["status"], 0))
	if status == 4 {
		return models.LiveResolved{}, fmt.Errorf("主播当前没有开播（本场已结束）")
	}
	if status != 2 {
		return models.LiveResolved{}, fmt.Errorf("主播当前没有开播")
	}

	streamURL, _ := room["stream_url"].(map[string]any)
	hlsRaw := streamURL["hls_pull_url_map"]
	hlsMap, _ := hlsRaw.(map[string]any)

	// 清晰度从高到低，缺档就往下退
	pick := ""
	for _, k := range []string{"FULL_HD1", "HD1", "SD1", "SD2"} {
		if s := models.Str(hlsMap[k]); s != "" {
			pick = s
			break
		}
	}
	if pick == "" {
		pick = firstMapValue(hlsRaw)
	}
	if pick == "" {
		return models.LiveResolved{}, fmt.Errorf("抖音没有给出 HLS 线路")
	}

	owner, _ := room["owner"].(map[string]any)
	return models.LiveResolved{URL: pick, Referer: referer, Line: models.Str(owner["nickname"])}, nil
}

// ResolveRoom 把 `huya://` / `douyin://` 房间地址换成能播的 m3u8。
func ResolveRoom(ctx context.Context, rawURL string) (models.LiveResolved, error) {
	url := strings.TrimSpace(rawURL)
	m := roomURLRe.FindStringSubmatch(url)
	if m == nil {
		return models.LiveResolved{}, fmt.Errorf("不认识的房间地址：%s", url)
	}
	if strings.ToLower(m[1]) == "huya" {
		return resolveHuya(ctx, m[2])
	}
	return resolveDouyin(ctx, m[2])
}

// firstMapValue 取 JSON 对象里第一个值（按原始字段顺序，对齐 JS 的 Object.values(map)[0]）。
// Go 的 map 无序，这里重新按 token 流读一遍编码后的 JSON 保序。
func firstMapValue(v any) string {
	if v == nil {
		return ""
	}
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return ""
	}
	if d, isDelim := tok.(json.Delim); !isDelim || d != '{' {
		return ""
	}
	for dec.More() {
		if _, err := dec.Token(); err != nil { // 跳过键
			return ""
		}
		var val any
		if err := dec.Decode(&val); err != nil {
			return ""
		}
		return models.Str(val)
	}
	return ""
}

// trimErr 精简错误文案：剥掉 url.Error 的方法名与地址前缀，只留网络错误本体。
func trimErr(err error) string {
	msg := err.Error()
	for _, prefix := range []string{`Get "`, `Post "`} {
		if j := strings.Index(msg, `": `); j >= 0 && strings.HasPrefix(msg, prefix) {
			return msg[j+3:]
		}
	}
	return msg
}
