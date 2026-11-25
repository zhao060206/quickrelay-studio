package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"quickrelay-studio/internal/config"
)

// FunctionCall is the OpenAI function payload. Arguments is a JSON string on
// the wire, but some clients send an object; UnmarshalJSON accepts both.
type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

func (f *FunctionCall) UnmarshalJSON(data []byte) error {
	var aux struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	f.Name = aux.Name
	f.Arguments = argumentString(aux.Arguments)
	return nil
}

func (f FunctionCall) MarshalJSON() ([]byte, error) {
	args := f.Arguments
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	return json.Marshal(struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: f.Name, Arguments: args})
}

// ToolCall is an assistant tool call in an OpenAI chat message.
type ToolCall struct {
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

// Message is an OpenAI chat message. Content is decoded as any because the
// OpenAI schema allows a string, an array of typed parts, or null.
type Message struct {
	Role         string        `json:"role"`
	Content      any           `json:"content"`
	Name         string        `json:"name,omitempty"`
	ToolCalls    []ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID   string        `json:"tool_call_id,omitempty"`
	FunctionCall *FunctionCall `json:"function_call,omitempty"`
}

// ToolFunction is a function definition in the OpenAI tools array. The proxy
// forwards it verbatim and never executes it.
type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
	Strict      bool   `json:"strict,omitempty"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// StreamOptions is the OpenAI stream_options object.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ReasoningOptions 兼容客户端通过 reasoning.effort 传递思考强度。
type ReasoningOptions struct {
	Effort string `json:"effort,omitempty"`
}

type ChatRequest struct {
	Model             string            `json:"model"`
	Messages          []Message         `json:"messages"`
	Stream            bool              `json:"stream"`
	N                 int               `json:"n"`
	Tools             []Tool            `json:"tools,omitempty"`
	ToolChoice        any               `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
	Functions         []ToolFunction    `json:"functions,omitempty"`
	StreamOptions     *StreamOptions    `json:"stream_options,omitempty"`
	ReasoningEffort   string            `json:"reasoning_effort,omitempty"`
	Reasoning         *ReasoningOptions `json:"reasoning,omitempty"`
}

// resolveThinkingEffort 解析并归一化请求中的思考强度，优先级为 reasoning_effort -> reasoning.effort -> 默认配置。
func resolveThinkingEffort(req *ChatRequest, fallback string) string {
	raw := ""
	if req != nil {
		raw = strings.TrimSpace(req.ReasoningEffort)
		if raw == "" && req.Reasoning != nil {
			raw = strings.TrimSpace(req.Reasoning.Effort)
		}
	}
	if raw == "" {
		raw = strings.TrimSpace(fallback)
	}
	if raw == "" {
		return ""
	}
	switch strings.ToLower(raw) {
	case "low", "minimal":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "max", "xhigh", "ultra":
		return "max"
	case "none", "off":
		return ""
	default:
		log.Printf("ignoring unknown thinking effort %q", raw)
		return ""
	}
}

// uuid returns a random RFC 4122 v4 UUID.
func uuid() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]))
}

// argumentString normalizes an OpenAI function argument value to the JSON
// string expected on the wire. Strings are passed through; objects and arrays
// are compacted; missing/null values become "{}".
func argumentString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return "{}"
		}
		return s
	}
	var v any
	if err := json.Unmarshal(raw, &v); err == nil {
		if v == nil {
			return "{}"
		}
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
	}
	return strings.TrimSpace(string(raw))
}

var supportedImageExt = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// imageCollector 收集单轮对话中的图片附件，完成内容去重、唯一命名与上限控制。
type imageCollector struct {
	byHash map[string]string // sha256 -> 已分配的文件名
	order  []ImageAttachment
}

func newImageCollector() *imageCollector {
	return &imageCollector{
		byHash: make(map[string]string),
	}
}

// add 注册一张已校验通过的图片，若内容完全相同则返回已有文件名而不重复添加。
func (c *imageCollector) add(mimeType string, data []byte) string {
	sum := sha256.Sum256(data)
	hashHex := hex.EncodeToString(sum[:])
	if name, ok := c.byHash[hashHex]; ok {
		return name
	}
	ext := supportedImageExt[mimeType]
	if ext == "" {
		ext = ".png"
	}
	idx := len(c.order) + 1
	name := fmt.Sprintf("img-%02d-%s%s", idx, hashHex[:8], ext)
	c.byHash[hashHex] = name
	c.order = append(c.order, ImageAttachment{
		Name:     name,
		MIMEType: mimeType,
		Data:     data,
		SHA256:   hashHex,
	})
	return name
}

// addDocument 注册一份待上传的文档附件（如 PDF），按内容 SHA-256 去重并保留安全可读文件名。
func (c *imageCollector) addDocument(title, mimeType string, data []byte) string {
	sum := sha256.Sum256(data)
	hashHex := hex.EncodeToString(sum[:])
	if name, ok := c.byHash[hashHex]; ok {
		return name
	}
	ext := ".pdf"
	if strings.EqualFold(mimeType, "text/plain") {
		ext = ".txt"
	}
	safeTitle := sanitizeAttachmentTitle(title)
	if safeTitle != "" && strings.HasSuffix(strings.ToLower(safeTitle), ext) {
		safeTitle = safeTitle[:len(safeTitle)-len(ext)]
	}
	idx := len(c.order) + 1
	var name string
	if safeTitle != "" {
		name = fmt.Sprintf("doc-%02d-%s-%s%s", idx, hashHex[:6], safeTitle, ext)
	} else {
		name = fmt.Sprintf("doc-%02d-%s%s", idx, hashHex[:8], ext)
	}
	c.byHash[hashHex] = name
	c.order = append(c.order, ImageAttachment{
		Name:       name,
		MIMEType:   mimeType,
		Data:       data,
		SHA256:     hashHex,
		IsDocument: true,
	})
	return name
}

