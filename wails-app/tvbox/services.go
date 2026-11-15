// Package tvbox Wails 绑定层：按领域分组的 13 个服务结构体，共 45 个方法。
// 前端桥路径为 window.go.tvbox.<Service>.<Method>，与 Electron 版 DesktopApi 一一对应。
// 本包只做薄壳转发 + 跨服务联动（缓存失效），业务逻辑全部在 internal/*。
// 不 import wails：事件发射与外链浏览器通过函数变量由 main.go 注入，
// 使本包在无 WebView 依赖的环境下也能编译（CI/交叉检查用）。
//
// 作者：Shiro
package tvbox

import (
	"context"

	"tvbox-desktop/internal/ai"
	"tvbox-desktop/internal/category"
	"tvbox-desktop/internal/detail"
	"tvbox-desktop/internal/home"
	"tvbox-desktop/internal/library"
	"tvbox-desktop/internal/live"
	"tvbox-desktop/internal/livehealth"
	"tvbox-desktop/internal/parse"
	"tvbox-desktop/internal/player"
	"tvbox-desktop/internal/presets"
	"tvbox-desktop/internal/search"
	"tvbox-desktop/internal/settings"
	"tvbox-desktop/internal/sites"
	"tvbox-desktop/internal/sources"
	"tvbox-desktop/internal/stream"
	"tvbox-desktop/models"
)

// Version 应用版本（与 package.json 对齐）。
const Version = "0.1.0-wails"

// EmitEvent 由 main.go 注入：向渲染进程推送事件（对应 webContents.send）。
var EmitEvent func(event string, data any)

// OpenURL 由 main.go 注入：用系统浏览器打开外链（对应 shell.openExternal）。
var OpenURL func(rawURL string) error

// appInfo 启动时由 main.go 填好（数据目录、WebView 版本等只有装配层知道）。
var appInfo models.AppInfo

// SetAppInfo 装配层注入应用信息。
func SetAppInfo(info models.AppInfo) { appInfo = info }

// clearCaches 订阅源变更后要连带失效的派生缓存，与 Electron 版 index.ts 的联动一致。
func clearCaches() {
	live.ClearCache()
	category.ClearCache()
	home.ClearCache()
}

// App 应用级信息。
type App struct{}

// Info 应用信息。
func (App) Info(ctx context.Context) (models.AppInfo, error) { return appInfo, nil }

// OpenExternal 用系统浏览器打开外链（校验 http/https，防 file:// 之类的注入面）。
func (App) OpenExternal(ctx context.Context, rawURL string) error {
	if len(rawURL) < 8 || (rawURL[:7] != "http://" && rawURL[:8] != "https://") {
		return nil
	}
	if OpenURL == nil {
		return nil
	}
	return OpenURL(rawURL)
}

// Settings 应用设置。
type Settings struct{}

// Get 读取设置。
func (Settings) Get(ctx context.Context) (models.AppSettings, error) {
	return settings.Get(), nil
}

// Set 增量保存设置（对应 settings:set 通道）。
func (Settings) Set(ctx context.Context, patch map[string]any) (models.AppSettings, error) {
	return settings.Save(patch)
}

// Sources 订阅源管理。
type Sources struct{}

// List 全部订阅源。
func (Sources) List(ctx context.Context) ([]models.ConfigSource, error) {
	return sources.List(), nil
}

// Add 新增订阅源。
func (Sources) Add(ctx context.Context, input sources.AddInput) (models.ConfigSource, error) {
	src, err := sources.Add(ctx, input)
	clearCaches()
	return src, err
}

// Update 修改订阅源（名称/地址/启停）。
func (Sources) Update(ctx context.Context, id string, patch map[string]any) (models.ConfigSource, error) {
	src, err := sources.Update(ctx, id, patch)
	clearCaches()
	return src, err
}

// Remove 删除订阅源。
func (Sources) Remove(ctx context.Context, id string) error {
	err := sources.Remove(id)
	clearCaches()
	return err
}

