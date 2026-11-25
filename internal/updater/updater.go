// Package updater 实现内置 GitHub 版本检查、国内加速源自动降级、SHA-256 校验及安全解压。
package updater

import (
	"archive/zip"
	"context"
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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"quickrelay-studio/internal/config"
	"quickrelay-studio/internal/version"
)

const (
	checkPerSourceTimeout    = 8 * time.Second
	downloadPerSourceTimeout = 15 * time.Second
	maxManifestBytes         = 2 << 20
	maxSHA256SumsBytes       = 256 << 10
	maxDownloadBytes         = 256 << 20
)

var sha256HexRe = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)

// ManifestAsset 描述 release-manifest.json 中单个平台的发行包元数据。
type ManifestAsset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	URL    string `json:"url"`
}

// ReleaseManifest 为仓库根目录 release-manifest.json 的标准结构，
// 可通过 GitHub Raw、jsDelivr CDN 与国内镜像快速分发。
type ReleaseManifest struct {
	Version      string                   `json:"version"`
	Tag          string                   `json:"tag"`
	PublishedAt  string                   `json:"published_at"`
	ReleaseNotes string                   `json:"release_notes"`
	ReleaseURL   string                   `json:"release_url"`
	Assets       map[string]ManifestAsset `json:"assets"`
}

// CheckSourceSpec 描述单个版本检查源。
type CheckSourceSpec struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // "github" | "cn"
	Type string `json:"type"` // "api" | "manifest"
	URL  string `json:"url"`
}

// SourceAttempt 记录更新检查或下载过程中每个源的尝试结果。
type SourceAttempt struct {
	Source    string `json:"source"`
	Kind      string `json:"kind"`
	URL       string `json:"url"`
	OK        bool   `json:"ok"`
	ElapsedMs int64  `json:"elapsed_ms"`
	Error     string `json:"error,omitempty"`
}

// CheckResult 返回版本检查的完整结论及本次命中的源链路。
type CheckResult struct {
	CurrentVersion string          `json:"current_version"`
	LatestVersion  string          `json:"latest_version"`
	Tag            string          `json:"tag"`
	HasUpdate      bool            `json:"has_update"`
	PublishedAt    string          `json:"published_at"`
	ReleaseNotes   string          `json:"release_notes"`
	ReleaseURL     string          `json:"release_url"`
	AssetName      string          `json:"asset_name"`
	AssetSize      int64           `json:"asset_size"`
	SHA256         string          `json:"sha256"`
	DownloadURL    string          `json:"download_url"`
	SourceUsed     string          `json:"source_used"`
	SourceKind     string          `json:"source_kind"` // "github" | "cn"
	FallbackUsed   bool            `json:"fallback_used"`
	ElapsedMs      int64           `json:"elapsed_ms"`
	Mode           string          `json:"mode"`
	Repo           string          `json:"repo"`
	Attempts       []SourceAttempt `json:"attempts"`
}

// DownloadRequest 指定待下载并校验的新版安装包参数。
type DownloadRequest struct {
	Tag         string `json:"tag"`
	DownloadURL string `json:"download_url"`
	AssetName   string `json:"asset_name"`
	SHA256      string `json:"sha256"`
	Mode        string `json:"mode"`
}

// DownloadStatus 表示当前更新下载/校验/解压状态快照。
type DownloadStatus struct {
	State           string          `json:"state"` // idle | downloading | verifying | extracting | ready | error
	Tag             string          `json:"tag,omitempty"`
	AssetName       string          `json:"asset_name,omitempty"`
	DownloadURL     string          `json:"download_url,omitempty"`
	SourceUsed      string          `json:"source_used,omitempty"`
	SourceKind      string          `json:"source_kind,omitempty"`
	FallbackUsed    bool            `json:"fallback_used,omitempty"`
	DownloadedBytes int64           `json:"downloaded_bytes"`
	TotalBytes      int64           `json:"total_bytes"`
	Percent         float64         `json:"percent"`
	ExpectedSHA256  string          `json:"expected_sha256,omitempty"`
	ActualSHA256    string          `json:"actual_sha256,omitempty"`
	ZipPath         string          `json:"zip_path,omitempty"`
	StagingDir      string          `json:"staging_dir,omitempty"`
	Error           string          `json:"error,omitempty"`
	Attempts        []SourceAttempt `json:"attempts,omitempty"`
	UpdatedAt       int64           `json:"updated_at"`
}

