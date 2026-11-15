// Package mediamw 同源媒体代理中间件（改造方案 §5.1 / §5.2）。
//
// 为什么需要它：虎牙 / 抖音的 m3u8 和分片都校验 Referer，而且
// `Referer` 是浏览器的 forbidden header —— hls.js 用 XHR 发请求时
// `setRequestHeader('Referer', …)` 会被**静默忽略**，跨域直连一律 403。
// Electron 版靠 `webRequest.onBeforeSendHeaders` 在会话层补写；
// Go + Wails 没有这层钩子，改为把播放地址统一改写成
// `http://<本地服务>/media?u=<原始地址>`，由这里回源转发：
//
//	· 对播放器而言变成同源请求，浏览器不再做 CORS/Referer 拦截；
//	· 回源时由 Go 注入 Referer / Origin / UA（见 internal/stream.HeadersFor）；
//	· 播放列表里的相对地址逐条再改写回 /media，递归覆盖分片和变体。
//
// 作者：Shiro
package mediamw

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"tvbox-desktop/internal/httputil"
	"tvbox-desktop/internal/stream"
)

// mediaPath 同源代理的路径，与前端播放地址的改写约定一致。
const mediaPath = "/media"

// playlistReadTimeout 播放列表整体读入的上限：m3u8 是纯文本，正常远小于 1 秒；
// 卡住多半是线路已死，兜底 15 秒后强制断开，绝不能挂成直播流那样永不结束。
const playlistReadTimeout = 15 * time.Second

// passHeaders 透传给播放器的响应头（Range 分段播放、缓存校验都靠它们）。
var passHeaders = []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag"}

// uriParamRe 主播放列表变体行里的 URI="..." 参数。
var uriParamRe = regexp.MustCompile(`URI="([^"]+)"`)

// mediaTransport 与全局网络层同构的 Transport：连接复用 + 忽略非常规证书（采集站常见自签）。
var mediaTransport = &http.Transport{
	MaxIdleConns:        64,
	MaxIdleConnsPerHost: 8,
	IdleConnTimeout:     60 * time.Second,
	TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
	Proxy:               http.ProxyFromEnvironment,
}

// mediaClient 不设整体 Timeout：TS 分片是持续流，客户端断开由 r.Context() 传导。
var mediaClient = &http.Client{Transport: mediaTransport}

// Middleware 返回 Wails 资产服务器可用的 http.Handler 包装函数。
func Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != mediaPath || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
				next.ServeHTTP(w, r)
				return
			}
			serveMedia(w, r)
		})
	}
}

// serveMedia 处理一次 /media?u=… 回源。
func serveMedia(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("u"))
	if raw == "" {
		http.Error(w, "媒体代理：缺少 u 参数", http.StatusBadRequest)
		return
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		http.Error(w, "媒体代理：u 参数不是可回源的 http(s) 地址", http.StatusBadRequest)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, raw, nil)
	if err != nil {
		http.Error(w, "媒体代理：u 参数不是合法地址", http.StatusBadRequest)
		return
	}
	req.Header.Set("User-Agent", httputil.DefaultUA)
	req.Header.Set("Accept", "*/*")
	// 会话层补 Referer / Origin：这正是浏览器 XHR 做不到的那件事
	for k, v := range stream.HeadersFor(raw) {
		req.Header.Set(k, v)
	}
	// 分段播放（hls.js 拉 TS 分片、播放器拖动）依赖 Range 透传
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}

	res, err := mediaClient.Do(req)
	if err != nil {
		// 不回显上游原始错误：里面可能带签名参数等敏感查询串
		http.Error(w, "媒体回源失败", http.StatusBadGateway)
		return
	}
	defer res.Body.Close()

	// 播放列表判定：响应头声明 mpegurl，或路径以 .m3u8 结尾（不少源靠扩展名）
	pathLower := strings.ToLower(u.Path)
	isM3u8 := r.Method == http.MethodGet &&
		(strings.Contains(strings.ToLower(res.Header.Get("Content-Type")), "mpegurl") ||
			strings.HasSuffix(pathLower, ".m3u8"))

	if isM3u8 {
		servePlaylist(w, res)
		return
	}

	for _, h := range passHeaders {
		if v := res.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(res.StatusCode)
	if _, err := io.Copy(w, res.Body); err != nil {
		// 流式中途断开（切台、暂停）是常态：响应头已发出，只能静默终止
		return
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// servePlaylist 整体读入播放列表、改写地址后一次性返回。
func servePlaylist(w http.ResponseWriter, res *http.Response) {
	// 响应头到达后计时兜底断开：路径不带 .m3u8、仅靠 Content-Type 命中的线路
	// 可能实际是流，读下去永不结束
	timer := time.AfterFunc(playlistReadTimeout, func() { _ = res.Body.Close() })
	defer timer.Stop()

	data, err := io.ReadAll(res.Body)
	if err != nil {
		http.Error(w, "媒体回源失败", http.StatusBadGateway)
		return
	}

	// 相对地址要按上游**最终**地址解析（重定向后基址会变）
	base := res.Request.URL
	out := rewritePlaylist(data, base)

	ctype := res.Header.Get("Content-Type")
	if ctype == "" {
		ctype = "application/vnd.apple.mpegurl"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(res.StatusCode)
	_, _ = w.Write(out)
}

// rewritePlaylist 逐行改写 m3u8：
//
//	· #EXTINF / #EXT-X-STREAM-INF 之后的数据行；
//	· 变体行里的 URI="..." 参数；
//
// 只改写能解析成 http(s) 的地址，其余原样保留。
func rewritePlaylist(data []byte, base *url.URL) []byte {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var sb strings.Builder
	expectURI := false
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)

		switch {
		case strings.HasPrefix(lower, "#ext-x-stream-inf"):
			// 变体行：带宽等信息在行内，媒体地址在下一行，URI= 参数也可能直接内嵌
			sb.WriteString(rewriteURIs(line, base))
			expectURI = true
		case strings.HasPrefix(lower, "#extinf"):
			sb.WriteString(line)
			expectURI = true
		case strings.HasPrefix(trimmed, "#") || trimmed == "":
			sb.WriteString(line)
		default:
			if expectURI {
				sb.WriteString(rewriteLine(trimmed, base))
				expectURI = false
			} else {
				sb.WriteString(line)
			}
		}
		sb.WriteByte('\n')
	}
	return []byte(sb.String())
}

// rewriteURIs 替换一行里所有 URI="..." 参数。
func rewriteURIs(line string, base *url.URL) string {
	return uriParamRe.ReplaceAllStringFunc(line, func(m string) string {
		g := uriParamRe.FindStringSubmatch(m)
		if g == nil {
			return m
		}
		abs, ok := resolveRef(base, g[1])
		if !ok {
			return m
		}
		return `URI="` + toMediaURL(abs) + `"`
	})
}

// rewriteLine 改写一条数据行（媒体分片 / 变体地址）。
func rewriteLine(line string, base *url.URL) string {
	abs, ok := resolveRef(base, line)
	if !ok {
		return line
	}
	return toMediaURL(abs)
}

// resolveRef 以基址解析相对地址，仅 http(s) 才算可改写。
func resolveRef(base *url.URL, ref string) (string, bool) {
	if ref == "" || strings.HasPrefix(ref, "#") {
		return "", false
	}
	u, err := base.Parse(ref)
	if err != nil {
		return "", false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	return u.String(), true
}

// toMediaURL 把绝对地址包成同源代理地址。
func toMediaURL(abs string) string {
	return mediaPath + "?u=" + url.QueryEscape(abs)
}
