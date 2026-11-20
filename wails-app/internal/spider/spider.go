// Package spider JS/Python 脚本爬虫（Spider）统一门面。
// 业务链路（搜索/详情/分类/首页/测活/直播）只调用 Invoke，不感知引擎差异；
// 具体引擎（QuickJS / CPython 子进程）由 js / python 包在后续阶段经
// RegisterFactory 注册进 worker 池，未注册时 Invoke 返回 ErrNoEngine，
// 调用方按现有 unsupported 语义降级。
// 本包为纯 Go（不 import quickjs-go），保证全仓 CGO_ENABLED=0 交叉编译不破。
// 设计依据：.trae/documents/JS-Python爬虫Spider支持开发设计文档.md。
//
// 作者：Shiro
package spider

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"tvbox-desktop/internal/settings"
	"tvbox-desktop/models"
)

// 脚本形态常量（与 models.Site.Spider 取值一致）。
const (
	// KindJS QuickJS 语义的 TVBox JS 协议（猫源）。
	KindJS = "js"
	// KindPython CPython 子进程执行的 py 转发协议。
	KindPython = "python"
)

var (
	// ErrDisabled Spider 功能未开启（总开关或对应引擎开关关闭）。
	ErrDisabled = errors.New("Spider 功能未开启")
	// ErrNoEngine 引擎尚未注册（骨架阶段 / CGO 产物缺失且未切 Node 退路）。
	ErrNoEngine = errors.New("Spider 引擎未接入")

	// androidRe Android 编译产物（jar/dex），桌面端永久不支持（设计文档 §1.3）。
	androidRe = regexp.MustCompile(`(?i)^(jar|dex)://`)
	// resourceRe 依赖内置资源（proxy/assets/file 等），无对应资源包，不支持。
	resourceRe = regexp.MustCompile(`(?i)^(proxy|assets|file|clan|to):`)
	// jsPrefixRe / pyPrefixRe 显式判型前缀：`js: https://…` / `py: https://…`。
	jsPrefixRe = regexp.MustCompile(`(?i)^js:\s*`)
	pyPrefixRe = regexp.MustCompile(`(?i)^py(?:thon)?:\s*`)
	// httpRe http(s) 地址判定。
	httpRe = regexp.MustCompile(`(?i)^https?://`)
	// inlineJSRe / inlinePYRe 内联代码头部：配置把整段脚本写进 api 字段的形态（§6.3）。
	inlineJSRe = regexp.MustCompile(`(?is)^\s*(async\s+)?function\s+main\b`)
	inlinePYRe = regexp.MustCompile(`(?is)^\s*(def\s+\w|class\s+spider\b)`)
)

// Kind 判定站点 Spider 形态："js" / "python" / ""（不可运行）。
// 判定顺序（设计文档 §6.3 表）：显式前缀 → http(s) 地址文件后缀 → 内联代码头部 →
// type=0 站点的 jar 字段回退；jar://、dex://、proxy: 等维持不可运行。
func Kind(site models.Site) string {
	api := strings.TrimSpace(site.API)
	if api == "" || androidRe.MatchString(api) || resourceRe.MatchString(api) {
		return ""
	}
	// 1. 显式前缀优先；前缀后必须是可下载的 http(s) 地址
	if jsPrefixRe.MatchString(api) {
		if httpRe.MatchString(scriptURL(api)) {
			return KindJS
		}
		return ""
	}
	if pyPrefixRe.MatchString(api) {
		if httpRe.MatchString(scriptURL(api)) {
			return KindPython
		}
		return ""
	}
	// 2. http(s) 地址按脚本后缀判型（去 query/fragment）
	if httpRe.MatchString(api) {
		switch scriptExt(api) {
		case ".js":
			return KindJS
		case ".py":
			return KindPython
		}
		return ""
	}
	// 3. 内联代码（非 URL 文本才可能是脚本体）
	if inlineJSRe.MatchString(api) {
		return KindJS
	}
	if inlinePYRe.MatchString(api) {
		return KindPython
	}
	// 4. type=0（需配套 Spider）：jar 字段若指向 http 脚本，当全局脚本地址用
	if site.Type == 0 {
		jar := strings.TrimSpace(site.Jar)
		if httpRe.MatchString(jar) {
			switch scriptExt(jar) {
			case ".js":
				return KindJS
			case ".py":
				return KindPython
			}
		}
	}
	return ""
}

// Enabled 报告 Spider 总开关是否开启。
func Enabled() bool {
	return settings.Get().Spider.Enabled
}

// EnabledFor 报告指定形态能否执行：总开关 + JS 引擎开关（§8.1 设置项）。
func EnabledFor(kind string) bool {
	s := settings.Get().Spider
	if !s.Enabled {
		return false
	}
	if kind == KindJS && !s.JsEngine {
		return false
	}
	return true
}

// Status 引擎可用性 + 脚本缓存概览（绑定层 Spider.Status 的返回体，§8.2）。
type Status struct {
	// Enabled Spider 总开关。
	Enabled bool `json:"enabled"`
	// JsEngine JS 引擎开关（设置值，非探测值）。
	JsEngine bool `json:"jsEngine"`
	// JsOK JS 引擎是否已注册可用。
	JsOK bool `json:"jsOK"`
	// PythonOK Python 引擎是否已注册可用。
	PythonOK bool `json:"pythonOK"`
	// PythonVersion 探测到的解释器版本，P4 起填充；空表示未探测。
	PythonVersion string `json:"pythonVersion"`
	// CachedScripts data/spiders/ 脚本产物缓存清单。
	CachedScripts []CachedScript `json:"cachedScripts"`
}

// GetStatus 探测引擎注册状态与缓存占用，供设置页探测面板使用。
func GetStatus() Status {
	s := settings.Get().Spider
	return Status{
		Enabled:       s.Enabled,
		JsEngine:      s.JsEngine,
		JsOK:          registered(KindJS),
		PythonOK:      registered(KindPython),
		CachedScripts: ListCached(),
	}
}

// Invoke 统一入口：在站点脚本上执行一个协议方法，返回脚本产出的原始 JSON 串。
// method ∈ {home homeVod category detail search play live}；
// params 为 TVBox input 的业务字段（ext/group 由 Init 单独注入，见 params.go）。
// 站点不可运行、功能未开启或引擎未接入时返回对应错误，调用方按 unsupported 路径降级。
func Invoke(ctx context.Context, site models.Site, method string, params map[string]any) (string, error) {
	kind := site.Spider
	if kind == "" {
		kind = Kind(site)
	}
	if kind == "" {
		return "", errors.New("该站点不是可运行的 Spider 站点")
	}
	if !EnabledFor(kind) {
		return "", ErrDisabled
	}
	return poolInvoke(ctx, kind, site, method, params)
}
