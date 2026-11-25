package admin

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"quickrelay-studio/internal/store"
)

func (s *Server) handleAnalyticsSummary(w http.ResponseWriter, r *http.Request) {
	rangeKey := r.URL.Query().Get("range")
	summary, err := s.store.GetAnalyticsSummary(r.Context(), rangeKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "analytics_error", "Could not compute request analytics summary")
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func requestLogFilterFromRequest(r *http.Request, defaultLimit int) store.RequestLogFilter {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = defaultLimit
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	acctID, _ := strconv.ParseInt(q.Get("account_id"), 10, 64)
	return store.RequestLogFilter{
		Status:    strings.TrimSpace(q.Get("status")),
		Protocol:  strings.TrimSpace(q.Get("protocol")),
		Model:     strings.TrimSpace(q.Get("model")),
		AccountID: acctID,
		Query:     strings.TrimSpace(q.Get("q")),
		Limit:     limit,
		Offset:    offset,
	}
}

func (s *Server) handleRequestLogs(w http.ResponseWriter, r *http.Request) {
	filter := requestLogFilterFromRequest(r, 50)
	page, err := s.store.QueryRequestLogs(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "logs_query_error", "Could not query request logs")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func sanitizeCSVCell(v string) string {
	if v == "" {
		return ""
	}
	switch v[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + v
	default:
		return v
	}
}

func (s *Server) handleRequestLogsExport(w http.ResponseWriter, r *http.Request) {
	filter := requestLogFilterFromRequest(r, 2000)
	filter.Offset = 0
	page, err := s.store.QueryRequestLogs(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "logs_export_error", "Could not export request logs")
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="quickrelay-usage-logs.csv"`)
	w.WriteHeader(http.StatusOK)
	// 写入 UTF-8 BOM 确保 Windows Excel 直接双击打开中文列标题零乱码
	_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF})

	cw := csv.NewWriter(w)
	_ = cw.Write([]string{
		"ID", "时间", "请求ID", "协议", "接口", "请求模型", "路由模型", "解析模型",
		"思考强度", "流式", "账号ID", "账号邮箱", "云端ThreadID", "复用1M上下文",
		"状态", "结束原因", "重试次数", "耗时(ms)", "输入Token", "输出Token",
		"总Token", "缓存读取", "缓存写入", "工具调用数", "图片数", "首句预览", "错误信息",
	})
	for _, item := range page.Items {
		ts := time.Unix(item.CreatedAt, 0).Local().Format("2006-01-02 15:04:05")
		_ = cw.Write([]string{
			strconv.FormatInt(item.ID, 10),
			ts,
			sanitizeCSVCell(item.RequestID),
			sanitizeCSVCell(item.Protocol),
			sanitizeCSVCell(item.Endpoint),
			sanitizeCSVCell(item.RequestedModel),
			sanitizeCSVCell(item.UpstreamModel),
			sanitizeCSVCell(item.ResolvedModel),
			sanitizeCSVCell(item.ThinkingEffort),
			strconv.FormatBool(item.Stream),
			strconv.FormatInt(item.AccountID, 10),
			sanitizeCSVCell(item.AccountEmail),
			sanitizeCSVCell(item.ThreadID),
			strconv.FormatBool(item.SessionReused),
			sanitizeCSVCell(item.Status),
			sanitizeCSVCell(item.FinishReason),
			strconv.Itoa(item.Attempts),
			strconv.FormatInt(item.DurationMS, 10),
			strconv.Itoa(item.PromptTokens),
			strconv.Itoa(item.CompletionTokens),
			strconv.Itoa(item.TotalTokens),
			strconv.Itoa(item.CacheReadTokens),
			strconv.Itoa(item.CacheWriteTokens),
			strconv.Itoa(item.ToolCallsCount),
			strconv.Itoa(item.ImagesCount),
			sanitizeCSVCell(item.PromptPreview),
			sanitizeCSVCell(item.ErrorMessage),
		})
	}
	cw.Flush()
}

func (s *Server) handleRequestLogsClear(w http.ResponseWriter, r *http.Request) {
	if err := s.store.ClearRequestLogs(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "logs_clear_error", "Could not clear request logs")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cleared": true})
}

func (s *Server) handleConversationsList(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := s.store.ListConversations(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "conversations_error", "Could not list persisted conversations")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversations": items})
}

func (s *Server) handleConversationsClear(w http.ResponseWriter, r *http.Request) {
	if err := s.store.ClearConversations(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "conversations_clear_error", "Could not clear conversations")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cleared": true})
}

func (s *Server) handleConversationDelete(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		key = strings.TrimSpace(r.PathValue("key"))
	}
	if key == "" {
		writeError(w, http.StatusBadRequest, "invalid_conversation_key", fmt.Sprintf("Conversation key must not be empty"))
		return
	}
	if err := s.store.DeleteConversation(r.Context(), key); err != nil {
		writeError(w, http.StatusInternalServerError, "conversation_delete_error", "Could not delete conversation")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": key})
}
