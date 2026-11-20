/**
 * Wails v3 宿主桥接层。
 *
 * 原 Electron 版通过 preload 的 contextBridge 把 DesktopApi 挂到 window.api；
 * 迁移到 Wails 后，Go 侧绑定的服务挂在 window.go.tvbox.<Service>.<Method> 上，
 * 本文件用同样的方法名 / 参数 / Promise 返回结构重新实现一份，保持调用方零改动，
 * 并在文件末尾挂回 window.api。
 *
 * @author Shiro
 */
import type {
  AiFrame,
  AiSummaryInput,
  AiSummaryResult,
  AppInfo,
  AppSettings,
  CategoryBucket,
  CategoryPage,
  ConfigSource,
  SourcePresetInfo,
  FavoriteItem,
  HistoryItem,
  HomeSection,
  LibrarySnapshot,
  LiveResult,
  LiveResolved,
  LiveHealth,
  PlayerInfo,
  ParseInfo,
  ParseResult,
  ParseTestResult,
  SearchOptions,
  SearchProgress,
  SearchResponse,
  Site,
  SpiderStatus,
  TestResult,
  VodDetail
} from '@shared/types'

/**
 * 通用桥调用：window.go.tvbox.<Service>.<Method>(...args)。
 * Wails 绑定方法一律返回 Promise，方法名是大驼峰，与 Go 侧一一对应。
 */
const call = (svc: string, method: string, ...args: unknown[]) =>
  (window as any).go.tvbox[svc][method](...args)

