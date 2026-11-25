package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func makeFakePNG(tag byte) []byte {
	return []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 'I', 'H', 'D', 'R', tag}
}

func makeFakeJPEG(tag byte) []byte {
	return []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', tag}
}

func makeFakeWEBP(tag byte) []byte {
	return []byte{'R', 'I', 'F', 'F', 0x24, 0x00, 0x00, 0x00, 'W', 'E', 'B', 'P', 'V', 'P', '8', ' ', tag}
}

// TestFormatPromptWithImages_FormatsAndDedup 覆盖多种图片格式、工具回图、自动嗅探及去重逻辑。
func TestFormatPromptWithImages_FormatsAndDedup(t *testing.T) {
	pngBytes := makeFakePNG(1)
	jpegBytes := makeFakeJPEG(2)
	webpBytes := makeFakeWEBP(3)

	pngB64 := base64.StdEncoding.EncodeToString(pngBytes)
	jpegB64 := base64.StdEncoding.EncodeToString(jpegBytes)
	webpB64 := base64.StdEncoding.EncodeToString(webpBytes)

	msgs := []Message{
		{
			Role: "user",
			Content: []any{
				map[string]any{"type": "text", "text": "Look at object URL: "},
				map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": "data:image/png;base64," + pngB64},
				},
				map[string]any{"type": "text", "text": " and string URL: "},
				map[string]any{
					"type":      "image_url",
					"image_url": "data:image/jpeg;base64," + jpegB64,
				},
			},
		},
		{
			Role:       "tool",
			ToolCallID: "call_1",
			Name:       "capture",
			Content: []any{
				map[string]any{"type": "text", "text": "screenshot ready: "},
				// 无 MIME 声明的 input_image，验证通过魔数自动嗅探为 WEBP
				map[string]any{
					"type":      "input_image",
					"image_url": "data:application/octet-stream;base64," + webpB64,
				},
				// Anthropic 风格且内容与首张 PNG 完全一致，验证去重并复用首张文件名
				map[string]any{
					"type": "image",
					"source": map[string]any{
						"type":       "base64",
						"media_type": "image/png",
						"data":       pngB64,
					},
				},
			},
		},
	}

	prompt, imgs, err := formatPromptWithImages(context.Background(), msgs)
	if err != nil {
		t.Fatalf("formatPromptWithImages returned error: %v", err)
	}
	if len(imgs) != 3 {
		t.Fatalf("got %d deduplicated images, want 3", len(imgs))
	}
	if imgs[0].MIMEType != "image/png" || !strings.HasSuffix(imgs[0].Name, ".png") {
		t.Fatalf("img[0] unexpected: name=%q mime=%q", imgs[0].Name, imgs[0].MIMEType)
	}
	if imgs[1].MIMEType != "image/jpeg" || !strings.HasSuffix(imgs[1].Name, ".jpg") {
		t.Fatalf("img[1] unexpected: name=%q mime=%q", imgs[1].Name, imgs[1].MIMEType)
	}
	if imgs[2].MIMEType != "image/webp" || !strings.HasSuffix(imgs[2].Name, ".webp") {
		t.Fatalf("img[2] unexpected: name=%q mime=%q", imgs[2].Name, imgs[2].MIMEType)
	}

	// 首张 PNG 在 tool 消息中应再次出现完全相同的 [attached image: ...] 引用
	marker := "[attached image: " + imgs[0].Name + "]"
	if count := strings.Count(prompt, marker); count != 2 {
		t.Fatalf("expected %q to appear twice for deduplicated image, got %d in %q", marker, count, prompt)
	}
	if !strings.Contains(prompt, "Tool result (call_id=call_1, name=capture): screenshot ready: [attached image: "+imgs[2].Name+"]") {
		t.Fatalf("tool result prompt missing expected attachment reference: %q", prompt)
	}
}

