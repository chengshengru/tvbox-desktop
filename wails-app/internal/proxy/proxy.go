// Package proxy 系统代理可达性探测。翻译自 src/main/services/proxy.ts。
//
// 系统代理开着、代理软件却没在跑的时候，Chromium 会把**每一个**请求都打成
// net::ERR_PROXY_CONNECTION_FAILED —— 海报、m3u8、分片全军覆没，界面看起来
// 就是「视频全播放不了」。而 Go 侧的 HTTP 客户端不读系统代理，搜索 / 详情
// 一切正常，所以特别容易误判成播放器坏了。
//
// 启动时探一次：候选代理端口里只要有一个真的在监听就照旧走系统代理，
// 一个都连不上才把全局网络层切成直连。代理真在跑时行为完全不变。
//
// Windows 实现读注册表 Internet Settings（proxy_windows.go）；
// 其余平台读环境变量（proxy_other.go）。
//
// 作者：Shiro
package proxy

import (
	"fmt"
	"net"
	"strings"
	"time"

	"tvbox-desktop/internal/httputil"
)

// connectTimeout 代理端口探测超时。本地代理要么在要么不在，给太长会拖慢启动。
const connectTimeout = 800 * time.Millisecond

type proxyTarget struct {
	host string
	port int
}

// canConnect TCP 探通一个候选代理端口。
func canConnect(t proxyTarget) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", t.host, t.port), connectTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// EnsureReachable 探测系统代理是否真的可达：
// 候选端口全都连不上时调用 httputil.SetProxyDisabled(true) 切直连，
// 并返回日志文案；无事发生（含探测成功）时返回空串。调用方把非空文案打到日志即可。
func EnsureReachable() string {
	targets, err := platformTargets()
	if err != nil {
		return fmt.Sprintf("[proxy] 检查系统代理失败，保持默认设置：%v", err)
	}
	if len(targets) == 0 {
		return ""
	}

	for _, target := range targets {
		if canConnect(target) {
			return ""
		}
	}

	httputil.SetProxyDisabled(true)
	list := make([]string, 0, len(targets))
	for _, t := range targets {
		list = append(list, fmt.Sprintf("%s:%d", t.host, t.port))
	}
	return fmt.Sprintf("[proxy] 系统代理 %s 都连不上，本次运行改为直连", strings.Join(list, " / "))
}

// appendTarget 收集候选端口并去重（host:port 相同只留一条）。
func appendTarget(out []proxyTarget, host string, port int) []proxyTarget {
	if port <= 0 || port >= 65536 {
		return out
	}
	for _, t := range out {
		if t.host == host && t.port == port {
			return out
		}
	}
	return append(out, proxyTarget{host: host, port: port})
}
