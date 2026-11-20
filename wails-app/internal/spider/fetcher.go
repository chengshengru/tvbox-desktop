// fetcher.go：脚本产物获取与缓存（设计文档 §6.3）。
// http(s) 脚本地址 → 下载（非空、<8MB）→ 缓存 data/spiders/<sha16(url)>.<ext>，
// TTL 24h 后重下载，网络失败回退过期缓存（serve-stale）；内联代码不落盘。
//
// 作者：Shiro
package spider

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tvbox-desktop/internal/httputil"
	"tvbox-desktop/internal/store"
	"tvbox-desktop/models"
)

const (
	// maxScriptSize 脚本产物大小上限（8MB，§6.3）。
	maxScriptSize = 8 << 20
	// scriptTTL 缓存有效期，过期后重新下载校验。
	scriptTTL = 24 * time.Hour
	// downloadTimeout 脚本下载单请求超时（秒）。
	downloadTimeout = 30.0
)

// CachedScript 脚本缓存清单一行（设置页展示 + Spider.Status 返回，§9.2 审计要求）。
type CachedScript struct {
	// Name 缓存文件名（含来源哈希，可反查订阅里的脚本地址）。
	Name string `json:"name"`
	// Size 字节数。
	Size int64 `json:"size"`
	// ModTime 最后写入时间（unix 毫秒）。
	ModTime int64 `json:"modTime"`
}

// sha16 来源地址的稳定短哈希（缓存文件名主体）。
func sha16(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// cacheDir Spider 产物缓存目录（data/spiders/）。
func cacheDir() string {
	return filepath.Join(store.DataDir(), "spiders")
}

// scriptURL 去掉 js:/py: 判型前缀，得到真实脚本地址。
func scriptURL(api string) string {
	a := strings.TrimSpace(api)
	a = jsPrefixRe.ReplaceAllString(a, "")
	a = pyPrefixRe.ReplaceAllString(a, "")
	return strings.TrimSpace(a)
}

// scriptExt 取 URL 路径的脚本后缀（去 query/fragment），小写；非 .js/.py 返回空。
func scriptExt(rawURL string) string {
	p := rawURL
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	p = strings.ToLower(p)
	if strings.HasSuffix(p, ".js") {
		return ".js"
	}
	if strings.HasSuffix(p, ".py") {
		return ".py"
	}
	return ""
}

// isInline 判断 api 字段是否为内联代码（以入口函数/类声明开头）。
func isInline(api string) bool {
	return inlineJSRe.MatchString(api) || inlinePYRe.MatchString(api)
}

// resolveScript 取得站点脚本来源：内联代码直接返回，http(s) 地址走下载缓存。
// api 无脚本地址时回退 jar 字段（全局脚本地址，§6.3 末行）。
func resolveScript(ctx context.Context, kind string, site models.Site) (ScriptSource, error) {
	api := strings.TrimSpace(site.API)
	if isInline(api) {
		return ScriptSource{Kind: kind, Inline: api}, nil
	}
	url := scriptURL(api)
	if !httpRe.MatchString(url) {
		url = strings.TrimSpace(site.Jar)
	}
	if !httpRe.MatchString(url) {
		return ScriptSource{}, errors.New("无法定位脚本地址（仅支持 http(s) 下载或内联代码）")
	}

	ext := scriptExt(url)
	if ext == "" {
		// 地址无后缀时按判型结果补默认扩展名
		if kind == KindPython {
			ext = ".py"
		} else {
			ext = ".js"
		}
	}
	cachePath := filepath.Join(cacheDir(), sha16(url)+ext)
	if freshFile(cachePath) {
		return ScriptSource{Kind: kind, Path: cachePath}, nil
	}

	body, err := downloadScript(ctx, url)
	if err != nil {
		// 网络失败但存在过期缓存：serve-stale，避免脚本源抖动导致整站不可用
		if fileExists(cachePath) {
			return ScriptSource{Kind: kind, Path: cachePath}, nil
		}
		return ScriptSource{}, fmt.Errorf("脚本下载失败：%w", err)
	}
	if err := saveCache(cachePath, body); err != nil {
		// 写盘失败不致命：本次以内联体直接执行
		return ScriptSource{Kind: kind, Inline: body}, nil
	}
	return ScriptSource{Kind: kind, Path: cachePath}, nil
}

// downloadScript 下载脚本并做体积/内容校验（§6.3）。
func downloadScript(ctx context.Context, url string) (string, error) {
	res, err := httputil.Request(ctx, url, httputil.Options{Timeout: downloadTimeout})
	if err != nil {
		return "", err
	}
	if !res.OK {
		return "", fmt.Errorf("HTTP %d", res.Status)
	}
	if len(res.Body) == 0 {
		return "", errors.New("脚本内容为空")
	}
	if len(res.Body) > maxScriptSize {
		return "", fmt.Errorf("脚本超过 %dMB 上限", maxScriptSize>>20)
	}
	return httputil.DecodeResponse(res, ""), nil
}

// freshFile 缓存文件存在且未过 TTL。
func freshFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		return false
	}
	return time.Since(info.ModTime()) < scriptTTL
}

// fileExists 缓存文件存在（不论是否过期）。
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}

// saveCache 原子写缓存文件（先 .tmp 再 rename，避免半截文件被复用）。
func saveCache(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ListCached 列出 data/spiders/ 脚本产物缓存清单（按文件名排序由 ReadDir 保证）。
func ListCached() []CachedScript {
	entries, err := os.ReadDir(cacheDir())
	if err != nil {
		return nil
	}
	out := make([]CachedScript, 0, len(entries))
	for _, en := range entries {
		if en.IsDir() || !hasScriptExt(en.Name()) {
			continue
		}
		info, err := en.Info()
		if err != nil {
			continue
		}
		out = append(out, CachedScript{
			Name:    en.Name(),
			Size:    info.Size(),
			ModTime: info.ModTime().UnixMilli(),
		})
	}
	return out
}

// hasScriptExt 文件名是否脚本扩展名。
func hasScriptExt(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".js") || strings.HasSuffix(lower, ".py")
}

// ClearCache 清空脚本产物缓存（运行中的 worker 不受影响，下次调用重新下载）。
func ClearCache() error {
	dir := cacheDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var firstErr error
	for _, en := range entries {
		if en.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, en.Name())); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
