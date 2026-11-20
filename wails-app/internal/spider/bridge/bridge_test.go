// bridge 包离线单测：req 私网守卫、加解密往返、pdfh/pd 抽取。
// 纯 Go 可跑（不依赖 CGO 引擎）。
//
// 作者：Shiro
package bridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReqRejectsPrivate(t *testing.T) {
	// 默认策略：拒绝环回/私网/内网主机名（§9 R7）
	for _, u := range []string{
		"http://127.0.0.1:18080/x",
		"http://localhost/x",
		"http://192.168.1.10/x",
		"http://10.0.0.5/x",
		"http://intranet/x",
	} {
		if _, err := Req(context.Background(), u, "{}"); err == nil {
			t.Errorf("期望拒绝访问 %s", u)
		}
	}
}

func TestReqRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"echo":"` + string(b) + `"}`))
			return
		}
		_, _ = w.Write([]byte("hello spider"))
	}))
	defer srv.Close()

	// 测试环境放行私网（httptest 在 127.0.0.1）
	AllowPrivate = true
	t.Cleanup(func() { AllowPrivate = false })

	res, err := Req(context.Background(), srv.URL+"/get", `{"method":"GET"}`)
	if err != nil {
		t.Fatalf("GET 失败：%v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(res), &m); err != nil {
		t.Fatalf("返回非 JSON：%s", res)
	}
	if m["code"].(float64) != 200 || m["body"] != "hello spider" {
		t.Fatalf("GET 结果异常：%s", res)
	}

	res, err = Req(context.Background(), srv.URL+"/post", `{"method":"post","body":"wd=测试"}`)
	if err != nil {
		t.Fatalf("POST 失败：%v", err)
	}
	if !strings.Contains(res, "wd=测试") {
		t.Fatalf("POST 回显异常：%s", res)
	}

	// binary 形态：body 置空、base64 填充
	res, err = Req(context.Background(), srv.URL+"/get", `{"binary":true}`)
	if err != nil {
		t.Fatalf("binary GET 失败：%v", err)
	}
	// 注意：Unmarshal 进已非空的 map 会合并键，binary 结果必须用全新 map 断言
	var mb map[string]any
	if err := json.Unmarshal([]byte(res), &mb); err != nil {
		t.Fatalf("binary 返回非 JSON：%s", res)
	}
	if _, hasBody := mb["body"]; hasBody {
		t.Errorf("binary 模式不应带 body：%s", res)
	}
	if mb["base64"] != "aGVsbG8gc3BpZGVy" {
		t.Errorf("base64 字段异常：%v", mb["base64"])
	}
}

func TestCryptoRoundTrip(t *testing.T) {
	if got := MD5("hello"); got != "5d41402abc4b2a76b9719d911017c592" {
		t.Errorf("MD5 错误：%s", got)
	}
	if got := Base64Decode(Base64Encode("中文测试")); got != "中文测试" {
		t.Errorf("base64 往返错误：%s", got)
	}
	if got := HexDecode(HexEncode("abc")); got != "abc" {
		t.Errorf("hex 往返错误：%s", got)
	}

	key, iv := "1234567890123456", "abcdef1234567890"
	for _, mode := range []string{"cbc", "ecb", ""} {
		enc, err := AesEncode("hello 世界", key, iv, mode)
		if err != nil {
			t.Fatalf("AesEncode(%q) 失败：%v", mode, err)
		}
		dec, err := AesDecode(enc, key, iv, mode)
		if err != nil {
			t.Fatalf("AesDecode(%q) 失败：%v", mode, err)
		}
		if dec != "hello 世界" {
			t.Errorf("AES(%q) 往返错误：%s", mode, dec)
		}
	}

	// 3DES 24 字节密钥
	enc, err := DesEncode("des-test", "123456789012345678901234", "87654321", "cbc")
	if err != nil {
		t.Fatalf("DesEncode 失败：%v", err)
	}
	if dec, err := DesDecode(enc, "123456789012345678901234", "87654321", "cbc"); err != nil || dec != "des-test" {
		t.Errorf("3DES 往返错误：%s %v", dec, err)
	}

	if _, err := AesEncode("x", "badkey", iv, "cbc"); err == nil {
		t.Error("期望非法密钥长度报错")
	}

	rc, err := Rc4("rc4", "key")
	if err != nil || rc == "" {
		t.Errorf("Rc4 异常：%v", err)
	}
}

func TestCompressAndURL(t *testing.T) {
	if got := GzipDecode(GzipEncode("gzip 往返")); got != "gzip 往返" {
		t.Errorf("gzip 往返错误：%s", got)
	}
	if got := ZlibDecode(ZlibEncode("zlib 往返")); got != "zlib 往返" {
		t.Errorf("zlib 往返错误：%s", got)
	}
	if got := UrlEncode("a b&c=中文"); got != "a%20b%26c%3D%E4%B8%AD%E6%96%87" {
		t.Errorf("UrlEncode 错误：%s", got)
	}
	if got := UrlDecode("a%20b%26c"); got != "a b&c" {
		t.Errorf("UrlDecode 错误：%s", got)
	}
}

func TestPdfhPd(t *testing.T) {
	html := `<ul><li class="v">A</li><li class="v">B</li><li class="v">C</li></ul>`
	if got := Pdfh(html, ".v&&Text"); got != "A" {
		t.Errorf("Pdfh 错误：%s", got)
	}
	if got := Pd(html, ".v&&Text"); got != "A*a*B*a*C" {
		t.Errorf("Pd 错误：%s", got)
	}
	if got := Pdfh(html, ".none&&Text"); got != "" {
		t.Errorf("Pdfh 未命中应为空：%s", got)
	}
	// || 回退分支
	if got := Pdfh(html, ".none&&Text||.v&&Text"); got != "A" {
		t.Errorf("Pdfh 回退分支错误：%s", got)
	}
}
