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

// nonLeadingMessages 跳过消息序列开头连续的 system / developer 消息，提取真实对话消息前缀，
// 使前导动态提示或工作区说明变动时不再破坏多轮会话前缀匹配。
func nonLeadingMessages(msgs []Message) []Message {
	idx := 0
	for idx < len(msgs) {
		role := strings.ToLower(strings.TrimSpace(msgs[idx].Role))
		if role != "system" && role != "developer" {
			break
		}
		idx++
	}
	return msgs[idx:]
}

// nonLeadingMessageFingerprints 为非前导消息逐条生成带 "nl:" 前缀的指纹，以与旧版记录区分并保持无缝兼容。
func nonLeadingMessageFingerprints(msgs []Message) []string {
	trimmed := nonLeadingMessages(msgs)
	out := make([]string, len(trimmed))
	for i, m := range trimmed {
		out[i] = "nl:" + messageFingerprint(m)
	}
	return out
}

// delta 将传入消息列表的非前导对话序列与已持久化前缀对比：
// - 新版 "nl:" 记录忽略开头 system/developer 动态变更，只要对话消息前缀一致即增量复用；
// - 旧版无 "nl:" 前缀记录保持逐条前缀兼容，同时支持跳过前导 system 条数微调后继续复用。
func (c *conversation) delta(msgs []Message) ([]Message, bool) {
	if c.threadID == "" || len(c.msgHashes) == 0 {
		return nil, false
	}
	if strings.HasPrefix(c.msgHashes[0], "nl:") {
		cur := nonLeadingMessages(msgs)
		if len(cur) <= len(c.msgHashes) {
			return nil, false
		}
		for i, expected := range c.msgHashes {
			if "nl:"+messageFingerprint(cur[i]) != expected {
				return nil, false
			}
		}
		return cur[len(c.msgHashes):], true
	}

	// 兼容旧版未带 nl: 前缀的历史持久化记录
	if len(msgs) > len(c.msgHashes) {
		matched := true
		for i, expected := range c.msgHashes {
			if messageFingerprint(msgs[i]) != expected {
				matched = false
				break
			}
		}
		if matched {
			return msgs[len(c.msgHashes):], true
		}
	}
	return nil, false
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
	c.msgHashes = nonLeadingMessageFingerprints(msgs)
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

// shortSHA256 返回输入文本的 SHA-256 前 32 位小写 hex（空串返回 ""）。
func shortSHA256(text string) string {
	if text == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:16])
}

// encodeBaseHash 将静态规范摘要与工具定义摘要编码到 base_hash 列，保持表结构免迁移。
func encodeBaseHash(guideHash, toolHash string) string {
	return "v2:" + guideHash + ":" + toolHash
}

// splitBaseHash 解析持久化记录中的规范摘要与工具定义摘要（兼容旧版纯 SHA-256 值）。
func splitBaseHash(stored string) (guideHash, toolHash string) {
	if strings.HasPrefix(stored, "v2:") {
		rest := strings.TrimPrefix(stored, "v2:")
		if idx := strings.IndexByte(rest, ':'); idx >= 0 {
			return rest[:idx], rest[idx+1:]
		}
		return rest, ""
	}
	return stored, ""
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

// conversationKey 标识一条会话：
// 1) 优先使用显式请求头 X-Conversation-Id / X-Thread-Id；
// 2) 旁路无工具辅助请求（建议/标题）分配一次性隔离键防止污染主会话；
// 3) 正常主会话仅绑定鉴权身份、是否有工具及首条非 system/developer 消息指纹，
//    不再把易变的系统提示词或 work_mode 焊死在会话路由键中，大幅提升多轮复用命中率。
func conversationKey(r *http.Request, req *ChatRequest, baseHash string) string {
	for _, header := range []string{"X-Conversation-Id", "X-Thread-Id"} {
		if id := strings.TrimSpace(r.Header.Get(header)); id != "" {
			return "h:" + id
		}
	}
	if isAuxiliarySideRequest(req, "") {
		return "aux:" + uuid()
	}
	h := sha256.New()
	h.Write([]byte(r.Header.Get("Authorization")))
	h.Write([]byte{0})
	h.Write([]byte(r.Header.Get("X-Api-Key")))
	hasToolsFlag := "t0"
	if len(req.Tools) > 0 || len(req.Functions) > 0 {
		hasToolsFlag = "t1"
	}
	for _, m := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "system" || role == "developer" {
			continue
		}
		h.Write([]byte{0})
		h.Write([]byte(hasToolsFlag))
		h.Write([]byte{0})
		h.Write([]byte(messageFingerprint(m)))
		return "c:" + hex.EncodeToString(h.Sum(nil))
	}
	h.Write([]byte{0})
	h.Write([]byte(baseHash))
	for _, m := range req.Messages {
		h.Write([]byte{0})
		h.Write([]byte(messageFingerprint(m)))
	}
	return "c:" + hex.EncodeToString(h.Sum(nil))
}
