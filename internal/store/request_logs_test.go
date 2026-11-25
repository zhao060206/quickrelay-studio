package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open(%q) failed: %v", dbPath, err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})
	return st
}

// TestRequestLogsAndAnalytics 验证请求明细持久化、条件检索、时间范围统计聚合与清空流程。
func TestRequestLogsAndAnalytics(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	now := time.Now().Unix()

	// 先插入两条账号以测试按账号聚合与 email 关联
	a1 := &Account{Email: "alice@example.com", Provider: "social", AccessToken: "tok1"}
	a2 := &Account{Email: "bob@example.com", Provider: "enterprise", AccessToken: "tok2"}
	if err := st.Save(ctx, a1); err != nil {
		t.Fatalf("Save a1: %v", err)
	}
	if err := st.Save(ctx, a2); err != nil {
		t.Fatalf("Save a2: %v", err)
	}

	entries := []RequestLog{
		{
			RequestID:        "msg_001",
			CreatedAt:        now - 120,
			Protocol:         "anthropic",
			Endpoint:         "/v1/messages",
			RequestedModel:   "claude-sonnet-4-5",
			UpstreamModel:    "smart",
			ResolvedModel:    "claude-sonnet-4-5",
			ThinkingEffort:   "high",
			Stream:           true,
			AccountID:        a1.ID,
			AccountEmail:     a1.Email,
			ThreadID:         "thread-a",
			SessionReused:    true,
			Status:           "ok",
			FinishReason:     "tool_use",
			Attempts:         1,
			DurationMS:       450,
			PromptTokens:     1000,
			CompletionTokens: 200,
			TotalTokens:      1500,
			CacheReadTokens:  300,
			CacheWriteTokens: 0,
			CostUSD:          0.012,
			ToolCallsCount:   2,
			ImagesCount:      1,
			PromptPreview:    "请帮我重构 Claude Code 接口",
		},
		{
			RequestID:        "chatcmpl_002",
			CreatedAt:        now - 60,
			Protocol:         "openai",
			Endpoint:         "/v1/chat/completions",
			RequestedModel:   "claude-haiku-4-5",
			UpstreamModel:    "fast",
			ResolvedModel:    "fast",
			Stream:           false,
			AccountID:        a2.ID,
			AccountEmail:     a2.Email,
			Status:           "failed",
			ErrorMessage:     "upstream timeout error",
			Attempts:         2,
			DurationMS:       150,
			PromptTokens:     100,
			CompletionTokens: 0,
			TotalTokens:      100,
			CostUSD:          0.001,
			PromptPreview:    "生成标题",
		},
		{
			RequestID:        "msg_old",
			CreatedAt:        now - int64(4*24*3600), // 4 天前，不应进入 24h 窗口但应进入 7d
			Protocol:         "anthropic",
			Endpoint:         "/v1/messages",
			RequestedModel:   "claude-opus-4-5",
			UpstreamModel:    "smart",
			ResolvedModel:    "smart",
			Status:           "ok",
			AccountID:        a1.ID,
			AccountEmail:     a1.Email,
			DurationMS:       300,
			PromptTokens:     500,
			CompletionTokens: 100,
			TotalTokens:      600,
			CostUSD:          0.005,
			PromptPreview:    "四天前的历史调用",
		},
	}

	for _, e := range entries {
		if err := st.InsertRequestLog(ctx, e); err != nil {
			t.Fatalf("InsertRequestLog(%s): %v", e.RequestID, err)
		}
	}

	// 1. 测试筛选：按协议与状态筛选
	pageAnthropic, err := st.QueryRequestLogs(ctx, RequestLogFilter{
		Protocol: "anthropic",
		Status:   "ok",
		Limit:    10,
	})
	if err != nil {
		t.Fatalf("QueryRequestLogs anthropic: %v", err)
	}
	if pageAnthropic.Total != 2 || len(pageAnthropic.Items) != 2 {
		t.Fatalf("expected 2 ok anthropic rows, got total=%d len=%d", pageAnthropic.Total, len(pageAnthropic.Items))
	}
	if pageAnthropic.Items[0].RequestID != "msg_001" || !pageAnthropic.Items[0].SessionReused {
		t.Fatalf("unexpected first item order/fields: %+v", pageAnthropic.Items[0])
	}

	// 2. 测试关键词检索
	pageQuery, err := st.QueryRequestLogs(ctx, RequestLogFilter{Query: "timeout"})
	if err != nil {
		t.Fatalf("QueryRequestLogs query: %v", err)
	}
	if pageQuery.Total != 1 || pageQuery.Items[0].RequestID != "chatcmpl_002" {
		t.Fatalf("expected 1 match for timeout, got %+v", pageQuery)
	}

	// 3. 测试 24h 统计聚合（仅包含前两条，不包含 4 天前）
	sum24h, err := st.GetAnalyticsSummary(ctx, "24h")
	if err != nil {
		t.Fatalf("GetAnalyticsSummary 24h: %v", err)
	}
	if sum24h.TotalRequests != 2 || sum24h.SuccessRequests != 1 || sum24h.FailedRequests != 1 {
		t.Fatalf("unexpected 24h counts: %+v", sum24h)
	}
	if sum24h.SuccessRate != 50.0 {
		t.Fatalf("expected 50.0 success rate, got %v", sum24h.SuccessRate)
	}
	if sum24h.TotalTokens != 1600 || sum24h.CacheReadTokens != 300 || sum24h.SessionReusedCnt != 1 || sum24h.ToolCallsCount != 2 {
		t.Fatalf("unexpected 24h token/session metrics: %+v", sum24h)
	}
	if len(sum24h.Timeline) != 24 {
		t.Fatalf("expected 24 hourly buckets, got %d", len(sum24h.Timeline))
	}
	if len(sum24h.ByProtocol) != 2 || len(sum24h.ByModel) != 2 || len(sum24h.ByAccount) != 2 {
		t.Fatalf("unexpected breakdowns: proto=%d model=%d acc=%d", len(sum24h.ByProtocol), len(sum24h.ByModel), len(sum24h.ByAccount))
	}

	// 4. 测试 7d 统计聚合（应包含全部 3 条）
	sum7d, err := st.GetAnalyticsSummary(ctx, "7d")
	if err != nil {
		t.Fatalf("GetAnalyticsSummary 7d: %v", err)
	}
	if sum7d.TotalRequests != 3 || sum7d.SuccessRequests != 2 || len(sum7d.Timeline) != 7 {
		t.Fatalf("unexpected 7d summary: %+v (buckets=%d)", sum7d, len(sum7d.Timeline))
	}

	// 5. 测试清空明细
	if err := st.ClearRequestLogs(ctx); err != nil {
		t.Fatalf("ClearRequestLogs: %v", err)
	}
	afterClear, err := st.QueryRequestLogs(ctx, RequestLogFilter{})
	if err != nil {
		t.Fatalf("QueryRequestLogs after clear: %v", err)
	}
	if afterClear.Total != 0 {
		t.Fatalf("expected 0 logs after clear, got %d", afterClear.Total)
	}
}

