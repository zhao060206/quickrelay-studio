package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The upstream user_message frame is capped at 64 KB. A caller prompt that
// cannot fit is uploaded out of band through the tenant attachment service and
// referenced from a short user_message instead. This mirrors the desktop
// renderer's nZ()/aZn() + lZn() flow (upload-attachment -> attachment-dlp-status
// -> qbs-event files/upload/{conversation} -> payload.attachments[].path).
const (
	attachmentPromptPrefix     = "prompt-"
	attachmentGuidelinesPrefix = "guidelines-"
	attachmentPromptSuffix     = ".txt"
	attachmentMimeType         = "text/plain"
	attachmentMinPromptSize    = 8 * 1024
	// The desktop client polls the DLP verdict every 1.5 s for up to 60
	// attempts (90 s); match that budget so larger files are not cut short.
	attachmentPollInterval = 1500 * time.Millisecond
	attachmentPollAttempts = 60

	maxImageBytes             = 20 << 20 // 单张图片最大 20 MB
	maxImagesPerTurn          = 16       // 单轮最多上传 16 张图片
	maxConcurrentImageUploads = 3        // 图片附件并发上传上限
)

// ImageAttachment 表示从 OpenAI/Codex/Anthropic 请求消息中提取出的待上传图片或文档附件。
type ImageAttachment struct {
	Name       string
	MIMEType   string
	Data       []byte
	SHA256     string
	IsDocument bool
}

// attachmentError marks a failure in the out-of-band path that should fall
// back to the normal frame-trimming behavior instead of failing the request.
type attachmentError struct{ err error }

func (e *attachmentError) Error() string { return e.err.Error() }
func (e *attachmentError) Unwrap() error { return e.err }

func attachErr(format string, args ...any) error {
	return &attachmentError{err: fmt.Errorf(format, args...)}
}

var attachmentHTTPClient = &http.Client{Timeout: 2 * time.Minute}

// attachmentRef is the metadata referenced from user_message.payload.attachments.
type attachmentRef struct {
	ID   string
	Path string
	Name string
	Size int
}

// attachmentFileName 基于 messageID 前 8 位生成会话内唯一的附件文件名。
func attachmentFileName(prefix, messageID, suffix string) string {
	suffixID := strings.ReplaceAll(messageID, "-", "")
	if len(suffixID) > 8 {
		suffixID = suffixID[:8]
	}
	if suffixID == "" {
		suffixID = "00000000"
	}
	return prefix + suffixID + suffix
}

// attachmentPromptName returns a per-message file name so a later upload in the
// same conversation cannot be confused with an earlier prompt.txt.
func attachmentPromptName(messageID string) string {
	return attachmentFileName(attachmentPromptPrefix, messageID, attachmentPromptSuffix)
}

// attachmentGuidelinesName 生成本次请求的工作区规范带外附件文件名。
func attachmentGuidelinesName(messageID string) string {
	return attachmentFileName(attachmentGuidelinesPrefix, messageID, attachmentPromptSuffix)
}

// attachmentGuidelinesNote 在超限工作区规范转为附件后生成主帧短引用，明确允许模型直接调用本地工具而不必先读冗长附件。
func attachmentGuidelinesNote(name string) string {
	return attachmentGuidelinesNoteWithEnv(name, "")
}

// attachmentGuidelinesNoteWithEnv 生成带关键工作目录摘要的工作区规范附件引用。
func attachmentGuidelinesNoteWithEnv(name, envSummary string) string {
	var b strings.Builder
	b.WriteString("<workspace_guidelines>\n")
	b.WriteString("Extended client guidelines and skill catalog are attached in ")
	b.WriteString(name)
	b.WriteString(" for optional reference if you need specific repository conventions or skill paths. You do NOT need to read that attachment before handling straightforward local workspace tasks; follow the user's request and use the complete local client tools in <client_tool_protocol> below directly.")
	if trimmed := strings.TrimSpace(envSummary); trimmed != "" {
		b.WriteString("\n\nKey workspace environment summary (extracted for direct use without opening the attachment):\n")
		b.WriteString(trimmed)
	}
	b.WriteString("\n</workspace_guidelines>")
	return b.String()
}

// fileAttachmentMeta 构造上游 user_message.payload.attachments 所需的统一文本附件描述。
func fileAttachmentMeta(att attachmentRef, mime string) map[string]any {
	meta := map[string]any{
		"type":     "file",
		"name":     att.Name,
		"size":     att.Size,
		"mimeType": mime,
		"path":     att.Path,
	}
	if att.ID != "" {
		meta["attachmentId"] = att.ID
	}
	return meta
}