type downloadCandidate struct {
	Name string
	Kind string
	URL  string
}

// Manager 管理更新检查、国内源自动降级、异步下载及 SHA-256 校验解压。
type Manager struct {
	cfg        *config.Config
	updatesDir string
	client     *http.Client

	// 测试钩子：覆盖源构建策略及放宽本地回环 HTTP 限制
	allowInsecureHTTP bool
	customCheckSpecs  func(repo, mode, customMirror string) []CheckSourceSpec
	customDownloadFn  func(url, mode, customMirror string) []downloadCandidate

	mu     sync.Mutex
	status DownloadStatus
	last   *CheckResult
}

// NewManager 创建更新管理器实例，updatesDir 通常位于 data/updates。
func NewManager(cfg *config.Config, dataDir string) *Manager {
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSHandshakeTimeout: 8 * time.Second,
	}
	updatesDir := filepath.Join(strings.TrimSpace(dataDir), "updates")
	return &Manager{
		cfg:        cfg,
		updatesDir: updatesDir,
		client:     &http.Client{Transport: tr},
		status: DownloadStatus{
			State:     "idle",
			UpdatedAt: time.Now().Unix(),
		},
	}
}

func (m *Manager) repoAndMode(modeOverride string) (repo, mode, mirror string) {
	if m.cfg != nil {
		repo = strings.TrimSpace(m.cfg.Updater.Repo)
		mode = strings.ToLower(strings.TrimSpace(m.cfg.Updater.Mode))
		mirror = strings.TrimRight(strings.TrimSpace(m.cfg.Updater.CustomMirror), "/")
	}
	if repo == "" {
		repo = version.DefaultRepo
	}
	if strings.TrimSpace(modeOverride) != "" {
		mode = strings.ToLower(strings.TrimSpace(modeOverride))
	}
	switch mode {
	case "auto", "cn", "github":
	default:
		mode = "auto"
	}
	return repo, mode, mirror
}

// NormalizeVersion 提取规范化数字版本（保留预发布后缀供比较）。
func NormalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
	if idx := strings.IndexByte(v, '+'); idx >= 0 {
		v = v[:idx]
	}
	return strings.TrimSpace(v)
}

func parseSemverParts(v string) (nums []int, prerelease string) {
	norm := NormalizeVersion(v)
	if idx := strings.IndexByte(norm, '-'); idx >= 0 {
		prerelease = norm[idx+1:]
		norm = norm[:idx]
	}
	segments := strings.Split(norm, ".")
	nums = make([]int, len(segments))
	for i, seg := range segments {
		n, err := strconv.Atoi(strings.TrimSpace(seg))
		if err == nil && n >= 0 {
			nums[i] = n
		}
	}
	return nums, prerelease
}

// CompareVersions 按语义化版本比较 a 与 b：a > b 返回 1，a < b 返回 -1，相等返回 0。
func CompareVersions(a, b string) int {
	aNums, aPre := parseSemverParts(a)
	bNums, bPre := parseSemverParts(b)
	maxLen := len(aNums)
	if len(bNums) > maxLen {
		maxLen = len(bNums)
	}
	for i := 0; i < maxLen; i++ {
		av, bv := 0, 0
		if i < len(aNums) {
			av = aNums[i]
		}
		if i < len(bNums) {
			bv = bNums[i]
		}
		if av > bv {
			return 1
		}
		if av < bv {
			return -1
		}
	}
	if aPre == "" && bPre != "" {
		return 1
	}
	if aPre != "" && bPre == "" {
		return -1
	}
	if aPre < bPre {
		return -1
	}
	if aPre > bPre {
		return 1
	}
	return 0
}

