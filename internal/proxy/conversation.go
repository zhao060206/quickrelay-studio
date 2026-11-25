package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quickrelay-studio/internal/store"
)

const dbPersistTimeout = 3 * time.Second

// conversation 保存单条代理会话的上下文映射与消息指纹，并在可用时同步持久化到 SQLite。
type conversation struct {
	mu        sync.Mutex
	key       string
	loaded    bool
	threadID  string
	accountID int64
	baseHash  string
	msgHashes []string
	lastUsed  atomic.Int64
}

func (c *conversation) touch() {
	c.lastUsed.Store(time.Now().UnixNano())
}

func (c *conversation) idle() time.Duration {
	last := c.lastUsed.Load()
	if last == 0 {
		return 0
	}
	return time.Since(time.Unix(0, last))
}

// delta 将传入完整消息列表的哈希前缀与已持久化的历史消息哈希对比，
// 仅当长度严格递增且已有前缀逐条一致时，返回本次需要增量发送的新增消息。
func (c *conversation) delta(msgs []Message) ([]Message, bool) {
	if c.threadID == "" || len(c.msgHashes) == 0 || len(msgs) <= len(c.msgHashes) {
		return nil, false
	}
	for i, expected := range c.msgHashes {
		if messageFingerprint(msgs[i]) != expected {
			return nil, false
		}
	}
	return msgs[len(c.msgHashes):], true
}

func (c *conversation) clearInMemory() {
	c.threadID = ""
	c.accountID = 0
	c.baseHash = ""
	c.msgHashes = nil
}

type conversationStore struct {
	mu        sync.Mutex
	entries   map[string]*conversation
	ttl       time.Duration
	max       int
	store     *store.Store
	saveCount atomic.Uint64
}

// newConversationStore 创建会话缓存；当传入非 nil 的 *store.Store 时，支持跨进程重启从 SQLite 恢复会话映射。
func newConversationStore(ttl time.Duration, max int, st ...*store.Store) *conversationStore {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	if max < 1 {
		max = 256
	}
	var db *store.Store
	if len(st) > 0 {
		db = st[0]
	}
	return &conversationStore{
		entries: make(map[string]*conversation),
		ttl:     ttl,
		max:     max,
		store:   db,
	}
}

// acquire 锁定并返回一个会话实例；若内存未命中且配置了持久化存储，则从 SQLite 恢复未过期的会话映射。
func (s *conversationStore) acquire(key string) *conversation {
	s.mu.Lock()
	for k, c := range s.entries {
		if c.idle() > s.ttl {
			delete(s.entries, k)
		}
	}
	if len(s.entries) >= s.max {
		var oldestKey string
		var oldest time.Time
		for k, c := range s.entries {
			used := time.Unix(0, c.lastUsed.Load())
			if oldestKey == "" || used.Before(oldest) {
				oldestKey, oldest = k, used
			}
		}
		if oldestKey != "" {
			delete(s.entries, oldestKey)
		}
	}
	c := s.entries[key]
	if c == nil {
		c = &conversation{key: key}
		s.entries[key] = c
	}
	s.mu.Unlock()

	c.mu.Lock()
	if !c.loaded {
		s.loadFromDB(c)
		c.loaded = true
	}
	c.touch()
	return c
}

func (s *conversationStore) release(c *conversation) {
	c.touch()
	c.mu.Unlock()
}

// loadFromDB 在已持有 c.mu 且内存未命中时从 SQLite 恢复会话映射；超时则自动删除旧记录。
func (s *conversationStore) loadFromDB(c *conversation) {
	if s.store == nil || c.key == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbPersistTimeout)
	defer cancel()

	rec, err := s.store.GetConversation(ctx, c.key)
	if err != nil {
		log.Printf("conversation load key=%s failed: %v", c.key, err)
		return
	}
	if rec == nil || rec.ThreadID == "" {
		return
	}
	if rec.LastUsed > 0 && time.Since(time.Unix(rec.LastUsed, 0)) > s.ttl {
		_ = s.store.DeleteConversation(ctx, c.key)
		return
	}
	c.threadID = rec.ThreadID
	c.accountID = rec.AccountID
	c.baseHash = rec.BaseHash
	c.msgHashes = rec.MessageHashes
	if rec.LastUsed > 0 {
		c.lastUsed.Store(time.Unix(rec.LastUsed, 0).UnixNano())
	}
}