func attachmentShortText(name string) string {
	return "The user's full message for this turn is in the attached file " + name + ". Read it and respond to it."
}

// attachmentShortTextWithLatest 在超长对话通过文本附件传递时，内联用户最新一句话的 UTF-8 安全预览，
// 降低模型在仅看到文件名时误判意图或反复探测的风险。
func attachmentShortTextWithLatest(name, latestUser string) string {
	preview := strings.TrimSpace(latestUser)
	if preview == "" {
		return "The full conversation transcript for this turn is attached in " + name + ". Read it with your built-in file reader and respond to it."
	}
	const maxPreviewBytes = 1200
	if len(preview) > maxPreviewBytes {
		n := maxPreviewBytes
		for n > 0 && !utf8.RuneStart(preview[n]) {
			n--
		}
		preview = strings.TrimSpace(preview[:n]) + "…"
	}
	if strings.HasPrefix(preview, "Tool result") {
		return "The full conversation transcript for this turn is attached in " + name + " (you may inspect it with your file reader if needed). The latest client tool result(s) are:\n" + preview
	}
	return "The full conversation transcript for this turn is attached in " + name + " (you may inspect it with your built-in file reader to see full context). The latest user message is:\n" + preview
}

// userMessageBytes returns the wire size of one user_message frame. The
// upstream limit is checked against the reassembled inner JSON, not the
// WebSocket envelope.
func userMessageBytes(payload map[string]any) (int, error) {
	body, err := json.Marshal(map[string]any{"type": "user_message", "payload": payload})
	if err != nil {
		return 0, err
	}
	return len(body), nil
}

// shouldAttachPrompt reports whether the prompt is large enough to justify an
// out-of-band upload. Even when the caller's system prompt alone is oversized,
// uploading the prompt is still worthwhile: the short reference leaves more
// room for the system prompt before fitUserMessage has to trim it.
func shouldAttachPrompt(promptSize int) bool {
	return promptSize >= attachmentMinPromptSize
}

// uploadPromptAttachment 将超长文本作为纯文本附件上传，保留兼容原有调用方式。
func uploadPromptAttachment(ctx context.Context, sess *Session, threadID, name string, data []byte) (attachmentRef, error) {
	return uploadAttachment(ctx, sess, threadID, name, attachmentMimeType, data)
}

