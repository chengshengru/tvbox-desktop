// TVBox Desktop（Go + Wails v3 形态）应用装配入口。
// 职责：便携数据目录、启动钩子（代理探测/内置订阅/源预热）、窗口与 AssetServer
// 中间件（同源 /media 媒体转发，不占端口）、13 个领域绑定服务注册。
// 业务逻辑全部在 internal/*，绑定薄壳在 tvbox/*。
//
// 作者：Shiro
package main

import (
	"context"
	"embed"
	"log"
	"os"
	"path/filepath"
	goruntime "runtime"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"tvbox-desktop/internal/mediamw"
	"tvbox-desktop/internal/presets"
	"tvbox-desktop/internal/proxy"
	"tvbox-desktop/internal/settings"
	"tvbox-desktop/internal/sources"
	"tvbox-desktop/internal/store"
	"tvbox-desktop/models"
	"tvbox-desktop/tvbox"
)

//go:embed all:frontend/dist
var assets embed.FS

// portableDataDir 便携数据目录：exe 同级 data/，建不出来（如 Program Files 只读）
// 则退回系统配置目录，与 Electron 版兜底逻辑一致。
func portableDataDir() string {
	exe, err := os.Executable()
	if err == nil {
		dir := filepath.Join(filepath.Dir(exe), "data")
		if mkErr := os.MkdirAll(dir, 0o755); mkErr == nil {
			return dir
		}
	}
	base, err := os.UserConfigDir()
	if err != nil {
		base = "."
	}
	dir := filepath.Join(base, "tvbox-desktop")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

func main() {
	dataDir := portableDataDir()
	store.Init(dataDir)

	// 对应 Electron 版启动时的 ensureReachableProxy：系统代理开着但代理软件没跑时强制直连
	if msg := proxy.EnsureReachable(); msg != "" {
		log.Println(msg)
	}

	app := application.New(application.Options{
		Name:        "TVBox Desktop",
		Description: "TVBox 桌面客户端（Go + Wails）",
		Services: []application.Service{
			application.NewService(&tvbox.App{}),
			application.NewService(&tvbox.Settings{}),
			application.NewService(&tvbox.Sources{}),
			application.NewService(&tvbox.Sites{}),
			application.NewService(&tvbox.Search{}),
			application.NewService(&tvbox.Detail{}),
			application.NewService(&tvbox.Category{}),
			application.NewService(&tvbox.Home{}),
			application.NewService(&tvbox.Live{}),
			application.NewService(&tvbox.Library{}),
			application.NewService(&tvbox.Player{}),
			application.NewService(&tvbox.Parse{}),
			application.NewService(&tvbox.Ai{}),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
			// 同源 /media 媒体转发中间件：解决 CORS + Referer 防盗链，进程不监听端口
			Middleware: mediamw.Middleware(),
		},
		Windows: application.WindowsOptions{
			// WebView2 缓存也放便携目录，避免污染 %APPDATA%
			WebviewUserDataPath: filepath.Join(dataDir, "webview"),
		},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "desktop.tvbox.wails",
		},
		// 退出前把防抖窗口内的脏页全部落盘（对应 before-quit 的 flushStore）
		OnShutdown: func() { _ = store.Flush("") },
	})

	// 注入宿主能力到绑定层（保持 tvbox 包不依赖 wails）
	tvbox.EmitEvent = func(name string, data any) { app.Event.Emit(name, data) }
	tvbox.OpenURL = func(rawURL string) error { return app.Browser.OpenURL(rawURL) }
	tvbox.PickFileDialog = func(ctx context.Context) (string, error) {
		d := app.Dialog.OpenFile()
		d.SetTitle("选择 TVBox 配置 JSON")
		d.AddFilter("TVBox 配置", "*.json")
		return d.PromptForSingleSelection()
	}

	cfg := settings.Get()
	win := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "TVBox",
		Width:            cfg.Window.Width,
		Height:           cfg.Window.Height,
		MinWidth:         1100,
		MinHeight:        700,
		BackgroundColour: application.NewRGB(255, 255, 255),
	})

	// 窗口尺寸记忆：resize 防抖 400ms 落盘（与 Electron 版一致，避免拖拽期间高频写盘）
	var resizeTimer *time.Timer
	win.OnWindowEvent(events.Common.WindowDidResize, func(*application.WindowEvent) {
		if resizeTimer != nil {
			resizeTimer.Stop()
		}
		resizeTimer = time.AfterFunc(400*time.Millisecond, func() {
			w, h := win.Width(), win.Height()
			if w > 0 && h > 0 {
				settings.SaveWindow(w, h)
			}
		})
	})

	tvbox.SetAppInfo(models.AppInfo{
		Version:  tvbox.Version,
		Electron: "WebView2", // 字段名沿用 Electron 版，前端展示位填宿主内核
		Chrome:   goruntime.GOOS,
		Node:     goruntime.Version(),
		Platform: goruntime.GOOS,
		UserData: dataDir,
	})

	// 内置订阅首启安装 + 订阅源解析预热：异步做，不阻塞窗口出现
	go func() {
		ctx := context.Background()
		if err := presets.EnsureBundled(ctx); err != nil {
			log.Printf("内置订阅安装失败：%v", err)
		}
		if err := sources.Hydrate(ctx); err != nil {
			log.Printf("订阅源预热失败：%v", err)
		}
	}()

	if err := app.Run(); err != nil {
		log.Fatalf("应用退出异常：%v", err)
	}
}
