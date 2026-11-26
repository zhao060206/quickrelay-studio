package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quickrelay-studio/internal/accounts"
	"quickrelay-studio/internal/config"
	"quickrelay-studio/internal/store"
)

type capturedUpload struct {
	Name        string
	ContentType string
	Body        []byte
}

func startMockAttachmentTenant(t *testing.T, failOn func(name string, seq int32) bool) (*httptest.Server, *[]capturedUpload, *sync.Mutex) {
	t.Helper()
	var seq atomic.Int32
	var mu sync.Mutex
	var uploads []capturedUpload

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/integration/quick-work/upload-attachment":
			n := seq.Add(1)
			name := r.URL.Query().Get("name")
			ctype := r.URL.Query().Get("contentType")
			data, _ := io.ReadAll(r.Body)
			if failOn != nil && failOn(name, n) {
				http.Error(w, "simulated upload failure", http.StatusInternalServerError)
				return
			}
			mu.Lock()
			uploads = append(uploads, capturedUpload{Name: name, ContentType: ctype, Body: data})
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"attachmentId": fmt.Sprintf("att-%d", n),
				"path":         fmt.Sprintf("/data/s3files/users/u1/sessions/s1/workspace/attached_files/%s", name),
			})
		case "/integration/quick-work/attachment-dlp-status":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"dlpJobStatus": "COMPLETED",
				"policyAction": "ALLOW",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &uploads, &mu
}

func assertNoFrameExceeded(t *testing.T, turns []detailedCapturedTurn) {
	t.Helper()
	for i, tr := range turns {
		if tr.RawFrameBytes > maxUserMessageBytes {
			t.Fatalf("turn %d raw user_message frame %d bytes exceeds limit %d", i, tr.RawFrameBytes, maxUserMessageBytes)
		}
	}
}