// uploadAttachment 执行三步带外上传（upload-attachment -> DLP 轮询 -> qbs-event 绑定会话），
// 支持任意指定 contentType，返回可供 user_message 引用的服务端 path 与 attachmentId。
func uploadAttachment(ctx context.Context, sess *Session, threadID, name, contentType string, data []byte) (attachmentRef, error) {
	ref := attachmentRef{Name: name, Size: len(data)}
	if sess == nil {
		return ref, attachErr("attachment upload: no session")
	}
	base, err := tenantBase(sess.TenantURL)
	if err != nil {
		return ref, err
	}
	token := sess.jwt()
	if token == "" {
		return ref, attachErr("attachment upload: no jwt")
	}
	if len(data) == 0 {
		return ref, attachErr("attachment upload: empty data")
	}
	if strings.TrimSpace(contentType) == "" {
		contentType = attachmentMimeType
	}

	// 1. 上传原始字节流，租户服务返回 attachmentId 并触发 DLP 扫描。
	q := url.Values{}
	q.Set("name", name)
	q.Set("contentType", contentType)
	q.Set("contentLength", strconv.Itoa(len(data)))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/integration/quick-work/upload-attachment?"+q.Encode(), bytes.NewReader(data))
	if err != nil {
		return ref, attachErr("attachment upload request: %v", err)
	}
	setAttachmentHeaders(req, token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := attachmentHTTPClient.Do(req)
	if err != nil {
		return ref, attachErr("attachment upload: %v", err)
	}
	body, readErr := readLimited(resp.Body, 1<<20)
	resp.Body.Close()
	if readErr != nil {
		return ref, attachErr("attachment upload read: %v", readErr)
	}
	if resp.StatusCode >= 400 {
		return ref, attachErr("attachment upload HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	var up struct {
		AttachmentID string      `json:"attachmentId"`
		Path         string      `json:"path"`
		Files        []pathEntry `json:"files"`
	}
	if err := json.Unmarshal(body, &up); err != nil {
		return ref, attachErr("attachment upload decode: %v", err)
	}
	ref.ID = strings.TrimSpace(up.AttachmentID)
	ref.Path = firstPath(up.Path, up.Files)
	if ref.ID == "" && ref.Path == "" {
		return ref, attachErr("attachment upload returned no attachment id or path")
	}

	// 2. 等待 DLP 扫描判定。WARN 在桌面端需要人工交互确认，API 中继视为拦截。
	if ref.ID != "" {
		if err := waitAttachmentDLP(ctx, base, token, ref.ID); err != nil {
			return ref, err
		}
	}
	if ref.Path != "" {
		return ref, nil
	}

	// 3. 将附件绑定到当前会话工作区；缺少此 path 时上游模型看不到文件。
	path, err := deliverAttachment(ctx, base, token, threadID, ref.ID)
	if err != nil {
		return ref, err
	}
	ref.Path = path
	return ref, nil
}

// uploadImageAttachments 受并发信号量限制上传多张图片，按输入顺序返回成功的附件元数据
// 及每张失败图片的简短描述（单张失败不会中止其余图片上传）。
func uploadImageAttachments(ctx context.Context, sess *Session, threadID string, images []ImageAttachment) ([]map[string]any, []string) {
	if len(images) == 0 {
		return nil, nil
	}

	type uploadOutcome struct {
		meta map[string]any
		err  error
	}

	outcomes := make([]uploadOutcome, len(images))
	sem := make(chan struct{}, maxConcurrentImageUploads)
	var wg sync.WaitGroup

	for i, img := range images {
		wg.Add(1)
		go func(idx int, item ImageAttachment) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				outcomes[idx] = uploadOutcome{err: ctx.Err()}
				return
			}

			att, err := uploadAttachment(ctx, sess, threadID, item.Name, item.MIMEType, item.Data)
			if err != nil {
				outcomes[idx] = uploadOutcome{err: err}
				return
			}
			if att.Path == "" {
				outcomes[idx] = uploadOutcome{err: attachErr("attachment upload returned no path")}
				return
			}

			attType := "image"
			if item.IsDocument {
				attType = "file"
			}
			meta := map[string]any{
				"type":     attType,
				"name":     att.Name,
				"size":     att.Size,
				"mimeType": item.MIMEType,
				"path":     att.Path,
			}
			if att.ID != "" {
				meta["attachmentId"] = att.ID
			}
			log.Printf("user_message %s attachment thread=%s bytes=%d mime=%s name=%s id=%s path=%s",
				attType, threadID, att.Size, item.MIMEType, att.Name, att.ID, att.Path)
			outcomes[idx] = uploadOutcome{meta: meta}
		}(i, img)
	}
	wg.Wait()

	var metas []map[string]any
	var failed []string
	for i, out := range outcomes {
		if out.err != nil {
			log.Printf("user_message image attachment failed thread=%s name=%s mime=%s bytes=%d: %v",
				threadID, images[i].Name, images[i].MIMEType, len(images[i].Data), out.err)
			failed = append(failed, fmt.Sprintf("%s: %v", images[i].Name, out.err))
			continue
		}
		if out.meta != nil {
			metas = append(metas, out.meta)
		}
	}
	return metas, failed
}

// dlpPollDelay 返回第 attempt 次（从 1 开始）DLP 状态复查前的渐进退避间隔，降低小文件首轮等待。
func dlpPollDelay(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 300 * time.Millisecond
	case 2:
		return 600 * time.Millisecond
	case 3:
		return 1000 * time.Millisecond
	default:
		return attachmentPollInterval
	}
}

