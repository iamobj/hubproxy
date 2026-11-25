package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// RegistryMapping Registry映射配置
type RegistryMapping struct {
	Upstream string `toml:"upstream"`
	AuthHost string `toml:"authHost"`
	AuthType string `toml:"authType"`
	Enabled  bool   `toml:"enabled"`
}

// ReleaseCacheConfig GitHub Release本地持久化缓存配置
type ReleaseCacheConfig struct {
	Enabled      bool     `toml:"enabled"`
	Dir          string   `toml:"dir"`
	MaxVersions  int      `toml:"maxVersions"`
	MaxTotalSize string   `toml:"maxTotalSize"`
	MaxFileSize  string   `toml:"maxFileSize"`
	LatestTTL    string   `toml:"latestTTL"`
	WhiteList    []string `toml:"whiteList"`
	BlackList    []string `toml:"blackList"`
}

// AppConfig 应用配置结构体
type AppConfig struct {
	Server struct {
		Host           string `toml:"host"`
		Port           int    `toml:"port"`
		FileSize       int64  `toml:"fileSize"`
		EnableH2C      bool   `toml:"enableH2C"`
		EnableFrontend bool   `toml:"enableFrontend"`
	} `toml:"server"`

	RateLimit struct {
		RequestLimit int     `toml:"requestLimit"`
		PeriodHours  float64 `toml:"periodHours"`
	} `toml:"rateLimit"`

	Security struct {
		WhiteList []string `toml:"whiteList"`
		BlackList []string `toml:"blackList"`
	} `toml:"security"`

	Access struct {
		WhiteList []string `toml:"whiteList"`
		BlackList []string `toml:"blackList"`
		Proxy     string   `toml:"proxy"`
	} `toml:"access"`

	Download struct {
		MaxImages int `toml:"maxImages"`
	} `toml:"download"`

	Registries map[string]RegistryMapping `toml:"registries"`

	TokenCache struct {
		Enabled    bool   `toml:"enabled"`
		DefaultTTL string `toml:"defaultTTL"`
	} `toml:"tokenCache"`

	ReleaseCache ReleaseCacheConfig `toml:"releaseCache"`
}

var (
	appConfig     *AppConfig
	appConfigLock sync.RWMutex

	cachedConfig     *AppConfig
	configCacheTime  time.Time
	configCacheTTL   = 5 * time.Second
	configCacheMutex sync.RWMutex
)

// DefaultConfig 返回默认配置
func DefaultConfig() *AppConfig {
	return &AppConfig{
		Server: struct {
			Host           string `toml:"host"`
			Port           int    `toml:"port"`
			FileSize       int64  `toml:"fileSize"`
			EnableH2C      bool   `toml:"enableH2C"`
			EnableFrontend bool   `toml:"enableFrontend"`
		}{
			Host:           "0.0.0.0",
			Port:           5000,
			FileSize:       2 * 1024 * 1024 * 1024,
			EnableH2C:      false,
			EnableFrontend: true,
		},
		RateLimit: struct {
			RequestLimit int     `toml:"requestLimit"`
			PeriodHours  float64 `toml:"periodHours"`
		}{
			RequestLimit: 500,
			PeriodHours:  3.0,
		},
		Security: struct {
			WhiteList []string `toml:"whiteList"`
			BlackList []string `toml:"blackList"`
		}{
			WhiteList: []string{},
			BlackList: []string{},
		},
		Access: struct {
			WhiteList []string `toml:"whiteList"`
			BlackList []string `toml:"blackList"`
			Proxy     string   `toml:"proxy"`
		}{
			WhiteList: []string{},
			BlackList: []string{},
			Proxy:     "",
		},
		Download: struct {
			MaxImages int `toml:"maxImages"`
		}{
			MaxImages: 10,
		},
		Registries: map[string]RegistryMapping{
			"ghcr.io": {
				Upstream: "ghcr.io",
				AuthHost: "ghcr.io/token",
				AuthType: "github",
				Enabled:  true,
			},
			"gcr.io": {
				Upstream: "gcr.io",
				AuthHost: "gcr.io/v2/token",
				AuthType: "google",
				Enabled:  true,
			},
			"quay.io": {
				Upstream: "quay.io",
				AuthHost: "quay.io/v2/auth",
				AuthType: "quay",
				Enabled:  true,
			},
			"registry.k8s.io": {
				Upstream: "registry.k8s.io",
				AuthHost: "registry.k8s.io",
				AuthType: "anonymous",
				Enabled:  true,
			},
		},
		TokenCache: struct {
			Enabled    bool   `toml:"enabled"`
			DefaultTTL string `toml:"defaultTTL"`
		}{
			Enabled:    true,
			DefaultTTL: "20m",
		},
		ReleaseCache: ReleaseCacheConfig{
			Enabled:      false,
			Dir:          "./cache/releases",
			MaxVersions:  3,
			MaxTotalSize: "20GB",
			MaxFileSize:  "1GB",
			LatestTTL:    "10m",
			WhiteList:    []string{},
			BlackList:    []string{},
		},
	}
}

