package utils

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"hubproxy/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func setupReleaseCacheTest(t *testing.T, tomlExtra string, rt roundTripFunc) string {
	t.Helper()

	cacheDir := filepath.Join(t.TempDir(), "release-cache")
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cfgBody := fmt.Sprintf(`
[releaseCache]
enabled = true
dir = %q
%s
`, cacheDir, tomlExtra)

	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", cfgPath)
	if err := config.LoadConfig(); err != nil {
		t.Fatal(err)
	}

	oldGlobal := globalHTTPClient
	oldProbe := releaseProbeHTTPClient
	t.Cleanup(func() {
		globalHTTPClient = oldGlobal
		releaseProbeHTTPClient = oldProbe
	})

	globalHTTPClient = &http.Client{Transport: rt}
	releaseProbeHTTPClient = &http.Client{
		Transport: rt,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	if err := InitReleaseCache(); err != nil {
		t.Fatal(err)
	}
	return cacheDir
}

func performReleaseGet(t *testing.T, targetURL string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodGet, "/"+targetURL, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c.Request = req

	target, ok := ParseGitHubReleaseURL(targetURL)
	if !ok {
		t.Fatalf("ParseGitHubReleaseURL(%q) returned false", targetURL)
	}
	blocked := map[string]bool{"text/html": true}
	handled := GetReleaseCache().ServeReleaseDownload(c, target, targetURL, blocked)
	if !handled {
		// 模拟回落到原透传逻辑
		resp, err := GetGlobalHTTPClient().Get(targetURL)
		if err != nil {
			c.String(http.StatusBadGateway, err.Error())
			return w
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				c.Header(k, v)
			}
		}
		c.Status(resp.StatusCode)
		_, _ = io.Copy(c.Writer, resp.Body)
	}
	return w
}

func TestParseGitHubReleaseURL(t *testing.T) {
	tests := []struct {
		url      string
		wantOK   bool
		wantTag  string
		wantFile string
		isLatest bool
	}{
		{"https://github.com/Owner/Repo/releases/download/v1.0.0/app.tar.gz", true, "v1.0.0", "app.tar.gz", false},
		{"https://github.com/owner/repo/releases/download/cli/v1.2.3/app.zip", true, "cli/v1.2.3", "app.zip", false},
		{"https://github.com/owner/repo/releases/latest/download/app.tar.gz", true, "", "app.tar.gz", true},
		{"https://github.com/owner/repo/releases/download/v1.0.0/install.sh", false, "", "", false},
		{"https://github.com/owner/repo/releases/download/v1.0.0/install.PS1", false, "", "", false},
		{"https://github.com/owner/repo/releases/tag/v1.0.0", false, "", "", false},
		{"https://github.com/owner/repo/releases/download/../evil/app.tar.gz", false, "", "", false},
		{"https://example.com/owner/repo/releases/download/v1.0.0/app.tar.gz", false, "", "", false},
	}

	for _, tt := range tests {
		got, ok := ParseGitHubReleaseURL(tt.url)
		if ok != tt.wantOK {
			t.Fatalf("ParseGitHubReleaseURL(%q) ok=%v, want %v", tt.url, ok, tt.wantOK)
		}
		if !ok {
			continue
		}
		if got.Tag != tt.wantTag || got.Asset != tt.wantFile || got.IsLatest != tt.isLatest {
			t.Fatalf("ParseGitHubReleaseURL(%q) = %+v", tt.url, got)
		}
	}
}