// TestFormatPromptWithImages_MaxLimitKeepsLatest 验证超过单轮 16 张上限时保留最新的图片。
func TestFormatPromptWithImages_MaxLimitKeepsLatest(t *testing.T) {
	parts := make([]any, 0, 20)
	for i := 0; i < 20; i++ {
		raw := makeFakePNG(byte(i + 1))
		parts = append(parts, map[string]any{
			"type":      "input_image",
			"image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw),
		})
	}
	_, imgs, err := formatPromptWithImages(context.Background(), []Message{{
		Role:    "user",
		Content: parts,
	}})
	if err != nil {
		t.Fatalf("formatPromptWithImages failed: %v", err)
	}
	if len(imgs) != maxImagesPerTurn {
		t.Fatalf("got %d images, want %d", len(imgs), maxImagesPerTurn)
	}
	if !strings.HasPrefix(imgs[0].Name, "img-05-") {
		t.Fatalf("expected oldest kept image to start with img-05-, got %q", imgs[0].Name)
	}
	if !strings.HasPrefix(imgs[len(imgs)-1].Name, "img-20-") {
		t.Fatalf("expected newest kept image to start with img-20-, got %q", imgs[len(imgs)-1].Name)
	}
}

// TestContentText_SkipsRawBase64ForInputImage 验证纯文本压平路径不会泄露或序列化 base64 图片负载。
func TestContentText_SkipsRawBase64ForInputImage(t *testing.T) {
	secret := "SUPERSECRETBASE64PAYLOAD"
	msgs := []any{
		map[string]any{"type": "text", "text": "hello "},
		map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + secret},
		map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": "image/png",
				"data":       secret,
			},
		},
		map[string]any{
			"type": "image",
			"source": map[string]any{
				"type": "url",
				"url":  "https://example.com/pic.png",
			},
		},
	}
	got := contentText(msgs)
	if strings.Contains(got, secret) {
		t.Fatalf("contentText leaked raw base64: %q", got)
	}
	want := "hello [image][image][image: https://example.com/pic.png]"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestUploadImageAttachments_MockServer 使用模拟 HTTP 服务验证图片三步上传与部分失败降级。
func TestUploadImageAttachments_MockServer(t *testing.T) {
	var seq atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/integration/quick-work/upload-attachment":
			name := r.URL.Query().Get("name")
			ct := r.URL.Query().Get("contentType")
			if name == "fail.png" {
				http.Error(w, "simulated error", http.StatusInternalServerError)
				return
			}
			if ct != "image/png" {
				http.Error(w, fmt.Sprintf("unexpected contentType %q", ct), http.StatusBadRequest)
				return
			}
			n := seq.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"attachmentId": fmt.Sprintf("att-%d-%s", n, name),
			})
		case "/integration/quick-work/attachment-dlp-status":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"dlpJobStatus": "COMPLETED",
				"policyAction": "ALLOW",
			})
		case "/integration/quick-work/qbs-event":
			var ev struct {
				EventBody struct {
					InvokeAgent struct {
						Path string `json:"path"`
						Body struct {
							AttachmentID string `json:"attachmentId"`
						} `json:"body"`
					} `json:"invokeAgent"`
				} `json:"eventBody"`
			}
			if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if ev.EventBody.InvokeAgent.Path != "files/upload/thread-test" {
				http.Error(w, "unexpected invoke path: "+ev.EventBody.InvokeAgent.Path, http.StatusBadRequest)
				return
			}
			inner, _ := json.Marshal(map[string]string{
				"path": "/data/s3files/test/" + ev.EventBody.InvokeAgent.Body.AttachmentID + ".png",
			})
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"eventResponse": map[string]any{
					"invokeAgentResponse": map[string]any{
						"body": string(inner),
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	sess := &Session{
		JWT:       "test-jwt",
		TenantURL: ts.URL,
		Provider:  "social",
	}
	images := []ImageAttachment{
		{Name: "ok.png", MIMEType: "image/png", Data: makeFakePNG(1)},
		{Name: "fail.png", MIMEType: "image/png", Data: makeFakePNG(2)},
	}

	metas, failed := uploadImageAttachments(context.Background(), sess, "thread-test", images)
	if len(metas) != 1 {
		t.Fatalf("got %d uploaded metas, want 1 (failed=%v)", len(metas), failed)
	}
	if len(failed) != 1 || !strings.Contains(failed[0], "fail.png") {
		t.Fatalf("unexpected failed list: %v", failed)
	}
	m := metas[0]
	if m["type"] != "image" || m["mimeType"] != "image/png" || m["name"] != "ok.png" {
		t.Fatalf("unexpected image meta: %#v", m)
	}
	attID, _ := m["attachmentId"].(string)
	path, _ := m["path"].(string)
	if attID == "" || !strings.HasPrefix(path, "/data/s3files/test/") {
		t.Fatalf("missing attachmentId or path in meta: %#v", m)
	}
}
