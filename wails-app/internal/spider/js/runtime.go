//go:build cgo

// runtime.go：QuickJS 脚本运行时（设计文档 §6.4）。
// 每个 worker 独立 Runtime+Context：堆上限取设置项（默认 64MB），
// 中断看门狗在每次调用前设 deadline，超时由 QuickJS 中断回调掐停脚本。
// 调用方（worker 池）保证同一 worker 串行使用，因此内部不加锁。
//
// 作者：Shiro
package js

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	qjs "github.com/buke/quickjs-go"

	"tvbox-desktop/internal/settings"
	"tvbox-desktop/internal/spider"
	"tvbox-desktop/internal/spider/bridge"
)

//go:embed host.js
var hostJS string

// worker 一个 QuickJS 脚本运行时，实现 spider.Engine。
type worker struct {
	rt  *qjs.Runtime
	ctx *qjs.Context

	// source 脚本来源标识（日志前缀）
	source string
	// store htmlParse 元素句柄表，随 worker 释放
	store *bridge.HTMLStore
	// deadline 本次调用的中断期限（看门狗）
	deadline time.Time
	// callCtx 当前调用上下文，透传给 bridge.req 做取消
	callCtx context.Context
}

// Register 把 JS 引擎工厂注册进 Spider worker 池。
// CGO_ENABLED=0 构建走 stub.go 的空实现，spider.Invoke 返回 ErrNoEngine 降级。
func Register() {
	spider.RegisterFactory(spider.KindJS, newWorker)
}

// newWorker 建运行时 → 注入宿主函数 → 加载 host.js → 加载脚本体。
func newWorker(_ context.Context, src spider.ScriptSource) (spider.Engine, error) {
	memMB := settings.Get().Spider.MemoryLimitMB
	if memMB <= 0 {
		memMB = 64
	}
	rt := qjs.NewRuntime(qjs.WithMemoryLimit(uint64(memMB) << 20))
	w := &worker{
		rt:      rt,
		ctx:     rt.NewContext(),
		source:  srcLabel(src),
		store:   bridge.NewHTMLStore(),
		callCtx: context.Background(),
	}
	// 中断看门狗：超过 deadline 返回非 0，QuickJS 在字节码循环里掐停
	w.deadline = time.Now().Add(30 * time.Second)
	rt.SetInterruptHandler(func() int {
		if time.Now().After(w.deadline) {
			return 1
		}
		return 0
	})

	w.inject()
	if err := w.eval(hostJS, "host.js"); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("宿主兼容层加载失败：%w", err)
	}

	code := src.Inline
	if code == "" {
		b, err := os.ReadFile(src.Path)
		if err != nil {
			_ = w.Close()
			return nil, fmt.Errorf("读取脚本缓存失败：%w", err)
		}
		code = string(b)
	}
	if err := w.eval(code, "spider.js"); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("脚本加载失败：%w", err)
	}
	return w, nil
}

// srcLabel 日志用的脚本来源标识：缓存文件名或 inline。
func srcLabel(src spider.ScriptSource) string {
	if src.Path != "" {
		return filepath.Base(src.Path)
	}
	return "inline"
}

// argStr 宽松取第 i 个参数的字符串形态。
func argStr(args []*qjs.Value, i int) string {
	if i < len(args) && args[i] != nil {
		return args[i].String()
	}
	return ""
}

// setGlobalFunc 注入一个返回字符串的宿主全局函数。
// 注意：Globals().Set 会消费函数值的引用（所有权转移），之后绝不能 Free，
// 否则 JS_FreeRuntime 阶段双重释放直接崩溃（quickjs-go 所有权约定）。
func (w *worker) setGlobalFunc(name string, fn func(args []*qjs.Value) string) {
	f := w.ctx.NewFunction(func(c *qjs.Context, this *qjs.Value, args []*qjs.Value) *qjs.Value {
		return c.NewString(fn(args))
	})
	w.ctx.Globals().Set(name, f)
}

// okJSON / errJSON 信封型宿主函数返回值（host.js unwrapV 解包）。
func okJSON(v string) string {
	b, _ := json.Marshal(map[string]any{"ok": true, "v": v})
	return string(b)
}

func errJSON(err error) string {
	b, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
	return string(b)
}