func sanitizeAttachmentTitle(title string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return ""
	}
	var b strings.Builder
	count := 0
	for _, r := range title {
		if count >= 40 {
			break
		}
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_':
			b.WriteRune(r)
			count++
		case r > 0x7f && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
			count++
		case r == ' ':
			b.WriteByte('_')
			count++
		}
	}
	return strings.Trim(b.String(), "._-")
}

// images 返回最终待上传的图片及文档附件列表，超过单轮上限时保留最新的附件。
func (c *imageCollector) images() []ImageAttachment {
	if c == nil || len(c.order) == 0 {
		return nil
	}
	if len(c.order) <= maxImagesPerTurn {
		return append([]ImageAttachment(nil), c.order...)
	}
	log.Printf("image attachments capped %d -> %d (keeping latest)", len(c.order), maxImagesPerTurn)
	kept := c.order[len(c.order)-maxImagesPerTurn:]
	return append([]ImageAttachment(nil), kept...)
}

func imagePartURL(p map[string]any) string {
	switch v := p["image_url"].(type) {
	case string:
		return v
	case map[string]any:
		if s, ok := v["url"].(string); ok {
			return s
		}
	}
	if s, ok := p["url"].(string); ok {
		return s
	}
	return ""
}

// extractImageSource 统一解析 image_url、input_image 与 image（含 Anthropic source）结构。
func extractImageSource(p map[string]any) (rawURL, b64Data, b64MIME string) {
	rawURL = strings.TrimSpace(imagePartURL(p))
	if rawURL != "" {
		return rawURL, "", ""
	}
	src, ok := p["source"].(map[string]any)
	if !ok {
		return "", "", ""
	}
	srcType, _ := src["type"].(string)
	switch strings.ToLower(strings.TrimSpace(srcType)) {
	case "base64":
		b64MIME, _ = src["media_type"].(string)
		b64Data, _ = src["data"].(string)
	case "url":
		rawURL, _ = src["url"].(string)
	}
	return strings.TrimSpace(rawURL), b64Data, strings.TrimSpace(b64MIME)
}

func normalizeImageMIME(raw string) string {
	if semi := strings.IndexByte(raw, ';'); semi >= 0 {
		raw = raw[:semi]
	}
	mime := strings.ToLower(strings.TrimSpace(raw))
	if mime == "image/jpg" || mime == "image/pjpeg" {
		return "image/jpeg"
	}
	return mime
}

func detectImageMIME(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}):
		return "image/png"
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif"
	case len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	default:
		return normalizeImageMIME(http.DetectContentType(data))
	}
}

func decodeImageBase64(declaredMIME, b64 string) (string, []byte, error) {
	clean := strings.NewReplacer("\r", "", "\n", "", " ", "", "\t", "").Replace(strings.TrimSpace(b64))
	if clean == "" {
		return "", nil, fmt.Errorf("empty base64 image data")
	}
	if base64.StdEncoding.DecodedLen(len(clean)) > maxImageBytes+4 {
		return "", nil, fmt.Errorf("image exceeds max %d bytes", maxImageBytes)
	}
	data, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(clean)
		if err != nil {
			return "", nil, fmt.Errorf("invalid base64 image: %w", err)
		}
	}
	if len(data) == 0 || len(data) > maxImageBytes {
		return "", nil, fmt.Errorf("decoded image size %d out of range", len(data))
	}
	mime := normalizeImageMIME(declaredMIME)
	if mime == "" || mime == "application/octet-stream" {
		mime = detectImageMIME(data)
	}
	if _, ok := supportedImageExt[mime]; !ok {
		return "", nil, fmt.Errorf("unsupported image mime %q", mime)
	}
	return mime, data, nil
}

func decodeImageDataURI(uri string) (string, []byte, error) {
	if len(uri) < 5 || !strings.EqualFold(uri[:5], "data:") {
		return "", nil, fmt.Errorf("not a data uri")
	}
	comma := strings.IndexByte(uri, ',')
	if comma < 0 {
		return "", nil, fmt.Errorf("malformed data uri")
	}
	meta := uri[5:comma]
	body := uri[comma+1:]
	parts := strings.Split(meta, ";")
	declaredMIME := ""
	if len(parts) > 0 {
		declaredMIME = parts[0]
	}
	isBase64 := false
	for _, seg := range parts[1:] {
		if strings.EqualFold(strings.TrimSpace(seg), "base64") {
			isBase64 = true
			break
		}
	}
	if !isBase64 {
		return "", nil, fmt.Errorf("data uri missing base64 encoding")
	}
	return decodeImageBase64(declaredMIME, body)
}

func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified()
}

