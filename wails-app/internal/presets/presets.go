// Package presets 内置推荐订阅：打包版「装完即用」的订阅源。
// 翻译自 src/main/services/presets.ts。
//
// 作者：Shiro
package presets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"tvbox-desktop/internal/sources"
	"tvbox-desktop/internal/store"
	"tvbox-desktop/models"
)

// bundledFlagStore 记住「内置订阅已经送过了」，避免用户删光后重启又冒出来。
const bundledFlagStore = "bundled-sources"

// SourcePreset 一个内置订阅预设。
type SourcePreset struct {
	// ID 预设标识。
	ID string
	// Name 展示名。
	Name string
	// Desc 简介（设置页展示）。
	Desc string
	// SiteCount 给界面显示用：这个订阅里有几个站点。
	SiteCount int
	// LiveCount 给界面显示用：这个订阅里有几个直播源。
	LiveCount int
	// ConfigJSON 内嵌配置原文，落盘时原样写出。
	ConfigJSON string
}

// publicCMSJSON 是 PUBLIC_CMS 预设的配置原文（与 TS 逐字段一致）。
//
// 只留 IPv4 那一条直播源。实测 Gather.m3u 的 123 条线路里 108 条（88%）拉不起来，
// 而且它和 iptv4.txt 一个重名的频道都没有（补不上线路），留着只会让频道列表
// 里多出一堆点开就失败的卡。
//
// 首页内容位写在这里的行优先于「每个源一条最近更新」的默认行为，
// 让首启就有几行带分类的内容，而不是五条一模一样的「最近更新」。
// typeId 用的是苹果CMS v10 的默认分类 id（6=动作片、13=国产剧、42=日本动漫），
// 这五条都在真实源上逐条验证过 20 条有内容。
const publicCMSJSON = `{
  "format": "tvbox-desktop/1",
  "name": "公开可用源（苹果CMS 采集站）",
  "note": "实测存活的公开采集接口，全部是标准苹果CMS v10 的 /api.php/provide/vod。判断标准有三条：列表接口有数据、带 wd 的搜索有结果、详情能解析出剧集地址。直播源一 txt 一 m3u，用来覆盖两种解析格式。",
  "sites": [
    {
      "key": "ffzy",
      "name": "非凡资源",
      "type": "cms",
      "api": "https://api.ffzyapi.com/api.php/provide/vod",
      "searchable": 1
    },
    {
      "key": "lzi",
      "name": "量子资源",
      "type": "cms",
      "api": "https://cj.lziapi.com/api.php/provide/vod",
      "searchable": 1
    },
    {
      "key": "dyttzy",
      "name": "电影天堂资源",
      "type": "cms",
      "api": "https://caiji.dyttzyapi.com/api.php/provide/vod",
      "searchable": 1
    },
    {
      "key": "guangsu",
      "name": "光速资源",
      "type": "cms",
      "api": "https://api.guangsuapi.com/api.php/provide/vod",
      "searchable": 1
    },
    {
      "key": "subo",
      "name": "素博资源",
      "type": "cms",
      "api": "https://subocaiji.com/api.php/provide/vod",
      "searchable": 1
    }
  ],
  "lives": [
    {
      "name": "公开直播源·IPv4（txt）",
      "type": 0,
      "url": "https://live.zbds.top/tv/iptv4.txt"
    }
  ],
  "tvboxDesktop": {
    "home": {
      "sections": [
        {
          "title": "电影 · 最近更新",
          "site": "dyttzy"
        },
        {
          "title": "电影 · 动作片",
          "site": "ffzy",
          "typeId": 6
        },
        {
          "title": "电视剧 · 国产剧",
          "site": "lzi",
          "typeId": 13
        },
        {
          "title": "动漫 · 日本",
          "site": "guangsu",
          "typeId": 42
        },
        {
          "title": "综艺 · 最近更新",
          "site": "subo"
        }
      ]
    }
  }
}`

// presets 内置预设列表（等价 TS 的 SOURCE_PRESETS）。
//
// 打包版走 EnsureBundled：装完直接就是「带源」状态，打开就能搜。
// 设置页里仍然把它列出来，用户可以随时删掉，删干净了也不会自己长回来。
//
// 点「添加」时会把配置内容落到 <数据目录>/presets/<id>.json，
// 而不是引用仓库里的 samples/，这样仓库删了、应用换台机器也能继续用。
var presets = []SourcePreset{
	{
		ID:         "public-cms",
		Name:       "公开可用源（苹果CMS 采集站）",
		Desc:       "实测存活的 5 个公开采集接口 + 2 个公开直播源。这类源随时可能失效，失效后在「影视源」「电视直播」页一看便知。",
		SiteCount:  5,
		LiveCount:  2,
		ConfigJSON: publicCMSJSON,
	},
}

// ListPresets 列出内置预设的摘要信息。
func ListPresets() []models.SourcePresetInfo {
	out := make([]models.SourcePresetInfo, 0, len(presets))
	for _, p := range presets {
		out = append(out, models.SourcePresetInfo{
			ID:        p.ID,
			Name:      p.Name,
			Desc:      p.Desc,
			SiteCount: p.SiteCount,
			LiveCount: p.LiveCount,
		})
	}
	return out
}

// dataDir 数据目录：借 store 的文件路径反推（store.Init 已指向 exe 同级 data/）。
func dataDir() string {
	return filepath.Dir(store.Path("sources"))
}

// AddPreset 把预设落成数据目录里的一个本地配置文件，再注册成配置源。
func AddPreset(ctx context.Context, id string) (models.ConfigSource, error) {
	var preset *SourcePreset
	for i := range presets {
		if presets[i].ID == id {
			preset = &presets[i]
			break
		}
	}
	if preset == nil {
		return models.ConfigSource{}, errors.New("没有这个内置订阅")
	}

	dir := filepath.Join(dataDir(), "presets")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return models.ConfigSource{}, fmt.Errorf("创建预设目录失败：%w", err)
	}
	file := filepath.Join(dir, preset.ID+".json")
	if err := os.WriteFile(file, []byte(preset.ConfigJSON), 0o644); err != nil {
		return models.ConfigSource{}, fmt.Errorf("写入内置订阅配置失败：%w", err)
	}

	for _, s := range sources.List() {
		if s.URL == file {
			return models.ConfigSource{}, errors.New("这个内置订阅已经添加过了")
		}
	}
	return sources.Add(ctx, sources.AddInput{Name: preset.Name, URL: file, Kind: "file"})
}

// EnsureBundled 首启把内置订阅装好，让打包版「装完即用」。
//
// 只在一个配置源都没有的时候执行 —— 用户自己删光了就不该再冒出来。
// 装上之后立刻同步一次，这样首屏的「影视源」和「电视直播」就有数据，
// 不用等用户手动点「同步配置」。
//
// 只带**订阅源**，不带设置：API Key、外部播放器路径、收藏与观看历史
// 都属于使用者的个人数据，绝不能跟着安装包发出去。
//
// 用一个标志位记住「已经送过了」：用户之后自己把源删光、重启，不会又长回来。
func EnsureBundled(ctx context.Context) error {
	if len(presets) == 0 {
		return nil
	}
	if store.ReadJSON[bool](bundledFlagStore, false) {
		return nil
	}
	if len(sources.List()) > 0 {
		store.WriteStore(bundledFlagStore, true)
		return nil
	}
	source, err := AddPreset(ctx, presets[0].ID)
	if err != nil {
		// 写不进去就退回首启引导页，用户仍然可以手动添加
		return err
	}
	store.WriteStore(bundledFlagStore, true)
	_, err = sources.Sync(ctx, source.ID)
	return err
}