// buildCheckSpecs 按模式顺序构造官方源与国内加速源探测链。
func buildCheckSpecs(repo, mode, customMirror string) []CheckSourceSpec {
	official := []CheckSourceSpec{
		{
			Name: "github-api",
			Kind: "github",
			Type: "api",
			URL:  fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo),
		},
		{
			Name: "github-raw",
			Kind: "github",
			Type: "manifest",
			URL:  fmt.Sprintf("https://raw.githubusercontent.com/%s/main/release-manifest.json", repo),
		},
	}

	rawURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/main/release-manifest.json", repo)
	var cn []CheckSourceSpec
	if customMirror != "" {
		cn = append(cn, CheckSourceSpec{
			Name: "custom-mirror",
			Kind: "cn",
			Type: "manifest",
			URL:  strings.TrimRight(customMirror, "/") + "/" + rawURL,
		})
	}
	cn = append(cn,
		CheckSourceSpec{
			Name: "gh-proxy",
			Kind: "cn",
			Type: "manifest",
			URL:  "https://gh-proxy.com/" + rawURL,
		},
		CheckSourceSpec{
			Name: "ghfast",
			Kind: "cn",
			Type: "manifest",
			URL:  "https://ghfast.top/" + rawURL,
		},
		CheckSourceSpec{
			Name: "gh-proxy-org",
			Kind: "cn",
			Type: "manifest",
			URL:  "https://gh-proxy.org/" + rawURL,
		},
		CheckSourceSpec{
			Name: "jsdelivr-cdn",
			Kind: "cn",
			Type: "manifest",
			URL:  fmt.Sprintf("https://cdn.jsdelivr.net/gh/%s@main/release-manifest.json", repo),
		},
		CheckSourceSpec{
			Name: "jsdelivr-fastly",
			Kind: "cn",
			Type: "manifest",
			URL:  fmt.Sprintf("https://fastly.jsdelivr.net/gh/%s@main/release-manifest.json", repo),
		},
	)

	switch mode {
	case "cn":
		return append(cn, official...)
	case "github":
		return official
	default: // auto
		return append(official, cn...)
	}
}

func (m *Manager) buildDownloadCandidates(rawURL, mode, customMirror string) []downloadCandidate {
	if m.customDownloadFn != nil {
		return m.customDownloadFn(rawURL, mode, customMirror)
	}
	direct := downloadCandidate{Name: "github-release", Kind: "github", URL: rawURL}
	var cn []downloadCandidate
	if customMirror != "" {
		cn = append(cn, downloadCandidate{
			Name: "custom-mirror",
			Kind: "cn",
			URL:  strings.TrimRight(customMirror, "/") + "/" + rawURL,
		})
	}
	cn = append(cn,
		downloadCandidate{Name: "gh-proxy", Kind: "cn", URL: "https://gh-proxy.com/" + rawURL},
		downloadCandidate{Name: "ghfast", Kind: "cn", URL: "https://ghfast.top/" + rawURL},
		downloadCandidate{Name: "gh-proxy-org", Kind: "cn", URL: "https://gh-proxy.org/" + rawURL},
	)

	switch mode {
	case "cn":
		return append(cn, direct)
	case "github":
		return []downloadCandidate{direct}
	default:
		return append([]downloadCandidate{direct}, cn...)
	}
}

func (m *Manager) fetchSmallBody(ctx context.Context, rawURL string, accept string, maxBytes int64) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, checkPerSourceTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "QuickRelay-Studio/"+version.Current)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("response too large (>%d bytes)", maxBytes)
	}
	return body, nil
}

type githubReleaseAsset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Digest             string `json:"digest"`
}

type githubReleasePayload struct {
	TagName     string               `json:"tag_name"`
	Name        string               `json:"name"`
	Body        string               `json:"body"`
	PublishedAt string               `json:"published_at"`
	HTMLURL     string               `json:"html_url"`
	Assets      []githubReleaseAsset `json:"assets"`
}

func parseSHA256Sums(content, targetAsset string) string {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		hash := strings.TrimSpace(fields[0])
		file := strings.TrimPrefix(strings.TrimSpace(fields[len(fields)-1]), "*")
		if sha256HexRe.MatchString(hash) && (targetAsset == "" || strings.EqualFold(filepath.Base(file), filepath.Base(targetAsset))) {
			return strings.ToLower(hash)
		}
	}
	return ""
}