func resolvePublicIPs(ctx context.Context, host string) ([]net.IP, error) {
	h := strings.Trim(strings.TrimSpace(host), "[]")
	if h == "" {
		return nil, fmt.Errorf("empty host")
	}
	if ip := net.ParseIP(h); ip != nil {
		if isBlockedIP(ip) {
			return nil, fmt.Errorf("blocked private or local ip %s", ip.String())
		}
		return []net.IP{ip}, nil
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", h)
	if err != nil {
		return nil, fmt.Errorf("lookup host %q: %w", h, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("host %q resolved no ips", h)
	}
	for _, ip := range ips {
		if isBlockedIP(ip) {
			return nil, fmt.Errorf("host %q resolved to blocked ip %s", h, ip.String())
		}
	}
	return ips, nil
}

// fetchRemoteImage 拉取远程 HTTP/HTTPS 图片，校验所有解析 IP 非内网以防止 SSRF。
func fetchRemoteImage(ctx context.Context, rawURL string) (string, []byte, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", nil, fmt.Errorf("invalid image url: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", nil, fmt.Errorf("unsupported url scheme %q", parsed.Scheme)
	}
	if parsed.Hostname() == "" {
		return "", nil, fmt.Errorf("missing host in image url")
	}
	if _, err := resolvePublicIPs(ctx, parsed.Hostname()); err != nil {
		return "", nil, err
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		DialContext: func(dctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := resolvePublicIPs(dctx, host)
			if err != nil {
				return nil, err
			}
			return dialer.DialContext(dctx, network, net.JoinHostPort(ips[0].String(), port))
		},
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{
		Timeout:   15 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			rs := strings.ToLower(req.URL.Scheme)
			if rs != "http" && rs != "https" {
				return fmt.Errorf("blocked redirect scheme %q", req.URL.Scheme)
			}
			if _, err := resolvePublicIPs(req.Context(), req.URL.Hostname()); err != nil {
				return err
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", nil, fmt.Errorf("remote image HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return "", nil, err
	}
	if len(data) == 0 {
		return "", nil, fmt.Errorf("empty remote image")
	}
	if len(data) > maxImageBytes {
		return "", nil, fmt.Errorf("remote image exceeds %d bytes", maxImageBytes)
	}
	mime := detectImageMIME(data)
	if _, ok := supportedImageExt[mime]; !ok {
		return "", nil, fmt.Errorf("remote content is not a supported image (%s)", mime)
	}
	return mime, data, nil
}

func decodeDocumentBase64(declaredMIME, b64 string) (string, []byte, error) {
	clean := strings.NewReplacer("\r", "", "\n", "", " ", "", "\t", "").Replace(strings.TrimSpace(b64))
	if clean == "" {
		return "", nil, fmt.Errorf("empty base64 document data")
	}
	if base64.StdEncoding.DecodedLen(len(clean)) > maxImageBytes+4 {
		return "", nil, fmt.Errorf("document exceeds max %d bytes", maxImageBytes)
	}
	data, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(clean)
		if err != nil {
			return "", nil, fmt.Errorf("invalid base64 document: %w", err)
		}
	}
	if len(data) == 0 || len(data) > maxImageBytes {
		return "", nil, fmt.Errorf("decoded document size %d out of range", len(data))
	}
	mime := normalizeImageMIME(declaredMIME)
	if bytes.HasPrefix(data, []byte("%PDF-")) {
		mime = "application/pdf"
	}
	if mime == "" || mime == "application/octet-stream" {
		if utf8.Valid(data) {
			mime = "text/plain"
		}
	}
	if mime != "application/pdf" && !strings.HasPrefix(mime, "text/") {
		return "", nil, fmt.Errorf("unsupported document mime %q", mime)
	}
	return mime, data, nil
}

func formatDocumentPart(p map[string]any, collector *imageCollector) string {
	title, _ := p["title"].(string)
	title = strings.TrimSpace(title)
	rawURL, b64Data, b64MIME := extractImageSource(p)
	if b64Data == "" {
		if fileObj, ok := p["file"].(map[string]any); ok {
			if title == "" {
				if fn, ok := fileObj["filename"].(string); ok {
					title = strings.TrimSpace(fn)
				}
			}
			if fd, ok := fileObj["file_data"].(string); ok && strings.TrimSpace(fd) != "" {
				rawURL = strings.TrimSpace(fd)
			}
		}
	}
	if collector == nil {
		if title != "" {
			return "[document: " + title + "]"
		}
		if rawURL != "" && !(len(rawURL) >= 5 && strings.EqualFold(rawURL[:5], "data:")) {
			return "[document: " + rawURL + "]"
		}
		return "[document]"
	}
	if b64Data == "" && len(rawURL) >= 5 && strings.EqualFold(rawURL[:5], "data:") {
		if comma := strings.IndexByte(rawURL, ','); comma > 5 {
			meta := rawURL[5:comma]
			parts := strings.Split(meta, ";")
			if len(parts) > 0 {
				b64MIME = parts[0]
			}
			for _, seg := range parts[1:] {
				if strings.EqualFold(strings.TrimSpace(seg), "base64") {
					b64Data = rawURL[comma+1:]
					break
				}
			}
		}
	}
	if b64Data != "" {
		mime, data, err := decodeDocumentBase64(b64MIME, b64Data)
		if err != nil {
			log.Printf("document base64 decode skipped: %v", err)
			if title != "" {
				return "[document: " + title + "]"
			}
			return "[document]"
		}
		if strings.HasPrefix(mime, "text/") {
			header := "<document>"
			if title != "" {
				header = fmt.Sprintf(`<document title="%s">`, strings.ReplaceAll(title, `"`, `'`) )
			}
			return "\n" + header + "\n" + string(data) + "\n</document>\n"
		}
		name := collector.addDocument(title, mime, data)
		return "[attached document: " + name + "]"
	}
	if rawURL != "" {
		if title != "" {
			return fmt.Sprintf("[document: %s (%s)]", title, rawURL)
		}
		return "[document: " + rawURL + "]"
	}
	if title != "" {
		return "[document: " + title + "]"
	}
	return "[document]"
}

func formatImagePart(ctx context.Context, p map[string]any, collector *imageCollector) string {
	rawURL, b64Data, b64MIME := extractImageSource(p)
	if collector == nil {
		if b64Data != "" || (len(rawURL) >= 5 && strings.EqualFold(rawURL[:5], "data:")) {
			return "[image]"
		}
		if rawURL != "" {
			return "[image: " + rawURL + "]"
		}
		return "[image]"
	}

	if b64Data != "" {
		mime, data, err := decodeImageBase64(b64MIME, b64Data)
		if err != nil {
			log.Printf("image base64 decode skipped: %v", err)
			return "[image]"
		}
		name := collector.add(mime, data)
		return "[attached image: " + name + "]"
	}
	if len(rawURL) >= 5 && strings.EqualFold(rawURL[:5], "data:") {
		mime, data, err := decodeImageDataURI(rawURL)
		if err != nil {
			log.Printf("image data uri decode skipped: %v", err)
			return "[image]"
		}
		name := collector.add(mime, data)
		return "[attached image: " + name + "]"
	}
	if rawURL != "" {
		mime, data, err := fetchRemoteImage(ctx, rawURL)
		if err != nil {
			log.Printf("remote image fetch skipped url=%q: %v", rawURL, err)
			return "[image: " + rawURL + "]"
		}
		name := collector.add(mime, data)
		return "[attached image: " + name + "]"
	}
	return "[image]"
}

// contentText 将 OpenAI 消息 content 字段快速压平为纯文本，不解码图片字节，供会话键与快速校验使用。
func contentText(v any) string {
	return contentTextWithCollector(context.Background(), v, nil)
}

func contentTextWithCollector(ctx context.Context, v any, collector *imageCollector) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, part := range c {
			switch p := part.(type) {
			case string:
				b.WriteString(p)
			case map[string]any:
				typ, _ := p["type"].(string)
				switch typ {
				case "text", "input_text", "output_text":
					if t, ok := p["text"].(string); ok {
						b.WriteString(t)
					}
				case "image_url", "input_image", "image":
					b.WriteString(formatImagePart(ctx, p, collector))
				case "document":
					b.WriteString(formatDocumentPart(p, collector))
				case "input_audio":
					b.WriteString("[audio]")
				case "file":
					if _, hasFile := p["file"]; hasFile {
						b.WriteString(formatDocumentPart(p, collector))
					} else if _, hasSrc := p["source"]; hasSrc {
						b.WriteString(formatDocumentPart(p, collector))
					} else {
						b.WriteString("[file]")
					}
				default:
					if t, ok := p["text"].(string); ok {
						b.WriteString(t)
					} else if body, err := json.Marshal(p); err == nil {
						b.Write(body)
					}
				}
			default:
				if body, err := json.Marshal(p); err == nil {
					b.Write(body)
				}
			}
		}
		return b.String()
	case nil:
		return ""
	default:
		body, err := json.Marshal(c)
		if err != nil {
			return ""
		}
		return string(body)
	}
}

