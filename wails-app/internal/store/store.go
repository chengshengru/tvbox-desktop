// Package store 极简 JSON 文件存储：内存缓存 + 防抖落盘 + 互斥锁。
// 不引入原生数据库模块，文件格式与 Electron 版 data/*.json 完全互认。
// 翻译自 src/main/services/store.ts。
//
// 作者：Shiro
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const debounce = 200 * time.Millisecond

var (
	mu     sync.Mutex
	dir    string
	memory = map[string]json.RawMessage{}
	dirty  = map[string]bool{}
	timers = map[string]*time.Timer{}
	inited = map[string]bool{} // 已从磁盘读过的名字，避免重复 IO
)

// Init 指定数据目录（exe 同级 data/）。必须在任何读写前调用。
func Init(dataDir string) {
	mu.Lock()
	defer mu.Unlock()
	dir = dataDir
	_ = os.MkdirAll(dataDir, 0o755)
}

func fileOf(name string) string {
	return filepath.Join(dir, name+".json")
}

// ReadJSON 读一个 store 文件并反序列化；不存在或损坏时返回 fallback。
func ReadJSON[T any](name string, fallback T) T {
	raw, ok := readRaw(name)
	var zero T
	if !ok {
		return fallback
	}
	if err := json.Unmarshal(raw, &zero); err != nil {
		return fallback
	}
	return zero
}

// ReadMap 读一个 store 文件为宽松 map（settings 这类需要增量合并的用它）。
func ReadMap(name string) map[string]any {
	raw, ok := readRaw(name)
	if !ok {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

func readRaw(name string) (json.RawMessage, bool) {
	mu.Lock()
	defer mu.Unlock()
	if v, ok := memory[name]; ok {
		return v, len(v) > 0
	}
	if !inited[name] {
		inited[name] = true
		data, err := os.ReadFile(fileOf(name))
		if err == nil {
			memory[name] = json.RawMessage(data)
			return data, true
		}
	}
	return nil, false
}

// WriteStore 写内存缓存并防抖落盘（对应 TS writeStore）。
func WriteStore(name string, data any) {
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return
	}
	mu.Lock()
	memory[name] = raw
	dirty[name] = true
	if t, ok := timers[name]; ok {
		t.Stop()
	}
	timers[name] = time.AfterFunc(debounce, func() { _ = Flush(name) })
	mu.Unlock()
}

// Flush 立即落盘。name 为空时落全部脏页（对应 before-quit 的 flushStore()）。
func Flush(name string) error {
	mu.Lock()
	names := []string{}
	if name != "" {
		if dirty[name] {
			names = []string{name}
		}
	} else {
		for n := range dirty {
			names = append(names, n)
		}
	}
	type job struct {
		n   string
		raw json.RawMessage
	}
	jobs := make([]job, 0, len(names))
	for _, n := range names {
		delete(dirty, n)
		if t, ok := timers[n]; ok {
			t.Stop()
			delete(timers, n)
		}
		jobs = append(jobs, job{n, memory[n]})
	}
	mu.Unlock()

	var firstErr error
	for _, j := range jobs {
		tmp := fileOf(j.n) + ".tmp"
		if err := os.WriteFile(tmp, j.raw, 0o644); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := os.Rename(tmp, fileOf(j.n)); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Path 供调试与「打开数据目录」使用。
func Path(name string) string {
	mu.Lock()
	defer mu.Unlock()
	return fileOf(name)
}

// DataDir 返回当前数据目录绝对路径。
// Spider 脚本产物缓存（data/spiders/）等派生存储用它定位，避免各自重复算便携目录。
func DataDir() string {
	mu.Lock()
	defer mu.Unlock()
	return dir
}