func (m *Manager) checkFromAPI(ctx context.Context, spec CheckSourceSpec) (*CheckResult, error) {
	body, err := m.fetchSmallBody(ctx, spec.URL, "application/vnd.github+json", maxManifestBytes)
	if err != nil {
		return nil, err
	}
	var rel githubReleasePayload
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, fmt.Errorf("invalid github api json: %w", err)
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return nil, errors.New("missing tag_name in github release")
	}

	var chosen *githubReleaseAsset
	var shaURL string
	for i := range rel.Assets {
		a := &rel.Assets[i]
		lower := strings.ToLower(a.Name)
		if strings.HasSuffix(lower, ".zip") {
			if chosen == nil || strings.Contains(lower, "win") {
				chosen = a
			}
		} else if lower == "sha256sums.txt" || strings.HasSuffix(lower, ".sha256") {
			shaURL = a.BrowserDownloadURL
		}
	}

	res := &CheckResult{
		CurrentVersion: version.Current,
		LatestVersion:  NormalizeVersion(rel.TagName),
		Tag:            strings.TrimSpace(rel.TagName),
		PublishedAt:    strings.TrimSpace(rel.PublishedAt),
		ReleaseNotes:   strings.TrimSpace(rel.Body),
		ReleaseURL:     strings.TrimSpace(rel.HTMLURL),
	}
	if chosen != nil {
		res.AssetName = chosen.Name
		res.AssetSize = chosen.Size
		res.DownloadURL = chosen.BrowserDownloadURL
		if strings.HasPrefix(strings.ToLower(chosen.Digest), "sha256:") {
			cand := strings.TrimPrefix(strings.ToLower(chosen.Digest), "sha256:")
			if sha256HexRe.MatchString(cand) {
				res.SHA256 = cand
			}
		}
	}
	if res.SHA256 == "" && shaURL != "" && res.AssetName != "" {
		if sumBody, sErr := m.fetchSmallBody(ctx, shaURL, "text/plain", maxSHA256SumsBytes); sErr == nil {
			res.SHA256 = parseSHA256Sums(string(sumBody), res.AssetName)
		}
	}
	res.HasUpdate = CompareVersions(res.LatestVersion, version.Current) > 0
	return res, nil
}

func (m *Manager) checkFromManifest(ctx context.Context, spec CheckSourceSpec) (*CheckResult, error) {
	body, err := m.fetchSmallBody(ctx, spec.URL, "application/json", maxManifestBytes)
	if err != nil {
		return nil, err
	}
	var mf ReleaseManifest
	if err := json.Unmarshal(body, &mf); err != nil {
		return nil, fmt.Errorf("invalid manifest json: %w", err)
	}
	latestVer := NormalizeVersion(mf.Version)
	if latestVer == "" {
		latestVer = NormalizeVersion(mf.Tag)
	}
	if latestVer == "" {
		return nil, errors.New("manifest missing version")
	}
	tag := strings.TrimSpace(mf.Tag)
	if tag == "" {
		tag = "v" + latestVer
	}
	asset := mf.Assets["windows_amd64"]
	if asset.Name == "" {
		for _, cand := range mf.Assets {
			asset = cand
			break
		}
	}
	res := &CheckResult{
		CurrentVersion: version.Current,
		LatestVersion:  latestVer,
		Tag:            tag,
		HasUpdate:      CompareVersions(latestVer, version.Current) > 0,
		PublishedAt:    strings.TrimSpace(mf.PublishedAt),
		ReleaseNotes:   strings.TrimSpace(mf.ReleaseNotes),
		ReleaseURL:     strings.TrimSpace(mf.ReleaseURL),
		AssetName:      strings.TrimSpace(asset.Name),
		AssetSize:      asset.Size,
		SHA256:         strings.ToLower(strings.TrimSpace(asset.SHA256)),
		DownloadURL:    strings.TrimSpace(asset.URL),
	}
	return res, nil
}

