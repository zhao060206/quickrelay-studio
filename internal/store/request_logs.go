package store

import (
	"context"
	"math"
	"strings"
	"sync/atomic"
	"time"
)

const (
	maxRequestLogRows  = 10000
	requestLogTrimStep = 64
)

// RequestLog 保存单次 API 请求的结构化耗时、模型、账号与 Token 明细，用于审计与图表统计。
type RequestLog struct {
	ID               int64   `json:"id"`
	RequestID        string  `json:"request_id"`
	CreatedAt        int64   `json:"created_at"`
	Protocol         string  `json:"protocol"`
	Endpoint         string  `json:"endpoint"`
	RequestedModel   string  `json:"requested_model"`
	UpstreamModel    string  `json:"upstream_model"`
	ResolvedModel    string  `json:"resolved_model"`
	ThinkingEffort   string  `json:"thinking_effort"`
	Stream           bool    `json:"stream"`
	AccountID        int64   `json:"account_id"`
	AccountEmail     string  `json:"account_email"`
	ThreadID         string  `json:"thread_id"`
	SessionReused    bool    `json:"session_reused"`
	Status           string  `json:"status"`
	FinishReason     string  `json:"finish_reason"`
	ErrorMessage     string  `json:"error_message"`
	Attempts         int     `json:"attempts"`
	DurationMS       int64   `json:"duration_ms"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	CacheReadTokens  int     `json:"cache_read_tokens"`
	CacheWriteTokens int     `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	ToolCallsCount   int     `json:"tool_calls_count"`
	ImagesCount      int     `json:"images_count"`
	PromptPreview    string  `json:"prompt_preview"`
}

// RequestLogFilter 定义使用记录分页查询与导出过滤条件。
type RequestLogFilter struct {
	Status    string
	Protocol  string
	Model     string
	AccountID int64
	Query     string
	Limit     int
	Offset    int
}

// RequestLogPage 返回分页查询结果。
type RequestLogPage struct {
	Total int          `json:"total"`
	Items []RequestLog `json:"items"`
}

// AnalyticsBucket 表示趋势图中的一个时间分桶统计点。
type AnalyticsBucket struct {
	BucketStart int64  `json:"bucket_start"`
	Label       string `json:"label"`
	Requests    int64  `json:"requests"`
	Successes   int64  `json:"successes"`
	Failures    int64  `json:"failures"`
	TotalTokens int64  `json:"total_tokens"`
	CacheRead   int64  `json:"cache_read_tokens"`
	AvgDurMS    int64  `json:"avg_duration_ms"`
}

// ModelBreakdown 按实际解析或映射模型汇总的请求与 Token 分布。
type ModelBreakdown struct {
	Model       string `json:"model"`
	Requests    int64  `json:"requests"`
	Successes   int64  `json:"successes"`
	TotalTokens int64  `json:"total_tokens"`
	AvgDurMS    int64  `json:"avg_duration_ms"`
}

// AccountBreakdown 按账号汇总的请求、成功率与缓存命中情况。
type AccountBreakdown struct {
	AccountID   int64  `json:"account_id"`
	Email       string `json:"email"`
	Requests    int64  `json:"requests"`
	Successes   int64  `json:"successes"`
	Failures    int64  `json:"failures"`
	TotalTokens int64  `json:"total_tokens"`
	AvgDurMS    int64  `json:"avg_duration_ms"`
}

// ProtocolBreakdown 按入口协议（OpenAI / Anthropic）汇总使用量。
type ProtocolBreakdown struct {
	Protocol    string `json:"protocol"`
	Requests    int64  `json:"requests"`
	Successes   int64  `json:"successes"`
	TotalTokens int64  `json:"total_tokens"`
}