const api = {
  info: (): Promise<AppInfo> => call('App', 'Info'),
  openExternal: (url: string): Promise<void> => call('App', 'OpenExternal', url),

  settings: {
    get: (): Promise<AppSettings> => call('Settings', 'Get'),
    set: (patch: Partial<AppSettings>): Promise<AppSettings> => call('Settings', 'Set', patch)
  },

  sources: {
    list: (): Promise<ConfigSource[]> => call('Sources', 'List'),
    add: (input: { name?: string; url: string; kind?: 'url' | 'file' }): Promise<ConfigSource> =>
      call('Sources', 'Add', input),
    update: (
      id: string,
      patch: Partial<Pick<ConfigSource, 'name' | 'url' | 'enabled'>>
    ): Promise<ConfigSource> => call('Sources', 'Update', id, patch),
    remove: (id: string): Promise<void> => call('Sources', 'Remove', id),
    sync: (id: string): Promise<ConfigSource> => call('Sources', 'Sync', id),
    syncAll: (): Promise<ConfigSource[]> => call('Sources', 'SyncAll'),
    pickFile: (): Promise<string | null> => call('Sources', 'PickFile'),
    presets: (): Promise<SourcePresetInfo[]> => call('Sources', 'Presets'),
    addPreset: (id: string): Promise<ConfigSource> => call('Sources', 'AddPreset', id)
  },

  sites: {
    list: (): Promise<Site[]> => call('Sites', 'List'),
    setEnabled: (key: string, enabled: boolean): Promise<void> =>
      call('Sites', 'SetEnabled', key, enabled),
    setAllEnabled: (keys: string[], enabled: boolean): Promise<void> =>
      call('Sites', 'SetAllEnabled', keys, enabled),
    test: (key: string): Promise<TestResult> => call('Sites', 'Test', key)
  },

  search: {
    run: (wd: string, options?: SearchOptions): Promise<SearchResponse> =>
      call('Search', 'Run', wd, options),
    cancel: (): Promise<void> => call('Search', 'Cancel'),
    /** 订阅进度事件，返回取消订阅的函数（回调参数是 progress 对象本身） */
    onProgress: (cb: (progress: SearchProgress) => void): (() => void) => {
      const off = (window as any).runtime.EventsOn(
        'search:progress',
        (progress: SearchProgress): void => cb(progress)
      )
      return () => {
        if (typeof off === 'function') off()
      }
    }
  },

  detail: {
    get: (siteKey: string, vodId: string): Promise<VodDetail> =>
      call('Detail', 'Get', siteKey, vodId)
  },

  category: {
    list: (): Promise<CategoryBucket[]> => call('Category', 'List'),
    browse: (bucket: string, sub = '', page = 1): Promise<CategoryPage> =>
      call('Category', 'Browse', bucket, sub, page)
  },

  home: {
    /** 首页内容位。force=true 时忽略 10 分钟缓存重新拉 */
    sections: (force = false): Promise<HomeSection[]> => call('Home', 'Sections', force)
  },

  live: {
    load: (force = false): Promise<LiveResult> => call('Live', 'Load', force),
    resolve: (url: string): Promise<LiveResolved> => call('Live', 'Resolve', url),
    health: (): Promise<Record<string, LiveHealth>> => call('Live', 'Health'),
    probe: (
      urls: string[],
      options?: { timeout?: number; force?: boolean }
    ): Promise<Record<string, LiveHealth>> => call('Live', 'Probe', urls, options),
    clearHealth: (): Promise<Record<string, LiveHealth>> => call('Live', 'ClearHealth')
  },

  library: {
    get: (): Promise<LibrarySnapshot> => call('Library', 'Get'),
    toggleFavorite: (input: Omit<FavoriteItem, 'addedAt'>): Promise<{ favorited: boolean }> =>
      call('Library', 'ToggleFavorite', input),
    removeFavorite: (key: string): Promise<void> => call('Library', 'RemoveFavorite', key),
    clearFavorites: (): Promise<void> => call('Library', 'ClearFavorites'),
    record: (input: Omit<HistoryItem, 'updatedAt'>): Promise<void> =>
      call('Library', 'Record', input),
    /** key 是归一化片名（HistoryGroup.key），会删掉这部片在所有源上的记录 */
    removeHistory: (key: string): Promise<void> => call('Library', 'RemoveHistory', key),
    clearHistory: (): Promise<void> => call('Library', 'ClearHistory')
  },

  player: {
    detect: (): Promise<PlayerInfo[]> => call('Player', 'Detect'),
    open: (url: string, title: string): Promise<{ player: string; args: string[] }> =>
      call('Player', 'Open', url, title)
  },

  parse: {
    list: (): Promise<ParseInfo[]> => call('Parse', 'List'),
    /** 网页型地址走解析换直链；直链会原样返回（direct=true） */
    resolve: (url: string, options?: { flag?: string; prefer?: string }): Promise<ParseResult> =>
      call('Parse', 'Resolve', url, options ?? {}),
    test: (url: string, target?: string): Promise<ParseTestResult> =>
      call('Parse', 'Test', url, target),
    add: (input: {
      name?: string
      type?: number
      url: string
      flags?: string[]
    }): Promise<ParseInfo[]> => call('Parse', 'Add', input),
    remove: (url: string): Promise<ParseInfo[]> => call('Parse', 'Remove', url),
    resetStats: (): Promise<ParseInfo[]> => call('Parse', 'ResetStats')
  },

  ai: {
    summarize: (input: AiSummaryInput): Promise<AiSummaryResult> => call('Ai', 'Summarize', input),
    /** frames 是前端从 <video> 抽的帧，传给 Go 侧拼成多模态请求 */
    summarizeVisual: (input: AiSummaryInput, frames: AiFrame[]): Promise<AiSummaryResult> =>
      call('Ai', 'SummarizeVisual', input, frames)
  },

  spider: {
    /** 引擎可用性 + 脚本缓存概览（设置页「爬虫引擎」探测面板） */
    status: (): Promise<SpiderStatus> => call('Spider', 'Status'),
    clearCache: (): Promise<void> => call('Spider', 'ClearCache')
  }
}

export type DesktopApi = typeof api

// 与 Electron preload 的 contextBridge.exposeInMainWorld('api', api) 等价
;(window as any).api = api