// TestConversationsListAndClear 验证 1M 持久化会话列表、数量统计、关联邮箱及清理功能。
func TestConversationsListAndClear(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	acct := &Account{Email: "conv@example.com", Provider: "social", AccessToken: "tok"}
	if err := st.Save(ctx, acct); err != nil {
		t.Fatalf("Save account: %v", err)
	}

	now := time.Now().Unix()
	if err := st.UpsertConversation(ctx, &ConversationRecord{
		Key:           "conv_k1",
		ThreadID:      "th_111",
		AccountID:     acct.ID,
		BaseHash:      "abc",
		MessageHashes: []string{"h1", "h2", "h3"},
		LastUsed:      now,
	}); err != nil {
		t.Fatalf("UpsertConversation: %v", err)
	}

	cnt, err := st.CountConversations(ctx)
	if err != nil || cnt != 1 {
		t.Fatalf("expected 1 conversation, got %d (err=%v)", cnt, err)
	}

	list, err := st.ListConversations(ctx, 20)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListConversations failed: %v len=%d", err, len(list))
	}
	if list[0].Key != "conv_k1" || list[0].AccountEmail != "conv@example.com" || list[0].TurnsTracked != 3 {
		t.Fatalf("unexpected conversation summary: %+v", list[0])
	}

	if err := st.ClearConversations(ctx); err != nil {
		t.Fatalf("ClearConversations: %v", err)
	}
	cnt, err = st.CountConversations(ctx)
	if err != nil || cnt != 0 {
		t.Fatalf("expected 0 conversations after clear, got %d (err=%v)", cnt, err)
	}
}