// AnalyticsSummary 返回指定时间窗口内的全量聚合指标与可视化图表数据。
type AnalyticsSummary struct {
	Range             string              `json:"range"`
	SinceUnix         int64               `json:"since_unix"`
	GeneratedAt       int64               `json:"generated_at"`
	TotalRequests     int64               `json:"total_requests"`
	SuccessRequests   int64               `json:"success_requests"`
	FailedRequests    int64               `json:"failed_requests"`
	SuccessRate       float64             `json:"success_rate"`
	AvgDurationMS     int64               `json:"avg_duration_ms"`
	PromptTokens      int64               `json:"prompt_tokens"`
	CompletionTokens  int64               `json:"completion_tokens"`
	TotalTokens       int64               `json:"total_tokens"`
	CacheReadTokens   int64               `json:"cache_read_tokens"`
	CacheWriteTokens  int64               `json:"cache_write_tokens"`
	CacheHitRate      float64             `json:"cache_hit_rate"`
	SessionReusedCnt  int64               `json:"session_reused_count"`
	ToolCallsCount    int64               `json:"tool_calls_count"`
	TotalCostUSD      float64             `json:"total_cost_usd"`
	Timeline          []AnalyticsBucket   `json:"timeline"`
	ByModel           []ModelBreakdown    `json:"by_model"`
	ByAccount         []AccountBreakdown  `json:"by_account"`
	ByProtocol        []ProtocolBreakdown `json:"by_protocol"`
}

var logInsertCounter atomic.Uint64

func truncateRunes(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return strings.TrimSpace(string(runes[:maxRunes-1])) + "…"
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// InsertRequestLog 将单次代理请求明细写入 SQLite，并按需裁剪历史记录保留最新 10,000 条。
func (s *Store) InsertRequestLog(ctx context.Context, entry RequestLog) error {
	if s == nil || s.db == nil {
		return nil
	}
	if entry.CreatedAt <= 0 {
		entry.CreatedAt = time.Now().Unix()
	}
	if strings.TrimSpace(entry.Protocol) == "" {
		entry.Protocol = "openai"
	}
	if strings.TrimSpace(entry.Status) == "" {
		entry.Status = "ok"
	}
	if entry.Attempts < 1 {
		entry.Attempts = 1
	}
	if entry.DurationMS < 0 {
		entry.DurationMS = 0
	}
	if math.IsNaN(entry.CostUSD) || math.IsInf(entry.CostUSD, 0) || entry.CostUSD < 0 {
		entry.CostUSD = 0
	}
	preview := truncateRunes(entry.PromptPreview, 120)
	errMsg := truncateRunes(entry.ErrorMessage, 280)

	const insSQL = `INSERT INTO request_logs (
		request_id, created_at, protocol, endpoint,
		requested_model, upstream_model, resolved_model, thinking_effort,
		stream, account_id, account_email, thread_id, session_reused,
		status, finish_reason, error_message, attempts, duration_ms,
		prompt_tokens, completion_tokens, total_tokens, cache_read_tokens, cache_write_tokens,
		cost_usd, tool_calls_count, images_count, prompt_preview
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	_, err := s.db.ExecContext(ctx, insSQL,
		entry.RequestID, entry.CreatedAt, entry.Protocol, entry.Endpoint,
		entry.RequestedModel, entry.UpstreamModel, entry.ResolvedModel, entry.ThinkingEffort,
		boolToInt(entry.Stream), entry.AccountID, entry.AccountEmail, entry.ThreadID, boolToInt(entry.SessionReused),
		entry.Status, entry.FinishReason, errMsg, entry.Attempts, entry.DurationMS,
		max(entry.PromptTokens, 0), max(entry.CompletionTokens, 0), max(entry.TotalTokens, 0),
		max(entry.CacheReadTokens, 0), max(entry.CacheWriteTokens, 0),
		entry.CostUSD, max(entry.ToolCallsCount, 0), max(entry.ImagesCount, 0), preview,
	)
	if err != nil {
		return err
	}

	if logInsertCounter.Add(1)%requestLogTrimStep == 0 {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM request_logs WHERE id NOT IN (
			SELECT id FROM request_logs ORDER BY created_at DESC, id DESC LIMIT ?
		)`, maxRequestLogRows)
	}
	return nil
}