func roleLabel(role string) string {
	role = strings.TrimSpace(role)
	if role == "" {
		return "User"
	}
	return strings.ToUpper(role[:1]) + strings.ToLower(role[1:])
}

// formatPrompt 将 OpenAI 消息列表转换为传给 QuickWork user_message 的文本。
func formatPrompt(msgs []Message) (string, error) {
	return formatPromptCore(context.Background(), msgs, msgs, nil, false, false)
}

// formatPromptWithImages 在格式化提示词的同时解码并去重所有可上传的图片及文档附件。
func formatPromptWithImages(ctx context.Context, msgs []Message) (string, []ImageAttachment, error) {
	collector := newImageCollector()
	prompt, err := formatPromptCore(ctx, msgs, msgs, collector, false, false)
	if err != nil {
		return "", nil, err
	}
	return prompt, collector.images(), nil
}

// formatResumeDeltaWithImages 专用于同一 thread_id 续聊的增量切片格式化：
// 若增量切片以本轮之前模型刚在云端生成的首条 assistant 回复开头且后面紧跟工具回传或新提问，
// 则跳过该重复 assistant 正文与万字工具 JSON，并在 tool result 上附带精简目标摘要（file_path/command 等），
// 消除每轮无意义双倍回传导致的附件上传耗时与 KV Cache 写入膨胀。
func formatResumeDeltaWithImages(ctx context.Context, allMsgs []Message, delta []Message) (string, []ImageAttachment, error) {
	collector := newImageCollector()
	prompt, err := formatPromptCore(ctx, allMsgs, delta, collector, true, true)
	if err != nil {
		return "", nil, err
	}
	return prompt, collector.images(), nil
}

var toolTargetKeys = []string{"file_path", "path", "target_file", "filename", "command", "cmd", "pattern", "query", "url", "uri", "dir", "directory"}

