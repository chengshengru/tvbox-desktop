// Package settings 应用设置读写：默认值 + 磁盘增量合并。
// 翻译自 src/main/services/settings.ts。
//
// 作者：Shiro
package settings

import (
	"encoding/json"

	"tvbox-desktop/internal/httputil"
	"tvbox-desktop/internal/store"
	"tvbox-desktop/models"
)

const name = "settings"

// Default 默认设置（与 TS DEFAULT_SETTINGS 一致）。
func Default() models.AppSettings {
	return models.AppSettings{
		Concurrency:      8,
		Timeout:          20,
		UserAgent:        httputil.DefaultUA,
		Accent:           "#2563EB",
		Density:          "compact",
		SidebarCollapsed: false,
		// 采集站里「XX解说 / 一口气看完」这类二创短片和正片同名同封面，默认挡掉
		FilterCommentary: true,
		ExternalPlayer: models.ExternalPlayerConfig{
			Command: "",
			Args:    []string{"{url}"},
			Prefer:  []string{"PotPlayer", "mpv", "VLC"},
		},
		Window: models.WindowBounds{Width: 1440, Height: 900},
		// API Key 默认留空：没填之前「AI 总结本集」按钮会提示去设置页填
		AI: models.AiSettings{APIKey: "", Model: "deepseek-flash", BaseURL: ""},
	}
}

// asMap 结构体 → map（用于做 TS 式对象展开合并）。
func asMap(v any) map[string]any {
	data, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]any{}
	}
	return m
}

// overlay 浅合并（patch 里的 nil 视为未提供，跳过）。
func overlay(base, patch map[string]any) map[string]any {
	if base == nil {
		base = map[string]any{}
	}
	for k, v := range patch {
		if v == nil {
			continue
		}
		base[k] = v
	}
	return base
}

// mergedMap 默认值 ← 磁盘值 ← 额外 patch；externalPlayer/window/ai 三个嵌套对象做二级合并。
func mergedMap(patch map[string]any) map[string]any {
	m := asMap(Default())
	stored := store.ReadMap(name)
	for _, key := range []string{"externalPlayer", "window", "ai"} {
		deepMergeInto(m, stored, key)
	}
	m = overlay(m, stripNested(stored))
	if patch != nil {
		for _, key := range []string{"externalPlayer", "window", "ai"} {
			deepMergeInto(m, patch, key)
		}
		m = overlay(m, stripNested(patch))
	}
	return m
}

// deepMergeInto 把 src[key]（map）合并进 dst[key]（map），只二级。
func deepMergeInto(dst, src map[string]any, key string) {
	if src == nil {
		return
	}
	sv, ok := src[key].(map[string]any)
	if !ok || sv == nil {
		return
	}
	dv, _ := dst[key].(map[string]any)
	if dv == nil {
		dv = map[string]any{}
		dst[key] = dv
	}
	for k, v := range sv {
		dv[k] = v
	}
}

// stripNested 去掉三个已做二级合并的键，剩下的做浅合并。
func stripNested(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := map[string]any{}
	for k, v := range m {
		switch k {
		case "externalPlayer", "window", "ai":
			continue
		}
		out[k] = v
	}
	return out
}

// Get 读取当前设置（默认值兜底）。
func Get() models.AppSettings {
	var s models.AppSettings
	data, err := json.Marshal(mergedMap(nil))
	if err != nil {
		return Default()
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return Default()
	}
	return s
}

// Save 增量保存设置，返回合并后的全量。
func Save(patch map[string]any) (models.AppSettings, error) {
	m := mergedMap(patch)
	store.WriteStore(name, m)
	var s models.AppSettings
	data, err := json.Marshal(m)
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(data, &s)
	return s, err
}

// SaveWindow 窗口尺寸记忆专用：只动 window 两个字段。
func SaveWindow(width, height int) {
	_, _ = Save(map[string]any{"window": map[string]any{"width": width, "height": height}})
}
