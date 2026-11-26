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
// turnCh 保证同一条会话在多请求并发时串行执行且可响应 context 取消/超时。
type conversation struct {
	turnCh            chan struct{}
	key               string
	loaded            bool
	refs              int    // 由 conversationStore.mu 保护：正在执行与排队等待的总引用数
	version           uint64 // 由 conversationStore.mu 保护：单删或驱逐版本号，防旧请求写回复活
	threadID          string
	accountID         int64
	baseHash          string
	msgHashes         []string
	lastAssistantHash string
	lastUsed          atomic.Int64
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

// canonicalToolArgs 将工具参数 JSON 规范化为键有序、无多余空白的紧凑 JSON 文本，
// 避免客户端回传时因字段顺序或缩进差异导致误判。
func canonicalToolArgs(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "{}"
	}
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err == nil {
		if v == nil {
			return "{}"
		}
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
	}
	return trimmed
}

// assistantOutputFingerprint 基于最终对外输出的可见文本与结构化工具调用（按顺序）生成确定性 SHA-256 指纹，
// 不依赖思考文本或界面字段，也不持久化原始回复正文。
func assistantOutputFingerprint(text string, toolCalls []ToolCall) string {
	normText := strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	h := sha256.New()
	h.Write([]byte("asst:v1"))
	h.Write([]byte{0})
	h.Write([]byte(normText))
	for _, tc := range toolCalls {
		h.Write([]byte{0})
		h.Write([]byte(strings.TrimSpace(tc.ID)))
		h.Write([]byte{0})
		h.Write([]byte(strings.TrimSpace(tc.Function.Name)))
		h.Write([]byte{0})
		h.Write([]byte(canonicalToolArgs(tc.Function.Arguments)))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// assistantMessageOutputFingerprint 从客户端下一轮回传的 assistant 消息中提取与对外输出一致的指纹。
func assistantMessageOutputFingerprint(m Message) string {
	text := contentText(m.Content)
	calls := make([]ToolCall, 0, len(m.ToolCalls)+1)
	calls = append(calls, m.ToolCalls...)
	if m.FunctionCall != nil && len(calls) == 0 {
		calls = append(calls, ToolCall{
			Type:     "function",
			Function: *m.FunctionCall,
		})
	}
	return assistantOutputFingerprint(text, calls)
}

// matchesLastAssistant 校验客户端回传的首条增量消息是否确实属于本会话上一轮真实对外输出的助手回复。
// 当历史记录缺失上一轮回复指纹（如旧版本升级记录）或回复内容/工具调用不一致时返回 false，促使新建完整线程。
func (c *conversation) matchesLastAssistant(delta []Message) bool {
	if c.lastAssistantHash == "" || len(delta) == 0 {
		return false
	}
	role := strings.ToLower(strings.TrimSpace(delta[0].Role))
	if role != "assistant" {
		return false
	}
	return assistantMessageOutputFingerprint(delta[0]) == c.lastAssistantHash
}

// delta 将传入消息列表的对话前缀与已持久化前缀对比，并严格核对紧接其后的上一轮助手回复指纹：
// 只有前缀与上一轮助手输出全部一致时才返回增量切片并允许省略上轮回复，否则返回 false 走完整历史新建线程。
func (c *conversation) delta(msgs []Message) ([]Message, bool) {
	if c.threadID == "" || len(c.msgHashes) == 0 || c.lastAssistantHash == "" {
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
		delta := cur[len(c.msgHashes):]
		if !c.matchesLastAssistant(delta) {
			return nil, false
		}
		return delta, true
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
			delta := msgs[len(c.msgHashes):]
			if !c.matchesLastAssistant(delta) {
				return nil, false
			}
			return delta, true
		}
	}
	return nil, false
}

func (c *conversation) clearInMemory() {
	c.threadID = ""
	c.accountID = 0
	c.baseHash = ""
	c.msgHashes = nil
	c.lastAssistantHash = ""
}

type conversationStore struct {
	mu         sync.Mutex
	entries    map[string]*conversation
	ttl        time.Duration
	max        int
	store      *store.Store
	clearEpoch uint64
	saveCount  atomic.Uint64
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

// pruneIdleLocked 在已持有 s.mu 的前提下仅淘汰引用数为 0 的空闲过期条目，
// 当缓存超出容量时优先清理空闲最久未使用的条目；所有条目皆忙时允许暂时超额以保串行性。
func (s *conversationStore) pruneIdleLocked(protectKey string) {
	for k, c := range s.entries {
		if k == protectKey || c.refs > 0 {
			continue
		}
		if c.idle() > s.ttl {
			c.version++
			delete(s.entries, k)
		}
	}
	for len(s.entries) > s.max {
		var oldestKey string
		var oldest time.Time
		for k, c := range s.entries {
			if k == protectKey || c.refs > 0 {
				continue
			}
			used := time.Unix(0, c.lastUsed.Load())
			if oldestKey == "" || used.Before(oldest) {
				oldestKey, oldest = k, used
			}
		}
		if oldestKey == "" {
			// 没有可安全淘汰的空闲会话，暂时允许超过目标容量直到有请求释放。
			break
		}
		s.entries[oldestKey].version++
		delete(s.entries, oldestKey)
	}
}

func (s *conversationStore) releaseRef(c *conversation) {
	if s == nil || c == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.refs > 0 {
		c.refs--
	}
	if c.refs == 0 {
		s.pruneIdleLocked("")
	}
}

// acquire 登记引用并以可响应 ctx 取消的方式串行获取会话执行权，同时返回用于防失效竞态的 (epoch, version) 快照。
func (s *conversationStore) acquire(ctx context.Context, key string) (*conversation, uint64, uint64, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, 0, 0, err
		}

		s.mu.Lock()
		for k, c := range s.entries {
			if c.refs == 0 && c.idle() > s.ttl {
				c.version++
				delete(s.entries, k)
			}
		}
		c := s.entries[key]
		if c == nil {
			// 即将新增条目时，先为空闲容量腾出空位（仅淘汰 refs == 0 的条目）
			for len(s.entries) >= s.max {
				var oldestKey string
				var oldest time.Time
				for k, item := range s.entries {
					if item.refs > 0 {
						continue
					}
					used := time.Unix(0, item.lastUsed.Load())
					if oldestKey == "" || used.Before(oldest) {
						oldestKey, oldest = k, used
					}
				}
				if oldestKey == "" {
					break
				}
				s.entries[oldestKey].version++
				delete(s.entries, oldestKey)
			}
			c = &conversation{
				turnCh: make(chan struct{}, 1),
				key:    key,
			}
			c.touch()
			s.entries[key] = c
		}
		c.refs++
		snapEpoch := s.clearEpoch
		snapVer := c.version
		s.mu.Unlock()

		// 在不持有全局 s.mu 的状态下等待该会话执行权，支持请求取消/超时退出
		select {
		case c.turnCh <- struct{}{}:
		case <-ctx.Done():
			s.releaseRef(c)
			return nil, 0, 0, ctx.Err()
		}

		if err := ctx.Err(); err != nil {
			<-c.turnCh
			s.releaseRef(c)
			return nil, 0, 0, err
		}

		s.mu.Lock()
		invalidated := s.clearEpoch != snapEpoch || c.version != snapVer || s.entries[key] != c
		if invalidated {
			if c.refs > 0 {
				c.refs--
			}
			s.mu.Unlock()
			<-c.turnCh
			continue
		}
		if !c.loaded {
			s.loadFromDB(c)
			c.loaded = true
		}
		c.touch()
		s.mu.Unlock()
		return c, snapEpoch, snapVer, nil
	}
}

