// Package httputil 统一网络层：所有对外请求都从这里走。
// 放在 Go 侧的好处：不受 CORS 限制、可自定义 UA/Referer、可处理 GBK 等非 UTF-8 编码。
// 翻译自 src/main/services/http.ts。
//
// 作者：Shiro
package httputil

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/transform"

	"tvbox-desktop/models"
)

// DefaultUA 默认浏览器 UA（部分站点校验 UA 指纹）。
const DefaultUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

// Options 一次请求的选项，字段语义与 TS RequestOptions 对齐。
type Options struct {
	Header map[string]string
	// Timeout 单请求超时（秒），0 表示默认 20s。
	Timeout float64
	Method  string
	Body    string
	// Encoding 强制指定编码，空则自动嗅探。
	Encoding string
	// AllowHTTPError 允许非 2xx 也当成功（部分站点返回 500 但带数据）。
	AllowHTTPError bool
	// Client 可注入自定义客户端（媒体中间件用长超时/流式），nil 用默认。
	Client *http.Client
}

// RawResponse 原始响应。
type RawResponse struct {
	OK       bool
	Status   int
	FinalURL string
	Headers  map[string]string
	Body     []byte
}

// transport 进程级共享 Transport：连接复用 + 忽略非常规证书（采集站常见自签）。
var transport = &http.Transport{
	MaxIdleConns:        64,
	MaxIdleConnsPerHost: 8,
	IdleConnTimeout:     60 * time.Second,
	TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
	Proxy:               http.ProxyFromEnvironment,
}

// SetProxyDisabled 强制全局直连（对应 Electron 版 ensureReachableProxy 探测失败后
// setProxy({mode:'direct'}) 的兜底：系统代理开着但代理软件没跑时，避免每个请求都失败）。
func SetProxyDisabled(disabled bool) {
	if disabled {
		transport.Proxy = nil
	} else {
		transport.Proxy = http.ProxyFromEnvironment
	}
}

func defaultClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// 与 fetch 的 redirect:'follow' 对齐，上限同 TS 默认
			if len(via) >= 10 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

// Request 发起一次请求并整体读进内存。ctx 用于取消（对应 AbortController）。
func Request(ctx context.Context, rawURL string, opts Options) (*RawResponse, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 20
	}
	client := opts.Client
	if client == nil {
		client = defaultClient(time.Duration(timeout * float64(time.Second)))
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout*float64(time.Second)))
	defer cancel()

	method := opts.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if opts.Body != "" {
		body = strings.NewReader(opts.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, fmt.Errorf("非法地址：%s", rawURL)
	}
	req.Header.Set("User-Agent", DefaultUA)
	req.Header.Set("Accept", "*/*")
	for k, v := range opts.Header {
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}

	res, err := client.Do(req)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("请求超时（%ds）", int(timeout))
		}
		return nil, fmt.Errorf("%s", trimErr(err))
	}
	defer res.Body.Close()

	data, err := io.ReadAll(res.Body)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("请求超时（%ds）", int(timeout))
		}
		return nil, fmt.Errorf("读取响应失败：%s", trimErr(err))
	}

	headers := map[string]string{}
	for k, v := range res.Header {
		if len(v) > 0 {
			headers[strings.ToLower(k)] = v[0]
		}
	}
	finalURL := rawURL
	if res.Request != nil && res.Request.URL != nil {
		finalURL = res.Request.URL.String()
	}
	ok := res.StatusCode >= 200 && res.StatusCode < 300
	if !ok && !opts.AllowHTTPError {
		return nil, fmt.Errorf("HTTP %d %s", res.StatusCode, res.Status)
	}
	return &RawResponse{OK: ok, Status: res.StatusCode, FinalURL: finalURL, Headers: headers, Body: data}, nil
}

// FetchText 请求并解码为文本。
func FetchText(ctx context.Context, rawURL string, opts Options) (string, error) {
	res, err := Request(ctx, rawURL, opts)
	if err != nil {
		return "", err
	}
	return DecodeResponse(res, opts.Encoding), nil
}

// FetchJSON 请求并宽松解析 JSON（对应 fetchJson）。
func FetchJSON(ctx context.Context, rawURL string, opts Options) (any, error) {
	text, err := FetchText(ctx, rawURL, opts)
	if err != nil {
		return nil, err
	}
	return ParseLooseJSON(text)
}

var headerCharsetRe = regexp.MustCompile(`(?i)charset=["']?([\w-]+)`)
var metaCharsetRe = regexp.MustCompile(`(?i)<meta[^>]+charset=["']?([\w-]+)`)
var xmlEncodingRe = regexp.MustCompile(`(?i)<\?xml[^>]+encoding=["']([\w-]+)["']`)