// Check 检查最新发布版本。在 auto 模式下官方源不可用时自动降级到国内加速源。
func (m *Manager) Check(ctx context.Context, modeOverride string) (*CheckResult, error) {
	repo, mode, mirror := m.repoAndMode(modeOverride)
	var specs []CheckSourceSpec
	if m.customCheckSpecs != nil {
		specs = m.customCheckSpecs(repo, mode, mirror)
	} else {
		specs = buildCheckSpecs(repo, mode, mirror)
	}
	if len(specs) == 0 {
		return nil, errors.New("no update sources configured")
	}

	overallStart := time.Now()
	attempts := make([]SourceAttempt, 0, len(specs))
	var finalRes *CheckResult
	var lastErr error

	for _, spec := range specs {
		stepStart := time.Now()
		var res *CheckResult
		var err error
		if spec.Type == "api" {
			res, err = m.checkFromAPI(ctx, spec)
		} else {
			res, err = m.checkFromManifest(ctx, spec)
		}
		elapsed := time.Since(stepStart).Milliseconds()
		attempt := SourceAttempt{
			Source:    spec.Name,
			Kind:      spec.Kind,
			URL:       spec.URL,
			OK:        err == nil,
			ElapsedMs: elapsed,
		}
		if err != nil {
			attempt.Error = err.Error()
			attempts = append(attempts, attempt)
			lastErr = err
			continue
		}
		attempts = append(attempts, attempt)
		finalRes = res
		finalRes.SourceUsed = spec.Name
		finalRes.SourceKind = spec.Kind
		break
	}

	if finalRes == nil {
		return nil, &CheckFailedError{
			Mode:     mode,
			Repo:     repo,
			Attempts: attempts,
			Err:      lastErr,
		}
	}

	fallbackUsed := false
	for _, a := range attempts[:len(attempts)-1] {
		if !a.OK && a.Kind != finalRes.SourceKind {
			fallbackUsed = true
			break
		}
	}
	finalRes.FallbackUsed = fallbackUsed
	finalRes.ElapsedMs = time.Since(overallStart).Milliseconds()
	finalRes.Mode = mode
	finalRes.Repo = repo
	finalRes.Attempts = attempts

	m.mu.Lock()
	m.last = finalRes
	m.mu.Unlock()
	return finalRes, nil
}

// CheckFailedError 包含所有源都失败时的逐跳排错明细。
type CheckFailedError struct {
	Mode     string
	Repo     string
	Attempts []SourceAttempt
	Err      error
}

func (e *CheckFailedError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("检查更新失败（模式=%s，已尝试 %d 个源）: %v", e.Mode, len(e.Attempts), e.Err)
	}
	return fmt.Sprintf("检查更新失败（模式=%s，已尝试 %d 个源）", e.Mode, len(e.Attempts))
}

// LastCheck 返回最近一次成功的版本检查快照。
func (m *Manager) LastCheck() *CheckResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last
}

// Status 返回当前下载/校验/解压状态快照的浅拷贝。
func (m *Manager) Status() DownloadStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.status
	if len(m.status.Attempts) > 0 {
		out.Attempts = append([]SourceAttempt(nil), m.status.Attempts...)
	}
	return out
}

func sanitizeSingleSegment(raw string, fallback string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		s = fallback
	}
	s = strings.ReplaceAll(s, "..", "_")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	res := strings.Trim(b.String(), "._")
	if res == "" {
		return fallback
	}
	return res
}

func (m *Manager) validateDownloadURL(rawURL string) error {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("invalid download url: %w", err)
	}
	if parsed.Host == "" {
		return errors.New("download url missing host")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if m.allowInsecureHTTP && parsed.Scheme == "http" {
		return nil
	}
	return errors.New("download url must use https://")
}

// StartDownload 异步启动安装包下载、自动降级国内源、核验 SHA-256 并解压至 staging 目录。
func (m *Manager) StartDownload(req DownloadRequest) (DownloadStatus, error) {
	if err := m.validateDownloadURL(req.DownloadURL); err != nil {
		return DownloadStatus{}, err
	}
	expectedSHA := strings.ToLower(strings.TrimSpace(req.SHA256))
	if expectedSHA != "" && !sha256HexRe.MatchString(expectedSHA) {
		return DownloadStatus{}, errors.New("sha256 must be 64 hex characters")
	}

	safeTag := sanitizeSingleSegment(req.Tag, "latest")
	assetBase := filepath.Base(strings.TrimSpace(req.AssetName))
	if assetBase == "" || assetBase == "." || assetBase == "/" {
		assetBase = "QuickRelay-Studio-update.zip"
	}
	safeAsset := sanitizeSingleSegment(assetBase, "QuickRelay-Studio-update.zip")

	_, mode, mirror := m.repoAndMode(req.Mode)
	candidates := m.buildDownloadCandidates(strings.TrimSpace(req.DownloadURL), mode, mirror)
	if len(candidates) == 0 {
		return DownloadStatus{}, errors.New("no download sources available")
	}

	m.mu.Lock()
	switch m.status.State {
	case "downloading", "verifying", "extracting":
		cur := m.status
		m.mu.Unlock()
		return cur, errors.New("已有一个更新任务正在进行中")
	}
	m.status = DownloadStatus{
		State:          "downloading",
		Tag:            safeTag,
		AssetName:      safeAsset,
		DownloadURL:    strings.TrimSpace(req.DownloadURL),
		ExpectedSHA256: expectedSHA,
		UpdatedAt:      time.Now().Unix(),
	}
	snap := m.status
	m.mu.Unlock()

	go m.runDownloadWorkflow(safeTag, safeAsset, expectedSHA, candidates)
	return snap, nil
}

