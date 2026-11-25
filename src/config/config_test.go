package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigUsesConfigPathAndEnvOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom.toml")
	data := []byte(`
[server]
host = "127.0.0.1"
port = 5999

[access]
proxy = "socks5://127.0.0.1:1080"
`)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", path)
	t.Setenv("SERVER_PORT", "6001")
	t.Setenv("ACCESS_PROXY", "")

	if err := LoadConfig(); err != nil {
		t.Fatal(err)
	}

	cfg := GetConfig()
	if cfg.Server.Host != "127.0.0.1" {
		t.Fatalf("Server.Host = %q", cfg.Server.Host)
	}
	if cfg.Server.Port != 6001 {
		t.Fatalf("Server.Port = %d, want 6001", cfg.Server.Port)
	}
	if cfg.Access.Proxy != "" {
		t.Fatalf("Access.Proxy = %q, want empty override", cfg.Access.Proxy)
	}
}

func TestReleaseCacheConfigAndEnvOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release-cache.toml")
	data := []byte(`
[releaseCache]
enabled = true
dir = "/tmp/rcache"
maxVersions = 5
maxTotalSize = "10GB"
maxFileSize = "500MB"
latestTTL = "15m"
whiteList = ["owner/repo"]
blackList = ["bad/*"]
`)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", path)
	t.Setenv("RELEASE_CACHE_MAX_VERSIONS", "0")
	t.Setenv("RELEASE_CACHE_LATEST_TTL", "0")
	t.Setenv("RELEASE_CACHE_WHITELIST", "org1/*, org2/app")

	if err := LoadConfig(); err != nil {
		t.Fatal(err)
	}

	cfg := GetConfig()
	if !cfg.ReleaseCache.Enabled || cfg.ReleaseCache.Dir != "/tmp/rcache" {
		t.Fatalf("unexpected ReleaseCache: %+v", cfg.ReleaseCache)
	}
	if cfg.ReleaseCache.MaxVersions != 0 {
		t.Fatalf("MaxVersions = %d, want 0", cfg.ReleaseCache.MaxVersions)
	}
	if cfg.ReleaseCache.LatestTTL != "0" {
		t.Fatalf("LatestTTL = %q, want 0", cfg.ReleaseCache.LatestTTL)
	}
	if len(cfg.ReleaseCache.WhiteList) != 2 || cfg.ReleaseCache.WhiteList[0] != "org1/*" || cfg.ReleaseCache.WhiteList[1] != "org2/app" {
		t.Fatalf("WhiteList = %#v", cfg.ReleaseCache.WhiteList)
	}
}

func TestParseByteSize(t *testing.T) {
	tests := []struct {
		input   string
		want    int64
		wantErr bool
	}{
		{"0", 0, false},
		{"", 0, false},
		{"1024", 1024, false},
		{"2KB", 2 * 1024, false},
		{"512MB", 512 * 1024 * 1024, false},
		{"2GB", 2 * 1024 * 1024 * 1024, false},
		{"1.5GB", 1610612736, false},
		{"-1GB", 0, true},
		{"invalid", 0, true},
	}

	for _, tt := range tests {
		got, err := ParseByteSize(tt.input)
		if (err != nil) != tt.wantErr {
			t.Fatalf("ParseByteSize(%q) err = %v, wantErr=%v", tt.input, err, tt.wantErr)
		}
		if !tt.wantErr && got != tt.want {
			t.Fatalf("ParseByteSize(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}