func (s *conversationStore) release(c *conversation) {
	if s == nil || c == nil {
		return
	}
	c.touch()
	<-c.turnCh
	s.releaseRef(c)
}

// loadFromDB 在已持有 s.mu 且内存未命中时从 SQLite 恢复会话映射；超时则自动删除旧记录。
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
	c.lastAssistantHash = rec.LastAssistantHash
	if rec.LastUsed > 0 {
		c.lastUsed.Store(time.Unix(rec.LastUsed, 0).UnixNano())
	}
}

// save 在与失效操作共用的短临界区内校验会话版本并同步写回内存与 SQLite，防止已被清理的旧映射写回复活。
func (s *conversationStore) save(c *conversation, snapEpoch, snapVer uint64, threadID string, accountID int64, baseHash string, msgs []Message, lastAssistantHash string) {
	if s == nil || c == nil || threadID == "" {
		return
	}
	hashes := nonLeadingMessageFingerprints(msgs)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clearEpoch != snapEpoch || c.version != snapVer || s.entries[c.key] != c {
		return
	}
	c.threadID = threadID
	c.accountID = accountID
	c.baseHash = baseHash
	c.msgHashes = hashes
	c.lastAssistantHash = lastAssistantHash
	c.loaded = true
	c.touch()

	if s.store == nil || c.key == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbPersistTimeout)
	defer cancel()

	nowUnix := time.Now().Unix()
	rec := &store.ConversationRecord{
		Key:               c.key,
		ThreadID:          threadID,
		AccountID:         accountID,
		BaseHash:          baseHash,
		MessageHashes:     hashes,
		LastAssistantHash: lastAssistantHash,
		LastUsed:          nowUnix,
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

// reset 清空当前会话上下文映射，并在版本仍有效时移除对应的持久化记录。
func (s *conversationStore) reset(c *conversation, snapEpoch, snapVer uint64) {
	if s == nil || c == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c.clearInMemory()
	c.loaded = true
	if s.clearEpoch != snapEpoch || c.version != snapVer {
		return
	}
	if s.store == nil || c.key == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbPersistTimeout)
	defer cancel()
	if err := s.store.DeleteConversation(ctx, c.key); err != nil {
		log.Printf("conversation delete key=%s failed: %v", c.key, err)
	}
}

