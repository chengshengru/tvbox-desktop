// Package bridge TVBox 猫协议宿主全局函数：脚本侧网络访问、HTML 解析、加解密的统一入口。
// 所有 HTTP 最终落到 httputil，代理/UA/GBK/Referer/超时语义与主进程完全一致；
// QuickJS / Python 引擎侧只负责参数序列化与返回值转发（见 js / python 包）。
//
// 作者：Shiro
package bridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"tvbox-desktop/internal/httputil"
	"tvbox-desktop/internal/stream"
)

// AllowPrivate 是否允许 req 访问私网/环回地址（默认拒绝，§9 R7：防恶意脚本打内网）。
// 设置页开关后续接入；单元测试临时放开以走 httptest。
var AllowPrivate bool

// reqOptions 猫协议 req(url, options) 的 options 字段集。
type reqOptions struct {
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	// Data 部分脚本用 data 传表单体，与 body 等价取其一。
	Data    string  `json:"data"`
	Timeout float64 `json:"timeout"`
	// Binary true 时 body 置空、base64 填充（脚本读图片/字节流）。
	Binary bool `json:"binary"`
}

// Req 执行猫协议 req(url, options)：optsJSON 为 host.js 序列化的 options JSON 串，
// 返回 {code, headers, body|base64} 的 JSON 串。非 2xx 不报错，状态码原样透出由脚本判断。
func Req(ctx context.Context, rawURL, optsJSON string) (string, error) {
	if err := checkPrivate(rawURL); err != nil {
		return "", err
	}
	var o reqOptions
	if s := strings.TrimSpace(optsJSON); s != "" && s != "{}" {
		if err := json.Unmarshal([]byte(s), &o); err != nil {
			return "", fmt.Errorf("req options 非法 JSON：%w", err)
		}
	}

	// Referer/UA 注入规则自动套用，与主进程采集行为一致（设计文档 §2.2）
	header := map[string]string{}
	for k, v := range stream.HeadersFor(rawURL) {
		header[k] = v
	}
	for k, v := range o.Headers {
		header[k] = v
	}

	body := o.Body
	if body == "" {
		body = o.Data
	}
	method := strings.ToUpper(strings.TrimSpace(o.Method))
	if method == "" {
		method = "GET"
	}

	res, err := httputil.Request(ctx, rawURL, httputil.Options{
		Method:  method,
		Header:  header,
		Body:    body,
		Timeout: o.Timeout,
	})
	if err != nil {
		return "", err
	}

	out := map[string]any{
		"code":    res.Status,
		"headers": res.Headers,
	}
	if o.Binary {
		out["base64"] = base64.StdEncoding.EncodeToString(res.Body)
	} else {
		out["body"] = httputil.DecodeResponse(res, "")
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// checkPrivate 私网/环回地址守卫（§9.4 R7）。域名只拦本机形态，
// 完整 DNS 解析校验成本高且有重绑定窗口，风险由总开关承担。
func checkPrivate(rawURL string) error {
	if AllowPrivate {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("URL 非法：%w", err)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return errors.New("URL 缺少主机名")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return fmt.Errorf("已拒绝访问私网/环回地址：%s", host)
		}
		return nil
	}
	if host == "localhost" || !strings.Contains(host, ".") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return fmt.Errorf("已拒绝访问内网主机名：%s", host)
	}
	return nil
}