// TestFrameHistorySplit_VerbatimCurrentTurnAndRepeatedText 验证 P1 核心场景（OpenAI & Anthropic）：
// 1) 普通短消息：0 次附件上传，原文一致；
// 2) 长历史 + 超 1,200 字节中文且含引号/反斜杠/换行的当前提问：附件只有历史，当前轮逐字完整留在正文，帧 <= 64,512 字节；
// 3) 重复文本（相同句子出现在历史、代码块、工具结果和当前轮）：严格按消息边界切分，不误切；
// 4) 纯工具结果与多条并行工具结果轮：附件只有历史，正文按顺序保留全部 Tool result 及关联，不复读旧提问。
func TestFrameHistorySplit_VerbatimCurrentTurnAndRepeatedText(t *testing.T) {
	wsSrv, _, captured, capMu := startDetailedMockUpstream(t, func(attempt int32, r *http.Request, msgType string, payload map[string]any) (string, map[string]any) {
		return fmt.Sprintf("done-%d", attempt), nil
	})
	tenantSrv, uploads, upMu := startMockAttachmentTenant(t, nil)

	dbPath := filepath.Join(t.TempDir(), "p1_split.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	if err := st.Save(context.Background(), &store.Account{
		Email: "p1@example.com", Provider: "social",
		TenantURL: tenantSrv.URL,
		WSHost:    "ws://" + strings.TrimPrefix(wsSrv.URL, "http://"), WSPath: "/ws",
		AccessToken: "jwt-p1", ActorARN: "arn:aws:quick:us-east-1:p1", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("st.Save: %v", err)
	}

	cfg := config.Default()
	cfg.SessionReuse = true
	cfg.ToolMode = config.ToolModeTextBridge
	srv := NewServer(&cfg, accounts.New(st, &cfg))
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}

	// 场景 1：普通短消息（0 附件）
	code, _ := doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"Authorization": "Bearer k-short"}, map[string]any{
		"model":    "aws-quick-smart",
		"messages": []map[string]any{{"role": "user", "content": "短消息逐字测试"}},
	})
	if code != http.StatusOK {
		t.Fatalf("short turn status=%d", code)
	}
	upMu.Lock()
	if len(*uploads) != 0 {
		upMu.Unlock()
		t.Fatalf("short message should trigger 0 uploads, got %d", len(*uploads))
	}
	upMu.Unlock()

	// 场景 2：长历史 + >1200 字节中文且包含反斜杠、双引号、换行的当前提问 + 历史与代码里包含与当前轮重复的文本
	repeatedPhrase := "请精确保留这里的末尾校验句：ALPHA-9981"
	longHistoryFiller := strings.Repeat("历史记录片段包含背景资料、文档条目与系统说明，用于拉长总长度超出单帧 64KB 预算。\n", 950)
	historyMsgs := []map[string]any{
		{"role": "user", "content": "早期问题中提到过：" + repeatedPhrase + "\n" + longHistoryFiller},
		{"role": "assistant", "content": "```go\nconst note = \"" + repeatedPhrase + "\"\n```"},
	}
	longCurrentUser := "第一行带有Windows路径：`E:\\zy\\quickrelay-studio\\internal\\proxy\\upstream.go` 与 \"引号\" 转义。\n" +
		strings.Repeat("这是一段超过一千两百字节的复杂中文工作指令，要求按字逐句完整送达模型主帧正文而绝对不能从第1200字节处截断。", 25) +
		"\n" + repeatedPhrase + " ——【当前轮末尾收尾标记-END-999】"
	if len(longCurrentUser) <= 1200 {
		t.Fatalf("expected longCurrentUser > 1200 bytes, got %d", len(longCurrentUser))
	}

	msgsLong := append(append([]map[string]any(nil), historyMsgs...), map[string]any{"role": "user", "content": longCurrentUser})
	code, body := doProxyJSONCall(t, client, httpSrv.URL+"/v1/messages", map[string]string{"Authorization": "Bearer k-long"}, map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 256,
		"messages":   msgsLong,
	})
	if code != http.StatusOK {
		t.Fatalf("long history + long current turn Anthropic status=%d body=%s", code, body)
	}

	upMu.Lock()
	if len(*uploads) != 1 {
		upMu.Unlock()
		t.Fatalf("expected exactly 1 history upload, got %d", len(*uploads))
	}
	histUpload := string((*uploads)[0].Body)
	upMu.Unlock()

	if !strings.Contains(histUpload, "早期问题中提到过") || strings.Contains(histUpload, "【当前轮末尾收尾标记-END-999】") {
		t.Fatalf("history attachment must contain history only and NOT the current turn tail")
	}

	capMu.Lock()
	longTurn := (*captured)[len(*captured)-1]
	capMu.Unlock()
	if !strings.Contains(longTurn.Text, longCurrentUser) {
		t.Fatalf("main frame lost verbatim current turn (len=%d)", len(longCurrentUser))
	}
	if strings.Contains(longTurn.Text, "历史记录片段包含背景资料") {
		t.Fatalf("main frame unexpectedly kept huge history inline")
	}

	// 场景 3：纯工具结果与多条并行工具结果轮（验证 OpenAI & Anthropic 两个入口，不复读上轮提问）
	toolHistoryMsgs := []map[string]any{
		{"role": "user", "content": "旧的用户提问指令-OLD-PROMPT\n" + longHistoryFiller},
		{"role": "assistant", "content": "我将并行调用两个工具，文本中也提到 " + repeatedPhrase, "tool_calls": []map[string]any{
			{"id": "call_1", "type": "function", "function": map[string]any{"name": "read_a", "arguments": `{"p":"a.txt"}`}},
			{"id": "call_2", "type": "function", "function": map[string]any{"name": "read_b", "arguments": `{"p":"b.txt"}`}},
		}},
		{"role": "tool", "tool_call_id": "call_1", "name": "read_a", "content": "工具A完整结果：包含重复句 " + repeatedPhrase + " 与结尾-TOOL-A-OK"},
		{"role": "tool", "tool_call_id": "call_2", "name": "read_b", "content": "工具B完整结果：包含换行\n和结尾-TOOL-B-OK"},
	}
	code, body = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"Authorization": "Bearer k-tools"}, map[string]any{
		"model":    "aws-quick-smart",
		"messages": toolHistoryMsgs,
		"tools":    testDummyTools(),
	})
	if code != http.StatusOK {
		t.Fatalf("parallel tool results turn status=%d body=%s", code, body)
	}

	upMu.Lock()
	if len(*uploads) != 2 {
		upMu.Unlock()
		t.Fatalf("expected 2nd upload for parallel tool history, got %d", len(*uploads))
	}
	toolHistUpload := string((*uploads)[1].Body)
	upMu.Unlock()
	if !strings.Contains(toolHistUpload, "旧的用户提问指令-OLD-PROMPT") || strings.Contains(toolHistUpload, "结尾-TOOL-A-OK") || strings.Contains(toolHistUpload, "结尾-TOOL-B-OK") {
		t.Fatalf("tool history attachment polluted with current tool results or missing history")
	}

	capMu.Lock()
	toolTurn := (*captured)[len(*captured)-1]
	allTurns := append([]detailedCapturedTurn(nil), (*captured)...)
	capMu.Unlock()
	idxA := strings.Index(toolTurn.Text, "Tool result (call_id=call_1, name=read_a): 工具A完整结果")
	idxB := strings.Index(toolTurn.Text, "Tool result (call_id=call_2, name=read_b): 工具B完整结果")
	if idxA < 0 || idxB <= idxA || !strings.Contains(toolTurn.Text, "结尾-TOOL-B-OK") {
		t.Fatalf("main frame lost ordered tool results: %q", toolTurn.Text)
	}
	if strings.Contains(toolTurn.Text, "旧的用户提问指令-OLD-PROMPT") {
		t.Fatalf("tool result turn mistakenly echoed prior user question in main frame")
	}
	assertNoFrameExceeded(t, allTurns)
}

