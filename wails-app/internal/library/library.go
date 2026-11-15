// Package library 收藏与观看历史。
// 翻译自 src/main/services/library.ts。
//
// 都按 `${siteKey}::${vodId}` 作为主键 —— 同一部片在不同站点算不同的条目，
// 因为换源之后剧集地址完全不同，进度没法通用。
//
// 历史只保留最近 MAX_HISTORY 条，避免看久了文件无限膨胀。
//
// 作者：Shiro
package library

import (
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"

	"tvbox-desktop/internal/store"
	"tvbox-desktop/models"
)

const (
	favoritesStore = "favorites"
	historyStore   = "history"
	maxHistory     = 200
)

// ItemKey 收藏/历史的主键。
func ItemKey(siteKey string, vodID string) string {
	return siteKey + "::" + vodID
}

// punctRe 对应 TS 的 /[\s\p{P}\p{S}]/gu：空白 + Unicode 标点 + 符号。
var punctRe = regexp.MustCompile(`[\s\p{P}\p{S}]`)

// NormalizeName 片名的归一化键：全角转半角（NFKC）、统一小写、去掉空格与标点。
// 用来判断「这几个站上的是不是同一部剧」。
//
// 刻意保守：只做上面这些，不去剥「第二季」「(国语)」这类后缀。
// 宁可漏合（「庆余年」和「庆余年 第二季」算两条），也不要把不同季的进度串到一起。
//
// 注意：分组 key 算法必须与 Electron 版 library.ts 完全一致，
// 否则 data/history.json 升级后分组会错乱。
func NormalizeName(name string) string {
	key := punctRe.ReplaceAllString(strings.ToLower(norm.NFKC.String(name)), "")
	if key != "" {
		return key
	}
	return strings.ToLower(strings.TrimSpace(name))
}

func readFavorites() []models.FavoriteItem {
	list := store.ReadJSON[[]models.FavoriteItem](favoritesStore, nil)
	if list == nil {
		return []models.FavoriteItem{}
	}
	return list
}

func readHistory() []models.HistoryItem {
	list := store.ReadJSON[[]models.HistoryItem](historyStore, nil)
	if list == nil {
		return []models.HistoryItem{}
	}
	return list
}

// listFavorites 按收藏时间倒序。
func listFavorites() []models.FavoriteItem {
	list := readFavorites()
	sort.SliceStable(list, func(i, j int) bool { return list[i].AddedAt > list[j].AddedAt })
	return list
}

