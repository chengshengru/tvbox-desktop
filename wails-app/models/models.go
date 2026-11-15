// Package models 主进程（Go）与渲染进程共用的数据结构定义。
// 字段与 JSON tag 严格对齐 src/shared/types.ts，保证 data/ 目录两代格式互认。
//
// 作者：Shiro
package models

// Site 一个可用的采集/网页站点。
//
// 站点类型：0=需spider 1=网页/规则 2=自定义 3=苹果CMS 4=CMS变体
type Site struct {
	Key         string            `json:"key"`
	Name        string            `json:"name"`
	Type        int               `json:"type"`
	API         string            `json:"api"`
	Ext         string            `json:"ext,omitempty"`
	Jar         string            `json:"jar,omitempty"`
	Searchable  bool              `json:"searchable"`
	QuickSearch bool              `json:"quickSearch"`
	Filterable  bool              `json:"filterable"`
	PlayerType  int               `json:"playerType"`
	Categories  []string          `json:"categories,omitempty"`
	Style       *SiteStyle        `json:"style,omitempty"`
	Header      map[string]string `json:"header,omitempty"`
	Timeout     *float64          `json:"timeout,omitempty"`
	SourceID    string            `json:"sourceId"`
	SourceName  string            `json:"sourceName"`
	Enabled     bool              `json:"enabled"`
	Health      *float64          `json:"health,omitempty"`
	Unsupported string            `json:"unsupported,omitempty"`
	Rules       *HtmlRules        `json:"rules,omitempty"`
}

// SiteStyle 海报样式（type=ratio 时按 ratio 排版）。
type SiteStyle struct {
	Type  string   `json:"type,omitempty"`
	Ratio *float64 `json:"ratio,omitempty"`
}

// HtmlRules 手写配置的网页解析规则（CSS 选择器串）。
type HtmlRules struct {
	List          string `json:"list,omitempty"`
	Name          string `json:"name,omitempty"`
	Link          string `json:"link,omitempty"`
	Pic           string `json:"pic,omitempty"`
	Remarks       string `json:"remarks,omitempty"`
	Search        string `json:"search,omitempty"`
	DetailName    string `json:"detailName,omitempty"`
	DetailContent string `json:"detailContent,omitempty"`
	PlayList      string `json:"playList,omitempty"`
	PlayItem      string `json:"playItem,omitempty"`
}

// ParseItem 配置里的一条解析接口。
type ParseItem struct {
	Name string `json:"name"`
	Type int    `json:"type"`
	URL  string `json:"url"`
	Ext  any    `json:"ext,omitempty"`
}

// ParseStat 一条解析接口的实测战绩，用来做「可用性排行」。
type ParseStat struct {
	OK        int    `json:"ok"`
	Fail      int    `json:"fail"`
	LastOK    int64  `json:"lastOk"`
	LastMs    int64  `json:"lastMs"`
	LastError string `json:"lastError,omitempty"`
}

// ParseInfo 界面上的一条解析接口（含来源与战绩）。
type ParseInfo struct {
	ParseItem
	Origin string    `json:"origin"`
	Flags  []string  `json:"flags"`
	Stat   ParseStat `json:"stat"`
}

// ParseAttempt 一轮轮询里单个接口的尝试记录。
type ParseAttempt struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Ms    int64  `json:"ms"`
	URL   string `json:"url,omitempty"`
	Error string `json:"error,omitempty"`
}

// ParseResult 解析换址的最终结果。
type ParseResult struct {
	OK        bool           `json:"ok"`
	URL       string         `json:"url"`
	Direct    bool           `json:"direct"`
	ParseName string         `json:"parseName,omitempty"`
	Ms        int64          `json:"ms"`
	Attempts  []ParseAttempt `json:"attempts"`
	Error     string         `json:"error,omitempty"`
}

// ParseTestResult 设置页「测一下」的结果。
type ParseTestResult struct {
	OK     bool   `json:"ok"`
	Ms     int64  `json:"ms"`
	Target string `json:"target"`
	URL    string `json:"url,omitempty"`
	Error  string `json:"error,omitempty"`
}

// LiveSource 配置里的一个直播源（type 可能是数字也可能是字符串）。
type LiveSource struct {
	Name string `json:"name"`
	Type any    `json:"type,omitempty"`
	URL  string `json:"url"`
	EPG  string `json:"epg,omitempty"`
}