func compactToolCallTarget(args string) string {
	args = strings.TrimSpace(args)
	if args == "" || args == "{}" {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal([]byte(args), &obj) != nil || len(obj) == 0 {
		return ""
	}
	for _, key := range toolTargetKeys {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		s, ok := raw.(string)
		if !ok {
			continue
		}
		s = strings.Join(strings.Fields(s), " ")
		if s == "" {
			continue
		}
		const maxTargetRunes = 120
		if utf8.RuneCountInString(s) > maxTargetRunes {
			runes := []rune(s)
			s = strings.TrimSpace(string(runes[:maxTargetRunes-1])) + "…"
		}
		return s
	}
	return ""
}

// formatPromptCore converts an OpenAI transcript into the single text field that
// QuickWork's user_message frame accepts. System/developer messages are sent
// separately via system_prompt, so they are not duplicated here. Plain user
// prompts pass through unchanged. Tool calls and tool results are rendered
// explicitly so the upstream agent can follow a client-side tool loop.
func formatPromptCore(ctx context.Context, allMsgs []Message, msgs []Message, collector *imageCollector, skipLeadingAssistant bool, includeToolTarget bool) (string, error) {
	if len(msgs) == 0 {
		return "", fmt.Errorf("messages must not be empty")
	}

	refMsgs := allMsgs
	if len(refMsgs) == 0 {
		refMsgs = msgs
	}
	toolNames := make(map[string]string)
	toolTargets := make(map[string]string)
	for _, m := range refMsgs {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				if tc.Function.Name != "" {
					toolNames[tc.ID] = tc.Function.Name
				}
				if includeToolTarget {
					if tgt := compactToolCallTarget(tc.Function.Arguments); tgt != "" {
						toolTargets[tc.ID] = tgt
					}
				}
			}
		}
	}

	skipAssistantIdx := -1
	if skipLeadingAssistant {
		firstIdx := -1
		followingCount := 0
		for i, m := range msgs {
			role := strings.ToLower(strings.TrimSpace(m.Role))
			if role == "" {
				role = "user"
			}
			if role == "system" || role == "developer" {
				continue
			}
			if firstIdx < 0 {
				firstIdx = i
			} else {
				followingCount++
			}
		}
		if firstIdx >= 0 && followingCount > 0 {
			firstRole := strings.ToLower(strings.TrimSpace(msgs[firstIdx].Role))
			if firstRole == "assistant" {
				skipAssistantIdx = firstIdx
			}
		}
	}

	singleUser := false
	singleUserText := ""
	nonSystemCount := 0
	blocks := make([]string, 0, len(msgs))
	for i, m := range msgs {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "" {
			role = "user"
		}
		if role == "system" || role == "developer" {
			continue
		}
		if i == skipAssistantIdx {
			continue
		}
		nonSystemCount++
		content := contentTextWithCollector(ctx, m.Content, collector)
		if role == "user" {
			singleUser = true
			singleUserText = content
		} else {
			singleUser = false
		}
		hasContent := strings.TrimSpace(content) != ""

		switch role {
		case "assistant":
			if hasContent {
				blocks = append(blocks, "Assistant: "+content)
			}
			for _, tc := range m.ToolCalls {
				id := strings.TrimSpace(tc.ID)
				if id == "" {
					id = "unknown"
				}
				name := strings.TrimSpace(tc.Function.Name)
				if name == "" {
					name = "unknown"
				}
				args := strings.TrimSpace(tc.Function.Arguments)
				if args == "" {
					args = "{}"
				}
				blocks = append(blocks, fmt.Sprintf("Assistant tool call (id=%s, name=%s): %s", id, name, args))
			}
			if m.FunctionCall != nil {
				name := strings.TrimSpace(m.FunctionCall.Name)
				if name == "" {
					name = "unknown"
				}
				args := strings.TrimSpace(m.FunctionCall.Arguments)
				if args == "" {
					args = "{}"
				}
				blocks = append(blocks, fmt.Sprintf("Assistant function call (name=%s): %s", name, args))
			}
			if !hasContent && len(m.ToolCalls) == 0 && m.FunctionCall == nil {
				continue
			}
		case "tool":
			id := strings.TrimSpace(m.ToolCallID)
			name := strings.TrimSpace(m.Name)
			if name == "" && id != "" {
				name = toolNames[id]
			}
			label := "Tool result"
			var meta []string
			if id != "" {
				meta = append(meta, "call_id="+id)
			}
			if name != "" {
				meta = append(meta, "name="+name)
			}
			if includeToolTarget && id != "" {
				if tgt := toolTargets[id]; tgt != "" {
					meta = append(meta, "target="+tgt)
				}
			}
			if len(meta) > 0 {
				label += " (" + strings.Join(meta, ", ") + ")"
			}
			if !hasContent {
				content = "(empty)"
			}
			blocks = append(blocks, label+": "+content)
		case "function":
			label := "Function result"
			if name := strings.TrimSpace(m.Name); name != "" {
				label += " (name=" + name + ")"
			}
			if !hasContent {
				content = "(empty)"
			}
			blocks = append(blocks, label+": "+content)
		case "user":
			if hasContent {
				blocks = append(blocks, "User: "+content)
			}
		default:
			if hasContent {
				blocks = append(blocks, roleLabel(role)+": "+content)
			}
		}
	}

	if len(blocks) == 0 {
		return "", fmt.Errorf("messages contain no text or tool calls")
	}
	if nonSystemCount == 1 && singleUser {
		if strings.TrimSpace(singleUserText) == "" {
			return "", fmt.Errorf("user message contains no text")
		}
		return singleUserText, nil
	}
	return strings.Join(blocks, "\n\n"), nil
}

var (
	reLeadingAgentIdentity = regexp.MustCompile(`(?im)^you are (?:codex|claude code|cursor|windsurf|antigravity)\b[^.\n]*\.?`)
	reGenericAgentIdentity = regexp.MustCompile(`(?i)\bYou are an?\s+(?:AI\s+)?(?:coding\s+)?agent\b[^.\n]*\.`)
	reActAsAgentIdentity   = regexp.MustCompile(`(?i)\b(?:act|behave|respond)\s+as\s+(?:codex|claude code)\b`)
)

// sanitizeCallerInstructions 温和改写显式的“覆盖自身身份”语句，默认按综合模式处理。
func sanitizeCallerInstructions(text string) string {
	return sanitizeCallerInstructionsForMode(text, config.WorkModeGeneral)
}

