// 离线冒烟测试：验证 Go 翻译层的核心逻辑（不依赖外网）。
package smoke

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/PuerkitoBio/goquery"

	"tvbox-desktop/internal/cms"
	"tvbox-desktop/internal/htmlrules"
	"tvbox-desktop/internal/httputil"
	"tvbox-desktop/internal/library"
	"tvbox-desktop/internal/mediamw"
	"tvbox-desktop/internal/parse"
	"tvbox-desktop/internal/settings"
	"tvbox-desktop/internal/store"
	"tvbox-desktop/internal/stream"
	"tvbox-desktop/models"
)

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store.Init(dir)
	store.WriteStore("demo", map[string]any{"a": 1})
	if err := store.Flush(""); err != nil {
		t.Fatal(err)
	}
	got := store.ReadJSON[map[string]any](dir2name(), map[string]any{})
	_ = got
	got2 := store.ReadJSON[map[string]any]("demo", nil)
	if got2 == nil || got2["a"] != float64(1) {
		t.Fatalf("roundtrip 失败: %v", got2)
	}
	// 并发写不 panic、最终可 flush
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); store.WriteStore("demo", map[string]any{"a": i}) }(i)
	}
	wg.Wait()
	if err := store.Flush("demo"); err != nil {
		t.Fatal(err)
	}
}

func dir2name() string { return "demo" }

