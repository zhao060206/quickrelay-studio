package updater

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"quickrelay-studio/internal/config"
	"quickrelay-studio/internal/version"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2.1.0", "2.0.0", 1},
		{"v2.1.0", "2.1.0", 0},
		{"V2.10.0", "2.2.0", 1},
		{"2.1.0-rc.1", "2.1.0", -1},
		{"2.1.0", "2.1.0-beta.2", 1},
		{"2.0.0", "2.1.0", -1},
	}
	for _, tc := range cases {
		if got := CompareVersions(tc.a, tc.b); got != tc.want {
			t.Fatalf("CompareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestCheckAutoFallbackAndModes(t *testing.T) {
	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream timeout", http.StatusBadGateway)
	}))
	defer failServer.Close()

	cnManifest := ReleaseManifest{
		Version:      "9.9.0",
		Tag:          "v9.9.0",
		PublishedAt:  "2026-11-26T00:00:00Z",
		ReleaseNotes: "新增更新降级测试说明",
		ReleaseURL:   "https://github.com/zhao060206/quickrelay-studio/releases/tag/v9.9.0",
		Assets: map[string]ManifestAsset{
			"windows_amd64": {
				Name:   "QuickRelay-Studio-v9.9.0-win-x64.zip",
				Size:   1024,
				SHA256: stringsRepeat("a", 64),
				URL:    "https://github.com/zhao060206/quickrelay-studio/releases/download/v9.9.0/QuickRelay-Studio-v9.9.0-win-x64.zip",
			},
		},
	}
	cnServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(cnManifest)
	}))
	defer cnServer.Close()

	cfg := config.Default()
	mgr := NewManager(&cfg, t.TempDir())
	mgr.customCheckSpecs = func(repo, mode, customMirror string) []CheckSourceSpec {
		gh := CheckSourceSpec{Name: "github-api", Kind: "github", Type: "api", URL: failServer.URL}
		cn := CheckSourceSpec{Name: "gh-proxy", Kind: "cn", Type: "manifest", URL: cnServer.URL}
		switch mode {
		case "cn":
			return []CheckSourceSpec{cn, gh}
		case "github":
			return []CheckSourceSpec{gh}
		default:
			return []CheckSourceSpec{gh, cn}
		}
	}

	// 1. auto 模式下官方源失败自动降级国内源
	resAuto, err := mgr.Check(context.Background(), "auto")
	if err != nil {
		t.Fatalf("Check(auto) unexpected error: %v", err)
	}
	if !resAuto.FallbackUsed || resAuto.SourceKind != "cn" || resAuto.SourceUsed != "gh-proxy" {
		t.Fatalf("expected fallback to cn gh-proxy, got %+v", resAuto)
	}
	if !resAuto.HasUpdate || resAuto.LatestVersion != "9.9.0" || resAuto.CurrentVersion != version.Current {
		t.Fatalf("unexpected version result: %+v", resAuto)
	}

	// 2. cn 模式下直接命中国内源且无降级
	resCN, err := mgr.Check(context.Background(), "cn")
	if err != nil {
		t.Fatalf("Check(cn) unexpected error: %v", err)
	}
	if resCN.FallbackUsed || resCN.SourceKind != "cn" {
		t.Fatalf("expected direct cn hit without fallback, got %+v", resCN)
	}

	// 3. github 模式下官方失败不降级
	if _, err := mgr.Check(context.Background(), "github"); err == nil {
		t.Fatalf("expected error in github-only mode when github fails")
	}
}

func makeTestZipBytes(t *testing.T, files map[string]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	raw := buf.Bytes()
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:])
}

func waitDownloadDone(t *testing.T, mgr *Manager) DownloadStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st := mgr.Status()
		if st.State == "ready" || st.State == "error" {
			return st
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting download status, last=%+v", mgr.Status())
	return DownloadStatus{}
}

func TestStartDownloadFallbackVerifyAndExtract(t *testing.T) {
	zipBytes, expectedSHA := makeTestZipBytes(t, map[string]string{
		"QuickRelay-Studio/QuickRelay-Studio.exe": "desktop-bin",
		"QuickRelay-Studio/quickrelay.exe":        "core-bin",
		"QuickRelay-Studio/README.md":             "hello",
	})

	failDL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "blocked", http.StatusForbidden)
	}))
	defer failDL.Close()

	okDL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(zipBytes)
	}))
	defer okDL.Close()

	cfg := config.Default()
	dataDir := t.TempDir()
	mgr := NewManager(&cfg, dataDir)
	mgr.allowInsecureHTTP = true
	mgr.customDownloadFn = func(rawURL, mode, customMirror string) []downloadCandidate {
		return []downloadCandidate{
			{Name: "github-release", Kind: "github", URL: failDL.URL},
			{Name: "gh-proxy", Kind: "cn", URL: okDL.URL},
		}
	}

	if _, err := mgr.StartDownload(DownloadRequest{
		Tag:         "v2.2.0",
		DownloadURL: okDL.URL + "/pkg.zip",
		AssetName:   "QuickRelay-Studio-v2.2.0-win-x64.zip",
		SHA256:      expectedSHA,
		Mode:        "auto",
	}); err != nil {
		t.Fatalf("StartDownload failed: %v", err)
	}

	st := waitDownloadDone(t, mgr)
	if st.State != "ready" {
		t.Fatalf("expected state=ready, got %+v", st)
	}
	if !st.FallbackUsed || st.SourceUsed != "gh-proxy" || st.ActualSHA256 != expectedSHA {
		t.Fatalf("unexpected download status: %+v", st)
	}
	// 验证顶层目录 QuickRelay-Studio/ 已被自动剥离
	if _, err := os.Stat(filepath.Join(st.StagingDir, "QuickRelay-Studio.exe")); err != nil {
		t.Fatalf("expected QuickRelay-Studio.exe in staging root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(st.StagingDir, "quickrelay.exe")); err != nil {
		t.Fatalf("expected quickrelay.exe in staging root: %v", err)
	}

	// 验证 SHA-256 不匹配时严格拒绝安装
	mgr2 := NewManager(&cfg, t.TempDir())
	mgr2.allowInsecureHTTP = true
	mgr2.customDownloadFn = func(rawURL, mode, customMirror string) []downloadCandidate {
		return []downloadCandidate{{Name: "gh-proxy", Kind: "cn", URL: okDL.URL}}
	}
	if _, err := mgr2.StartDownload(DownloadRequest{
		Tag:         "v2.2.0",
		DownloadURL: okDL.URL + "/pkg.zip",
		AssetName:   "bad.zip",
		SHA256:      stringsRepeat("0", 64),
	}); err != nil {
		t.Fatalf("StartDownload bad hash start failed: %v", err)
	}
	st2 := waitDownloadDone(t, mgr2)
	if st2.State != "error" {
		t.Fatalf("expected error on sha256 mismatch, got %+v", st2)
	}
}

func stringsRepeat(ch string, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = ch[0]
	}
	return string(b)
}