// sanitizeCallerInstructionsForMode 按当前工作场景模式温和改写身份声明，既防注入误判又避免在文档场景下引入编码措辞。
func sanitizeCallerInstructionsForMode(text, workMode string) string {
	leadReplacement := "The user is working with you through a local desktop workspace client and asks you to follow the workflow guidelines below."
	genericReplacement := "The user is collaborating with you on a local desktop workspace."
	switch config.NormalizeWorkMode(workMode) {
	case config.WorkModeDocument:
		leadReplacement = "The user is working with you through a local desktop document workspace app and asks you to follow the workflow preferences below."
		genericReplacement = "The user is collaborating with you on a local desktop document workspace."
	case config.WorkModeCoding:
		leadReplacement = "The user is working with you through a local coding workspace client and asks you to follow the workflow guidelines below."
		genericReplacement = "The user is collaborating with you on a local coding workspace."
	}
	text = reLeadingAgentIdentity.ReplaceAllString(text, leadReplacement)
	text = reGenericAgentIdentity.ReplaceAllString(text, genericReplacement)
	text = reActAsAgentIdentity.ReplaceAllString(text, "assist the user in this workspace")
	return text
}

const (
	// 综合协作模式（默认）：兼容代码与文档办公，措辞中立自然。
	clientCollaborationIntroGeneral = "This conversation is forwarded through the user's local desktop workspace app, and the target files reside inside folders authorized on the user's computer (not in remote Quick spaces, so do not search remote Quick spaces for local files). When reading, creating, editing, or exporting local files is helpful, emit a <client_tool_call> using the actions in <client_tool_protocol> so the user's desktop app performs it locally and returns the result in the next turn. For large documents or complex deliverables, work incrementally across multiple concise tool calls rather than exceeding streaming limits. Always match the user's language (for example, Simplified Chinese when the user writes in Chinese) for all user-facing replies while keeping file paths and tool JSON keys in their original form."

	// 文档与学术办公模式：专门消除代码工程偏向词汇，贴合论文审稿与 Word/PDF 办公协作。
	clientCollaborationIntroDocument = "This conversation is connected to the user's local desktop document app, where the user's manuscripts, papers, and office files (.docx, .pdf, .md, .pptx, .xlsx, etc.) reside in their authorized local computer folder rather than in remote Quick spaces (so do not search remote Quick spaces for those files). Your document actions use the <client_tool_call> text JSON bridge in <client_tool_protocol>: emit <client_tool_call> requests to list folders or read local documents first, and the desktop app will execute them locally and return the file contents in the next turn. Base your reviews, summaries, and edits strictly on the retrieved file contents, produce long reports in clear incremental turns if needed, and always reply in the user's language while preserving exact file paths and tool JSON keys."

	// 代码工程模式：保留完整开发协作引导与脚手架规则。
	clientCollaborationIntroCoding = "This conversation is connected to the user's local coding workspace client. The user's default expectation for file creation, code edits, HTML/SVG exports, and workspace checks is to perform them directly in their local project directory via the local tools in <client_tool_protocol>, rather than writing deliverables only to a remote sandbox workspace. Whenever fulfilling the request involves creating, modifying, exporting, or inspecting files, emit a <client_tool_call> using the listed local client tools (such as exec_command or apply_patch) so the user's client runs it locally and sends back the output. For large or intricate deliverables, work incrementally across multiple concise tool calls (scaffold first, then refine details and verify) so every turn finishes well within upstream streaming limits. Always match the user's language (for example, Simplified Chinese when the user writes in Chinese) for all user-facing progress notes, replies, and explanations while keeping code identifiers, file paths, and tool JSON keys in their original form."
)

// collaborationIntroForMode 根据工作场景模式返回对应的外层协作引导词。
func collaborationIntroForMode(workMode string) string {
	switch config.NormalizeWorkMode(workMode) {
	case config.WorkModeDocument:
		return clientCollaborationIntroDocument
	case config.WorkModeCoding:
		return clientCollaborationIntroCoding
	default:
		return clientCollaborationIntroGeneral
	}
}

// collectGuidelineParts 按消息顺序提取调用方的 system/developer 文本并执行温和清洗；
// 当 leadingOnly 为 true 时仅提取首条对话消息之前的连续前导规范（用于计算稳定的会话基线指纹）。
func collectGuidelineParts(msgs []Message, leadingOnly bool) []string {
	return collectGuidelinePartsForMode(msgs, leadingOnly, config.WorkModeGeneral)
}

func collectGuidelinePartsForMode(msgs []Message, leadingOnly bool, workMode string) []string {
	var parts []string
	for _, m := range msgs {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role != "system" && role != "developer" {
			if leadingOnly {
				break
			}
			continue
		}
		text := contentText(m.Content)
		if strings.TrimSpace(text) != "" {
			parts = append(parts, sanitizeCallerInstructionsForMode(text, workMode))
		}
	}
	return parts
}

// wrapWorkspaceGuidelines 将已清洗的规范片段包裹进工作区规范标签中。
func wrapWorkspaceGuidelines(parts []string) string {
	return wrapWorkspaceGuidelinesForMode(parts, config.WorkModeGeneral)
}

func wrapWorkspaceGuidelinesForMode(parts []string, workMode string) string {
	if len(parts) == 0 {
		return ""
	}
	header := "These are the user's workspace context and workflow preferences:"
	switch config.NormalizeWorkMode(workMode) {
	case config.WorkModeDocument:
		header = "These are the user's document workspace context and formatting preferences:"
	case config.WorkModeCoding:
		header = "These are the user's project guidelines, coding conventions, and workflow preferences:"
	}
	return "<workspace_guidelines>\n" + header + "\n\n" +
		strings.Join(parts, "\n\n") +
		"\n</workspace_guidelines>"
}

// callerGuidelines 提取全部调用方 system/developer 规范并包裹为 <workspace_guidelines> 块。
func callerGuidelines(msgs []Message) string {
	return callerGuidelinesForMode(msgs, config.WorkModeGeneral)
}

func callerGuidelinesForMode(msgs []Message, workMode string) string {
	return wrapWorkspaceGuidelinesForMode(collectGuidelinePartsForMode(msgs, false, workMode), workMode)
}