func TestCompareSemVer(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.2.0", "v1.1.9", 1},
		{"v1.2.0", "1.2.0", 0},
		{"v1.2.0", "v1.2.0-rc.1", 1},
		{"v1.2.0-rc.2", "v1.2.0-rc.1", 1},
		{"v1.2.0-1", "v1.2.0-alpha", -1},
		{"v2.0", "v1.9.9", 1},
	}
	for _, tc := range cases {
		sa, okA := ParseSemVer(tc.a)
		sb, okB := ParseSemVer(tc.b)
		if !okA || !okB {
			t.Fatalf("ParseSemVer failed for %q / %q", tc.a, tc.b)
		}
		if got := CompareSemVer(sa, sb); got != tc.want {
			t.Fatalf("CompareSemVer(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestReleaseCacheHitMissRangeAndSemVerEviction(t *testing.T) {
	var upstreamGets atomic.Int32
	assets := map[string]string{
		"/owner/repo/releases/download/v0.9.0/app.bin": "payload-0.9.0",
		"/owner/repo/releases/download/v1.0.0/app.bin": "payload-1.0.0",
		"/owner/repo/releases/download/v1.1.0/app.bin": "payload-1.1.0",
		"/owner/repo/releases/download/v1.2.0/app.bin": "payload-1.2.0",
	}

	setupReleaseCacheTest(t, `
maxVersions = 2
maxTotalSize = "100MB"
maxFileSize = "10MB"
`, func(req *http.Request) (*http.Response, error) {
		upstreamGets.Add(1)
		body, ok := assets[req.URL.Path]
		if !ok {
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("not found")),
			}, nil
		}
		h := make(http.Header)
		h.Set("Content-Type", "application/octet-stream")
		return &http.Response{
			StatusCode:    http.StatusOK,
			ContentLength: int64(len(body)),
			Header:        h,
			Body:          io.NopCloser(strings.NewReader(body)),
		}, nil
	})

	u100 := "https://github.com/owner/repo/releases/download/v1.0.0/app.bin"
	w := performReleaseGet(t, u100, nil)
	if w.Code != http.StatusOK || w.Body.String() != "payload-1.0.0" || w.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("v1.0.0 first req: code=%d cache=%q body=%q", w.Code, w.Header().Get("X-Cache"), w.Body.String())
	}

	// 第二次请求应命中本地且不回源，并支持 Range 206
	w = performReleaseGet(t, u100, map[string]string{"Range": "bytes=0-6"})
	if w.Code != http.StatusPartialContent || w.Body.String() != "payload" || w.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("v1.0.0 range hit: code=%d cache=%q body=%q", w.Code, w.Header().Get("X-Cache"), w.Body.String())
	}
	if upstreamGets.Load() != 1 {
		t.Fatalf("upstreamGets = %d, want 1", upstreamGets.Load())
	}

	// 拉取 v1.1.0 和 v1.2.0（maxVersions=2 -> 淘汰 v1.0.0）
	_ = performReleaseGet(t, "https://github.com/owner/repo/releases/download/v1.1.0/app.bin", nil)
	_ = performReleaseGet(t, "https://github.com/owner/repo/releases/download/v1.2.0/app.bin", nil)

	mgr := GetReleaseCache()
	if _, _, ok := mgr.OpenCachedFile(&ReleaseTarget{Owner: "owner", Repo: "repo", Tag: "v1.0.0", Asset: "app.bin"}); ok {
		t.Fatalf("v1.0.0 should have been evicted")
	}
	if _, _, ok := mgr.OpenCachedFile(&ReleaseTarget{Owner: "owner", Repo: "repo", Tag: "v1.1.0", Asset: "app.bin"}); !ok {
		t.Fatalf("v1.1.0 should remain cached")
	}
	if _, _, ok := mgr.OpenCachedFile(&ReleaseTarget{Owner: "owner", Repo: "repo", Tag: "v1.2.0", Asset: "app.bin"}); !ok {
		t.Fatalf("v1.2.0 should remain cached")
	}

	// 请求历史老版本 v0.9.0：应 BYPASS 透传且不挤占 v1.1.0/v1.2.0
	wOld := performReleaseGet(t, "https://github.com/owner/repo/releases/download/v0.9.0/app.bin", nil)
	if wOld.Code != http.StatusOK || wOld.Body.String() != "payload-0.9.0" || wOld.Header().Get("X-Cache") != "BYPASS" {
		t.Fatalf("v0.9.0 req: code=%d cache=%q body=%q", wOld.Code, wOld.Header().Get("X-Cache"), wOld.Body.String())
	}
	if _, _, ok := mgr.OpenCachedFile(&ReleaseTarget{Owner: "owner", Repo: "repo", Tag: "v1.1.0", Asset: "app.bin"}); !ok {
		t.Fatalf("v1.1.0 must not be evicted by older v0.9.0")
	}
}