// Sync 同步单个订阅源。
func (Sources) Sync(ctx context.Context, id string) (models.ConfigSource, error) {
	src, err := sources.Sync(ctx, id)
	clearCaches()
	return src, err
}

// SyncAll 同步全部启用的订阅源。
func (Sources) SyncAll(ctx context.Context) ([]models.ConfigSource, error) {
	list, err := sources.SyncAll(ctx)
	clearCaches()
	return list, err
}

// PickFile 弹系统文件对话框选一个 JSON 配置，取消返回空串。
func (Sources) PickFile(ctx context.Context) (string, error) {
	if PickFileDialog == nil {
		return "", nil
	}
	return PickFileDialog(ctx)
}

// PickFileDialog 由 main.go 注入的原生文件对话框函数。
var PickFileDialog func(ctx context.Context) (string, error)

// Presets 内置推荐订阅列表。
func (Sources) Presets(ctx context.Context) ([]models.SourcePresetInfo, error) {
	return presets.ListPresets(), nil
}

// AddPreset 一键添加内置订阅。
func (Sources) AddPreset(ctx context.Context, id string) (models.ConfigSource, error) {
	src, err := presets.AddPreset(ctx, id)
	clearCaches()
	return src, err
}

// Sites 站点管理。
type Sites struct{}

// List 全部站点（聚合自启用的订阅源）。
func (Sites) List(ctx context.Context) ([]models.Site, error) {
	return sites.GetSites(), nil
}

// SetEnabled 单站启停。
func (Sites) SetEnabled(ctx context.Context, key string, enabled bool) error {
	sites.SetEnabled(key, enabled)
	return nil
}

// SetAllEnabled 批量启停。
func (Sites) SetAllEnabled(ctx context.Context, keys []string, enabled bool) error {
	sites.SetAllEnabled(keys, enabled)
	return nil
}

// Test 站点测活。
func (Sites) Test(ctx context.Context, key string) (models.TestResult, error) {
	return sites.Test(ctx, key)
}

// Search 聚合搜索。
type Search struct{}

// Run 多站点并发搜索；进度经 EmitEvent("search:progress") 逐站推送。
// options 允许为 nil（前端未传时）。
func (Search) Run(ctx context.Context, wd string, options *models.SearchOptions) (models.SearchResponse, error) {
	opts := models.SearchOptions{}
	if options != nil {
		opts = *options
	}
	emit := func(p models.SearchProgress) {
		if EmitEvent != nil {
			EmitEvent("search:progress", p)
		}
	}
	return search.Run(ctx, wd, opts, emit)
}

// Cancel 取消进行中的搜索。
func (Search) Cancel(ctx context.Context) error {
	search.Cancel()
	return nil
}

// Detail 影片详情。
type Detail struct{}

// Get 详情（含全部线路与剧集）。
func (Detail) Get(ctx context.Context, siteKey string, vodID string) (models.VodDetail, error) {
	return detail.Get(ctx, siteKey, vodID)
}

// Category 分类。
type Category struct{}

// List 分类树。
func (Category) List(ctx context.Context) ([]models.CategoryBucket, error) {
	return category.List(ctx)
}

// Browse 分类浏览一页。
func (Category) Browse(ctx context.Context, bucket string, sub string, page int) (models.CategoryPage, error) {
	return category.Browse(ctx, bucket, sub, page)
}

// Home 首页内容位。
type Home struct{}

// Sections 首页各内容位。
func (Home) Sections(ctx context.Context, force bool) ([]models.HomeSection, error) {
	return home.Sections(ctx, force)
}

// Live 直播。
type Live struct{}

// Load 载入直播频道（合并多源）。
func (Live) Load(ctx context.Context, force bool) (models.LiveResult, error) {
	return live.Load(ctx, force)
}

// Resolve 房间号频道换真实播放地址。
func (Live) Resolve(ctx context.Context, rawURL string) (models.LiveResolved, error) {
	return stream.ResolveRoom(ctx, rawURL)
}

// Health 全部线路体检缓存。
func (Live) Health(ctx context.Context) (map[string]models.LiveHealth, error) {
	return livehealth.Get(), nil
}

