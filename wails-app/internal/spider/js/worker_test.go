//go:build cgo

// js 引擎离线单测（P1 退出条件）：3 个离线 async 样本产出正确 JSON，
// 外加 init 状态保持、req 私网守卫透传、看门狗超时、堆上限四个安全用例。
//
// 作者：Shiro
package js

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"tvbox-desktop/internal/spider"
)

// runSample 用内联脚本建 worker 并调用一次协议方法，返回解析后的 JSON。
func runSample(t *testing.T, code, method string, params map[string]any) map[string]any {
	t.Helper()
	w, err := newWorker(context.Background(), spider.ScriptSource{Kind: spider.KindJS, Inline: code})
	if err != nil {
		t.Fatalf("建 worker 失败：%v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	res, err := w.Invoke(context.Background(), method, spider.BuildInput(method, params))
	if err != nil {
		t.Fatalf("Invoke 失败：%v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res), &out); err != nil {
		t.Fatalf("结果非 JSON：%q err=%v", res, err)
	}
	return out
}

// 样本 1：async/await + Promise（猫源主流写法）
func TestAsyncSample(t *testing.T) {
	out := runSample(t, `
async function main(input) {
  const j = JSON.parse(input)
  await Promise.resolve()
  return JSON.stringify({ class: [], list: [{ vod_id: '1', vod_name: '测试_' + j.wd }] })
}`, "search", map[string]any{"wd": "狼人", "quick": 1})
	list, _ := out["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("list 长度异常：%v", out["list"])
	}
	if got := list[0].(map[string]any)["vod_name"]; got != "测试_狼人" {
		t.Errorf("vod_name 错误：%v", got)
	}
}

// 样本 2：pdfh/pd 宿主抽取函数
func TestPdfhSample(t *testing.T) {
	out := runSample(t, `
function main(input) {
  const j = JSON.parse(input)
  const html = '<ul><li class="v">A</li><li class="v">B</li></ul>'
  return JSON.stringify({ t: j.type, first: pdfh(html, '.v&&Text'), all: pd(html, '.v&&Text') })
}`, "category", map[string]any{"t": "电影"})
	if out["t"] != "cateList" {
		t.Errorf("input.type 编组错误：%v", out["t"])
	}
	if out["first"] != "A" || out["all"] != "A*a*B" {
		t.Errorf("pdfh/pd 结果错误：%v / %v", out["first"], out["all"])
	}
}

// 样本 3：加解密/压缩/编码族
func TestCryptoSample(t *testing.T) {
	out := runSample(t, `
async function main() {
  const t = 'hello'
  const enc = aesEncode(t, '1234567890123456', '1234567890123456', 'cbc')
  return JSON.stringify({
    md5: md5(t),
    back: base64Decode(base64Encode('中文')),
    aes: aesDecode(enc, '1234567890123456', '1234567890123456', 'cbc'),
    gz: gzipDecode(gzipEncode(t)),
    ue: urlEncode('a b&c'),
    proxy: getProxyUrl('http://x/m3u8?a=1')
  })
}`, "search", nil)
	if out["md5"] != "5d41402abc4b2a76b9719d911017c592" {
		t.Errorf("md5 错误：%v", out["md5"])
	}
	if out["back"] != "中文" || out["aes"] != "hello" || out["gz"] != "hello" {
		t.Errorf("往返错误：%v", out)
	}
	if out["ue"] != "a%20b%26c" {
		t.Errorf("urlEncode 错误：%v", out["ue"])
	}
	if out["proxy"] != "/media?u=http%3A%2F%2Fx%2Fm3u8%3Fa%3D1" {
		t.Errorf("getProxyUrl 错误：%v", out["proxy"])
	}
}

// init 状态跨调用保持（worker 复用 + 老式 init() 全局函数兼容）
func TestInitStateKept(t *testing.T) {
	w, err := newWorker(context.Background(), spider.ScriptSource{Kind: spider.KindJS, Inline: `
var STORE = ''
function init(ext, group) { STORE = ext + '|' + group }
async function main(input) {
  const j = JSON.parse(input)
  if (j.type === 'init') { init(j.ext, j.group); return '{}' }
  return JSON.stringify({ store: STORE })
}`})
	if err != nil {
		t.Fatalf("建 worker 失败：%v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	if err := w.Init(context.Background(), `{"api":"x"}`, "grp"); err != nil {
		t.Fatalf("Init 失败：%v", err)
	}
	res, err := w.Invoke(context.Background(), "search", spider.BuildInput("search", map[string]any{"wd": "a"}))
	if err != nil {
		t.Fatalf("Invoke 失败：%v", err)
	}
	if !strings.Contains(res, `{"store":"{\"api\":\"x\"}|grp"}`) {
		t.Errorf("init 状态未保持：%s", res)
	}
}

// req 守卫透传：私网地址在宿主侧被拒，脚本能拿到异常
func TestReqGuardPropagates(t *testing.T) {
	out := runSample(t, `
async function main() {
  try { req('http://127.0.0.1:9/x'); return JSON.stringify({ blocked: false }) }
  catch (e) { return JSON.stringify({ blocked: true, msg: String(e.message || e) }) }
}`, "search", nil)
	if out["blocked"] != true {
		t.Errorf("期望 req 私网被拒：%v", out)
	}
}

// 看门狗：死循环脚本必须在超时后被掐停，而不是挂死进程
func TestWatchdogInterrupt(t *testing.T) {
	w, err := newWorker(context.Background(), spider.ScriptSource{Kind: spider.KindJS, Inline: `
function main() { while (true) {} }`})
	if err != nil {
		t.Fatalf("建 worker 失败：%v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := w.Invoke(ctx, "search", spider.BuildInput("search", nil)); err == nil {
		t.Fatal("期望死循环被看门狗掐停")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("掐停耗时过长：%v", d)
	}
}

// 堆上限：超 64MB 分配应报内存异常而非耗尽宿主
func TestMemoryLimit(t *testing.T) {
	w, err := newWorker(context.Background(), spider.ScriptSource{Kind: spider.KindJS, Inline: `
function main() {
  const chunks = []
  while (true) { chunks.push(new Array(100000).fill('x')) }
}`})
	if err != nil {
		t.Fatalf("建 worker 失败：%v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := w.Invoke(ctx, "search", spider.BuildInput("search", nil)); err == nil {
		t.Fatal("期望触发内存上限错误")
	}
}

// htmlParse：cheerio 风格子集（select/eq/first/attr/text）
func TestHtmlParseSample(t *testing.T) {
	out := runSample(t, `
function main() {
  const html = '<ul><li class="v" data-id="7"><b>片A</b></li><li class="v" data-id="8">片B</li></ul>'
  const items = htmlParse(html).select('.v')
  return JSON.stringify({
    n: items.length,
    first: items.eq(0).text(),
    id: items.eq(1).attr('data-id'),
    deep: htmlParse(html).select('ul').first().select('.v').eq(1).text(),
    empty: items.eq(9).text()
  })
}`, "search", nil)
	if out["n"].(float64) != 2 {
		t.Fatalf("select 数量错误：%v", out)
	}
	if out["first"] != "片A" || out["id"] != "8" || out["deep"] != "片B" {
		t.Errorf("htmlParse 结果错误：%v", out)
	}
	if out["empty"] != "" {
		t.Errorf("越界 eq 应为空串：%v", out["empty"])
	}
}

// require shim：catExtUrl 子集可用，未知模块明确报错
func TestRequireShim(t *testing.T) {
	out := runSample(t, `
function main() {
  const ext = require('catExtUrl')
  let msg = ''
  try { require('fs') } catch (e) { msg = String(e.message || e) }
  return JSON.stringify({ u: ext.getProxyUrl('http://a/b.ts'), hasReq: typeof ext.req === 'function', err: msg })
}`, "search", nil)
	if out["u"] != "/media?u=http%3A%2F%2Fa%2Fb.ts" {
		t.Errorf("catExtUrl.getProxyUrl 错误：%v", out["u"])
	}
	if out["hasReq"] != true {
		t.Errorf("catExtUrl.req 未透出：%v", out)
	}
	if !strings.Contains(out["err"].(string), "不支持的 require 模块") {
		t.Errorf("未知模块应明确报错：%v", out["err"])
	}
}

// RSA：脚本侧 rsaEncode/rsaDecode 双 padding 往返（密钥经 input 注入，避免内嵌转义）
func TestRsaSample(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	privPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(priv)}))

	out := runSample(t, `
async function main(input) {
  const j = JSON.parse(input)
  const enc = rsaEncode(j.text, j.pub, 'pkcs1')
  const enc2 = rsaEncode(j.text, j.pub, 'oaep')
  return JSON.stringify({ dec: rsaDecode(enc, j.priv, 'pkcs1'), ok: rsaDecode(enc2, j.priv, 'oaep') === j.text })
}`, "search", map[string]any{"text": "rsa-js", "pub": pubPEM, "priv": privPEM})
	if out["dec"] != "rsa-js" || out["ok"] != true {
		t.Errorf("脚本侧 RSA 往返错误：%v", out)
	}
}