func TestReleaseCacheLatestResolutionAndTTL(t *testing.T) {
	var currentLatest atomic.Value
	currentLatest.Store("v1.0.0")
	var probeCount atomic.Int32
	var downloadCount atomic.Int32

	setupReleaseCacheTest(t, `
maxVersions = 2
latestTTL = "1h"
`, func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/owner/repo/releases/latest/download/app.bin" {
			probeCount.Add(1)
			h := make(http.Header)
			h.Set("Location", "https://github.com/owner/repo/releases/download/"+currentLatest.Load().(string)+"/app.bin")
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     h,
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		}
		if req.URL.Path == "/owner/repo/releases/download/v1.0.0/app.bin" {
			downloadCount.Add(1)
			body := "v1-data"
			return &http.Response{
				StatusCode:    http.StatusOK,
				ContentLength: int64(len(body)),
				Header:        http.Header{"Content-Type": []string{"application/octet-stream"}},
				Body:          io.NopCloser(strings.NewReader(body)),
			}, nil
		}
		if req.URL.Path == "/owner/repo/releases/download/v1.1.0/app.bin" {
			downloadCount.Add(1)
			body := "v2-data"
			return &http.Response{
				StatusCode:    http.StatusOK,
				ContentLength: int64(len(body)),
				Header:        http.Header{"Content-Type": []string{"application/octet-stream"}},
				Body:          io.NopCloser(strings.NewReader(body)),
			}, nil
		}
		return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})

	// 先通过明确版本缓存 v1.0.0
	w1 := performReleaseGet(t, "https://github.com/owner/repo/releases/download/v1.0.0/app.bin", nil)
	if w1.Body.String() != "v1-data" || downloadCount.Load() != 1 {
		t.Fatalf("unexpected v1.0.0 setup: %q downloads=%d", w1.Body.String(), downloadCount.Load())
	}

	// 请求 latest：应只做 1 次 302 探测，并直接命中已缓存的 v1.0.0（downloadCount 不增加！）
	w2 := performReleaseGet(t, "https://github.com/owner/repo/releases/latest/download/app.bin", nil)
	if w2.Body.String() != "v1-data" || w2.Header().Get("X-Cache") != "HIT" || w2.Header().Get("X-Cache-Tag") != "v1.0.0" {
		t.Fatalf("latest hit failed: cache=%q tag=%q body=%q", w2.Header().Get("X-Cache"), w2.Header().Get("X-Cache-Tag"), w2.Body.String())
	}
	if probeCount.Load() != 1 || downloadCount.Load() != 1 {
		t.Fatalf("probe=%d download=%d, want 1/1", probeCount.Load(), downloadCount.Load())
	}

	// TTL 内再次请求 latest：probeCount 仍为 1
	w3 := performReleaseGet(t, "https://github.com/owner/repo/releases/latest/download/app.bin", nil)
	if w3.Header().Get("X-Cache") != "HIT" || probeCount.Load() != 1 {
		t.Fatalf("latest TTL hit failed: probe=%d", probeCount.Load())
	}

	// 上游发布 v1.1.0 并模拟 TTL 过期：应探测到新版本并落盘
	currentLatest.Store("v1.1.0")
	mgr := GetReleaseCache()
	mgr.mu.Lock()
	lm := mgr.latestMap["owner/repo"]
	lm.CheckedAt = time.Now().Add(-2 * time.Hour)
	mgr.latestMap["owner/repo"] = lm
	mgr.mu.Unlock()

	w4 := performReleaseGet(t, "https://github.com/owner/repo/releases/latest/download/app.bin", nil)
	if w4.Body.String() != "v2-data" || w4.Header().Get("X-Cache") != "MISS" || w4.Header().Get("X-Cache-Tag") != "v1.1.0" {
		t.Fatalf("latest refresh failed: cache=%q tag=%q body=%q", w4.Header().Get("X-Cache"), w4.Header().Get("X-Cache-Tag"), w4.Body.String())
	}
	if probeCount.Load() != 2 || downloadCount.Load() != 2 {
		t.Fatalf("probe=%d download=%d, want 2/2", probeCount.Load(), downloadCount.Load())
	}
}