// inject 注入全部 __go_* 桥函数（与 host.js 一一对应）。
func (w *worker) inject() {
	// req 返回值本身是 JSON 对象，失败用 {ok:false,error} 信封（host 侧统一判 ok）
	w.setGlobalFunc("__go_req", func(args []*qjs.Value) string {
		res, err := bridge.Req(w.callCtx, argStr(args, 0), argStr(args, 1))
		if err != nil {
			return errJSON(err)
		}
		return res
	})
	w.setGlobalFunc("__go_pdfh", func(args []*qjs.Value) string {
		return bridge.Pdfh(argStr(args, 0), argStr(args, 1))
	})
	w.setGlobalFunc("__go_pd", func(args []*qjs.Value) string {
		return bridge.Pd(argStr(args, 0), argStr(args, 1))
	})

	w.setGlobalFunc("__go_md5", func(args []*qjs.Value) string { return bridge.MD5(argStr(args, 0)) })
	w.setGlobalFunc("__go_sha1", func(args []*qjs.Value) string { return bridge.SHA1(argStr(args, 0)) })
	w.setGlobalFunc("__go_sha256", func(args []*qjs.Value) string { return bridge.SHA256(argStr(args, 0)) })
	w.setGlobalFunc("__go_b64enc", func(args []*qjs.Value) string { return bridge.Base64Encode(argStr(args, 0)) })
	w.setGlobalFunc("__go_b64dec", func(args []*qjs.Value) string { return bridge.Base64Decode(argStr(args, 0)) })
	w.setGlobalFunc("__go_hexenc", func(args []*qjs.Value) string { return bridge.HexEncode(argStr(args, 0)) })
	w.setGlobalFunc("__go_hexdec", func(args []*qjs.Value) string { return bridge.HexDecode(argStr(args, 0)) })

	w.setGlobalFunc("__go_aes_enc", func(args []*qjs.Value) string {
		v, err := bridge.AesEncode(argStr(args, 0), argStr(args, 1), argStr(args, 2), argStr(args, 3))
		if err != nil {
			return errJSON(err)
		}
		return okJSON(v)
	})
	w.setGlobalFunc("__go_aes_dec", func(args []*qjs.Value) string {
		v, err := bridge.AesDecode(argStr(args, 0), argStr(args, 1), argStr(args, 2), argStr(args, 3))
		if err != nil {
			return errJSON(err)
		}
		return okJSON(v)
	})
	w.setGlobalFunc("__go_des_enc", func(args []*qjs.Value) string {
		v, err := bridge.DesEncode(argStr(args, 0), argStr(args, 1), argStr(args, 2), argStr(args, 3))
		if err != nil {
			return errJSON(err)
		}
		return okJSON(v)
	})
	w.setGlobalFunc("__go_des_dec", func(args []*qjs.Value) string {
		v, err := bridge.DesDecode(argStr(args, 0), argStr(args, 1), argStr(args, 2), argStr(args, 3))
		if err != nil {
			return errJSON(err)
		}
		return okJSON(v)
	})
	w.setGlobalFunc("__go_rc4", func(args []*qjs.Value) string {
		v, err := bridge.Rc4(argStr(args, 0), argStr(args, 1))
		if err != nil {
			return errJSON(err)
		}
		return okJSON(v)
	})
	w.setGlobalFunc("__go_rsa_enc", func(args []*qjs.Value) string {
		v, err := bridge.RsaEncode(argStr(args, 0), argStr(args, 1), argStr(args, 2))
		if err != nil {
			return errJSON(err)
		}
		return okJSON(v)
	})
	w.setGlobalFunc("__go_rsa_dec", func(args []*qjs.Value) string {
		v, err := bridge.RsaDecode(argStr(args, 0), argStr(args, 1), argStr(args, 2))
		if err != nil {
			return errJSON(err)
		}
		return okJSON(v)
	})

	w.setGlobalFunc("__go_gzip_enc", func(args []*qjs.Value) string { return bridge.GzipEncode(argStr(args, 0)) })
	w.setGlobalFunc("__go_gzip_dec", func(args []*qjs.Value) string { return bridge.GzipDecode(argStr(args, 0)) })
	w.setGlobalFunc("__go_zlib_enc", func(args []*qjs.Value) string { return bridge.ZlibEncode(argStr(args, 0)) })
	w.setGlobalFunc("__go_zlib_dec", func(args []*qjs.Value) string { return bridge.ZlibDecode(argStr(args, 0)) })
	w.setGlobalFunc("__go_urlenc", func(args []*qjs.Value) string { return bridge.UrlEncode(argStr(args, 0)) })
	w.setGlobalFunc("__go_urldec", func(args []*qjs.Value) string { return bridge.UrlDecode(argStr(args, 0)) })

	w.setGlobalFunc("__go_log", func(args []*qjs.Value) string {
		bridge.Log(w.source, argStr(args, 0))
		return ""
	})

	// htmlParse 句柄族：ids 以 JSON 数组串进出，避免在 JS/C 边界搬运对象
	w.setGlobalFunc("__go_html_open", func(args []*qjs.Value) string {
		return strconv.Itoa(w.store.Open(argStr(args, 0)))
	})
	w.setGlobalFunc("__go_html_find", func(args []*qjs.Value) string {
		return idsJSON(w.store.Find(idsFromJSON(argStr(args, 0)), argStr(args, 1)))
	})
	w.setGlobalFunc("__go_html_text", func(args []*qjs.Value) string {
		return w.store.Text(idFromStr(argStr(args, 0)))
	})
	w.setGlobalFunc("__go_html_html", func(args []*qjs.Value) string {
		return w.store.HTML(idFromStr(argStr(args, 0)))
	})
	w.setGlobalFunc("__go_html_attr", func(args []*qjs.Value) string {
		return w.store.Attr(idFromStr(argStr(args, 0)), argStr(args, 1))
	})
}