// GetConfig 安全地获取配置副本
func GetConfig() *AppConfig {
	configCacheMutex.RLock()
	if cachedConfig != nil && time.Since(configCacheTime) < configCacheTTL {
		config := cachedConfig
		configCacheMutex.RUnlock()
		return config
	}
	configCacheMutex.RUnlock()

	configCacheMutex.Lock()
	defer configCacheMutex.Unlock()

	if cachedConfig != nil && time.Since(configCacheTime) < configCacheTTL {
		return cachedConfig
	}

	appConfigLock.RLock()
	if appConfig == nil {
		appConfigLock.RUnlock()
		defaultCfg := DefaultConfig()
		cachedConfig = defaultCfg
		configCacheTime = time.Now()
		return defaultCfg
	}

	configCopy := *appConfig
	configCopy.Security.WhiteList = append([]string(nil), appConfig.Security.WhiteList...)
	configCopy.Security.BlackList = append([]string(nil), appConfig.Security.BlackList...)
	configCopy.Access.WhiteList = append([]string(nil), appConfig.Access.WhiteList...)
	configCopy.Access.BlackList = append([]string(nil), appConfig.Access.BlackList...)
	configCopy.ReleaseCache.WhiteList = append([]string(nil), appConfig.ReleaseCache.WhiteList...)
	configCopy.ReleaseCache.BlackList = append([]string(nil), appConfig.ReleaseCache.BlackList...)
	appConfigLock.RUnlock()

	cachedConfig = &configCopy
	configCacheTime = time.Now()

	return cachedConfig
}

// setConfig 安全地设置配置
func setConfig(cfg *AppConfig) {
	appConfigLock.Lock()
	defer appConfigLock.Unlock()
	appConfig = cfg

	configCacheMutex.Lock()
	cachedConfig = nil
	configCacheMutex.Unlock()
}

func configFilePath() string {
	if path := strings.TrimSpace(os.Getenv("CONFIG_PATH")); path != "" {
		return path
	}
	return "config.toml"
}

func LoadConfig() error {
	cfg := DefaultConfig()
	path := configFilePath()

	if data, err := os.ReadFile(path); err == nil {
		if err := toml.Unmarshal(data, cfg); err != nil {
			return fmt.Errorf("解析配置文件 %s 失败: %v", path, err)
		}
	} else {
		fmt.Printf("未找到配置文件 %s，使用默认配置\n", path)
	}

	overrideFromEnv(cfg)
	setConfig(cfg)

	return nil
}