func TestReleaseCacheConcurrentMissSingleUpstreamAndRestartRecovery(t *testing.T) {
	var upstreamGets atomic.Int32
	payload := bytes.Repeat([]byte("ABCDEFGH"), 8192) // 64KB

	cacheDir := setupReleaseCacheTest(t, `
maxVersions = 2
`, func(req *http.Request) (*http.Response, error) {
		upstreamGets.Add(1)
		time.Sleep(40 * time.Millisecond) // 制造并发窗口
		return &http.Response{
			StatusCode:    http.StatusOK,
			ContentLength: int64(len(payload)),
			Header:        http.Header{"Content-Type": []string{"application/octet-stream"}},
			Body:          io.NopCloser(bytes.NewReader(payload)),
		}, nil
	})

	const workers = 6
	var wg sync.WaitGroup
	results := make([]*httptest.ResponseRecorder, workers)
	urlStr := "https://github.com/owner/repo/releases/download/v1.0.0/big.bin"

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = performReleaseGet(t, urlStr, nil)
		}(i)
	}
	wg.Wait()

	if upstreamGets.Load() != 1 {
		t.Fatalf("concurrent downloads hit upstream %d times, want 1", upstreamGets.Load())
	}
	for i, w := range results {
		if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), payload) {
			t.Fatalf("worker %d failed: code=%d len=%d", i, w.Code, w.Body.Len())
		}
	}

	// 写入一个残留 .tmp- 文件，验证重启 InitReleaseCache 会清理残留并恢复可用索引
	staleTmp := filepath.Join(cacheDir, "owner", "repo", releaseTagsDirName, sanitizeTagDir("v1.0.0"), ".tmp-orphan")
	if err := os.WriteFile(staleTmp, []byte("junk"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := InitReleaseCache(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staleTmp); !os.IsNotExist(err) {
		t.Fatalf("expected stale tmp file to be removed on restart")
	}

	wAfter := performReleaseGet(t, urlStr, nil)
	if wAfter.Header().Get("X-Cache") != "HIT" || !bytes.Equal(wAfter.Body.Bytes(), payload) {
		t.Fatalf("restart recovery failed: cache=%q len=%d", wAfter.Header().Get("X-Cache"), wAfter.Body.Len())
	}
}

func TestReleaseCacheGlobalSizeEvictionAndRepoFilter(t *testing.T) {
	setupReleaseCacheTest(t, `
maxVersions = 5
maxTotalSize = "25B"
whiteList = ["allowed/*"]
blackList = ["allowed/blocked"]
`, func(req *http.Request) (*http.Response, error) {
		body := strings.Repeat("x", 12)
		return &http.Response{
			StatusCode:    http.StatusOK,
			ContentLength: int64(len(body)),
			Header:        http.Header{"Content-Type": []string{"application/octet-stream"}},
			Body:          io.NopCloser(strings.NewReader(body)),
		}, nil
	})

	// 被黑名单或不在白名单的仓库必须 BYPASS
	wDenied := performReleaseGet(t, "https://github.com/other/repo/releases/download/v1.0.0/a.bin", nil)
	if wDenied.Header().Get("X-Cache") != "BYPASS" {
		t.Fatalf("non-whitelisted repo cache=%q, want BYPASS", wDenied.Header().Get("X-Cache"))
	}
	wBlocked := performReleaseGet(t, "https://github.com/allowed/blocked/releases/download/v1.0.0/a.bin", nil)
	if wBlocked.Header().Get("X-Cache") != "BYPASS" {
		t.Fatalf("blacklisted repo cache=%q, want BYPASS", wBlocked.Header().Get("X-Cache"))
	}

	// 依次缓存 3 个各 12B 文件（总 36B > maxTotalSize 25B），应触发全局 LRU 淘汰最老的 v1.0.0
	_ = performReleaseGet(t, "https://github.com/allowed/repo1/releases/download/v1.0.0/a.bin", nil)
	time.Sleep(5 * time.Millisecond)
	_ = performReleaseGet(t, "https://github.com/allowed/repo2/releases/download/v1.0.0/a.bin", nil)
	time.Sleep(5 * time.Millisecond)
	_ = performReleaseGet(t, "https://github.com/allowed/repo3/releases/download/v1.0.0/a.bin", nil)

	mgr := GetReleaseCache()
	mgr.mu.RLock()
	total := mgr.totalSize
	mgr.mu.RUnlock()
	if total > 25 {
		t.Fatalf("totalSize = %d, want <= 25", total)
	}
	if _, _, ok := mgr.OpenCachedFile(&ReleaseTarget{Owner: "allowed", Repo: "repo1", Tag: "v1.0.0", Asset: "a.bin"}); ok {
		t.Fatalf("oldest global entry repo1/v1.0.0 should have been evicted")
	}
}