// TestFrameHistorySplit_ImagesNoDuplicateAndOversizedAndErrors 验证：
// 1) 图片混合：历史与当前轮含图片时图片不重复上传，历史 text 外置、当前轮与图片元数据保留；
// 2) 当轮本身单独超限：有上传能力时全文 100% 进附件、正文明确预览、帧不超限；上传失败明确报错且 0 帧发出；
// 3) 历史外置上传失败与无上传能力 gateway 超限时：明确报错且 0 帧发出，绝不静默截半。
func TestFrameHistorySplit_ImagesNoDuplicateAndOversizedAndErrors(t *testing.T) {
	wsSrv, _, captured, capMu := startDetailedMockUpstream(t, func(attempt int32, r *http.Request, msgType string, payload map[string]any) (string, map[string]any) {
		return fmt.Sprintf("img-ok-%d", attempt), nil
	})

	var failMode atomic.Int32 // 0=ok, 1=fail txt prompt upload
	tenantSrv, uploads, upMu := startMockAttachmentTenant(t, func(name string, seq int32) bool {
		return failMode.Load() == 1 && strings.HasPrefix(name, "prompt-")
	})

	dbPath := filepath.Join(t.TempDir(), "p1_edge.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	if err := st.Save(context.Background(), &store.Account{
		Email: "p1edge@example.com", Provider: "social",
		TenantURL: tenantSrv.URL,
		WSHost:    "ws://" + strings.TrimPrefix(wsSrv.URL, "http://"), WSPath: "/ws",
		AccessToken: "jwt-p1e", ActorARN: "arn:aws:quick:us-east-1:p1e", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("st.Save: %v", err)
	}

	cfg := config.Default()
	cfg.SessionReuse = true
	cfg.ToolMode = config.ToolModeTextBridge
	srv := NewServer(&cfg, accounts.New(st, &cfg))
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}

	// 1x1 PNG
	pngB64 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	pngDataURI := "data:image/png;base64," + pngB64
	hugeHistory := strings.Repeat("混合图文历史上下文记录，需要被外置到历史文本附件中。", 1100)

	// 1. 图片混合 + 长历史（同张图只应上传 1 次 + 另 1 次历史文本 = 共 2 次）
	msgsImg := []map[string]any{
		{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": hugeHistory},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": pngDataURI}},
		}},
		{"role": "assistant", "content": "已查看图1。"},
		{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "当前轮提问：请结合图1继续确认尾标 IMG-END-777"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": pngDataURI}},
		}},
	}
	code, body := doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"Authorization": "Bearer k-img"}, map[string]any{
		"model": "aws-quick-smart", "messages": msgsImg,
	})
	if code != http.StatusOK {
		t.Fatalf("mixed image+history status=%d body=%s", code, body)
	}

	upMu.Lock()
	imgUploads := 0
	txtUploads := 0
	for _, u := range *uploads {
		if strings.HasPrefix(u.Name, "img-") {
			imgUploads++
		} else if strings.HasPrefix(u.Name, "prompt-") {
			txtUploads++
		}
	}
	upMu.Unlock()
	if imgUploads != 1 || txtUploads != 1 {
		t.Fatalf("expected deduplicated 1 image upload + 1 history text upload, got img=%d txt=%d", imgUploads, txtUploads)
	}

	capMu.Lock()
	imgTurn := (*captured)[len(*captured)-1]
	capMu.Unlock()
	if !strings.Contains(imgTurn.Text, "当前轮提问：请结合图1继续确认尾标 IMG-END-777") {
		t.Fatalf("mixed image turn lost current user text: %q", imgTurn.Text)
	}
	if len(imgTurn.Attachments) != 2 {
		t.Fatalf("expected 2 attachments (1 image + 1 history prompt), got %d", len(imgTurn.Attachments))
	}

	// 2. 当轮本身单独超限（>70KB 单条提问）：有上传能力时全文进附件、正文明确仅为预览且帧 <= limit
	upMu.Lock()
	*uploads = nil
	upMu.Unlock()
	oversizedCur := strings.Repeat("单轮超大型正文数据行，包含唯一片段。", 1500) + "【单轮自身尾部绝不丢失标记-OVERSIZE-TAIL】"
	code, body = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"Authorization": "Bearer k-over"}, map[string]any{
		"model":    "aws-quick-smart",
		"messages": []map[string]any{{"role": "user", "content": oversizedCur}},
	})
	if code != http.StatusOK {
		t.Fatalf("oversized single turn with upload status=%d body=%s", code, body)
	}
	upMu.Lock()
	if len(*uploads) != 1 {
		upMu.Unlock()
		t.Fatalf("expected 1 upload for oversized current turn, got %d", len(*uploads))
	}
	overUploaded := string((*uploads)[0].Body)
	upMu.Unlock()
	if overUploaded != oversizedCur {
		t.Fatalf("oversized single turn attachment did not preserve verbatim full prompt")
	}
	capMu.Lock()
	overTurn := (*captured)[len(*captured)-1]
	capMu.Unlock()
	if !strings.Contains(overTurn.Text, "COMPLETE verbatim content is attached") || overTurn.RawFrameBytes > maxUserMessageBytes {
		t.Fatalf("oversized current turn note or frame budget wrong: bytes=%d text=%q", overTurn.RawFrameBytes, overTurn.Text)
	}

	// 3. 长历史或超限当轮在上传失败时：必须明确失败，且 Mock WebSocket 0 次收到 user_message
	capMu.Lock()
	beforeFailFrames := len(*captured)
	capMu.Unlock()
	failMode.Store(1)
	code, _ = doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{"Authorization": "Bearer k-fail"}, map[string]any{
		"model":    "aws-quick-smart",
		"messages": []map[string]any{{"role": "user", "content": oversizedCur}},
	})
	if code == http.StatusOK {
		t.Fatalf("expected error when prompt upload fails on oversized current turn")
	}
	code, _ = doProxyJSONCall(t, client, httpSrv.URL+"/v1/messages", map[string]string{"Authorization": "Bearer k-fail-hist"}, map[string]any{
		"model": "claude-opus-5", "max_tokens": 128,
		"messages": []map[string]any{
			{"role": "user", "content": hugeHistory},
			{"role": "assistant", "content": "收到"},
			{"role": "user", "content": "不要丢我的当前轮"},
		},
	})
	if code == http.StatusOK {
		t.Fatalf("expected error when history upload fails")
	}
	capMu.Lock()
	afterFailFrames := len(*captured)
	capMu.Unlock()
	if afterFailFrames != beforeFailFrames {
		t.Fatalf("upload failure leaked %d incomplete user_message frames", afterFailFrames-beforeFailFrames)
	}
	failMode.Store(0)

	// 4. 无上传能力的 gateway 账号超限时：明确返回 HTTP 400 (request_too_large)，0 帧发出
	dbGWPath := filepath.Join(t.TempDir(), "p1_gw.db")
	stGW, err := store.Open(dbGWPath)
	if err != nil {
		t.Fatalf("store.Open gw: %v", err)
	}
	defer stGW.Close()
	if err := stGW.Save(context.Background(), &store.Account{
		Email: "gw@example.com", Provider: "gateway",
		WSHost: "ws://" + strings.TrimPrefix(wsSrv.URL, "http://"), WSPath: "/ws",
		AccessToken: "jwt-gw", ActorARN: "arn:aws:quick:us-east-1:gw", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("stGW.Save: %v", err)
	}
	srvGW := NewServer(&cfg, accounts.New(stGW, &cfg))
	httpGW := httptest.NewServer(srvGW.Handler())
	defer httpGW.Close()

	code, errGWBody := doProxyJSONCall(t, client, httpGW.URL+"/v1/chat/completions", map[string]string{"Authorization": "Bearer k-gw"}, map[string]any{
		"model":    "aws-quick-smart",
		"messages": []map[string]any{{"role": "user", "content": oversizedCur}},
	})
	if code != http.StatusBadRequest || !strings.Contains(string(errGWBody), "request_too_large") {
		t.Fatalf("expected 400 request_too_large on gateway without attachment upload, got status=%d body=%s", code, errGWBody)
	}
	capMu.Lock()
	afterGWFrames := len(*captured)
	allTurns := append([]detailedCapturedTurn(nil), (*captured)...)
	capMu.Unlock()
	if afterGWFrames != beforeFailFrames {
		t.Fatalf("gateway without upload support leaked %d truncated frames", afterGWFrames-beforeFailFrames)
	}
	assertNoFrameExceeded(t, allTurns)
}