// HeaderRule 按 host 通配的全局请求头规则。
type HeaderRule struct {
	Host   string            `json:"host"`
	Header map[string]string `json:"header"`
}

// ParsedConfig 解析后的 TVBox 配置。
type ParsedConfig struct {
	Name           string                `json:"name"`
	Notice         string                `json:"notice,omitempty"`
	Sites          []Site                `json:"sites"`
	Parses         []ParseItem           `json:"parses"`
	Lives          []LiveSource          `json:"lives"`
	Flags          []string              `json:"flags"`
	Headers        []HeaderRule          `json:"headers"`
	Theme          *ThemeConfig          `json:"theme,omitempty"`
	ExternalPlayer *ExternalPlayerConfig `json:"externalPlayer,omitempty"`
	Home           *HomeConfig           `json:"home,omitempty"`
	Raw            any                   `json:"raw,omitempty"`
}

// ThemeConfig 配置源自带的主题。
type ThemeConfig struct {
	Accent      string   `json:"accent,omitempty"`
	PosterRatio *float64 `json:"posterRatio,omitempty"`
	Density     string   `json:"density,omitempty"`
}

// ExternalPlayerConfig 外部播放器配置。
type ExternalPlayerConfig struct {
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	Prefer  []string `json:"prefer,omitempty"`
}

// HomeConfig 配置源自带的首页内容位。
type HomeConfig struct {
	Sections []HomeSectionCfg `json:"sections,omitempty"`
}

// HomeSectionCfg 首页内容位的一条配置。
type HomeSectionCfg struct {
	Title  string `json:"title"`
	Site   string `json:"site"`
	TypeID any    `json:"typeId,omitempty"`
}

// ConfigSource 一个订阅源（远程 URL 或本地文件）。
type ConfigSource struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	URL        string `json:"url"`
	Kind       string `json:"kind"`
	Enabled    bool   `json:"enabled"`
	LastSync   int64  `json:"lastSync"`
	Error      string `json:"error,omitempty"`
	SiteCount  int    `json:"siteCount"`
	LiveCount  int    `json:"liveCount"`
	ParseCount int    `json:"parseCount"`
	Notice     string `json:"notice,omitempty"`
	Raw        any    `json:"raw,omitempty"`
}

// SourcePresetInfo 内置推荐订阅（只把摘要送到界面）。
type SourcePresetInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Desc      string `json:"desc"`
	SiteCount int    `json:"siteCount"`
	LiveCount int    `json:"liveCount"`
}

// Vod 一条影片（字段名沿用苹果CMS 原始命名）。
type Vod struct {
	VodID       string `json:"vod_id"`
	VodName     string `json:"vod_name"`
	VodPic      string `json:"vod_pic"`
	VodRemarks  string `json:"vod_remarks"`
	VodYear     string `json:"vod_year"`
	VodArea     string `json:"vod_area"`
	VodActor    string `json:"vod_actor"`
	VodDirector string `json:"vod_director"`
	TypeName    string `json:"type_name"`
	VodScore    string `json:"vod_score"`
	SiteKey     string `json:"siteKey"`
	SiteName    string `json:"siteName"`
	SourceID    string `json:"sourceId"`
}

// Episode 一集。
type Episode struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Index int    `json:"index"`
}

// Line 一条播放线路。
type Line struct {
	Name     string    `json:"name"`
	Episodes []Episode `json:"episodes"`
}

// VodDetail 详情（整部剧 + 全部线路）。
type VodDetail struct {
	Vod
	VodContent string `json:"vod_content"`
	Lines      []Line `json:"lines"`
}

// Category 站点分类树里的一个节点。
type Category struct {
	TypeID   any    `json:"type_id"`
	TypeName string `json:"type_name"`
}

// VodPage 一页列表结果。
type VodPage struct {
	Page      int   `json:"page"`
	Pagecount int   `json:"pagecount"`
	Total     int   `json:"total"`
	List      []Vod `json:"list"`
}

// LiveChannel 一条原始频道。
type LiveChannel struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Group string `json:"group"`
	Logo  string `json:"logo,omitempty"`
}

// LiveGroup 原始分组。
type LiveGroup struct {
	Name     string        `json:"name"`
	Channels []LiveChannel `json:"channels"`
}