func (m *Manager) updateStatus(fn func(*DownloadStatus)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(&m.status)
	m.status.UpdatedAt = time.Now().Unix()
}

func (m *Manager) runDownloadWorkflow(safeTag, safeAsset, expectedSHA string, candidates []downloadCandidate) {
	tagDir := filepath.Join(m.updatesDir, safeTag)
	if err := os.MkdirAll(tagDir, 0o755); err != nil {
		m.updateStatus(func(s *DownloadStatus) {
			s.State = "error"
			s.Error = fmt.Sprintf("创建更新目录失败: %v", err)
		})
		return
	}

	zipPath := filepath.Join(tagDir, safeAsset)
	partPath := zipPath + ".part"
	stagingDir := filepath.Join(tagDir, "staging")

	var attempts []SourceAttempt
	var chosenCand downloadCandidate
	var actualSHA string
	var finalErr error

	for _, cand := range candidates {
		_ = os.Remove(partPath)
		m.updateStatus(func(s *DownloadStatus) {
			s.State = "downloading"
			s.SourceUsed = cand.Name
			s.SourceKind = cand.Kind
			s.DownloadedBytes = 0
			s.TotalBytes = 0
			s.Percent = 0
		})

		start := time.Now()
		gotSHA, dlErr := m.downloadOneCandidate(cand, partPath, expectedSHA)
		elapsed := time.Since(start).Milliseconds()

		attempt := SourceAttempt{
			Source:    cand.Name,
			Kind:      cand.Kind,
			URL:       cand.URL,
			OK:        dlErr == nil,
			ElapsedMs: elapsed,
		}
		if dlErr != nil {
			attempt.Error = dlErr.Error()
			attempts = append(attempts, attempt)
			finalErr = dlErr
			_ = os.Remove(partPath)
			continue
		}

		attempts = append(attempts, attempt)
		chosenCand = cand
		actualSHA = gotSHA
		finalErr = nil
		break
	}

	if finalErr != nil {
		m.updateStatus(func(s *DownloadStatus) {
			s.State = "error"
			s.Error = fmt.Sprintf("所有下载源均失败: %v", finalErr)
			s.Attempts = attempts
		})
		return
	}

	_ = os.Remove(zipPath)
	if err := os.Rename(partPath, zipPath); err != nil {
		_ = os.Remove(partPath)
		m.updateStatus(func(s *DownloadStatus) {
			s.State = "error"
			s.Error = fmt.Sprintf("保存安装包失败: %v", err)
			s.Attempts = attempts
		})
		return
	}

	m.updateStatus(func(s *DownloadStatus) {
		s.State = "extracting"
		s.ActualSHA256 = actualSHA
		s.ZipPath = zipPath
		s.Percent = 100
		s.Attempts = attempts
	})

	_ = os.RemoveAll(stagingDir)
	if err := extractZipSafely(zipPath, stagingDir); err != nil {
		_ = os.RemoveAll(stagingDir)
		m.updateStatus(func(s *DownloadStatus) {
			s.State = "error"
			s.Error = fmt.Sprintf("解压更新包失败: %v", err)
			s.Attempts = attempts
		})
		return
	}

	fallbackUsed := false
	for _, a := range attempts[:len(attempts)-1] {
		if !a.OK && a.Kind != chosenCand.Kind {
			fallbackUsed = true
			break
		}
	}

	m.updateStatus(func(s *DownloadStatus) {
		s.State = "ready"
		s.SourceUsed = chosenCand.Name
		s.SourceKind = chosenCand.Kind
		s.FallbackUsed = fallbackUsed
		s.ActualSHA256 = actualSHA
		s.ZipPath = zipPath
		s.StagingDir = stagingDir
		s.Percent = 100
		s.Error = ""
		s.Attempts = attempts
	})
}