func waitAttachmentDLP(ctx context.Context, base, token, attachmentID string) error {
	query := "/integration/quick-work/attachment-dlp-status?attachmentId=" + url.QueryEscape(attachmentID)
	for attempt := range attachmentPollAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return attachErr("attachment DLP: %v", ctx.Err())
			case <-time.After(dlpPollDelay(attempt)):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+query, nil)
		if err != nil {
			return attachErr("attachment DLP request: %v", err)
		}
		setAttachmentHeaders(req, token)
		resp, err := attachmentHTTPClient.Do(req)
		if err != nil {
			return attachErr("attachment DLP: %v", err)
		}
		body, readErr := readLimited(resp.Body, 1<<20)
		resp.Body.Close()
		if readErr != nil {
			return attachErr("attachment DLP read: %v", readErr)
		}
		if resp.StatusCode >= 400 {
			return attachErr("attachment DLP HTTP %d: %s", resp.StatusCode, snippet(body))
		}
		var out struct {
			DLPJobStatus string `json:"dlpJobStatus"`
			PolicyAction string `json:"policyAction"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return attachErr("attachment DLP decode: %v", err)
		}
		status := strings.ToUpper(strings.TrimSpace(out.DLPJobStatus))
		action := strings.ToUpper(strings.TrimSpace(out.PolicyAction))
		switch action {
		case "ALLOW":
			return nil
		case "BLOCK", "DENY", "REJECT", "WARN":
			return attachErr("attachment blocked by DLP (%s)", action)
		}
		switch status {
		case "SKIPPED", "NOT_APPLICABLE", "DISABLED":
			return nil
		case "FAILED", "ERROR":
			return attachErr("attachment DLP scan failed (%s)", status)
		case "COMPLETED":
			// The job finished without an explicit allow verdict.
			return attachErr("attachment DLP completed without an allow verdict")
		}
	}
	return attachErr("attachment DLP scan did not settle")
}

func deliverAttachment(ctx context.Context, base, token, threadID, attachmentID string) (string, error) {
	// The envelope's body is the already-decoded JSON document: the DataPlane
	// deserializes invokeAgent verbatim (buildInvokeAgentEnvelope in the desktop
	// main process parses the renderer's body string before sending it).
	event := map[string]any{
		"eventType": "invokeAgent",
		"eventBody": map[string]any{"invokeAgent": map[string]any{
			"path":        "files/upload/" + url.PathEscape(threadID),
			"queryParams": map[string]any{"_method": "POST"},
			"body":        map[string]any{"attachmentId": attachmentID},
		}},
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return "", attachErr("attachment deliver encode: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/integration/quick-work/qbs-event", bytes.NewReader(raw))
	if err != nil {
		return "", attachErr("attachment deliver request: %v", err)
	}
	setAttachmentHeaders(req, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := attachmentHTTPClient.Do(req)
	if err != nil {
		return "", attachErr("attachment deliver: %v", err)
	}
	body, readErr := readLimited(resp.Body, 2<<20)
	resp.Body.Close()
	if readErr != nil {
		return "", attachErr("attachment deliver read: %v", readErr)
	}
	if resp.StatusCode >= 400 {
		return "", attachErr("attachment deliver HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	path := extractAttachmentPath(body)
	if path == "" {
		return "", attachErr("attachment deliver returned no path: %s", snippet(body))
	}
	return path, nil
}

func tenantBase(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", attachErr("no tenant_url for attachment upload")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "", attachErr("invalid tenant_url for attachment upload")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func setAttachmentHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	// The DataPlane routes are browser-facing; the desktop app always sends
	// this Origin.
	req.Header.Set("Origin", "https://quick.aws.com")
}

func firstPath(path string, files []pathEntry) string {
	if strings.TrimSpace(path) != "" {
		return path
	}
	if len(files) > 0 {
		return strings.TrimSpace(files[0].Path)
	}
	return ""
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, limit))
}

func snippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// extractAttachmentPath pulls {path} or {files:[{path}]} out of the qbs-event
// response. The renderer receives it nested as
// eventResponse.invokeAgentResponse -> {type:"api_response", body:"<json>"}.
func extractAttachmentPath(raw []byte) string {
	var top struct {
		Path          string          `json:"path"`
		Files         []pathEntry     `json:"files"`
		EventResponse json.RawMessage `json:"eventResponse"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return ""
	}
	if p := firstPath(top.Path, top.Files); p != "" {
		return p
	}
	if len(top.EventResponse) == 0 {
		return extractFromAPIResponse(raw)
	}
	var ev struct {
		InvokeAgentResponse json.RawMessage `json:"invokeAgentResponse"`
	}
	if err := json.Unmarshal(top.EventResponse, &ev); err != nil || len(ev.InvokeAgentResponse) == 0 {
		return extractFromAPIResponse(raw)
	}
	var encoded string
	if err := json.Unmarshal(ev.InvokeAgentResponse, &encoded); err == nil {
		return extractFromAPIResponse([]byte(encoded))
	}
	return extractFromAPIResponse(ev.InvokeAgentResponse)
}

type pathEntry struct {
	Path string `json:"path"`
}

func extractFromAPIResponse(raw []byte) string {
	var api struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(raw, &api); err == nil {
		if api.Body != "" {
			if p := extractFromPathBody([]byte(api.Body)); p != "" {
				return p
			}
		}
	}
	// Some responses carry body as an object instead of a JSON string.
	var withObjectBody struct {
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(raw, &withObjectBody); err == nil && len(withObjectBody.Body) > 0 {
		if p := extractFromPathBody(withObjectBody.Body); p != "" {
			return p
		}
	}
	var direct struct {
		Path  string      `json:"path"`
		Files []pathEntry `json:"files"`
	}
	if err := json.Unmarshal(raw, &direct); err == nil {
		if p := firstPath(direct.Path, direct.Files); p != "" {
			return p
		}
	}
	return ""
}

func extractFromPathBody(raw []byte) string {
	var v struct {
		Path  string      `json:"path"`
		Files []pathEntry `json:"files"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return firstPath(v.Path, v.Files)
}
