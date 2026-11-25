// Package accounts selects accounts for rotation and keeps their tokens fresh.
package accounts

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quickrelay-studio/internal/config"
	"quickrelay-studio/internal/oauth"
	"quickrelay-studio/internal/store"
)

// ErrNoAccounts means the database has no enabled account.
var ErrNoAccounts = errors.New("no accounts configured; run `quickrelay login`")

var ErrNoAvailableAccounts = errors.New("no available accounts remain for this request")

// Manager rotates over enabled accounts and refreshes tokens on demand.
type Manager struct {
	store *store.Store
	cfg   *config.Config

	next atomic.Uint64

	mu    sync.Mutex
	locks map[int64]*sync.Mutex

	usageMu      sync.Mutex
	usageRunning bool
	usageNextRun int64
	usageLocks   map[int64]*sync.Mutex
}

// Store 返回底层持久化存储实例，供上层代理复用同一 SQLite 数据库。
func (m *Manager) Store() *store.Store {
	if m == nil {
		return nil
	}
	return m.store
}

func New(st *store.Store, cfg *config.Config) *Manager {
	return &Manager{
		store:      st,
		cfg:        cfg,
		locks:      make(map[int64]*sync.Mutex),
		usageLocks: make(map[int64]*sync.Mutex),
	}
}

func (m *Manager) lockFor(id int64) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.locks[id]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[id] = l
	}
	return l
}

func lockAccount(ctx context.Context, lock *sync.Mutex) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if lock.TryLock() {
		return nil
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := ctx.Err(); err != nil {
				return err
			}
			if lock.TryLock() {
				return nil
			}
		}
	}
}

// Next returns the next usable account in round-robin order. Expiring tokens
// are refreshed before the account is returned. Accounts whose refresh fails
// are skipped so one broken login does not take the proxy down.
func (m *Manager) Next(ctx context.Context) (*store.Account, error) {
	return m.NextExcluding(ctx, nil)
}

func (m *Manager) NextExcluding(ctx context.Context, unavailable map[int64]bool) (*store.Account, error) {
	list, err := m.store.Enabled(ctx)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNoAccounts
	}
	start := int(m.next.Add(1)-1) % len(list)
	var errs []string
	for i := range len(list) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		a := &list[(start+i)%len(list)]
		if unavailable[a.ID] {
			continue
		}
		usable, err := m.ensureFresh(ctx, a)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			_ = m.store.SetError(ctx, a.ID, err.Error())
			if unavailable != nil {
				unavailable[a.ID] = true
			}
			errs = append(errs, fmt.Sprintf("account %d (%s): %v", a.ID, a.Email, err))
			continue
		}
		if err := m.store.MarkUsed(ctx, usable.ID); err != nil {
			return nil, err
		}
		return usable, nil
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoAvailableAccounts, strings.Join(errs, "; "))
	}
	return nil, ErrNoAvailableAccounts
}

// NextPinned returns one specific account, refreshing it when needed. It is
// used to keep a conversation on the account that owns its upstream session.
func (m *Manager) NextPinned(ctx context.Context, id int64) (*store.Account, error) {
	a, err := m.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.Disabled {
		return nil, fmt.Errorf("account %d is disabled", id)
	}
	usable, err := m.ensureFresh(ctx, a)
	if err != nil {
		_ = m.store.SetError(ctx, a.ID, err.Error())
		return nil, err
	}
	if err := m.store.MarkUsed(ctx, usable.ID); err != nil {
		return nil, err
	}
	return usable, nil
}

func (m *Manager) ensureFresh(ctx context.Context, a *store.Account) (*store.Account, error) {
	if !a.Expiring(time.Now(), m.cfg.RefreshBefore.Duration()) {
		if a.JWT() == "" {
			return nil, errors.New("account has no access token")
		}
		return a, nil
	}
	lock := m.lockFor(a.ID)
	if err := lockAccount(ctx, lock); err != nil {
		return nil, err
	}
	defer lock.Unlock()

	// Another request may have refreshed it while we waited.
	latest, err := m.store.Get(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	if !latest.Expiring(time.Now(), m.cfg.RefreshBefore.Duration()) {
		if latest.JWT() == "" {
			return nil, errors.New("account has no access token")
		}
		return latest, nil
	}
	return m.refreshLocked(ctx, latest)
}

// refreshLocked refreshes and persists one account. The caller holds its lock.
func (m *Manager) refreshLocked(ctx context.Context, a *store.Account) (*store.Account, error) {
	refreshed, err := oauth.Refresh(ctx, a)
	if err != nil {
		return nil, err
	}
	if err := m.store.Update(ctx, refreshed); err != nil {
		return nil, err
	}
	return refreshed, nil
}

// RefreshOne forces a refresh of one account.
func (m *Manager) RefreshOne(ctx context.Context, id int64) (*store.Account, error) {
	a, err := m.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	lock := m.lockFor(id)
	if err := lockAccount(ctx, lock); err != nil {
		return nil, err
	}
	defer lock.Unlock()
	if a, err = m.store.Get(ctx, id); err != nil {
		return nil, err
	}
	refreshed, err := m.refreshLocked(ctx, a)
	if err != nil {
		_ = m.store.SetError(ctx, id, err.Error())
		return nil, err
	}
	return refreshed, nil
}

// RefreshResult is one account's outcome from RefreshAll.
type RefreshResult struct {
	Account *store.Account
	Err     error
}

// RefreshAll refreshes every stored account that has a refresh token.
func (m *Manager) RefreshAll(ctx context.Context) ([]RefreshResult, error) {
	list, err := m.store.List(ctx)
	if err != nil {
		return nil, err
	}
	results := make([]RefreshResult, 0, len(list))
	for i := range list {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		a := &list[i]
		if strings.TrimSpace(a.RefreshToken) == "" {
			results = append(results, RefreshResult{Account: a, Err: oauth.ErrNoRefreshToken})
			_ = m.store.SetError(ctx, a.ID, oauth.ErrNoRefreshToken.Error())
			continue
		}
		refreshed, err := m.RefreshOne(ctx, a.ID)
		if refreshed == nil {
			refreshed = a
		}
		results = append(results, RefreshResult{Account: refreshed, Err: err})
	}
	return results, nil
}

// ReportFailure records an upstream failure for an account.
func (m *Manager) ReportFailure(ctx context.Context, id int64, err error) {
	if err == nil {
		return
	}
	_ = m.store.SetError(ctx, id, err.Error())
}

type CallUsage struct {
	Duration         time.Duration
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CacheReadTokens  int
	CacheWriteTokens int
	CostUSD          *float64
	Credits          []store.CreditUsage
	Success          bool
}

func (m *Manager) RecordCall(ctx context.Context, id int64, usage CallUsage) {
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := m.store.RecordCall(recordCtx, id, store.CallUsage{
		Duration:         usage.Duration,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		CacheReadTokens:  usage.CacheReadTokens,
		CacheWriteTokens: usage.CacheWriteTokens,
		CostUSD:          usage.CostUSD,
		Credits:          usage.Credits,
		Success:          usage.Success,
	}); err != nil {
		log.Printf("account %d call usage could not be recorded", id)
	}
}

// EnabledCount returns the number of accounts in rotation.
func (m *Manager) EnabledCount(ctx context.Context) (int, error) {
	list, err := m.store.Enabled(ctx)
	if err != nil {
		return 0, err
	}
	return len(list), nil
}