// leadingGuidelines 仅提取会话开头连续的 system/developer 规范，不受后续轮次末尾追加动态 developer 上下文的影响。
func leadingGuidelines(msgs []Message) string {
	return leadingGuidelinesForMode(msgs, config.WorkModeGeneral)
}

func leadingGuidelinesForMode(msgs []Message, workMode string) string {
	return wrapWorkspaceGuidelinesForMode(collectGuidelinePartsForMode(msgs, true, workMode), workMode)
}

// deltaDeveloperGuidelines 提取续聊增量消息切片中新增的 system/developer 上下文（如时间更新或动态目录上下文）。
func deltaDeveloperGuidelines(delta []Message) string {
	return deltaDeveloperGuidelinesForMode(delta, config.WorkModeGeneral)
}

func deltaDeveloperGuidelinesForMode(delta []Message, workMode string) string {
	return callerGuidelinesForMode(delta, workMode)
}

// compactSkillsSection 对 <app-context> 中的 Available skills 列表执行常规无损压缩：
// 保留每个技能名称与 (file: ...) 路径，将冗长触发说明精简为最多 72 字符的首句摘要。
func compactSkillsSection(text string) string {
	return compactSkillsSectionWithLimit(text, 72)
}

// compactSkillsIndexOnly 执行二档极限压缩：仅保留技能名称与 (file: ...) 路径索引，
// 最大程度腾出主帧空间以保住完整 <client_tool_protocol>。
func compactSkillsIndexOnly(text string) string {
	return compactSkillsSectionWithLimit(text, 0)
}

func compactSkillsSectionWithLimit(text string, maxDescRunes int) string {
	if !strings.Contains(text, "Available skills") {
		return text
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	inSkills := false
	changed := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !inSkills {
			out = append(out, line)
			if trimmed == "### Available skills" || trimmed == "## Available skills" {
				inSkills = true
			}
			continue
		}

		// 遇到下一节标题或闭合 XML 标签（如 </skills_instructions>）时退出压缩区
		if isSkillsSectionBoundary(trimmed) {
			inSkills = false
			out = append(out, line)
			continue
		}

		if compacted, ok := compactSkillLineWithLimit(line, maxDescRunes); ok {
			if compacted != line {
				changed = true
			}
			out = append(out, compacted)
			continue
		}
		out = append(out, line)
	}

	if !changed {
		return text
	}
	return strings.Join(out, "\n")
}

// isSkillsSectionBoundary 判断当前行是否标志着 Available skills 列表的结束。
func isSkillsSectionBoundary(trimmed string) bool {
	if strings.HasPrefix(trimmed, "# ") || strings.HasPrefix(trimmed, "## ") || strings.HasPrefix(trimmed, "### ") {
		return true
	}
	return strings.HasPrefix(trimmed, "</") && strings.HasSuffix(trimmed, ">")
}

// compactSkillLineWithLimit 将单条技能描述行按字符上限压缩；当 maxDescRunes <= 0 时仅保留名称与 (file: ...) 路径。
func compactSkillLineWithLimit(line string, maxDescRunes int) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "- ") {
		return line, false
	}
	colon := strings.IndexByte(trimmed, ':')
	if colon <= 2 {
		return line, false
	}
	head := trimmed[:colon]
	rest := strings.TrimSpace(trimmed[colon+1:])
	if rest == "" {
		return line, false
	}

	// 提取尾部或行内的 "(file: ...)" 路径信息
	fileRef := ""
	desc := rest
	if idx := strings.LastIndex(rest, "(file:"); idx >= 0 {
		if end := strings.IndexByte(rest[idx:], ')'); end > 0 {
			fileRef = " " + strings.TrimSpace(rest[idx:idx+end+1])
			desc = strings.TrimSpace(rest[:idx])
		}
	}

	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	if maxDescRunes <= 0 {
		if fileRef == "" {
			return indent + head, true
		}
		return indent + head + ":" + fileRef, true
	}

	short := summarizeSkillDesc(desc, maxDescRunes)
	if short == "" {
		return indent + head + ":" + fileRef, true
	}
	return indent + head + ": " + short + fileRef, true
}

// summarizeSkillDesc 截取技能描述的第一句并按字符上限精简，保留完整 UTF-8 字符边界。
func summarizeSkillDesc(desc string, maxRunes int) string {
	if desc == "" {
		return ""
	}
	// 优先取首句
	for _, sep := range []string{"。", ". "} {
		if idx := strings.Index(desc, sep); idx > 0 {
			candidate := strings.TrimSpace(desc[:idx+len(sep)])
			if utf8.RuneCountInString(candidate) <= maxRunes {
				return strings.TrimRight(candidate, " ")
			}
			desc = strings.TrimSpace(desc[:idx])
			break
		}
	}
	if utf8.RuneCountInString(desc) <= maxRunes {
		return desc
	}
	runes := []rune(desc)
	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}
	return strings.TrimSpace(string(runes[:maxRunes-3])) + "..."
}

// systemPrompt 将调用方的 system/developer 指令清洗并包裹进工作区规范标签，再依次拼接扩展协议块（如工具定义）。
func systemPrompt(msgs []Message, extra ...string) string {
	var blocks []string
	if g := callerGuidelines(msgs); g != "" {
		blocks = append(blocks, g)
	}
	for _, text := range extra {
		if strings.TrimSpace(text) != "" {
			blocks = append(blocks, text)
		}
	}
	return strings.Join(blocks, "\n\n")
}

