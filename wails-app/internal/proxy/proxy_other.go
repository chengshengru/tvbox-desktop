//go:build !windows

package proxy

import (
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// platformTargets Linux/macOS 桩实现。
//
// 与 TS 版的差异（有意为之）：Electron 用 Chromium 的 resolveProxy（含 PAC 语义），
// 这里没有等价能力，改为读 HTTP_PROXY / HTTPS_PROXY / ALL_PROXY 环境变量，
// 把候选代理端口逐个 TCP 探通；PAC 之类的自动配置一律跳过、保持默认。
func platformTargets() ([]proxyTarget, error) {
	var out []proxyTarget
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			continue
		}
		if !strings.Contains(v, "://") {
			v = "http://" + v
		}
		u, err := url.Parse(v)
		if err != nil || u.Host == "" {
			continue
		}
		host, portStr, err := net.SplitHostPort(u.Host)
		if err != nil {
			continue
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			continue
		}
		out = appendTarget(out, host, port)
	}
	return out, nil
}