// listHistory 按归一化片名把「一个源一条」的记录合成「一部剧一条」。
// 组内按最近看过排序，items[0] 就是主记录；组与组之间也按最近看过排。
func listHistory() []models.HistoryGroup {
	list := readHistory()
	groups := map[string]*models.HistoryGroup{}
	order := []string{}
	for _, item := range list {
		key := NormalizeName(item.Name)
		if found, ok := groups[key]; ok {
			found.Items = append(found.Items, item)
			continue
		}
		groups[key] = &models.HistoryGroup{
			Key:       key,
			Name:      item.Name,
			Pic:       item.Pic,
			Items:     []models.HistoryItem{item},
			UpdatedAt: item.UpdatedAt,
		}
		order = append(order, key)
	}
	out := make([]models.HistoryGroup, 0, len(order))
	for _, key := range order {
		g := groups[key]
		sort.SliceStable(g.Items, func(i, j int) bool { return g.Items[i].UpdatedAt > g.Items[j].UpdatedAt })
		g.Name = g.Items[0].Name
		// 主记录没封面时退回组内第一张有封面的（存储里空封面即缺省，与 TS 的 ?? 语义等价）
		g.Pic = g.Items[0].Pic
		if g.Pic == "" {
			for _, it := range g.Items {
				if it.Pic != "" {
					g.Pic = it.Pic
					break
				}
			}
		}
		g.UpdatedAt = g.Items[0].UpdatedAt
		out = append(out, *g)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out
}

// Get 收藏 + 历史的整体快照。
func Get() models.LibrarySnapshot {
	return models.LibrarySnapshot{
		Favorites: listFavorites(),
		History:   listHistory(),
	}
}

// ToggleFavorite 收藏 / 取消收藏，返回 {"favorited": bool}，界面据此改按钮。
func ToggleFavorite(input models.FavoriteItem) (map[string]any, error) {
	key := ItemKey(input.SiteKey, input.VodID)
	list := readFavorites()
	idx := -1
	for i := range list {
		if ItemKey(list[i].SiteKey, list[i].VodID) == key {
			idx = i
			break
		}
	}
	if idx >= 0 {
		list = append(list[:idx], list[idx+1:]...)
		store.WriteStore(favoritesStore, list)
		return map[string]any{"favorited": false}, nil
	}
	input.AddedAt = time.Now().UnixMilli()
	list = append(list, input)
	store.WriteStore(favoritesStore, list)
	return map[string]any{"favorited": true}, nil
}

// RemoveFavorite 按主键删一条收藏。
func RemoveFavorite(key string) error {
	list := readFavorites()
	out := []models.FavoriteItem{}
	for _, f := range list {
		if ItemKey(f.SiteKey, f.VodID) != key {
			out = append(out, f)
		}
	}
	store.WriteStore(favoritesStore, out)
	return nil
}

// ClearFavorites 清空收藏。
func ClearFavorites() error {
	store.WriteStore(favoritesStore, []models.FavoriteItem{})
	return nil
}

// Record 记一次播放。同一个条目重复播放只更新进度与时间，不新增行，
// 否则拖一次进度条就会多出一条历史。
//
// 关键：**不允许用 0/0 覆盖已有的真实进度**。
// 播放页挂载的瞬间就会先写一条（那时 video 的 duration 还是 NaN），
// 所以「打开播放页 → 立刻退出」「在播放页上刷新/重开应用」都会送进来一条
// position 0 / duration 0 的记录。如果无条件覆盖，进度条就会永远显示不出来。
// 只有在同一集的进度确实是新的（拿到了有效时长）时才覆盖进度。
func Record(input models.HistoryItem) error {
	key := ItemKey(input.SiteKey, input.VodID)
	list := readHistory()
	idx := -1
	for i := range list {
		if ItemKey(list[i].SiteKey, list[i].VodID) == key {
			idx = i
			break
		}
	}
	var prev *models.HistoryItem
	if idx >= 0 {
		prev = &list[idx]
	}

	next := input
	next.UpdatedAt = time.Now().UnixMilli()

	// 同一集时，只有这次真的拿到了时长才允许改写进度；否则沿用上一次记下的进度。
	if prev != nil && prev.LineIndex == input.LineIndex && prev.EpIndex == input.EpIndex {
		known := input.Duration > 0
		if !known {
			next.Position = prev.Position
			next.Duration = prev.Duration
		}
	}

	if idx >= 0 {
		list = append(list[:idx], list[idx+1:]...)
	}
	list = append([]models.HistoryItem{next}, list...)

	// 超出上限时砍掉最旧的
	sort.SliceStable(list, func(i, j int) bool { return list[i].UpdatedAt > list[j].UpdatedAt })
	if len(list) > maxHistory {
		list = list[:maxHistory]
	}
	store.WriteStore(historyStore, list)
	return nil
}

// RemoveHistoryGroup 删掉一整组 —— 同一部剧在**所有**源上的记录一起去掉。
// key 是 NormalizeName 的结果。
func RemoveHistoryGroup(key string) error {
	list := readHistory()
	out := []models.HistoryItem{}
	for _, h := range list {
		if NormalizeName(h.Name) != key {
			out = append(out, h)
		}
	}
	store.WriteStore(historyStore, out)
	return nil
}

// ClearHistory 清空历史。
func ClearHistory() error {
	store.WriteStore(historyStore, []models.HistoryItem{})
	return nil
}
