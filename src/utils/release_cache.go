package utils

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"hubproxy/config"
)

const (
	releaseMetaFileName   = ".meta.json"
	latestMetaFileName    = ".latest.json"
	releaseTagsDirName    = "tags"
	maxTagDirPrefixLength = 48
	globalEvictHighRatio  = 0.90
)

// ReleaseTarget 解析后的 GitHub Release 下载目标
type ReleaseTarget struct {
	Owner    string
	Repo     string
	Tag      string
	Asset    string
	IsLatest bool
	RawURL   string
}

func (t *ReleaseTarget) RepoKey() string {
	return t.Owner + "/" + t.Repo
}

func (t *ReleaseTarget) AssetKey() string {
	return t.Owner + "/" + t.Repo + "/" + t.Tag + "/" + t.Asset
}

// ParseGitHubReleaseURL 解析 GitHub Release 附件下载 URL
// 仅匹配 /releases/download/{tag}/{asset} 与 /releases/latest/download/{asset}，并跳过 .sh/.ps1 脚本
func ParseGitHubReleaseURL(rawURL string) (*ReleaseTarget, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || !strings.EqualFold(u.Host, "github.com") {
		return nil, false
	}

	trimmed := strings.Trim(u.Path, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 6 || parts[2] != "releases" {
		return nil, false
	}

	owner := strings.ToLower(strings.TrimSpace(parts[0]))
	repo := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(parts[1], ".git")))
	if !isSafePathToken(owner) || !isSafePathToken(repo) {
		return nil, false
	}

	var (
		tag      string
		asset    string
		isLatest bool
	)

	if len(parts) == 6 && parts[3] == "latest" && parts[4] == "download" {
		isLatest = true
		asset = parts[5]
	} else if parts[3] == "download" {
		asset = parts[len(parts)-1]
		tag = strings.Join(parts[4:len(parts)-1], "/")
	} else {
		return nil, false
	}

	if !isSafePathToken(asset) {
		return nil, false
	}
	if !isLatest && !isSafeTagPath(tag) {
		return nil, false
	}

	lowerAsset := strings.ToLower(asset)
	if strings.HasSuffix(lowerAsset, ".sh") || strings.HasSuffix(lowerAsset, ".ps1") {
		return nil, false
	}

	return &ReleaseTarget{
		Owner:    owner,
		Repo:     repo,
		Tag:      tag,
		Asset:    asset,
		IsLatest: isLatest,
		RawURL:   rawURL,
	}, true
}

func isSafePathToken(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	return !strings.ContainsAny(s, "/\\\x00")
}

