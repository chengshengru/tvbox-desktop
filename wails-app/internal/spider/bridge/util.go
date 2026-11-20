// util.go：猫协议工具族全局函数（gzip/zlib/urlencode）与脚本日志转发（限流）。
//
// 作者：Shiro
package bridge

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/base64"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"
)

// GzipEncode 文本 gzip 压缩后 base64。
func GzipEncode(s string) string {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	_, _ = w.Write([]byte(s))
	_ = w.Close()
	return base64.StdEncoding.EncodeToString(b.Bytes())
}

// GzipDecode base64→gunzip；失败原样返回（脚本常对明文误调）。
func GzipDecode(s string) string {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return s
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return s
	}
	defer func() { _ = zr.Close() }()
	data, err := io.ReadAll(zr)
	if err != nil {
		return s
	}
	return string(data)
}

// ZlibEncode 文本 zlib 压缩后 base64。
func ZlibEncode(s string) string {
	var b bytes.Buffer
	w := zlib.NewWriter(&b)
	_, _ = w.Write([]byte(s))
	_ = w.Close()
	return base64.StdEncoding.EncodeToString(b.Bytes())
}

// ZlibDecode base64→zlib 解压；失败原样返回。
func ZlibDecode(s string) string {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return s
	}
	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return s
	}
	defer func() { _ = zr.Close() }()
	data, err := io.ReadAll(zr)
	if err != nil {
		return s
	}
	return string(data)
}

// UrlEncode 等价 JS encodeURIComponent：不转义 A-Za-z0-9-_.!~*'()。
func UrlEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		by := s[i]
		if (by >= 'A' && by <= 'Z') || (by >= 'a' && by <= 'z') || (by >= '0' && by <= '9') ||
			by == '-' || by == '_' || by == '.' || by == '!' || by == '~' ||
			by == '*' || by == '\'' || by == '(' || by == ')' {
			b.WriteByte(by)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", by)
	}
	return b.String()
}

// UrlDecode 等价 decodeURIComponent；非法转义序列原样返回。
func UrlDecode(s string) string {
	// PathUnescape 不像 QueryUnescape 那样把 + 当空格，语义与 JS 一致
	if v, err := url.PathUnescape(s); err == nil {
		return v
	}
	return s
}

// LogSink 脚本日志出口，由装配层注入（main.go → 前端 spider:log 事件）。
var LogSink func(source, line string)

var (
	logMu     sync.Mutex
	logWindow time.Time
	logCount  int
)

// Log 转发脚本日志，全局限流 20 条/s（§8.2），超出丢弃防止刷屏。
func Log(source, line string) {
	logMu.Lock()
	now := time.Now()
	if now.Sub(logWindow) >= time.Second {
		logWindow = now
		logCount = 0
	}
	logCount++
	allow := logCount <= 20
	sink := LogSink
	logMu.Unlock()
	if allow && sink != nil {
		sink(source, line)
	}
}