// detectEncoding 响应头 → 前 4KB meta → xml declaration 三级嗅探，兜底 utf-8。
func detectEncoding(body []byte, contentType, forced string) string {
	if forced != "" {
		return strings.ToLower(forced)
	}
	if m := headerCharsetRe.FindStringSubmatch(contentType); m != nil {
		return strings.ToLower(m[1])
	}
	head := body
	if len(head) > 4096 {
		head = head[:4096]
	}
	latin := string(head)
	if m := metaCharsetRe.FindStringSubmatch(latin); m != nil {
		return strings.ToLower(m[1])
	}
	if m := xmlEncodingRe.FindStringSubmatch(latin); m != nil {
		return strings.ToLower(m[1])
	}
	return "utf-8"
}

// encodingByName 常见编码名 → x/text 编码；不认识返回 nil（按 utf-8 处理）。
func encodingByName(name string) encoding.Encoding {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "gbk", "gb2312", "gb-2312", "cp936", "windows-936", "euc-cn":
		return simplifiedchinese.GBK
	case "gb18030":
		return simplifiedchinese.GB18030
	case "big5", "big-endian", "cp950", "big5-hkscs":
		return traditionalchinese.Big5
	case "shift_jis", "shiftjis", "sjis", "cp932":
		return japanese.ShiftJIS
	case "euc-jp":
		return japanese.EUCJP
	case "euc-kr", "cp949":
		return korean.EUCKR
	case "iso-8859-1", "latin1":
		return charmap.ISO8859_1
	case "windows-1251", "cp1251":
		return charmap.Windows1251
	case "windows-1252", "cp1252":
		return charmap.Windows1252
	}
	return nil
}

// DecodeResponse 已拿到 RawResponse 时的解码版本：按响应头/内容嗅探编码再转文本。
func DecodeResponse(res *RawResponse, forced string) string {
	enc := detectEncoding(res.Body, res.Headers["content-type"], forced)
	if enc == "utf-8" || enc == "utf8" {
		return string(res.Body)
	}
	e := encodingByName(enc)
	if e == nil {
		return string(res.Body)
	}
	out, _, err := transform.Bytes(e.NewDecoder(), res.Body)
	if err != nil {
		return string(res.Body)
	}
	return string(out)
}

// ParseLooseJSON 带容错的 JSON 解析：剥 BOM、失败时截取首个 { 到末个 }。
func ParseLooseJSON(text string) (any, error) {
	cleaned := strings.TrimSpace(strings.TrimPrefix(text, "\ufeff"))
	var v any
	if err := jsonUnmarshal([]byte(cleaned), &v); err == nil {
		return v, nil
	}
	start := strings.Index(cleaned, "{")
	end := strings.LastIndex(cleaned, "}")
	if start >= 0 && end > start {
		if err := jsonUnmarshal([]byte(cleaned[start:end+1]), &v); err == nil {
			return v, nil
		}
	}
	return nil, fmt.Errorf("返回内容不是合法 JSON")
}

// jsonUnmarshal 数字保留原文（UseNumber），避免大整数/混型字段精度丢失。
func jsonUnmarshal(data []byte, v *any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}

// MatchHeaderRules 按 host 通配规则匹配请求头，支持 *.example.com。
func MatchHeaderRules(rawURL string, rules []models.HeaderRule) map[string]string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	host := u.Host
	merged := map[string]string{}
	for _, rule := range rules {
		pattern := strings.TrimSpace(rule.Host)
		if pattern == "" {
			continue
		}
		if matchGlob(pattern, host) {
			for k, v := range rule.Header {
				merged[k] = v
			}
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// matchGlob 通配符匹配（* → 任意串），大小写不敏感，语义对齐 TS 版正则。
func matchGlob(pattern, s string) bool {
	re := regexp.MustCompile("^" + globToRegex(pattern) + "$")
	return re.MatchString(s)
}

// trimErr 精简错误文案：剥掉 url.Error 的方法名与地址前缀，只留网络错误本体。
func trimErr(err error) string {
	msg := err.Error()
	if j := strings.Index(msg, `": `); j >= 0 && strings.HasPrefix(msg, `Get "`) {
		return msg[j+3:]
	}
	if j := strings.Index(msg, `": `); j >= 0 && strings.HasPrefix(msg, `Post "`) {
		return msg[j+3:]
	}
	return msg
}

var globEscape = regexp.MustCompile(`[.+?^${}()|[\]\\]`)

func globToRegex(pattern string) string {
	escaped := globEscape.ReplaceAllStringFunc(pattern, func(m string) string {
		return `\` + m
	})
	return strings.ReplaceAll(escaped, "*", ".*")
}