// idFromStr 句柄数字符串解析；非法返回 0（store 侧表现为空集合）。
func idFromStr(s string) int {
	id, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return id
}

// idsFromJSON / idsJSON 句柄数组与 JSON 串的互转（host.js wrapSel 使用）。
func idsFromJSON(s string) []int {
	var ids []int
	_ = json.Unmarshal([]byte(s), &ids)
	return ids
}

func idsJSON(ids []int) string {
	b, err := json.Marshal(ids)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// eval 加载一段 JS（非 await），异常转 Go error。
func (w *worker) eval(code, name string) error {
	v := w.ctx.Eval(code, qjs.EvalFlagGlobal(true), qjs.EvalFileName(name))
	if v == nil {
		return errors.New("JS 求值返回空（运行时已失效）")
	}
	defer v.Free()
	if v.IsException() {
		if err := w.ctx.Exception(); err != nil {
			return fmt.Errorf("%s：%w", name, err)
		}
		return fmt.Errorf("%s：JS 异常", name)
	}
	return nil
}

// Init 执行脚本 init：走统一调用面 main({type:'init',ext,group})。
func (w *worker) Init(ctx context.Context, ext, group string) error {
	_, err := w.Invoke(ctx, "init", spider.BuildInput("init", map[string]any{"ext": ext, "group": group}))
	return err
}

// Invoke 调用 main(input)，EvalAwait 驱动 async/await 直到拿到结果或被看门狗掐停。
func (w *worker) Invoke(ctx context.Context, method string, params map[string]any) (string, error) {
	input, err := json.Marshal(params)
	if err != nil {
		return "", err
	}
	w.callCtx = ctx
	w.deadline = time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok {
		w.deadline = d
	}

	s := w.ctx.NewString(string(input))
	w.ctx.Globals().Set("__spider_input", s) // Set 消费引用，不可 Free
	v := w.ctx.Eval(`__spider_wrap(__spider_input)`,
		qjs.EvalFlagGlobal(true), qjs.EvalAwait(true), qjs.EvalFileName("invoke:"+method))
	if v == nil {
		return "", errors.New("JS 执行环境异常（运行时已失效）")
	}
	defer v.Free()
	if v.IsException() {
		if e := w.ctx.Exception(); e != nil {
			return "", e
		}
		return "", errors.New("脚本执行异常")
	}
	return v.String(), nil
}

// Close 释放上下文与运行时（QuickJS 上下文销毁即释放全部内存），句柄表一并清空。
func (w *worker) Close() error {
	if w.store != nil {
		w.store.Close()
	}
	if w.ctx != nil {
		w.ctx.Close()
	}
	if w.rt != nil {
		w.rt.Close()
	}
	return nil
}