func isSafeTagPath(tag string) bool {
	if tag == "" || strings.ContainsRune(tag, '\\') || strings.ContainsRune(tag, '\x00') {
		return false
	}
	for _, seg := range strings.Split(tag, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// SemVer 语义化版本号
type SemVer struct {
	Nums       []int
	PreRelease []string
}

// ParseSemVer 解析常见 GitHub Release 版本号（支持可选 v/V 前缀、2~4 段数字及预发布标签）
func ParseSemVer(tag string) (SemVer, bool) {
	s := strings.TrimSpace(tag)
	if s == "" {
		return SemVer{}, false
	}
	if s[0] == 'v' || s[0] == 'V' {
		s = s[1:]
	}
	if s == "" || s[0] < '0' || s[0] > '9' {
		return SemVer{}, false
	}

	if idx := strings.IndexByte(s, '+'); idx != -1 {
		s = s[:idx]
	}

	var preParts []string
	if idx := strings.IndexByte(s, '-'); idx != -1 {
		preRaw := s[idx+1:]
		if preRaw == "" {
			return SemVer{}, false
		}
		preParts = strings.Split(preRaw, ".")
		for _, p := range preParts {
			if p == "" {
				return SemVer{}, false
			}
		}
		s = s[:idx]
	}

	coreParts := strings.Split(s, ".")
	if len(coreParts) < 2 || len(coreParts) > 4 {
		return SemVer{}, false
	}

	nums := make([]int, len(coreParts))
	for i, part := range coreParts {
		if part == "" {
			return SemVer{}, false
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return SemVer{}, false
		}
		nums[i] = n
	}

	return SemVer{
		Nums:       nums,
		PreRelease: preParts,
	}, true
}

// CompareSemVer 比较两个 SemVer（a > b 返回 1，a < b 返回 -1，相等返回 0）
func CompareSemVer(a, b SemVer) int {
	maxLen := len(a.Nums)
	if len(b.Nums) > maxLen {
		maxLen = len(b.Nums)
	}
	for i := 0; i < maxLen; i++ {
		var av, bv int
		if i < len(a.Nums) {
			av = a.Nums[i]
		}
		if i < len(b.Nums) {
			bv = b.Nums[i]
		}
		if av != bv {
			if av > bv {
				return 1
			}
			return -1
		}
	}

	aHasPre := len(a.PreRelease) > 0
	bHasPre := len(b.PreRelease) > 0
	if !aHasPre && !bHasPre {
		return 0
	}
	if !aHasPre && bHasPre {
		return 1
	}
	if aHasPre && !bHasPre {
		return -1
	}

	minPre := len(a.PreRelease)
	if len(b.PreRelease) < minPre {
		minPre = len(b.PreRelease)
	}
	for i := 0; i < minPre; i++ {
		ap, bp := a.PreRelease[i], b.PreRelease[i]
		if ap == bp {
			continue
		}
		aNum, aErr := strconv.Atoi(ap)
		bNum, bErr := strconv.Atoi(bp)
		switch {
		case aErr == nil && bErr == nil:
			if aNum > bNum {
				return 1
			}
			return -1
		case aErr == nil && bErr != nil:
			return -1
		case aErr != nil && bErr == nil:
			return 1
		default:
			if ap > bp {
				return 1
			}
			return -1
		}
	}

	if len(a.PreRelease) > len(b.PreRelease) {
		return 1
	}
	if len(a.PreRelease) < len(b.PreRelease) {
		return -1
	}
	return 0
}

// ReleaseFileEntry 单个已缓存 Release 文件元数据
type ReleaseFileEntry struct {
	Name               string    `json:"name"`
	Size               int64     `json:"size"`
	ContentType        string    `json:"content_type,omitempty"`
	ContentDisposition string    `json:"content_disposition,omitempty"`
	ETag               string    `json:"etag,omitempty"`
	ModTime            time.Time `json:"mod_time"`
}

// ReleaseTagMetadata 单个 Tag 目录元数据
type ReleaseTagMetadata struct {
	Owner          string                      `json:"owner"`
	Repo           string                      `json:"repo"`
	Tag            string                      `json:"tag"`
	CreatedAt      time.Time                   `json:"created_at"`
	LastAccessedAt time.Time                   `json:"last_accessed_at"`
	Files          map[string]ReleaseFileEntry `json:"files"`
}

// LatestTagMapping latest -> 真实 Tag 映射
type LatestTagMapping struct {
	Tag       string    `json:"tag"`
	CheckedAt time.Time `json:"checked_at"`
}

type cachedTagRecord struct {
	meta      ReleaseTagMetadata
	dirKey    string
	dirPath   string
	semVer    SemVer
	hasSemVer bool
	totalSize int64
}

type probeInflight struct {
	done chan struct{}
	tag  string
	err  error
}

// ReleaseCacheManager 管理 GitHub Release 本地磁盘缓存
type ReleaseCacheManager struct {
	mu           sync.RWMutex
	dir          string
	repos        map[string]map[string]*cachedTagRecord // repoKey -> tag -> record
	latestMap    map[string]LatestTagMapping            // repoKey -> mapping
	totalSize    int64
	sessions     map[string]*DownloadSession
	probeMu      sync.Mutex
	probeFlights map[string]*probeInflight
}

var (
	globalReleaseCache   *ReleaseCacheManager
	globalReleaseCacheMu sync.RWMutex
)

// InitReleaseCache 根据当前配置初始化全局 Release 缓存管理器
func InitReleaseCache() error {
	cfg := config.GetConfig()
	dir := strings.TrimSpace(cfg.ReleaseCache.Dir)
	if dir == "" {
		dir = "./cache/releases"
	}

	mgr := &ReleaseCacheManager{
		dir:          dir,
		repos:        make(map[string]map[string]*cachedTagRecord),
		latestMap:    make(map[string]LatestTagMapping),
		sessions:     make(map[string]*DownloadSession),
		probeFlights: make(map[string]*probeInflight),
	}

	if cfg.ReleaseCache.Enabled {
		if err := mgr.loadFromDisk(cfg); err != nil {
			return err
		}
	}

	globalReleaseCacheMu.Lock()
	globalReleaseCache = mgr
	globalReleaseCacheMu.Unlock()
	return nil
}

// GetReleaseCache 获取全局 Release 缓存管理器
func GetReleaseCache() *ReleaseCacheManager {
	globalReleaseCacheMu.RLock()
	mgr := globalReleaseCache
	globalReleaseCacheMu.RUnlock()
	if mgr != nil {
		return mgr
	}
	_ = InitReleaseCache()
	globalReleaseCacheMu.RLock()
	defer globalReleaseCacheMu.RUnlock()
	return globalReleaseCache
}

func (m *ReleaseCacheManager) loadFromDisk(cfg *config.AppConfig) error {
	if err := os.MkdirAll(m.dir, 0755); err != nil {
		return fmt.Errorf("创建 Release 缓存目录失败: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	owners, err := os.ReadDir(m.dir)
	if err != nil {
		return fmt.Errorf("读取 Release 缓存目录失败: %w", err)
	}

	for _, ownerEntry := range owners {
		if !ownerEntry.IsDir() {
			continue
		}
		ownerPath := filepath.Join(m.dir, ownerEntry.Name())
		repos, err := os.ReadDir(ownerPath)
		if err != nil {
			continue
		}

		for _, repoEntry := range repos {
			if !repoEntry.IsDir() {
				continue
			}
			repoPath := filepath.Join(ownerPath, repoEntry.Name())
			owner := strings.ToLower(ownerEntry.Name())
			repo := strings.ToLower(repoEntry.Name())
			repoKey := owner + "/" + repo

			if latestData, err := os.ReadFile(filepath.Join(repoPath, latestMetaFileName)); err == nil {
				var lm LatestTagMapping
				if json.Unmarshal(latestData, &lm) == nil && lm.Tag != "" {
					m.latestMap[repoKey] = lm
				}
			}

			tagsRoot := filepath.Join(repoPath, releaseTagsDirName)
			tagDirs, err := os.ReadDir(tagsRoot)
			if err != nil {
				continue
			}

			for _, tagDir := range tagDirs {
				if !tagDir.IsDir() {
					continue
				}
				tagDirPath := filepath.Join(tagsRoot, tagDir.Name())
				m.cleanTempFiles(tagDirPath)

				metaBytes, err := os.ReadFile(filepath.Join(tagDirPath, releaseMetaFileName))
				if err != nil {
					_ = os.RemoveAll(tagDirPath)
					continue
				}

				var meta ReleaseTagMetadata
				if err := json.Unmarshal(metaBytes, &meta); err != nil || meta.Tag == "" {
					_ = os.RemoveAll(tagDirPath)
					continue
				}

				validFiles := make(map[string]ReleaseFileEntry)
				var tagSize int64
				dirtyMeta := false
				for name, entry := range meta.Files {
					if !isSafePathToken(name) {
						dirtyMeta = true
						continue
					}
					info, err := os.Stat(filepath.Join(tagDirPath, name))
					if err != nil || !info.Mode().IsRegular() || info.Size() != entry.Size {
						dirtyMeta = true
						continue
					}
					validFiles[name] = entry
					tagSize += entry.Size
				}

				if len(validFiles) == 0 {
					_ = os.RemoveAll(tagDirPath)
					continue
				}

				meta.Owner = owner
				meta.Repo = repo
				meta.Files = validFiles
				if meta.CreatedAt.IsZero() {
					meta.CreatedAt = time.Now()
					dirtyMeta = true
				}
				if meta.LastAccessedAt.IsZero() {
					meta.LastAccessedAt = meta.CreatedAt
					dirtyMeta = true
				}
				if dirtyMeta {
					_ = writeJSONAtomic(filepath.Join(tagDirPath, releaseMetaFileName), meta)
				}

				sv, ok := ParseSemVer(meta.Tag)
				rec := &cachedTagRecord{
					meta:      meta,
					dirKey:    tagDir.Name(),
					dirPath:   tagDirPath,
					semVer:    sv,
					hasSemVer: ok,
					totalSize: tagSize,
				}
				if m.repos[repoKey] == nil {
					m.repos[repoKey] = make(map[string]*cachedTagRecord)
				}
				m.repos[repoKey][meta.Tag] = rec
				m.totalSize += tagSize
			}
		}
	}

	for repoKey := range m.repos {
		m.evictRepoVersionsLocked(repoKey, cfg.ReleaseCache.MaxVersions)
	}
	m.evictGlobalLocked(parseMaxTotalBytes(cfg))
	return nil
}

func (m *ReleaseCacheManager) cleanTempFiles(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() && (strings.HasPrefix(entry.Name(), ".tmp-") || strings.Contains(entry.Name(), ".tmp-")) {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}

// IsRepoAllowed 检查仓库是否满足 ReleaseCache 白/黑名单规则
func (m *ReleaseCacheManager) IsRepoAllowed(owner, repo string, cfg *config.AppConfig) bool {
	if !cfg.ReleaseCache.Enabled {
		return false
	}
	if len(cfg.ReleaseCache.BlackList) > 0 && GlobalAccessController.MatchGitHubRepoList(owner, repo, cfg.ReleaseCache.BlackList) {
		return false
	}
	if len(cfg.ReleaseCache.WhiteList) > 0 && !GlobalAccessController.MatchGitHubRepoList(owner, repo, cfg.ReleaseCache.WhiteList) {
		return false
	}
	return true
}

// ResolveLatestTag 解析 /releases/latest/download/{asset} 对应的真实版本号 Tag
// 返回 (resolvedTag, staleFallback, err)
func (m *ReleaseCacheManager) ResolveLatestTag(ctx context.Context, target *ReleaseTarget, cfg *config.AppConfig) (string, bool, error) {
	ttl := parseLatestTTL(cfg)
	repoKey := target.RepoKey()

	m.mu.RLock()
	cached, hasCached := m.latestMap[repoKey]
	hasLocalFiles := hasCached && m.repos[repoKey] != nil && m.repos[repoKey][cached.Tag] != nil
	m.mu.RUnlock()

	if hasCached && ttl > 0 && time.Since(cached.CheckedAt) < ttl {
		return cached.Tag, false, nil
	}

	tag, err := m.probeLatestSingleflight(ctx, target)
	if err == nil && tag != "" {
		m.saveLatestMapping(target.Owner, target.Repo, tag)
		return tag, false, nil
	}

	if hasLocalFiles {
		return cached.Tag, true, nil
	}
	return "", false, err
}

func (m *ReleaseCacheManager) probeLatestSingleflight(ctx context.Context, target *ReleaseTarget) (string, error) {
	repoKey := target.RepoKey()

	m.probeMu.Lock()
	if inflight, exists := m.probeFlights[repoKey]; exists {
		m.probeMu.Unlock()
		select {
		case <-inflight.done:
			return inflight.tag, inflight.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	inflight := &probeInflight{done: make(chan struct{})}
	m.probeFlights[repoKey] = inflight
	m.probeMu.Unlock()

	tag, err := probeGitHubLatestTag(target)
	inflight.tag = tag
	inflight.err = err
	close(inflight.done)

	m.probeMu.Lock()
	delete(m.probeFlights, repoKey)
	m.probeMu.Unlock()

	return tag, err
}

func probeGitHubLatestTag(target *ReleaseTarget) (string, error) {
	probeURL := fmt.Sprintf("https://github.com/%s/%s/releases/latest/download/%s", target.Owner, target.Repo, target.Asset)
	client := GetReleaseProbeHTTPClient()

	for _, method := range []string{http.MethodHead, http.MethodGet} {
		req, err := http.NewRequest(method, probeURL, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", "hubproxy-release-probe")

		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		_ = resp.Body.Close()

		loc := resp.Header.Get("Location")
		if loc != "" {
			if tag, ok := extractTagFromLocation(loc, target.Owner, target.Repo); ok {
				return tag, nil
			}
		}
		if method == http.MethodHead && (resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusForbidden) {
			continue
		}
		return "", fmt.Errorf("探测 latest 版本失败: status=%d location=%q", resp.StatusCode, loc)
	}

	return "", errors.New("探测 latest 版本失败")
}

func extractTagFromLocation(location, expectedOwner, expectedRepo string) (string, bool) {
	parsed, ok := ParseGitHubReleaseURL(location)
	if ok && !parsed.IsLatest && parsed.Owner == expectedOwner && parsed.Repo == expectedRepo && parsed.Tag != "" {
		return parsed.Tag, true
	}
	u, err := url.Parse(location)
	if err != nil {
		return "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) >= 6 &&
		strings.EqualFold(parts[0], expectedOwner) &&
		strings.EqualFold(parts[1], expectedRepo) &&
		parts[2] == "releases" && parts[3] == "download" {
		tag := strings.Join(parts[4:len(parts)-1], "/")
		if isSafeTagPath(tag) {
			return tag, true
		}
	}
	return "", false
}

func (m *ReleaseCacheManager) saveLatestMapping(owner, repo, tag string) {
	repoKey := owner + "/" + repo
	mapping := LatestTagMapping{
		Tag:       tag,
		CheckedAt: time.Now(),
	}

	m.mu.Lock()
	m.latestMap[repoKey] = mapping
	m.mu.Unlock()

	repoDir := filepath.Join(m.dir, owner, repo)
	if os.MkdirAll(repoDir, 0755) == nil {
		_ = writeJSONAtomic(filepath.Join(repoDir, latestMetaFileName), mapping)
	}
}

// OpenCachedFile 查找并打开已缓存的本地文件（持读锁打开句柄防止并发淘汰抢占）
func (m *ReleaseCacheManager) OpenCachedFile(target *ReleaseTarget) (*os.File, ReleaseFileEntry, bool) {
	repoKey := target.RepoKey()

	m.mu.RLock()
	repoTags := m.repos[repoKey]
	if repoTags == nil {
		m.mu.RUnlock()
		return nil, ReleaseFileEntry{}, false
	}
	rec := repoTags[target.Tag]
	if rec == nil {
		m.mu.RUnlock()
		return nil, ReleaseFileEntry{}, false
	}
	entry, ok := rec.meta.Files[target.Asset]
	if !ok {
		m.mu.RUnlock()
		return nil, ReleaseFileEntry{}, false
	}
	filePath := filepath.Join(rec.dirPath, target.Asset)
	f, err := os.Open(filePath)
	m.mu.RUnlock()
	if err != nil {
		return nil, ReleaseFileEntry{}, false
	}

	m.touchTagAccess(repoKey, target.Tag)
	return f, entry, true
}

func (m *ReleaseCacheManager) touchTagAccess(repoKey, tag string) {
	now := time.Now()
	var (
		metaCopy ReleaseTagMetadata
		metaPath string
		should   bool
	)

	m.mu.Lock()
	if repoTags := m.repos[repoKey]; repoTags != nil {
		if rec := repoTags[tag]; rec != nil {
			if now.Sub(rec.meta.LastAccessedAt) >= 30*time.Second {
				rec.meta.LastAccessedAt = now
				metaCopy = rec.meta
				metaPath = filepath.Join(rec.dirPath, releaseMetaFileName)
				should = true
			}
		}
	}
	m.mu.Unlock()

	if should {
		_ = writeJSONAtomic(metaPath, metaCopy)
	}
}

// CanCacheTag 判断当前 Tag 是否满足 N 版本保留准入资格
func (m *ReleaseCacheManager) CanCacheTag(target *ReleaseTarget, maxVersions int) bool {
	if maxVersions <= 0 {
		return true
	}

	repoKey := target.RepoKey()
	m.mu.RLock()
	defer m.mu.RUnlock()

	repoTags := m.repos[repoKey]
	if repoTags == nil || len(repoTags) < maxVersions {
		return true
	}
	if _, exists := repoTags[target.Tag]; exists {
		return true
	}

	oldest := findOldestTagRecord(repoTags)
	if oldest == nil {
		return true
	}

	candSV, candHasSV := ParseSemVer(target.Tag)
	if candHasSV && oldest.hasSemVer {
		return CompareSemVer(candSV, oldest.semVer) > 0
	}
	// 非标准 SemVer Tag 时，新拉取的 Tag 视为较新（时间顺序）
	return true
}

func findOldestTagRecord(repoTags map[string]*cachedTagRecord) *cachedTagRecord {
	var oldest *cachedTagRecord
	for _, rec := range repoTags {
		if oldest == nil || compareTagRecords(rec, oldest) < 0 {
			oldest = rec
		}
	}
	return oldest
}

// compareTagRecords 返回 >0 表示 a 比 b 新，<0 表示 a 比 b 旧
func compareTagRecords(a, b *cachedTagRecord) int {
	if a.hasSemVer && b.hasSemVer {
		if cmp := CompareSemVer(a.semVer, b.semVer); cmp != 0 {
			return cmp
		}
	}
	if !a.meta.CreatedAt.Equal(b.meta.CreatedAt) {
		if a.meta.CreatedAt.After(b.meta.CreatedAt) {
			return 1
		}
		return -1
	}
	return strings.Compare(a.meta.Tag, b.meta.Tag)
}

func (m *ReleaseCacheManager) commitCachedFile(target *ReleaseTarget, tmpPath string, entry ReleaseFileEntry, cfg *config.AppConfig) (string, error) {
	repoKey := target.RepoKey()
	dirKey := sanitizeTagDir(target.Tag)
	tagDir := filepath.Join(m.dir, target.Owner, target.Repo, releaseTagsDirName, dirKey)
	if err := os.MkdirAll(tagDir, 0755); err != nil {
		return "", err
	}

	finalPath := filepath.Join(tagDir, target.Asset)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return "", err
	}

	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.repos[repoKey] == nil {
		m.repos[repoKey] = make(map[string]*cachedTagRecord)
	}
	rec := m.repos[repoKey][target.Tag]
	if rec == nil {
		sv, ok := ParseSemVer(target.Tag)
		rec = &cachedTagRecord{
			meta: ReleaseTagMetadata{
				Owner:          target.Owner,
				Repo:           target.Repo,
				Tag:            target.Tag,
				CreatedAt:      now,
				LastAccessedAt: now,
				Files:          make(map[string]ReleaseFileEntry),
			},
			dirKey:    dirKey,
			dirPath:   tagDir,
			semVer:    sv,
			hasSemVer: ok,
		}
		m.repos[repoKey][target.Tag] = rec
	}

	if old, existed := rec.meta.Files[target.Asset]; existed {
		rec.totalSize -= old.Size
		m.totalSize -= old.Size
	}
	rec.meta.Files[target.Asset] = entry
	rec.meta.LastAccessedAt = now
	rec.totalSize += entry.Size
	m.totalSize += entry.Size

	if err := writeJSONAtomic(filepath.Join(tagDir, releaseMetaFileName), rec.meta); err != nil {
		return "", err
	}

	m.evictRepoVersionsLocked(repoKey, cfg.ReleaseCache.MaxVersions)
	m.evictGlobalLocked(parseMaxTotalBytes(cfg))

	// 若刚写入的 Tag 自己因极端并发被淘汰了，返回是否仍在缓存中
	if m.repos[repoKey] == nil || m.repos[repoKey][target.Tag] == nil {
		return "", errors.New("缓存条目在淘汰阶段被清理")
	}
	return finalPath, nil
}

func (m *ReleaseCacheManager) evictRepoVersionsLocked(repoKey string, maxVersions int) {
	if maxVersions <= 0 {
		return
	}
	repoTags := m.repos[repoKey]
	if len(repoTags) <= maxVersions {
		return
	}

	list := make([]*cachedTagRecord, 0, len(repoTags))
	for _, rec := range repoTags {
		list = append(list, rec)
	}
	// 由新到旧排序
	sort.Slice(list, func(i, j int) bool {
		return compareTagRecords(list[i], list[j]) > 0
	})

	for _, victim := range list[maxVersions:] {
		m.removeTagLocked(repoKey, victim.meta.Tag)
	}
}

func (m *ReleaseCacheManager) evictGlobalLocked(maxTotalBytes int64) {
	if maxTotalBytes <= 0 || m.totalSize <= maxTotalBytes {
		return
	}

	targetSize := int64(float64(maxTotalBytes) * globalEvictHighRatio)
	type tagRef struct {
		repoKey string
		rec     *cachedTagRecord
	}

	all := make([]tagRef, 0)
	for repoKey, repoTags := range m.repos {
		for _, rec := range repoTags {
			all = append(all, tagRef{repoKey: repoKey, rec: rec})
		}
	}

	// 按最近访问时间从最冷到最热排序
	sort.Slice(all, func(i, j int) bool {
		if !all[i].rec.meta.LastAccessedAt.Equal(all[j].rec.meta.LastAccessedAt) {
			return all[i].rec.meta.LastAccessedAt.Before(all[j].rec.meta.LastAccessedAt)
		}
		return compareTagRecords(all[i].rec, all[j].rec) < 0
	})

	for _, item := range all {
		if m.totalSize <= targetSize {
			break
		}
		m.removeTagLocked(item.repoKey, item.rec.meta.Tag)
	}
}

func (m *ReleaseCacheManager) removeTagLocked(repoKey, tag string) {
	repoTags := m.repos[repoKey]
	if repoTags == nil {
		return
	}
	rec := repoTags[tag]
	if rec == nil {
		return
	}

	delete(repoTags, tag)
	m.totalSize -= rec.totalSize
	if m.totalSize < 0 {
		m.totalSize = 0
	}
	if len(repoTags) == 0 {
		delete(m.repos, repoKey)
	}
	_ = os.RemoveAll(rec.dirPath)
}

// DownloadSession 表示同一个未命中 Release 文件的并发共享回源会话
type DownloadSession struct {
	key        string
	target     *ReleaseTarget
	upstream   string
	maxAllowed int64
	cfg        *config.AppConfig
	blockedCT  map[string]bool

	ctx    context.Context
	cancel context.CancelFunc

	headersReady chan struct{}

	mu                 sync.Mutex
	cond               *sync.Cond
	subscribers        int
	bypass             bool
	startErr           error
	statusCode         int
	contentType        string
	contentDisposition string
	etag               string
	contentLength      int64
	tmpPath            string
	finalPath          string
	written            int64
	completed          bool
	streamErr          error
}

// ServeReleaseDownload 尝试通过 Release 缓存处理请求；返回 true 表示已完整响应，false 表示需回落原代理透传
func (m *ReleaseCacheManager) ServeReleaseDownload(c *gin.Context, target *ReleaseTarget, upstreamURL string, blockedContentTypes map[string]bool) bool {
	cfg := config.GetConfig()
	if !m.IsRepoAllowed(target.Owner, target.Repo, cfg) {
		c.Header("X-Cache", "BYPASS")
		return false
	}

	stale := false
	if target.IsLatest {
		resolvedTag, isStale, err := m.ResolveLatestTag(c.Request.Context(), target, cfg)
		if err != nil || resolvedTag == "" {
			c.Header("X-Cache", "BYPASS")
			return false
		}
		target.Tag = resolvedTag
		stale = isStale
		upstreamURL = fmt.Sprintf("https://github.com/%s/%s/releases/download/%s/%s", target.Owner, target.Repo, target.Tag, target.Asset)
	}

	c.Header("X-Cache-Tag", target.Tag)

	// 1. 本地命中（支持 GET/HEAD 与 Range 206）
	if f, entry, ok := m.OpenCachedFile(target); ok {
		defer func() { _ = f.Close() }()
		if stale {
			c.Header("X-Cache", "STALE")
		} else {
			c.Header("X-Cache", "HIT")
		}
		if entry.ContentType != "" {
			c.Header("Content-Type", entry.ContentType)
		}
		if entry.ContentDisposition != "" {
			c.Header("Content-Disposition", entry.ContentDisposition)
		}
		if entry.ETag != "" {
			c.Header("ETag", entry.ETag)
		}
		http.ServeContent(c.Writer, c.Request, target.Asset, entry.ModTime, f)
		return true
	}

	// 2. 非全量 GET（如 HEAD 或带 Range 的分片/续传请求）未命中时直接透传，不落残缺分片
	if c.Request.Method != http.MethodGet || c.GetHeader("Range") != "" {
		c.Header("X-Cache", "BYPASS")
		return false
	}

	// 3. 版本准入校验：老于已缓存 N 个版本的历史版本直接透传不挤占名额
	if !m.CanCacheTag(target, cfg.ReleaseCache.MaxVersions) {
		c.Header("X-Cache", "BYPASS")
		return false
	}

	maxAllowed := effectiveMaxCacheFileBytes(cfg)
	if maxAllowed <= 0 {
		c.Header("X-Cache", "BYPASS")
		return false
	}

	// 4. 获取或加入并发流式回源会话（边下边返 + 追尾共享）
	session, cachedFile, cachedEntry, hitNow := m.acquireOrJoinSession(target, upstreamURL, maxAllowed, cfg, blockedContentTypes)
	if hitNow {
		defer func() { _ = cachedFile.Close() }()
		c.Header("X-Cache", "HIT")
		if cachedEntry.ContentType != "" {
			c.Header("Content-Type", cachedEntry.ContentType)
		}
		if cachedEntry.ContentDisposition != "" {
			c.Header("Content-Disposition", cachedEntry.ContentDisposition)
		}
		if cachedEntry.ETag != "" {
			c.Header("ETag", cachedEntry.ETag)
		}
		http.ServeContent(c.Writer, c.Request, target.Asset, cachedEntry.ModTime, cachedFile)
		return true
	}

	return m.streamSessionToClient(c, session)
}

func (m *ReleaseCacheManager) acquireOrJoinSession(
	target *ReleaseTarget,
	upstreamURL string,
	maxAllowed int64,
	cfg *config.AppConfig,
	blockedCT map[string]bool,
) (*DownloadSession, *os.File, ReleaseFileEntry, bool) {
	key := target.AssetKey()

	m.mu.Lock()
	// 锁内再次检查本地文件，消除与刚落盘任务的竞态窗口
	if repoTags := m.repos[target.RepoKey()]; repoTags != nil {
		if rec := repoTags[target.Tag]; rec != nil {
			if entry, ok := rec.meta.Files[target.Asset]; ok {
				if f, err := os.Open(filepath.Join(rec.dirPath, target.Asset)); err == nil {
					m.mu.Unlock()
					m.touchTagAccess(target.RepoKey(), target.Tag)
					return nil, f, entry, true
				}
			}
		}
	}

	if existing, ok := m.sessions[key]; ok {
		existing.mu.Lock()
		existing.subscribers++
		existing.mu.Unlock()
		m.mu.Unlock()
		return existing, nil, ReleaseFileEntry{}, false
	}

	ctx, cancel := context.WithCancel(context.Background())
	session := &DownloadSession{
		key:         key,
		target:      target,
		upstream:    upstreamURL,
		maxAllowed:  maxAllowed,
		cfg:         cfg,
		blockedCT:   blockedCT,
		ctx:         ctx,
		cancel:      cancel,
		subscribers: 1,
		headersReady: make(chan struct{}),
	}
	session.cond = sync.NewCond(&session.mu)
	m.sessions[key] = session
	m.mu.Unlock()

	go m.runDownloadSession(session)
	return session, nil, ReleaseFileEntry{}, false
}

func (m *ReleaseCacheManager) runDownloadSession(s *DownloadSession) {
	defer func() {
		m.mu.Lock()
		delete(m.sessions, s.key)
		m.mu.Unlock()
		s.cancel()
	}()

	req, err := http.NewRequestWithContext(s.ctx, http.MethodGet, s.upstream, nil)
	if err != nil {
		s.failBeforeHeaders(err, false)
		return
	}
	req.Header.Set("User-Agent", "hubproxy-release-cache")

	resp, err := GetGlobalHTTPClient().Do(req)
	if err != nil {
		s.failBeforeHeaders(err, false)
		return
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	ct := resp.Header.Get("Content-Type")
	baseCT := strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	if resp.StatusCode != http.StatusOK || s.blockedCT[baseCT] {
		s.failBeforeHeaders(nil, true)
		return
	}

	contentLength := resp.ContentLength
	if contentLength > s.maxAllowed {
		s.failBeforeHeaders(nil, true)
		return
	}

	dirKey := sanitizeTagDir(s.target.Tag)
	tagDir := filepath.Join(m.dir, s.target.Owner, s.target.Repo, releaseTagsDirName, dirKey)
	if err := os.MkdirAll(tagDir, 0755); err != nil {
		s.failBeforeHeaders(err, true)
		return
	}

	tmpPath := filepath.Join(tagDir, fmt.Sprintf(".tmp-%s-%s", s.target.Asset, randomHex(6)))
	tmpFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0644)
	if err != nil {
		s.failBeforeHeaders(err, true)
		return
	}

	cleanupTmp := true
	defer func() {
		_ = tmpFile.Close()
		if cleanupTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	s.mu.Lock()
	s.statusCode = resp.StatusCode
	s.contentType = ct
	s.contentDisposition = resp.Header.Get("Content-Disposition")
	s.etag = resp.Header.Get("ETag")
	s.contentLength = contentLength
	s.tmpPath = tmpPath
	s.mu.Unlock()
	close(s.headersReady)

	buf := make([]byte, 64*1024)
	var totalWritten int64
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if totalWritten+int64(n) > s.maxAllowed {
				s.finishWithError(fmt.Errorf("文件大小超过缓存限制 %d 字节", s.maxAllowed))
				return
			}
			if _, writeErr := tmpFile.Write(buf[:n]); writeErr != nil {
				s.finishWithError(writeErr)
				return
			}
			totalWritten += int64(n)
			s.mu.Lock()
			s.written = totalWritten
			s.cond.Broadcast()
			s.mu.Unlock()
		}

		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			s.finishWithError(readErr)
			return
		}
	}

	if contentLength >= 0 && totalWritten != contentLength {
		s.finishWithError(fmt.Errorf("下载字节数不完整: got=%d want=%d", totalWritten, contentLength))
		return
	}

	if err := tmpFile.Sync(); err != nil {
		s.finishWithError(err)
		return
	}
	_ = tmpFile.Close()

	entry := ReleaseFileEntry{
		Name:               s.target.Asset,
		Size:               totalWritten,
		ContentType:        ct,
		ContentDisposition: s.contentDisposition,
		ETag:               s.etag,
		ModTime:            time.Now().UTC(),
	}

	finalPath, commitErr := m.commitCachedFile(s.target, tmpPath, entry, s.cfg)
	if commitErr != nil {
		s.finishWithError(commitErr)
		return
	}
	cleanupTmp = false

	s.mu.Lock()
	s.finalPath = finalPath
	s.completed = true
	s.cond.Broadcast()
	s.mu.Unlock()
}

func (s *DownloadSession) failBeforeHeaders(err error, bypass bool) {
	s.mu.Lock()
	s.startErr = err
	s.bypass = bypass
	s.mu.Unlock()
	close(s.headersReady)
}

func (s *DownloadSession) finishWithError(err error) {
	s.mu.Lock()
	s.streamErr = err
	s.cond.Broadcast()
	s.mu.Unlock()
}

func (s *DownloadSession) releaseSubscriber() {
	s.mu.Lock()
	s.subscribers--
	shouldCancel := s.subscribers <= 0 && !s.completed
	s.mu.Unlock()
	if shouldCancel {
		s.cancel()
	}
}

func (s *DownloadSession) openReaderFile(cacheDir string) (*os.File, error) {
	s.mu.Lock()
	tmpPath := s.tmpPath
	finalPath := s.finalPath
	completed := s.completed
	s.mu.Unlock()

	if f, err := os.Open(tmpPath); err == nil {
		return f, nil
	} else if completed && finalPath != "" {
		return os.Open(finalPath)
	} else {
		fallbackFinal := filepath.Join(cacheDir, s.target.Owner, s.target.Repo, releaseTagsDirName, sanitizeTagDir(s.target.Tag), s.target.Asset)
		if f2, err2 := os.Open(fallbackFinal); err2 == nil {
			return f2, nil
		}
		return nil, err
	}
}

func (m *ReleaseCacheManager) streamSessionToClient(c *gin.Context, s *DownloadSession) bool {
	defer s.releaseSubscriber()

	select {
	case <-s.headersReady:
	case <-c.Request.Context().Done():
		return true
	}

	s.mu.Lock()
	bypass := s.bypass
	startErr := s.startErr
	ct := s.contentType
	cd := s.contentDisposition
	etag := s.etag
	cl := s.contentLength
	s.mu.Unlock()

	if bypass {
		c.Header("X-Cache", "BYPASS")
		return false
	}
	if startErr != nil {
		c.String(http.StatusBadGateway, fmt.Sprintf("上游下载失败: %v", startErr))
		return true
	}

	f, err := s.openReaderFile(m.dir)
	if err != nil {
		c.String(http.StatusInternalServerError, fmt.Sprintf("读取缓存临时文件失败: %v", err))
		return true
	}
	defer func() { _ = f.Close() }()

	if ct != "" {
		c.Header("Content-Type", ct)
	}
	if cd != "" {
		c.Header("Content-Disposition", cd)
	}
	if etag != "" {
		c.Header("ETag", etag)
	}
	if cl >= 0 {
		c.Header("Content-Length", strconv.FormatInt(cl, 10))
	}
	c.Header("X-Cache", "MISS")
	c.Status(http.StatusOK)

	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-c.Request.Context().Done():
			s.mu.Lock()
			s.cond.Broadcast()
			s.mu.Unlock()
		case <-stopWatch:
		}
	}()

	buf := make([]byte, 64*1024)
	var offset int64

	for {
		s.mu.Lock()
		for offset >= s.written && !s.completed && s.streamErr == nil && c.Request.Context().Err() == nil {
			s.cond.Wait()
		}
		available := s.written
		completed := s.completed
		streamErr := s.streamErr
		clientErr := c.Request.Context().Err()
		s.mu.Unlock()

		if clientErr != nil {
			return true
		}

		for offset < available {
			toRead := int64(len(buf))
			if available-offset < toRead {
				toRead = available - offset
			}
			n, readErr := f.ReadAt(buf[:toRead], offset)
			if n > 0 {
				if _, writeErr := c.Writer.Write(buf[:n]); writeErr != nil {
					return true
				}
				offset += int64(n)
			}
			if readErr != nil && readErr != io.EOF {
				return true
			}
		}

		if flusher, ok := c.Writer.(http.Flusher); ok && !completed {
			flusher.Flush()
		}

		if completed && offset >= available {
			return true
		}
		if streamErr != nil {
			return true
		}
	}
}

func sanitizeTagDir(tag string) string {
	var b strings.Builder
	for _, r := range tag {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= maxTagDirPrefixLength {
			break
		}
	}
	prefix := strings.Trim(b.String(), "._-")
	if prefix == "" {
		prefix = "tag"
	}
	sum := sha256.Sum256([]byte(tag))
	return prefix + "-" + hex.EncodeToString(sum[:4])
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp-" + randomHex(4)
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func parseMaxTotalBytes(cfg *config.AppConfig) int64 {
	v, err := config.ParseByteSize(cfg.ReleaseCache.MaxTotalSize)
	if err != nil || v < 0 {
		return 0
	}
	return v
}

func effectiveMaxCacheFileBytes(cfg *config.AppConfig) int64 {
	maxFile := cfg.Server.FileSize
	if cfg.ReleaseCache.MaxFileSize != "" {
		if parsed, err := config.ParseByteSize(cfg.ReleaseCache.MaxFileSize); err == nil && parsed > 0 {
			if maxFile <= 0 || parsed < maxFile {
				maxFile = parsed
			}
		}
	}
	return maxFile
}

func parseLatestTTL(cfg *config.AppConfig) time.Duration {
	if strings.TrimSpace(cfg.ReleaseCache.LatestTTL) == "" {
		return 10 * time.Minute
	}
	d, err := time.ParseDuration(strings.TrimSpace(cfg.ReleaseCache.LatestTTL))
	if err != nil || d < 0 {
		return 10 * time.Minute
	}
	return d
}
