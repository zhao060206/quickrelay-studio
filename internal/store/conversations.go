package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// ConversationRecord 保存一条可跨进程重启恢复的代理会话映射及消息前缀指纹。
type ConversationRecord struct {
	Key               string
	ThreadID          string
	AccountID         int64
	BaseHash          string
	ChatAgentID       string
	ChatAgentBound    bool
	MessageHashes     []string
	LastAssistantHash string
	LastUsed          int64
}

func scanConversationRow(row interface{ Scan(dest ...any) error }) (*ConversationRecord, error) {
	var rec ConversationRecord
	var hashesJSON string
	var boundInt int
	err := row.Scan(
		&rec.Key, &rec.ThreadID, &rec.AccountID, &rec.BaseHash,
		&rec.ChatAgentID, &boundInt,
		&hashesJSON, &rec.LastAssistantHash, &rec.LastUsed,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rec.ChatAgentBound = boundInt != 0
	if strings.TrimSpace(hashesJSON) != "" {
		if err := json.Unmarshal([]byte(hashesJSON), &rec.MessageHashes); err != nil {
			// 若损坏则视作无历史前缀，回退重新建连
			rec.MessageHashes = nil
		}
	}
	return &rec, nil
}

// GetConversation 根据规范化的 conv_key 读取持久化的会话映射；若不存在则返回 (nil, nil)。
func (s *Store) GetConversation(ctx context.Context, key string) (*ConversationRecord, error) {
	if s == nil || s.db == nil || strings.TrimSpace(key) == "" {
		return nil, nil
	}
	const q = `SELECT conv_key, thread_id, account_id, base_hash, chat_agent_id, chat_agent_bound, message_hashes, last_assistant_hash, last_used
		FROM conversations WHERE conv_key = ?`
	return scanConversationRow(s.db.QueryRowContext(ctx, q, key))
}

// GetConversationByThreadID 按 thread_id 查询最近一次记录，用于显式 X-Thread-Id 跨键或旧库归属校验。
func (s *Store) GetConversationByThreadID(ctx context.Context, threadID string) (*ConversationRecord, error) {
	threadID = strings.TrimSpace(threadID)
	if s == nil || s.db == nil || threadID == "" {
		return nil, nil
	}
	const q = `SELECT conv_key, thread_id, account_id, base_hash, chat_agent_id, chat_agent_bound, message_hashes, last_assistant_hash, last_used
		FROM conversations WHERE thread_id = ? ORDER BY last_used DESC LIMIT 1`
	return scanConversationRow(s.db.QueryRowContext(ctx, q, threadID))
}

// UpsertConversation 新增或更新一条会话映射及其消息 SHA-256 指纹列表。
func (s *Store) UpsertConversation(ctx context.Context, rec *ConversationRecord) error {
	if s == nil || s.db == nil || rec == nil || strings.TrimSpace(rec.Key) == "" || strings.TrimSpace(rec.ThreadID) == "" {
		return nil
	}
	hashes := rec.MessageHashes
	if hashes == nil {
		hashes = []string{}
	}
	encoded, err := json.Marshal(hashes)
	if err != nil {
		return err
	}
	boundInt := 0
	if rec.ChatAgentBound {
		boundInt = 1
	}
	const stmt = `INSERT INTO conversations (conv_key, thread_id, account_id, base_hash, chat_agent_id, chat_agent_bound, message_hashes, last_assistant_hash, last_used)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(conv_key) DO UPDATE SET
			thread_id = excluded.thread_id,
			account_id = excluded.account_id,
			base_hash = excluded.base_hash,
			chat_agent_id = excluded.chat_agent_id,
			chat_agent_bound = excluded.chat_agent_bound,
			message_hashes = excluded.message_hashes,
			last_assistant_hash = excluded.last_assistant_hash,
			last_used = excluded.last_used`
	_, err = s.db.ExecContext(ctx, stmt,
		rec.Key, rec.ThreadID, rec.AccountID, rec.BaseHash,
		strings.TrimSpace(rec.ChatAgentID), boundInt,
		string(encoded), rec.LastAssistantHash, rec.LastUsed,
	)
	return err
}

// DeleteConversation 删除指定的持久化会话映射（用于会话失效或显式重置）。
func (s *Store) DeleteConversation(ctx context.Context, key string) error {
	if s == nil || s.db == nil || strings.TrimSpace(key) == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM conversations WHERE conv_key = ?`, key)
	return err
}

// PruneConversations 清理早于 cutoffUnix 的过期会话，并仅保留最近 maxKeep 条活跃会话记录。
func (s *Store) PruneConversations(ctx context.Context, cutoffUnix int64, maxKeep int) error {
	if s == nil || s.db == nil {
		return nil
	}
	if cutoffUnix > 0 {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM conversations WHERE last_used < ?`, cutoffUnix); err != nil {
			return err
		}
	}
	if maxKeep > 0 {
		const trimSQL = `DELETE FROM conversations WHERE conv_key NOT IN (
			SELECT conv_key FROM conversations ORDER BY last_used DESC LIMIT ?
		)`
		if _, err := s.db.ExecContext(ctx, trimSQL, maxKeep); err != nil {
			return err
		}
	}
	return nil
}

// ConversationSummary 描述管理面板展示的一条持久化 1M 上下文会话摘要。
type ConversationSummary struct {
	Key          string `json:"key"`
	ThreadID     string `json:"thread_id"`
	AccountID    int64  `json:"account_id"`
	AccountEmail string `json:"account_email"`
	TurnsTracked int    `json:"turns_tracked"`
	LastUsed     int64  `json:"last_used"`
}

// ListConversations 按最近活跃时间倒序列出持久化会话列表，并关联展示所属账号邮箱。
func (s *Store) ListConversations(ctx context.Context, limit int) ([]ConversationSummary, error) {
	if s == nil || s.db == nil {
		return []ConversationSummary{}, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	const q = `SELECT c.conv_key, c.thread_id, c.account_id, COALESCE(a.email, ''), c.message_hashes, c.last_used
		FROM conversations c
		LEFT JOIN accounts a ON a.id = c.account_id
		ORDER BY c.last_used DESC
		LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ConversationSummary, 0)
	for rows.Next() {
		var item ConversationSummary
		var hashesJSON string
		if err := rows.Scan(&item.Key, &item.ThreadID, &item.AccountID, &item.AccountEmail, &hashesJSON, &item.LastUsed); err != nil {
			return nil, err
		}
		var hashes []string
		if strings.TrimSpace(hashesJSON) != "" && json.Unmarshal([]byte(hashesJSON), &hashes) == nil {
			item.TurnsTracked = len(hashes)
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// CountConversations 返回当前持久化会话总数。
func (s *Store) CountConversations(ctx context.Context) (int, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations`).Scan(&count)
	return count, err
}

// ClearConversations 清空全部持久化会话映射，下次对话将自动新建云端会话。
func (s *Store) ClearConversations(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM conversations`)
	return err
}