// LiveURL 合并频道上的一条线路。
type LiveURL struct {
	URL    string `json:"url"`
	Source string `json:"source"`
}

// LiveChannelEntry 合并后的频道：同名频道在多个直播源出现时收成一条。
type LiveChannelEntry struct {
	Name  string    `json:"name"`
	Group string    `json:"group"`
	Logo  string    `json:"logo,omitempty"`
	URLs  []LiveURL `json:"urls"`
}

// LiveGroupEntry 合并后的分组。
type LiveGroupEntry struct {
	Name     string             `json:"name"`
	Channels []LiveChannelEntry `json:"channels"`
}

// LiveSourceStat 一个直播源的载入统计。
type LiveSourceStat struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Channels int    `json:"channels"`
	Error    string `json:"error,omitempty"`
}

// LiveResult 直播载入结果。
type LiveResult struct {
	Groups  []LiveGroupEntry `json:"groups"`
	Total   int              `json:"total"`
	Sources []LiveSourceStat `json:"sources"`
}

// LiveHealth 一条直播线路的体检结果。
type LiveHealth struct {
	OK        bool     `json:"ok"`
	Ms        int64    `json:"ms"`
	At        int64    `json:"at"`
	Height    *int     `json:"height,omitempty"`
	Bandwidth *float64 `json:"bandwidth,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// LiveResolved 房间号频道换出来的播放信息。
type LiveResolved struct {
	URL     string `json:"url"`
	Referer string `json:"referer"`
	Line    string `json:"line,omitempty"`
}

// FavoriteItem 收藏的一部影片。键是 siteKey::vodId。
type FavoriteItem struct {
	SiteKey  string `json:"siteKey"`
	SiteName string `json:"siteName"`
	VodID    string `json:"vodId"`
	Name     string `json:"name"`
	Pic      string `json:"pic,omitempty"`
	Remarks  string `json:"remarks,omitempty"`
	Score    string `json:"score,omitempty"`
	AddedAt  int64  `json:"addedAt"`
}

// HistoryItem 一条观看历史。
type HistoryItem struct {
	SiteKey   string  `json:"siteKey"`
	SiteName  string  `json:"siteName"`
	VodID     string  `json:"vodId"`
	Name      string  `json:"name"`
	Pic       string  `json:"pic,omitempty"`
	LineIndex int     `json:"lineIndex"`
	EpIndex   int     `json:"epIndex"`
	EpName    string  `json:"epName"`
	Position  float64 `json:"position"`
	Duration  float64 `json:"duration"`
	UpdatedAt int64   `json:"updatedAt"`
}

// HistoryGroup 同一部剧在各资源上的历史记录合并。
type HistoryGroup struct {
	Key       string        `json:"key"`
	Name      string        `json:"name"`
	Pic       string        `json:"pic,omitempty"`
	Items     []HistoryItem `json:"items"`
	UpdatedAt int64         `json:"updatedAt"`
}

// LibrarySnapshot 收藏 + 历史的整体快照。
type LibrarySnapshot struct {
	Favorites []FavoriteItem `json:"favorites"`
	History   []HistoryGroup `json:"history"`
}

// WindowBounds 窗口尺寸记忆。
type WindowBounds struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// AppSettings 应用设置。
type AppSettings struct {
	Concurrency      int                  `json:"concurrency"`
	Timeout          float64              `json:"timeout"`
	UserAgent        string               `json:"userAgent"`
	Accent           string               `json:"accent"`
	Density          string               `json:"density"`
	SidebarCollapsed bool                 `json:"sidebarCollapsed"`
	FilterCommentary bool                 `json:"filterCommentary"`
	ExternalPlayer   ExternalPlayerConfig `json:"externalPlayer"`
	Window           WindowBounds         `json:"window"`
	AI               AiSettings           `json:"ai"`
}

// AiSettings AI 剧情总结（DeepSeek）设置。
type AiSettings struct {
	APIKey  string `json:"apiKey"`
	Model   string `json:"model"`
	BaseURL string `json:"baseUrl"`
}

// AiSummaryInput 送给模型的一集资料。
type AiSummaryInput struct {
	VodName  string `json:"vodName"`
	TypeName string `json:"typeName,omitempty"`
	Year     string `json:"year,omitempty"`
	Area     string `json:"area,omitempty"`
	Actor    string `json:"actor,omitempty"`
	Director string `json:"director,omitempty"`
	Score    string `json:"score,omitempty"`
	Remarks  string `json:"remarks,omitempty"`
	Content  string `json:"content,omitempty"`
	EpName   string `json:"epName"`
	EpIndex  int    `json:"epIndex"`
	EpTotal  int    `json:"epTotal"`
	LineName string `json:"lineName,omitempty"`
}

// AiUsage token 用量。
type AiUsage struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Total      int `json:"total"`
}

// AiSummaryResult 总结结果。
type AiSummaryResult struct {
	Text  string   `json:"text"`
	Model string   `json:"model"`
	Ms    int64    `json:"ms"`
	Usage *AiUsage `json:"usage,omitempty"`
}

// AiFrame 从正在播的 video 上抽的一帧（b64 为裸 base64）。
type AiFrame struct {
	T   float64 `json:"t"`
	B64 string  `json:"b64"`
}

// AppInfo 应用信息。字段名沿用 Electron 版（electron 位填 WebView 版本，node 位填 Go 版本），
// 保证前端 Settings 页零改动。
type AppInfo struct {
	Version  string `json:"version"`
	Electron string `json:"electron"`
	Chrome   string `json:"chrome"`
	Node     string `json:"node"`
	Platform string `json:"platform"`
	UserData string `json:"userData"`
}

// TestResult 站点/解析连通性测试结果。
type TestResult struct {
	OK      bool   `json:"ok"`
	Ms      int64  `json:"ms"`
	Message string `json:"message"`
	Sample  string `json:"sample,omitempty"`
}

// SiteSearchResult 单个站点的一次搜索结果。
type SiteSearchResult struct {
	SiteKey  string `json:"siteKey"`
	SiteName string `json:"siteName"`
	OK       bool   `json:"ok"`
	Ms       int64  `json:"ms"`
	Message  string `json:"message"`
	List     []Vod  `json:"list"`
}

// SearchProgress 边搜边回报的进度事件负载。
type SearchProgress struct {
	Wd     string           `json:"wd"`
	Done   int              `json:"done"`
	Total  int              `json:"total"`
	Result SiteSearchResult `json:"result"`
}

// SearchResponse 聚合搜索的最终响应。
type SearchResponse struct {
	Wd      string             `json:"wd"`
	Ms      int64              `json:"ms"`
	Total   int                `json:"total"`
	Results []SiteSearchResult `json:"results"`
}

// PlayerInfo 一个可用的外部播放器。
// source: scan=扫常见安装路径；registry=系统文件关联；manual=设置里手填。
type PlayerInfo struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Source string `json:"source,omitempty"`
}

// SearchOptions 聚合搜索选项。
type SearchOptions struct {
	Sites        []string `json:"sites,omitempty"`
	LimitPerSite *int     `json:"limitPerSite,omitempty"`
	Concurrency  *int     `json:"concurrency,omitempty"`
}

// SiteClass 站点分类树里的一个节点。
type SiteClass struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	PID  string `json:"pid"`
}

// CategorySiteIds 一个站点在某个子分类下可查询的叶子分类 id。
type CategorySiteIds struct {
	SiteKey  string   `json:"siteKey"`
	SiteName string   `json:"siteName"`
	TypeIDs  []string `json:"typeIds"`
}

// CategorySub 大类下的子分类。
type CategorySub struct {
	Name  string            `json:"name"`
	Sites []CategorySiteIds `json:"sites"`
}

// CategoryBucket 归并后的大类。
type CategoryBucket struct {
	Name string        `json:"name"`
	Subs []CategorySub `json:"subs"`
}

// CategoryPage 分类浏览的一页。
type CategoryPage struct {
	Page      int                `json:"page"`
	PageCount int                `json:"pageCount"`
	Bucket    string             `json:"bucket"`
	Sub       string             `json:"sub"`
	Results   []SiteSearchResult `json:"results"`
}

// HomeSection 首页的一个内容位。
type HomeSection struct {
	Key      string `json:"key"`
	Title    string `json:"title"`
	Origin   string `json:"origin"`
	SiteKey  string `json:"siteKey,omitempty"`
	SiteName string `json:"siteName,omitempty"`
	Items    []Vod  `json:"items"`
	Error    string `json:"error,omitempty"`
}
