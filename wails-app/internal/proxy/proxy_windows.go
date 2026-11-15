//go:build windows

package proxy

import (
	"net"
	"strconv"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// platformTargets 读注册表 Internet Settings 的 ProxyEnable / ProxyServer，
// 拆出候选 host:port 逐个 TCP 探通。
//
// 与 TS 版的差异（有意为之）：Electron 用 Chromium 的 resolveProxy 拿代理规则串
// （可能执行 PAC 脚本），Go 侧没有等价能力。所以这里直接读注册表：
//   - ProxyServer 是 "host:port" 或 "http=host:port;https=host:port;socks=..." 形式时，
//     按候选端口逐个探；
//   - ProxyServer 是 PAC 脚本 URL（http 开头）时拿不到具体端口，跳过探测、保持默认。
func platformTargets() ([]proxyTarget, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return nil, err
	}
	defer k.Close()

	enable, _, err := k.GetIntegerValue("ProxyEnable")
	if err != nil || enable == 0 {
		// 没开系统代理：Go 网络层本来就走环境代理/直连，无需干预
		return nil, nil
	}
	server, _, err := k.GetStringValue("ProxyServer")
	if err != nil {
		return nil, nil
	}
	return parseProxyServer(strings.TrimSpace(server)), nil
}

// parseProxyServer 拆 ProxyServer 串：分号分段，段内可带 "scheme=" 前缀。
func parseProxyServer(server string) []proxyTarget {
	var out []proxyTarget
	for _, part := range strings.Split(server, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if i := strings.Index(part, "="); i >= 0 {
			part = strings.TrimSpace(part[i+1:])
		}
		if strings.HasPrefix(strings.ToLower(part), "http") {
			// PAC / 自动配置脚本 URL：拿不到具体端口，跳过
			continue
		}
		host, portStr, err := net.SplitHostPort(part)
		if err != nil {
			continue
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			continue
		}
		out = appendTarget(out, host, port)
	}
	return out
}