func buildRequestLogWhere(f RequestLogFilter) (string, []any) {
	var conds []string
	var args []any
	if st := strings.TrimSpace(f.Status); st != "" && st != "all" {
		conds = append(conds, "status = ?")
		args = append(args, st)
	}
	if proto := strings.TrimSpace(f.Protocol); proto != "" && proto != "all" {
		conds = append(conds, "protocol = ?")
		args = append(args, proto)
	}
	if model := strings.TrimSpace(f.Model); model != "" && model != "all" {
		conds = append(conds, "(upstream_model = ? OR resolved_model = ? OR requested_model = ?)")
		args = append(args, model, model, model)
	}
	if f.AccountID > 0 {
		conds = append(conds, "account_id = ?")
		args = append(args, f.AccountID)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		like := "%" + q + "%"
		conds = append(conds, "(request_id LIKE ? OR prompt_preview LIKE ? OR error_message LIKE ? OR account_email LIKE ? OR thread_id LIKE ?)")
		args = append(args, like, like, like, like, like)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// QueryRequestLogs 按过滤条件分页查询使用记录明细。
func (s *Store) QueryRequestLogs(ctx context.Context, filter RequestLogFilter) (RequestLogPage, error) {
	if s == nil || s.db == nil {
		return RequestLogPage{Items: []RequestLog{}}, nil
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 2000 {
		limit = 2000
	}
	offset := max(filter.Offset, 0)

	whereSQL, whereArgs := buildRequestLogWhere(filter)
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM request_logs"+whereSQL, whereArgs...).Scan(&total); err != nil {
		return RequestLogPage{}, err
	}

	querySQL := `SELECT id, request_id, created_at, protocol, endpoint,
		requested_model, upstream_model, resolved_model, thinking_effort,
		stream, account_id, account_email, thread_id, session_reused,
		status, finish_reason, error_message, attempts, duration_ms,
		prompt_tokens, completion_tokens, total_tokens, cache_read_tokens, cache_write_tokens,
		cost_usd, tool_calls_count, images_count, prompt_preview
		FROM request_logs` + whereSQL + ` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`
	queryArgs := append(append([]any(nil), whereArgs...), limit, offset)

	rows, err := s.db.QueryContext(ctx, querySQL, queryArgs...)
	if err != nil {
		return RequestLogPage{}, err
	}
	defer rows.Close()

	items := make([]RequestLog, 0)
	for rows.Next() {
		var r RequestLog
		var streamInt, reusedInt int
		if err := rows.Scan(
			&r.ID, &r.RequestID, &r.CreatedAt, &r.Protocol, &r.Endpoint,
			&r.RequestedModel, &r.UpstreamModel, &r.ResolvedModel, &r.ThinkingEffort,
			&streamInt, &r.AccountID, &r.AccountEmail, &r.ThreadID, &reusedInt,
			&r.Status, &r.FinishReason, &r.ErrorMessage, &r.Attempts, &r.DurationMS,
			&r.PromptTokens, &r.CompletionTokens, &r.TotalTokens, &r.CacheReadTokens, &r.CacheWriteTokens,
			&r.CostUSD, &r.ToolCallsCount, &r.ImagesCount, &r.PromptPreview,
		); err != nil {
			return RequestLogPage{}, err
		}
		r.Stream = streamInt != 0
		r.SessionReused = reusedInt != 0
		items = append(items, r)
	}
	if err := rows.Err(); err != nil {
		return RequestLogPage{}, err
	}
	return RequestLogPage{Total: total, Items: items}, nil
}

// ClearRequestLogs 清空全部请求明细记录。
func (s *Store) ClearRequestLogs(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM request_logs`)
	return err
}

// GetAnalyticsSummary 按指定时间范围（24h/7d/30d/all）统计请求 KPI、时间趋势及各维度分布。
func (s *Store) GetAnalyticsSummary(ctx context.Context, rangeKey string) (AnalyticsSummary, error) {
	now := time.Now()
	rangeKey = strings.ToLower(strings.TrimSpace(rangeKey))
	var since int64
	hourly := false
	numBuckets := 24

	switch rangeKey {
	case "7d":
		startDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -6)
		since = startDay.Unix()
		numBuckets = 7
	case "30d":
		startDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -29)
		since = startDay.Unix()
		numBuckets = 30
	case "all":
		rangeKey = "all"
		since = 0
		numBuckets = 30
	default:
		rangeKey = "24h"
		hourly = true
		since = time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, now.Location()).Add(-23 * time.Hour).Unix()
		numBuckets = 24
	}

	summary := AnalyticsSummary{
		Range:       rangeKey,
		SinceUnix:   since,
		GeneratedAt: now.Unix(),
		Timeline:    []AnalyticsBucket{},
		ByModel:     []ModelBreakdown{},
		ByAccount:   []AccountBreakdown{},
		ByProtocol:  []ProtocolBreakdown{},
	}
	if s == nil || s.db == nil {
		return summary, nil
	}

	whereClause := ""
	var args []any
	if since > 0 {
		whereClause = " WHERE created_at >= ?"
		args = []any{since}
	}

	// 1. 聚合总体 KPI
	const kpiSQL = `SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN status='ok' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status!='ok' THEN 1 ELSE 0 END), 0),
		COALESCE(AVG(CASE WHEN duration_ms>0 THEN duration_ms END), 0),
		COALESCE(SUM(prompt_tokens), 0),
		COALESCE(SUM(completion_tokens), 0),
		COALESCE(SUM(total_tokens), 0),
		COALESCE(SUM(cache_read_tokens), 0),
		COALESCE(SUM(cache_write_tokens), 0),
		COALESCE(SUM(session_reused), 0),
		COALESCE(SUM(tool_calls_count), 0),
		COALESCE(SUM(cost_usd), 0)
		FROM request_logs`
	var avgDurFloat float64
	err := s.db.QueryRowContext(ctx, kpiSQL+whereClause, args...).Scan(
		&summary.TotalRequests,
		&summary.SuccessRequests,
		&summary.FailedRequests,
		&avgDurFloat,
		&summary.PromptTokens,
		&summary.CompletionTokens,
		&summary.TotalTokens,
		&summary.CacheReadTokens,
		&summary.CacheWriteTokens,
		&summary.SessionReusedCnt,
		&summary.ToolCallsCount,
		&summary.TotalCostUSD,
	)
	if err != nil {
		return AnalyticsSummary{}, err
	}
	summary.AvgDurationMS = int64(math.Round(avgDurFloat))
	if summary.TotalRequests > 0 {
		summary.SuccessRate = math.Round(float64(summary.SuccessRequests)*1000/float64(summary.TotalRequests)) / 10
	}
	totalInputWithCache := summary.PromptTokens + summary.CacheReadTokens + summary.CacheWriteTokens
	if totalInputWithCache > 0 {
		summary.CacheHitRate = math.Round(float64(summary.CacheReadTokens)*1000/float64(totalInputWithCache)) / 10
	}

	// 2. 构建时间趋势桶并填充空桶
	timelineStart := since
	if timelineStart == 0 {
		timelineStart = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(numBuckets-1)).Unix()
	}
	step := int64(86400)
	if hourly {
		step = 3600
	}
	buckets := make([]AnalyticsBucket, numBuckets)
	bucketIndex := make(map[int64]int, numBuckets)
	_, tzOffset := now.Zone()
	for i := 0; i < numBuckets; i++ {
		ts := timelineStart + int64(i)*step
		t := time.Unix(ts, 0).In(now.Location())
		label := t.Format("01-02")
		if hourly {
			label = t.Format("15:00")
		}
		buckets[i] = AnalyticsBucket{BucketStart: ts, Label: label}
		bucketIndex[ts] = i
	}

	timelineWhere := " WHERE created_at >= ?"
	timelineSQL := `SELECT created_at, status, total_tokens, cache_read_tokens, duration_ms FROM request_logs` + timelineWhere
	tRows, err := s.db.QueryContext(ctx, timelineSQL, timelineStart)
	if err != nil {
		return AnalyticsSummary{}, err
	}
	durSum := make([]int64, numBuckets)
	for tRows.Next() {
		var ts, tok, cacheRead, dur int64
		var st string
		if err := tRows.Scan(&ts, &st, &tok, &cacheRead, &dur); err != nil {
			tRows.Close()
			return AnalyticsSummary{}, err
		}
		var bStart int64
		if hourly {
			bStart = ((ts + int64(tzOffset)) / 3600 * 3600) - int64(tzOffset)
		} else {
			bStart = ((ts + int64(tzOffset)) / 86400 * 86400) - int64(tzOffset)
		}
		idx, ok := bucketIndex[bStart]
		if !ok {
			continue
		}
		buckets[idx].Requests++
		if st == "ok" {
			buckets[idx].Successes++
		} else {
			buckets[idx].Failures++
		}
		buckets[idx].TotalTokens += tok
		buckets[idx].CacheRead += cacheRead
		durSum[idx] += dur
	}
	tRows.Close()
	if err := tRows.Err(); err != nil {
		return AnalyticsSummary{}, err
	}
	for i := range buckets {
		if buckets[i].Requests > 0 {
			buckets[i].AvgDurMS = durSum[i] / buckets[i].Requests
		}
	}
	summary.Timeline = buckets

	// 3. 模型分布
	const modelSQL = `SELECT
		CASE WHEN resolved_model != '' THEN resolved_model WHEN upstream_model != '' THEN upstream_model ELSE requested_model END AS model_name,
		COUNT(*),
		COALESCE(SUM(CASE WHEN status='ok' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(total_tokens), 0),
		COALESCE(AVG(CASE WHEN duration_ms>0 THEN duration_ms END), 0)
		FROM request_logs`
	mRows, err := s.db.QueryContext(ctx, modelSQL+whereClause+` GROUP BY model_name ORDER BY COUNT(*) DESC LIMIT 20`, args...)
	if err != nil {
		return AnalyticsSummary{}, err
	}
	for mRows.Next() {
		var m ModelBreakdown
		var avgM float64
		if err := mRows.Scan(&m.Model, &m.Requests, &m.Successes, &m.TotalTokens, &avgM); err != nil {
			mRows.Close()
			return AnalyticsSummary{}, err
		}
		if strings.TrimSpace(m.Model) == "" {
			m.Model = "(unknown)"
		}
		m.AvgDurMS = int64(math.Round(avgM))
		summary.ByModel = append(summary.ByModel, m)
	}
	mRows.Close()

	// 4. 账号分布
	const acctSQL = `SELECT
		account_id,
		CASE WHEN account_email != '' THEN account_email ELSE ('账号 #' || account_id) END AS acct_label,
		COUNT(*),
		COALESCE(SUM(CASE WHEN status='ok' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status!='ok' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(total_tokens), 0),
		COALESCE(AVG(CASE WHEN duration_ms>0 THEN duration_ms END), 0)
		FROM request_logs`
	aRows, err := s.db.QueryContext(ctx, acctSQL+whereClause+` GROUP BY account_id, acct_label ORDER BY COUNT(*) DESC LIMIT 30`, args...)
	if err != nil {
		return AnalyticsSummary{}, err
	}
	for aRows.Next() {
		var a AccountBreakdown
		var avgA float64
		if err := aRows.Scan(&a.AccountID, &a.Email, &a.Requests, &a.Successes, &a.Failures, &a.TotalTokens, &avgA); err != nil {
			aRows.Close()
			return AnalyticsSummary{}, err
		}
		a.AvgDurMS = int64(math.Round(avgA))
		summary.ByAccount = append(summary.ByAccount, a)
	}
	aRows.Close()

	// 5. 协议分布
	const protoSQL = `SELECT
		protocol,
		COUNT(*),
		COALESCE(SUM(CASE WHEN status='ok' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(total_tokens), 0)
		FROM request_logs`
	pRows, err := s.db.QueryContext(ctx, protoSQL+whereClause+` GROUP BY protocol ORDER BY COUNT(*) DESC`, args...)
	if err != nil {
		return AnalyticsSummary{}, err
	}
	for pRows.Next() {
		var p ProtocolBreakdown
		if err := pRows.Scan(&p.Protocol, &p.Requests, &p.Successes, &p.TotalTokens); err != nil {
			pRows.Close()
			return AnalyticsSummary{}, err
		}
		summary.ByProtocol = append(summary.ByProtocol, p)
	}
	pRows.Close()

	return summary, nil
}