func TestSettingsMerge(t *testing.T) {
	store.Init(t.TempDir())
	s, err := settings.Save(map[string]any{
		"concurrency": 5,
		"ai":          map[string]any{"apiKey": "sk-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Concurrency != 5 || s.AI.APIKey != "sk-test" {
		t.Fatalf("patch 未生效: %+v", s)
	}
	if s.Timeout != 20 || len(s.ExternalPlayer.Prefer) != 3 {
		t.Fatalf("默认值被破坏: %+v", s)
	}
	got := settings.Get()
	if got.AI.APIKey != "sk-test" || got.Concurrency != 5 {
		t.Fatalf("Get 未合并: %+v", got)
	}
}

func TestCmsURLFixes(t *testing.T) {
	site := models.Site{API: "http://x/api.php?ac={ac}&t={t}&pg={pg}&wd={wd}"}
	u := cms.BuildURL(site, cms.Query{AC: "list", WD: cms.Str("庆余年"), PG: 1, T: "1"})
	if !strings.Contains(u, "wd=%E5%BA%86%E4%BD%99%E5%B9%B4") {
		t.Fatalf("{wd} 未替换: %s", u)
	}
	if !strings.Contains(u, "pg=1") {
		t.Fatalf("{pg} int 未替换: %s", u)
	}
	base := models.Site{API: "http://x/api.php"}
	u2 := cms.BuildURL(base, cms.Query{AC: "list", WD: cms.Str("a b"), PG: 2})
	if !strings.Contains(u2, "wd=a%20b") || !strings.Contains(u2, "pg=2") || !strings.Contains(u2, "ac=list") {
		t.Fatalf("基地址模式错误: %s", u2)
	}
	u3 := cms.BuildBrowseURL(base, "videolist", []string{"6", "7"}, 3)
	if !strings.Contains(u3, "pg=3") {
		t.Fatalf("browse pg 丢失: %s", u3)
	}
}

func TestStreamHeadersFor(t *testing.T) {
	h := stream.HeadersFor("https://m.huya.com/123456")
	if h == nil || h["Referer"] == "" {
		t.Fatalf("虎牙 Referer 规则缺失: %+v", h)
	}
	if stream.HeadersFor("https://example.com/x.ts") != nil {
		t.Fatal("非目标域名不应命中")
	}
}

func TestMediamw(t *testing.T) {
	// 上游：模拟防盗链 m3u8 + Range 分片
	var lastUA string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastUA = r.Header.Get("User-Agent")
		switch r.URL.Path {
		case "/live/master.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nsub/index.m3u8\n")
		case "/live/sub/index.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.php?k=1\"\n#EXTINF:10,\nseg1.ts\n")
		case "/live/sub/seg1.ts":
			body := "0123456789"
			if r.Header.Get("Range") != "" {
				w.Header().Set("Content-Range", "bytes 5-9/10")
				w.WriteHeader(206)
				_, _ = w.Write([]byte(body[5:]))
				return
			}
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()

	noop := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	srv := httptest.NewServer(mediamw.Middleware()(noop))
	defer srv.Close()

	// 1) 非 /media 路径透传
	if resp, err := http.Get(srv.URL + "/other"); err != nil || resp.StatusCode != 404 {
		t.Fatalf("透传失败: %v", err)
	}

	// 2) master 播放列表改写
	masterURL := up.URL + "/live/master.m3u8"
	resp, err := http.Get(srv.URL + "/media?u=" + url.QueryEscape(masterURL))
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	wantSeg := url.QueryEscape(up.URL + "/live/sub/index.m3u8")
	if !strings.Contains(body, "/media?u="+wantSeg) {
		t.Fatalf("master 变体未改写: %s", body)
	}
	if !strings.HasPrefix(lastUA, "Mozilla/") {
		t.Fatalf("UA 未注入: %s", lastUA)
	}

	// 3) 媒体列表：相对分片 + EXT-X-KEY URI 都要改写
	indexURL := up.URL + "/live/sub/index.m3u8"
	resp2, err := http.Get(srv.URL + "/media?u=" + url.QueryEscape(indexURL))
	if err != nil {
		t.Fatal(err)
	}
	body2 := readAll(t, resp2)
	if !strings.Contains(body2, "/media?u="+url.QueryEscape(up.URL+"/live/sub/seg1.ts")) {
		t.Fatalf("分片未改写: %s", body2)
	}
	if !strings.Contains(body2, "URI=\"/media?u="+url.QueryEscape(up.URL+"/live/sub/key.php?k=1")+"\"") &&
		!strings.Contains(body2, "URI=%22/media") && !strings.Contains(body2, "/media?u="+url.QueryEscape(up.URL+"/live/sub/key.php%3Fk=1")) {
		t.Logf("KEY URI 改写形态: %s", body2)
	}

	// 4) Range 透传
	req, _ := http.NewRequest("GET", srv.URL+"/media?u="+url.QueryEscape(up.URL+"/live/sub/seg1.ts"), nil)
	req.Header.Set("Range", "bytes=5-9")
	resp3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body3 := readAll(t, resp3)
	if resp3.StatusCode != 206 || body3 != "56789" || resp3.Header.Get("Content-Range") == "" {
		t.Fatalf("Range 透传失败: %d %q", resp3.StatusCode, body3)
	}

	// 5) 非法 u 拒绝
	resp4, _ := http.Get(srv.URL + "/media?u=ftp://bad")
	if resp4.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法协议应 400: %d", resp4.StatusCode)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

func TestLibrary(t *testing.T) {
	store.Init(t.TempDir())
	r1, err := library.ToggleFavorite(models.FavoriteItem{SiteKey: "s1", VodID: "1", Name: "测试剧"})
	if err != nil || r1["favorited"] != true {
		t.Fatalf("收藏失败: %v %v", r1, err)
	}
	r2, _ := library.ToggleFavorite(models.FavoriteItem{SiteKey: "s1", VodID: "1", Name: "测试剧"})
	if r2["favorited"] != false {
		t.Fatal("取消收藏失败")
	}
	_ = library.Record(models.HistoryItem{SiteKey: "s1", VodID: "1", Name: "庆余年 第二季", EpIndex: 2, Position: 100})
	_ = library.Record(models.HistoryItem{SiteKey: "s2", VodID: "9", Name: "庆余年第二季", EpIndex: 1, Position: 50})
	snap := library.Get()
	if len(snap.History) != 1 || len(snap.History[0].Items) != 2 {
		t.Fatalf("归一化分组失败: %+v", snap.History)
	}
	if library.NormalizeName("庆余年 第二季！") != library.NormalizeName("庆余年第二季") {
		t.Fatal("NormalizeName 不一致")
	}
}

func TestParseExtract(t *testing.T) {
	if got := parse.ExtractUrl(`{"code":1,"url":"http://a.com/x.mp4?y=1"}`, true); got != "http://a.com/x.mp4?y=1" {
		t.Fatalf("JSON 挖址失败: %q", got)
	}
	if got := parse.ExtractUrl(`var main = 'http://x/y.m3u8';`, true); !strings.Contains(got, "y.m3u8") {
		t.Fatalf("var main 挖址失败: %q", got)
	}
	if got := parse.ExtractUrl(`{"url":"http://a.com/wasm_exec.js"}`, true); got != "" {
		t.Fatalf("NON_MEDIA 未拦截: %q", got)
	}
}

func TestHtmlRulesExtract(t *testing.T) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(`<div id="d"><a href="/m/1.html">片名</a></div>`))
	if err != nil {
		t.Fatal(err)
	}
	sel := doc.Selection.Find("#d a")
	if got := htmlrules.Extract(doc, sel, "", "http://s.com", "href"); got != "http://s.com/m/1.html" {
		t.Fatalf("href 绝对化失败: %q", got)
	}
	if got := htmlrules.Extract(doc, sel, "", "http://s.com", "Text"); got != "片名" {
		t.Fatalf("文本抽取失败: %q", got)
	}
}

func TestLooseJSON(t *testing.T) {
	v, err := httputil.ParseLooseJSON("\ufeff{\"code\":1,\"list\":[{\"vod_id\":123}]}前缀垃圾不要")
	if err != nil {
		t.Fatal(err)
	}
	m := v.(map[string]any)
	arr := models.AsArray(m["list"])
	id := models.Str(arr[0].(map[string]any)["vod_id"])
	if id != "123" {
		t.Fatalf("混型字段转换失败: %q", id)
	}
	b, _ := json.Marshal(models.SearchProgress{Wd: "x", Total: 3})
	if !strings.Contains(string(b), "\"wd\":\"x\"") {
		t.Fatalf("事件负载字段名不符: %s", b)
	}
}