// Probe 体检一批线路。
func (Live) Probe(ctx context.Context, urls []string, options *livehealth.ProbeOptions) (map[string]models.LiveHealth, error) {
	opts := livehealth.ProbeOptions{}
	if options != nil {
		opts = *options
	}
	return livehealth.Probe(ctx, urls, opts)
}

// ClearHealth 清空体检缓存。
func (Live) ClearHealth(ctx context.Context) (map[string]models.LiveHealth, error) {
	return livehealth.Clear(), nil
}

// Library 收藏与历史。
type Library struct{}

// Get 收藏 + 历史快照。
func (Library) Get(ctx context.Context) (models.LibrarySnapshot, error) {
	return library.Get(), nil
}

// ToggleFavorite 收藏/取消收藏。
func (Library) ToggleFavorite(ctx context.Context, input models.FavoriteItem) (map[string]any, error) {
	return library.ToggleFavorite(input)
}

// RemoveFavorite 取消收藏（按 siteKey::vodId）。
func (Library) RemoveFavorite(ctx context.Context, key string) error {
	return library.RemoveFavorite(key)
}

// ClearFavorites 清空收藏。
func (Library) ClearFavorites(ctx context.Context) error {
	return library.ClearFavorites()
}

// Record 记一条观看进度。
func (Library) Record(ctx context.Context, input models.HistoryItem) error {
	return library.Record(input)
}

// RemoveHistory 删掉归一化片名对应的一部剧在所有源上的历史。
func (Library) RemoveHistory(ctx context.Context, key string) error {
	return library.RemoveHistoryGroup(key)
}

// ClearHistory 清空历史。
func (Library) ClearHistory(ctx context.Context) error {
	return library.ClearHistory()
}

// Player 外部播放器。
type Player struct{}

// Detect 探测系统里可用的播放器。
func (Player) Detect(ctx context.Context) ([]models.PlayerInfo, error) {
	return player.Detect()
}

// Open 用首选播放器拉起一条流。
func (Player) Open(ctx context.Context, rawURL string, title string) (player.OpenResult, error) {
	return player.Open(rawURL, title)
}

// Parse 解析接口。
type Parse struct{}

// List 全部解析接口（含战绩排序）。
func (Parse) List(ctx context.Context) ([]models.ParseInfo, error) {
	return parse.List(), nil
}

// Resolve 网页型播放地址换直链。
func (Parse) Resolve(ctx context.Context, rawURL string, options *parse.ResolveOptions) (models.ParseResult, error) {
	opts := parse.ResolveOptions{}
	if options != nil {
		opts = *options
	}
	return parse.Resolve(ctx, rawURL, opts)
}

// Test 设置页「测一下」。
func (Parse) Test(ctx context.Context, rawURL string, target *string) (models.ParseTestResult, error) {
	t := ""
	if target != nil {
		t = *target
	}
	return parse.Test(ctx, rawURL, t)
}

// Add 手动添加解析接口。
func (Parse) Add(ctx context.Context, input parse.AddInput) ([]models.ParseInfo, error) {
	return parse.Add(input)
}

// Remove 删除手动添加的解析接口。
func (Parse) Remove(ctx context.Context, rawURL string) ([]models.ParseInfo, error) {
	return parse.Remove(rawURL)
}

// ResetStats 清零战绩。
func (Parse) ResetStats(ctx context.Context) ([]models.ParseInfo, error) {
	return parse.ResetStats(), nil
}

// Ai AI 剧情总结。
type Ai struct{}

// Summarize 文本总结。
func (Ai) Summarize(ctx context.Context, input models.AiSummaryInput) (models.AiSummaryResult, error) {
	return ai.Summarize(ctx, input)
}

// SummarizeVisual 带画面帧的多模态总结。
func (Ai) SummarizeVisual(ctx context.Context, input models.AiSummaryInput, frames []models.AiFrame) (models.AiSummaryResult, error) {
	return ai.SummarizeVisual(ctx, input, frames)
}