// invalidateKey 同步删除指定会话的持久化记录与内存映射，并递增版本号阻断在途旧请求写回。
func (s *conversationStore) invalidateKey(ctx context.Context, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store != nil {
		if err := s.store.DeleteConversation(ctx, key); err != nil {
			return err
		}
	}
	if c, ok := s.entries[key]; ok {
		c.version++
		delete(s.entries, key)
	}
	return nil
}

// invalidateAll 同步清空全部持久化记录与内存缓存，并推进全清纪元阻断任何清理前发起的在途请求写回。
func (s *conversationStore) invalidateAll(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store != nil {
		if err := s.store.ClearConversations(ctx); err != nil {
			return err
		}
	}
	s.clearEpoch++
	for _, c := range s.entries {
		c.version++
	}
	clear(s.entries)
	return nil
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
// 1) 旁路辅助请求（建议输入/标题生成）优先分配一次性隔离键，即使携带显式会话头也绝不抢锁或污染主会话；
// 2) 优先使用显式请求头 X-Conversation-Id / X-Thread-Id；
// 3) 正常主会话仅绑定鉴权身份、是否有工具及首条非 system/developer 消息指纹，
//    不再把易变的系统提示词或 work_mode 焊死在会话路由键中，大幅提升多轮复用命中率。
func conversationKey(r *http.Request, req *ChatRequest, baseHash string) string {
	if isAuxiliarySideRequest(req, "") {
		return "aux:" + uuid()
	}
	for _, header := range []string{"X-Conversation-Id", "X-Thread-Id"} {
		if id := strings.TrimSpace(r.Header.Get(header)); id != "" {
			return "h:" + id
		}
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
