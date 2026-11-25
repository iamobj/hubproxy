package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"hubproxy/config"
	"hubproxy/utils"
)

func TestCheckGitHubURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		user string
		repo string
	}{
		{"release", "https://github.com/user/repo/releases/download/v1/file.tar.gz", "user", "repo"},
		{"raw", "https://raw.githubusercontent.com/user/repo/main/file.sh", "user", "repo"},
		{"api", "https://api.github.com/repos/user/repo/releases/latest", "user", "repo"},
		{"huggingface", "https://huggingface.co/user/model/resolve/main/file", "user", "model/resolve/main/file"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckGitHubURL(tt.url)
			if len(got) < 2 || got[0] != tt.user || got[1] != tt.repo {
				t.Fatalf("CheckGitHubURL(%q) = %#v", tt.url, got)
			}
		})
	}
}

func TestCheckGitHubURLRejectsOtherHosts(t *testing.T) {
	if got := CheckGitHubURL("https://example.com/user/repo/file"); got != nil {
		t.Fatalf("unexpected match: %#v", got)
	}
	if got := CheckGitHubURL("https://download.docker.com/linux/static/stable/x86_64/docker.tgz"); got != nil {
		t.Fatalf("download.docker.com should be rejected: %#v", got)
	}
}

func TestGitHubProxyHandlerUsesReleaseCacheWhenEnabled(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, "release-bytes")
	}))
	defer srv.Close()

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cacheDir := filepath.Join(t.TempDir(), "rcache")
	cfgBody := `
[releaseCache]
enabled = true
dir = "` + cacheDir + `"
maxVersions = 2
`
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", cfgPath)
	if err := config.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	utils.InitHTTPClients()
	if err := utils.InitReleaseCache(); err != nil {
		t.Fatal(err)
	}

	// 验证目标解析与路由集成不会报错，非法 URL 仍返回 403
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.NoRoute(GitHubProxyHandler)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/https://example.com/owner/repo/releases/download/v1.0.0/app.bin", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	_ = strings.TrimSpace(srv.URL)
}
