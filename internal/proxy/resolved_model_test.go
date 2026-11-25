package proxy

import "testing"

// TestUsageAddPreservesResolvedModelID 验证 Usage.add 能保留并更新非空的实际响应模型代号。
func TestUsageAddPreservesResolvedModelID(t *testing.T) {
	var total Usage
	total.add(Usage{
		PromptTokens:    4,
		ResolvedModelID: "us.anthropic.claude-opus-5-5",
	})
	if total.ResolvedModelID != "us.anthropic.claude-opus-5-5" {
		t.Fatalf("got %q, want %q", total.ResolvedModelID, "us.anthropic.claude-opus-5-5")
	}

	// 空值不应覆盖已记录的底层模型代号
	total.add(Usage{
		CompletionTokens: 8,
		ResolvedModelID:  "   ",
	})
	if total.ResolvedModelID != "us.anthropic.claude-opus-5-5" {
		t.Fatalf("empty add overwrote ResolvedModelID to %q", total.ResolvedModelID)
	}

	// 后续存在新代号时应正确更新
	total.add(Usage{
		ResolvedModelID: "us.anthropic.claude-sonnet-5-5",
	})
	if total.ResolvedModelID != "us.anthropic.claude-sonnet-5-5" {
		t.Fatalf("got %q, want %q", total.ResolvedModelID, "us.anthropic.claude-sonnet-5-5")
	}
}

// TestResolvedResponseModelFallback 验证有实际模型代号时优先返回代号，缺失时回退为请求模式名。
func TestResolvedResponseModelFallback(t *testing.T) {
	if got := resolvedResponseModel("auto", Usage{ResolvedModelID: "us.anthropic.claude-opus-5-5"}); got != "us.anthropic.claude-opus-5-5" {
		t.Fatalf("got %q, want %q", got, "us.anthropic.claude-opus-5-5")
	}
	if got := resolvedResponseModel("fast", Usage{}); got != "fast" {
		t.Fatalf("got %q, want fallback %q", got, "fast")
	}
}