// overrideFromEnv 从环境变量覆盖配置
func overrideFromEnv(cfg *AppConfig) {
	if val := os.Getenv("SERVER_HOST"); val != "" {
		cfg.Server.Host = val
	}
	if val := os.Getenv("SERVER_PORT"); val != "" {
		if port, err := strconv.Atoi(val); err == nil && port > 0 {
			cfg.Server.Port = port
		}
	}
	if val := os.Getenv("ENABLE_H2C"); val != "" {
		if enable, err := strconv.ParseBool(val); err == nil {
			cfg.Server.EnableH2C = enable
		}
	}
	if val := os.Getenv("ENABLE_FRONTEND"); val != "" {
		if enable, err := strconv.ParseBool(val); err == nil {
			cfg.Server.EnableFrontend = enable
		}
	}
	if val := os.Getenv("MAX_FILE_SIZE"); val != "" {
		if size, err := strconv.ParseInt(val, 10, 64); err == nil && size > 0 {
			cfg.Server.FileSize = size
		}
	}

	if val := os.Getenv("RATE_LIMIT"); val != "" {
		if limit, err := strconv.Atoi(val); err == nil && limit > 0 {
			cfg.RateLimit.RequestLimit = limit
		}
	}
	if val := os.Getenv("RATE_PERIOD_HOURS"); val != "" {
		if period, err := strconv.ParseFloat(val, 64); err == nil && period > 0 {
			cfg.RateLimit.PeriodHours = period
		}
	}

	if val := os.Getenv("IP_WHITELIST"); val != "" {
		cfg.Security.WhiteList = append(cfg.Security.WhiteList, strings.Split(val, ",")...)
	}
	if val := os.Getenv("IP_BLACKLIST"); val != "" {
		cfg.Security.BlackList = append(cfg.Security.BlackList, strings.Split(val, ",")...)
	}

	if val, ok := os.LookupEnv("ACCESS_PROXY"); ok {
		cfg.Access.Proxy = strings.TrimSpace(val)
	}

	if val := os.Getenv("MAX_IMAGES"); val != "" {
		if maxImages, err := strconv.Atoi(val); err == nil && maxImages > 0 {
			cfg.Download.MaxImages = maxImages
		}
	}

	if val := strings.TrimSpace(os.Getenv("RELEASE_CACHE_ENABLED")); val != "" {
		if enabled, err := strconv.ParseBool(val); err == nil {
			cfg.ReleaseCache.Enabled = enabled
		}
	}
	if val := strings.TrimSpace(os.Getenv("RELEASE_CACHE_DIR")); val != "" {
		cfg.ReleaseCache.Dir = val
	}
	if val := strings.TrimSpace(os.Getenv("RELEASE_CACHE_MAX_VERSIONS")); val != "" {
		if maxVersions, err := strconv.Atoi(val); err == nil && maxVersions >= 0 {
			cfg.ReleaseCache.MaxVersions = maxVersions
		}
	}
	if val := strings.TrimSpace(os.Getenv("RELEASE_CACHE_MAX_TOTAL_SIZE")); val != "" {
		if _, err := ParseByteSize(val); err == nil {
			cfg.ReleaseCache.MaxTotalSize = val
		}
	}
	if val := strings.TrimSpace(os.Getenv("RELEASE_CACHE_MAX_FILE_SIZE")); val != "" {
		if _, err := ParseByteSize(val); err == nil {
			cfg.ReleaseCache.MaxFileSize = val
		}
	}
	if val := strings.TrimSpace(os.Getenv("RELEASE_CACHE_LATEST_TTL")); val != "" {
		if _, err := time.ParseDuration(val); err == nil {
			cfg.ReleaseCache.LatestTTL = val
		}
	}
	if val, ok := os.LookupEnv("RELEASE_CACHE_WHITELIST"); ok {
		cfg.ReleaseCache.WhiteList = splitCSV(val)
	}
	if val, ok := os.LookupEnv("RELEASE_CACHE_BLACKLIST"); ok {
		cfg.ReleaseCache.BlackList = splitCSV(val)
	}
}

func splitCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// ParseByteSize 解析带单位的字节大小配置（如 "20GB", "512MB", "0"）
func ParseByteSize(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" || s == "0" {
		return 0, nil
	}

	upper := strings.ToUpper(s)
	suffixes := []struct {
		suffix string
		mult   int64
	}{
		{"TIB", 1024 * 1024 * 1024 * 1024},
		{"GIB", 1024 * 1024 * 1024},
		{"MIB", 1024 * 1024},
		{"KIB", 1024},
		{"TB", 1024 * 1024 * 1024 * 1024},
		{"GB", 1024 * 1024 * 1024},
		{"MB", 1024 * 1024},
		{"KB", 1024},
		{"T", 1024 * 1024 * 1024 * 1024},
		{"G", 1024 * 1024 * 1024},
		{"M", 1024 * 1024},
		{"K", 1024},
		{"B", 1},
	}

	for _, item := range suffixes {
		if strings.HasSuffix(upper, item.suffix) {
			numStr := strings.TrimSpace(upper[:len(upper)-len(item.suffix)])
			val, err := strconv.ParseFloat(numStr, 64)
			if err != nil || val < 0 {
				return 0, fmt.Errorf("无效的字节大小: %q", raw)
			}
			return int64(val * float64(item.mult)), nil
		}
	}

	val, err := strconv.ParseInt(upper, 10, 64)
	if err != nil || val < 0 {
		return 0, fmt.Errorf("无效的字节大小: %q", raw)
	}
	return val, nil
}