// TestFrameHistorySplit_ResumeAndRetryRebuild 验证长会话在续聊及临时故障重试/切号全量重建时的历史外置与当前轮保真。
func TestFrameHistorySplit_ResumeAndRetryRebuild(t *testing.T) {
	var seq atomic.Int32
	wsSrv, _, captured, capMu := startDetailedMockUpstream(t, func(attempt int32, r *http.Request, msgType string, payload map[string]any) (string, map[string]any) {
		n := seq.Add(1)
		if n == 2 {
			// 第 2 轮触发 1 次临时错误以迫使切号走 switchToFullTranscript() 全量重建
			return "", map[string]any{"type": "error", "payload": map[string]any{"code": "AccessDeniedException", "message": "force account switch"}}
		}
		return fmt.Sprintf("r-%d", n), nil
	})
	tenantSrv, uploads, upMu := startMockAttachmentTenant(t, nil)

	dbPath := filepath.Join(t.TempDir(), "p1_resume_retry.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	for _, email := range []string{"rr1@example.com", "rr2@example.com"} {
		if err := st.Save(context.Background(), &store.Account{
			Email: email, Provider: "social", TenantURL: tenantSrv.URL,
			WSHost: "ws://" + strings.TrimPrefix(wsSrv.URL, "http://"), WSPath: "/ws",
			AccessToken: "jwt-" + email, ActorARN: "arn:aws:quick:us-east-1:" + email, ExpiresAt: time.Now().Add(time.Hour).Unix(),
		}); err != nil {
			t.Fatalf("st.Save(%s): %v", email, err)
		}
	}

	cfg := config.Default()
	cfg.SessionReuse = true
	cfg.MaxAttempts = 3
	srv := NewServer(&cfg, accounts.New(st, &cfg))
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}

	hugeBlock := strings.Repeat("需要保留的历史背景记录内容，确保全量重放超过 64KB。\n", 950)
	msgs := []map[string]any{{"role": "user", "content": hugeBlock + "轮1提问"}}
	code, _ := doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{
		"Authorization":     "Bearer k-rr",
		"X-Conversation-Id": "conv-rr",
	}, map[string]any{"model": "aws-quick-smart", "messages": msgs})
	if code != http.StatusOK {
		t.Fatalf("round1 status=%d", code)
	}

	// 第 2 轮（续聊遇 AccessDenied 切号 -> switchToFullTranscript() 重放完整历史并必须仅外置历史、完整内联第 2 轮提问）
	cur2 := "第二轮完整提问（含超过1200字节的中文与特殊引号\"和反斜杠\\）：" + strings.Repeat("精准验证全量重放时本轮完整保留。", 55) + "【R2-END】"
	msgs = append(msgs,
		map[string]any{"role": "assistant", "content": "r-1"},
		map[string]any{"role": "user", "content": cur2},
	)
	code, body := doProxyJSONCall(t, client, httpSrv.URL+"/v1/chat/completions", map[string]string{
		"Authorization":     "Bearer k-rr",
		"X-Conversation-Id": "conv-rr",
	}, map[string]any{"model": "aws-quick-smart", "messages": msgs})
	if code != http.StatusOK {
		t.Fatalf("round2 switch account rebuild status=%d body=%s", code, body)
	}

	upMu.Lock()
	lastUp := string((*uploads)[len(*uploads)-1].Body)
	upMu.Unlock()
	if !strings.Contains(lastUp, "轮1提问") || strings.Contains(lastUp, "【R2-END】") {
		t.Fatalf("rebuild upload should contain round1 history only and NOT round2 tail")
	}

	capMu.Lock()
	allTurns := append([]detailedCapturedTurn(nil), (*captured)...)
	capMu.Unlock()
	if len(allTurns) != 3 {
		t.Fatalf("expected 3 captured frames, got %d", len(allTurns))
	}
	rebuiltTurn := allTurns[2]
	if !rebuiltTurn.HasSessionCreate || !strings.Contains(rebuiltTurn.Text, cur2) {
		t.Fatalf("rebuilt turn lost full current turn cur2 or did not create fresh thread: %+v", rebuiltTurn)
	}
	assertNoFrameExceeded(t, allTurns)

	_ = base64.StdEncoding // keep standard usage explicit
}