// save 在成功完成一轮上游对话后更新内存指纹，并同步落盘到 SQLite（调用方须已持有 c.mu）。
func (s *conversationStore) save(c *conversation, threadID string, accountID int64, baseHash string, msgs []Message) {
	if c == nil || threadID == "" {
		return
	}
	c.threadID = threadID
	c.accountID = accountID
	c.baseHash = baseHash
	c.msgHashes = messageFingerprints(msgs)
	c.loaded = true
	c.touch()

	if s.store == nil || c.key == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbPersistTimeout)
	defer cancel()

	nowUnix := time.Now().Unix()
	rec := &store.ConversationRecord{
		Key:           c.key,
		ThreadID:      threadID,
		AccountID:     accountID,
		BaseHash:      baseHash,
		MessageHashes: c.msgHashes,
		LastUsed:      nowUnix,
	}
	if err := s.store.UpsertConversation(ctx, rec); err != nil {
		log.Printf("conversation persist key=%s failed: %v", c.key, err)
	}
	// 每写入 32 次执行一次过期与容量裁剪，避免冷数据无限堆积。
	if s.saveCount.Add(1)%32 == 0 {
		cutoff := time.Now().Add(-s.ttl).Unix()
		if err := s.store.PruneConversations(ctx, cutoff, s.max); err != nil {
			log.Printf("conversation prune failed: %v", err)
		}
	}
}

// reset 清空当前会话上下文映射，并移除对应的持久化记录（调用方须已持有 c.mu）。
func (s *conversationStore) reset(c *conversation) {
	if c == nil {
		return
	}
	c.clearInMemory()
	c.loaded = true
	if s.store == nil || c.key == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbPersistTimeout)
	defer cancel()
	if err := s.store.DeleteConversation(ctx, c.key); err != nil {
		log.Printf("conversation delete key=%s failed: %v", c.key, err)
	}
}

// conversationBaseHash 计算会话基线的 SHA-256 指纹，仅纳入会话开头静态工作区规范与工具协议，
// 确保后续轮次追加动态 developer 消息时不会破坏会话基线匹配。
func conversationBaseHash(leadingGuide, toolProtocol string) string {
	h := sha256.New()
	h.Write([]byte(leadingGuide))
	h.Write([]byte{0})
	h.Write([]byte(toolProtocol))
	return hex.EncodeToString(h.Sum(nil))
}

// messageFingerprints 为消息切片逐条生成稳定 SHA-256 指纹。
func messageFingerprints(msgs []Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = messageFingerprint(m)
	}
	return out
}

// messageFingerprint 生成单条消息的确定性内容摘要，覆盖角色、多模态内容、名称、工具调用及结果关联。
func messageFingerprint(m Message) string {
	h := sha256.New()
	role := strings.ToLower(strings.TrimSpace(m.Role))
	if role == "" {
		role = "user"
	}
	h.Write([]byte(role))
	h.Write([]byte{0})
	writeCanonicalContent(h, m.Content)
	h.Write([]byte{0})
	h.Write([]byte(strings.TrimSpace(m.Name)))
	h.Write([]byte{0})
	h.Write([]byte(strings.TrimSpace(m.ToolCallID)))
	if len(m.ToolCalls) > 0 {
		h.Write([]byte{0})
		if b, err := json.Marshal(m.ToolCalls); err == nil {
			h.Write(b)
		}
	}
	if m.FunctionCall != nil {
		h.Write([]byte{0})
		if b, err := json.Marshal(m.FunctionCall); err == nil {
			h.Write(b)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeCanonicalContent(h interface{ Write([]byte) (int, error) }, content any) {
	switch v := content.(type) {
	case nil:
		return
	case string:
		h.Write([]byte(v))
	default:
		if b, err := json.Marshal(v); err == nil {
			h.Write(b)
			return
		}
		h.Write([]byte(contentText(content)))
	}
}

// conversationKey 标识一条 OpenAI 会话：优先采用请求头 X-Conversation-Id 或 X-Thread-Id，
// 否则结合鉴权身份、前导基线哈希及首条对话消息指纹计算，保证随着多轮拼接与尾部动态上下文新增而保持稳定。
func conversationKey(r *http.Request, req *ChatRequest, baseHash string) string {
	for _, header := range []string{"X-Conversation-Id", "X-Thread-Id"} {
		if id := strings.TrimSpace(r.Header.Get(header)); id != "" {
			return "h:" + id
		}
	}
	h := sha256.New()
	h.Write([]byte(r.Header.Get("Authorization")))
	h.Write([]byte{0})
	h.Write([]byte(r.Header.Get("X-Api-Key")))
	h.Write([]byte{0})
	h.Write([]byte(baseHash))
	for _, m := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "system" || role == "developer" {
			continue
		}
		h.Write([]byte{0})
		h.Write([]byte(messageFingerprint(m)))
		return "c:" + hex.EncodeToString(h.Sum(nil))
	}
	for _, m := range req.Messages {
		h.Write([]byte{0})
		h.Write([]byte(messageFingerprint(m)))
	}
	return "c:" + hex.EncodeToString(h.Sum(nil))
}