func (m *Manager) downloadOneCandidate(cand downloadCandidate, partPath, expectedSHA string) (string, error) {
	hdrCtx, hdrCancel := context.WithTimeout(context.Background(), downloadPerSourceTimeout)
	req, err := http.NewRequestWithContext(hdrCtx, http.MethodGet, cand.URL, nil)
	if err != nil {
		hdrCancel()
		return "", err
	}
	req.Header.Set("User-Agent", "QuickRelay-Studio/"+version.Current)

	// 仅对建连与首包响应头应用 15s 超时；正文大文件下载阶段单独控制。
	resp, err := m.client.Do(req)
	if err != nil {
		hdrCancel()
		return "", err
	}
	hdrCancel()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	outFile, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	defer outFile.Close()

	total := resp.ContentLength
	m.updateStatus(func(s *DownloadStatus) {
		s.TotalBytes = total
	})

	hasher := sha256.New()
	buf := make([]byte, 64*1024)
	var written int64
	lastReport := time.Now()

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			written += int64(n)
			if written > maxDownloadBytes {
				return "", fmt.Errorf("安装包体积超过上限 (%d bytes)", maxDownloadBytes)
			}
			if _, wErr := outFile.Write(buf[:n]); wErr != nil {
				return "", wErr
			}
			_, _ = hasher.Write(buf[:n])
			if time.Since(lastReport) >= 120*time.Millisecond {
				lastReport = time.Now()
				curWritten := written
				m.updateStatus(func(s *DownloadStatus) {
					s.DownloadedBytes = curWritten
					if total > 0 {
						s.Percent = float64(curWritten) * 100.0 / float64(total)
					}
				})
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	if err := outFile.Sync(); err != nil {
		return "", err
	}

	actualSHA := hex.EncodeToString(hasher.Sum(nil))
	m.updateStatus(func(s *DownloadStatus) {
		s.State = "verifying"
		s.DownloadedBytes = written
		if total <= 0 {
			s.TotalBytes = written
		}
		s.Percent = 100
		s.ActualSHA256 = actualSHA
	})

	if expectedSHA != "" && !strings.EqualFold(actualSHA, expectedSHA) {
		return "", fmt.Errorf("SHA-256 校验失败: expected=%s got=%s", expectedSHA, actualSHA)
	}
	return actualSHA, nil
}

// extractZipSafely 安全解压 ZIP 至 destDir，防御 ZipSlip 并自动剥离单一顶层包裹目录。
func extractZipSafely(zipPath, destDir string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()

	// 检测所有非空条目是否共享同一个顶层目录前缀（例如 "QuickRelay-Studio/"）
	commonTop := detectCommonTopDir(r.File)

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}

	for _, f := range r.File {
		rel := filepath.ToSlash(strings.TrimLeft(f.Name, `/\`))
		if commonTop != "" {
			if rel == commonTop || rel == commonTop+"/" {
				continue
			}
			rel = strings.TrimPrefix(rel, commonTop+"/")
		}
		rel = strings.TrimSpace(rel)
		if rel == "" || rel == "." {
			continue
		}
		cleanRel := filepath.Clean(filepath.FromSlash(rel))
		if cleanRel == "." || strings.HasPrefix(cleanRel, "..") || filepath.IsAbs(cleanRel) {
			return fmt.Errorf("非法压缩条目路径: %s", f.Name)
		}
		targetPath := filepath.Join(absDest, cleanRel)
		if !strings.HasPrefix(targetPath, absDest+string(filepath.Separator)) && targetPath != absDest {
			return fmt.Errorf("压缩条目越界: %s", f.Name)
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(targetPath, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return err
		}
		if err := copyZipEntry(f, targetPath); err != nil {
			return err
		}
	}
	return nil
}

func detectCommonTopDir(files []*zip.File) string {
	candidate := ""
	hasFile := false
	for _, f := range files {
		p := strings.Trim(filepath.ToSlash(f.Name), "/")
		if p == "" {
			continue
		}
		parts := strings.Split(p, "/")
		if len(parts) == 1 && !f.FileInfo().IsDir() {
			// 根目录直接存在文件，不剥离
			return ""
		}
		if candidate == "" {
			candidate = parts[0]
		} else if parts[0] != candidate {
			return ""
		}
		if !f.FileInfo().IsDir() {
			hasFile = true
		}
	}
	if !hasFile {
		return ""
	}
	return candidate
}

func copyZipEntry(f *zip.File, targetPath string) error {
	src, err := f.Open()
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer dst.Close()

	_, err = io.Copy(dst, io.LimitReader(src, maxDownloadBytes))
	return err
}