// hasCallerInstructions reports whether the request carries system/developer
// text worth relaying at the head of the user turn.
func hasCallerInstructions(msgs []Message) bool {
	for _, m := range msgs {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role != "system" && role != "developer" {
			continue
		}
		if strings.TrimSpace(contentText(m.Content)) != "" {
			return true
		}
	}
	return false
}

// assembleRelayPrompt 将工作区规范、客户端工具协议与用户请求按结构组装为完整的发送负载。
func assembleRelayPrompt(guidelines, toolProtocol, transcript string) string {
	return assembleRelayPromptForMode(config.WorkModeGeneral, guidelines, toolProtocol, transcript)
}

func assembleRelayPromptForMode(workMode, guidelines, toolProtocol, transcript string) string {
	var blocks []string
	if strings.TrimSpace(guidelines) != "" {
		blocks = append(blocks, guidelines)
	}
	if strings.TrimSpace(toolProtocol) != "" {
		blocks = append(blocks, toolProtocol)
	}
	return relayPromptForMode(workMode, strings.Join(blocks, "\n\n"), transcript)
}

// relayPrompt 以诚实非对抗的协作隔离结构将工作区规范、客户端工具协议与用户请求分隔封装。
func relayPrompt(instructions, transcript string) string {
	return relayPromptForMode(config.WorkModeGeneral, instructions, transcript)
}

func relayPromptForMode(workMode, instructions, transcript string) string {
	if strings.TrimSpace(instructions) == "" {
		return transcript
	}
	var b strings.Builder
	b.WriteString("<client_collaboration_context>\n")
	b.WriteString(collaborationIntroForMode(workMode))
	b.WriteString("\n\n")
	b.WriteString(instructions)
	b.WriteString("\n</client_collaboration_context>\n\n<user_request>\n")
	b.WriteString(transcript)
	b.WriteString("\n</user_request>")
	return b.String()
}

// lastUserText returns the most recent non-empty user prompt, used for the
// upstream session title.
func lastUserText(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if !strings.EqualFold(strings.TrimSpace(msgs[i].Role), "user") {
			continue
		}
		if text := strings.TrimSpace(contentText(msgs[i].Content)); text != "" {
			return text
		}
	}
	return ""
}

// latestTurnPreview 在长对话外置为附件时提取本轮主帧内联预览：
// 若末尾消息为客户端工具回传结果（role=tool），则按顺序汇总工具结果，避免误复读上一轮 user 提问；否则回退最新 user 文本。
func latestTurnPreview(msgs []Message) string {
	toolNames := make(map[string]string)
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" && tc.Function.Name != "" {
				toolNames[tc.ID] = tc.Function.Name
			}
		}
	}

	// 从末尾向前寻找最后一条非 system/developer 消息，仅当其为 tool 时收集末尾连续工具回传块
	var toolIndices []int
	for i := len(msgs) - 1; i >= 0; i-- {
		role := strings.ToLower(strings.TrimSpace(msgs[i].Role))
		if role == "system" || role == "developer" {
			continue
		}
		if role == "tool" {
			toolIndices = append(toolIndices, i)
			continue
		}
		break
	}
	if len(toolIndices) > 0 {
		parts := make([]string, 0, len(toolIndices))
		for k := len(toolIndices) - 1; k >= 0; k-- {
			m := msgs[toolIndices[k]]
			name := strings.TrimSpace(m.Name)
			if name == "" {
				name = toolNames[strings.TrimSpace(m.ToolCallID)]
			}
			label := "Tool result"
			if name != "" {
				label = "Tool result (" + name + ")"
			}
			body := strings.TrimSpace(contentText(m.Content))
			if body == "" {
				body = "(empty)"
			}
			parts = append(parts, label+": "+body)
		}
		return strings.Join(parts, "\n\n")
	}
	return lastUserText(msgs)
}

// requestLogPreview 生成供请求记录页面展示的精准摘要：
// 若当前轮末尾是工具结果回传，在摘要前缀标明本轮所执行工具及参数目标，避免连续数十轮都只显示旧的 "继续"。
func requestLogPreview(msgs []Message) string {
	toolNames := make(map[string]string)
	toolTargets := make(map[string]string)
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				if tc.Function.Name != "" {
					toolNames[tc.ID] = tc.Function.Name
				}
				if tgt := compactToolCallTarget(tc.Function.Arguments); tgt != "" {
					toolTargets[tc.ID] = tgt
				}
			}
		}
	}
	var toolLabels []string
	for i := len(msgs) - 1; i >= 0; i-- {
		role := strings.ToLower(strings.TrimSpace(msgs[i].Role))
		if role == "system" || role == "developer" {
			continue
		}
		if role == "tool" || role == "function" {
			name := strings.TrimSpace(msgs[i].Name)
			id := strings.TrimSpace(msgs[i].ToolCallID)
			if name == "" && id != "" {
				name = toolNames[id]
			}
			if name == "" {
				name = "tool"
			}
			if tgt := toolTargets[id]; tgt != "" {
				toolLabels = append(toolLabels, name+"("+tgt+")")
			} else {
				toolLabels = append(toolLabels, name)
			}
			continue
		}
		if len(toolLabels) > 0 {
			break
		}
		if role == "user" {
			if text := strings.TrimSpace(contentText(msgs[i].Content)); text != "" {
				return text
			}
		}
		break
	}
	if len(toolLabels) > 0 {
		for l, r := 0, len(toolLabels)-1; l < r; l, r = l+1, r-1 {
			toolLabels[l], toolLabels[r] = toolLabels[r], toolLabels[l]
		}
		userTail := lastUserText(msgs)
		prefix := "[Tool -> " + strings.Join(toolLabels, ", ") + "]"
		if userTail != "" {
			return prefix + " " + userTail
		}
		return prefix
	}
	return lastUserText(msgs)
}
