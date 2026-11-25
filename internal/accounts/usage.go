package accounts

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sync"
	"time"

	"quickrelay-studio/internal/oauth"
	"quickrelay-studio/internal/store"
)

var ErrUsageBusy = errors.New("account usage synchronization is already running")

type UsageSyncStatus struct {
	Enabled         bool   `json:"enabled"`
	RefreshInterval string `json:"refresh_interval"`
	MaxAttempts     int    `json:"max_attempts"`
	NextRunAt       int64  `json:"next_run_at"`
	Running         bool   `json:"running"`
}

type UsageSyncResult struct {
	ID        int64  `json:"id"`
	OK        bool   `json:"ok"`
	ErrorCode string `json:"error_code,omitempty"`
}

func UsageErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrUsageBusy):
		return "usage_busy"
	case errors.Is(err, oauth.ErrUsageUnavailable):
		return "usage_unavailable"
	case errors.Is(err, oauth.ErrUsageUnauthorized), errors.Is(err, oauth.ErrNoRefreshToken):
		return "usage_unauthorized"
	default:
		return "usage_failed"
	}
}

func (m *Manager) UsageStatus() UsageSyncStatus {
	m.usageMu.Lock()
	defer m.usageMu.Unlock()
	return UsageSyncStatus{
		Enabled: m.cfg.Usage.Enabled, RefreshInterval: m.cfg.Usage.RefreshInterval.String(),
		MaxAttempts: m.cfg.Usage.MaxAttempts, NextRunAt: m.usageNextRun, Running: m.usageRunning,
	}
}

func (m *Manager) StartUsageSync(parent context.Context) func() {
	if !m.cfg.Usage.Enabled {
		return func() {}
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	m.usageMu.Lock()
	m.usageNextRun = time.Now().Unix()
	m.usageMu.Unlock()
	go func() {
		defer close(done)
		defer func() {
			m.usageMu.Lock()
			m.usageNextRun = 0
			m.usageMu.Unlock()
		}()
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				m.usageMu.Lock()
				m.usageNextRun = 0
				m.usageMu.Unlock()
				_, _ = m.RefreshAllUsage(ctx)
				if ctx.Err() != nil {
					return
				}
				interval := m.cfg.Usage.RefreshInterval.Duration()
				m.usageMu.Lock()
				m.usageNextRun = time.Now().Add(interval).Unix()
				m.usageMu.Unlock()
				timer.Reset(interval)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func (m *Manager) usageLockFor(id int64) *sync.Mutex {
	m.usageMu.Lock()
	defer m.usageMu.Unlock()
	lock := m.usageLocks[id]
	if lock == nil {
		lock = &sync.Mutex{}
		m.usageLocks[id] = lock
	}
	return lock
}

func (m *Manager) RefreshUsage(ctx context.Context, id int64) (*store.QuotaSnapshot, error) {
	lock := m.usageLockFor(id)
	if !lock.TryLock() {
		return nil, ErrUsageBusy
	}
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(ctx, m.cfg.RequestTimeout.Duration())
	defer cancel()
	account, err := m.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	refreshed := false
	if account.JWT() == "" {
		refreshed = true
		account, err = m.RefreshOne(ctx, id)
		if err != nil {
			if ctx.Err() == nil {
				err = oauth.ErrUsageUnauthorized
			}
			m.recordUsageError(ctx, id, err)
			return nil, err
		}
	} else {
		account, err = m.ensureFresh(ctx, account)
		if err != nil {
			m.recordUsageError(ctx, id, err)
			return nil, err
		}
	}
	for attempt := 1; attempt <= m.cfg.Usage.MaxAttempts; {
		var snapshot *oauth.UsageSnapshot
		snapshot, err = oauth.FetchUsage(ctx, account, m.cfg)
		if err == nil {
			quota := store.QuotaSnapshot{
				Source: snapshot.Source, Unit: snapshot.Unit,
				Monthly: quotaBucket(snapshot.Monthly), OverageMonthly: quotaBucket(snapshot.OverageMonthly),
				SubscriptionType: snapshot.SubscriptionType,
			}
			if err := m.store.SaveQuota(ctx, id, quota); err != nil {
				m.recordUsageError(ctx, id, err)
				return nil, err
			}
			latest, err := m.store.Get(ctx, id)
			if err != nil {
				return nil, err
			}
			return &latest.Quota, nil
		}
		if ctx.Err() != nil {
			break
		}
		if errors.Is(err, oauth.ErrUsageUnauthorized) && !refreshed {
			refreshed = true
			account, err = m.RefreshOne(ctx, id)
			if err != nil {
				if ctx.Err() == nil {
					err = oauth.ErrUsageUnauthorized
				}
				break
			}
			continue
		}
		var fetchError *oauth.UsageFetchError
		if attempt == m.cfg.Usage.MaxAttempts || !errors.As(err, &fetchError) || (fetchError.StatusCode != 0 && fetchError.StatusCode != http.StatusRequestTimeout && fetchError.StatusCode != http.StatusTooManyRequests && fetchError.StatusCode < 500) {
			break
		}
		delay := max(time.Duration(1<<(attempt-1))*250*time.Millisecond, fetchError.RetryAfter)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			err = ctx.Err()
		case <-timer.C:
		}
		if ctx.Err() != nil {
			break
		}
		attempt++
	}
	m.recordUsageError(ctx, id, err)
	return nil, err
}

func (m *Manager) recordUsageError(ctx context.Context, id int64, err error) {
	updateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_ = m.store.SetQuotaError(updateCtx, id, UsageErrorCode(err))
}

func quotaBucket(bucket *oauth.UsageBucket) *store.QuotaBucket {
	if bucket == nil {
		return nil
	}
	output := &store.QuotaBucket{Used: bucket.Used, Limit: bucket.Limit, ResetsAt: bucket.ResetsAt, Warning: bucket.Warning}
	if bucket.Used != nil && bucket.Limit != nil {
		remaining := math.Max(0, *bucket.Limit-*bucket.Used)
		output.Remaining = &remaining
	}
	return output
}

func (m *Manager) RefreshAllUsage(ctx context.Context) ([]UsageSyncResult, error) {
	m.usageMu.Lock()
	if m.usageRunning {
		m.usageMu.Unlock()
		return nil, ErrUsageBusy
	}
	m.usageRunning = true
	m.usageMu.Unlock()
	defer func() {
		m.usageMu.Lock()
		m.usageRunning = false
		m.usageMu.Unlock()
	}()
	list, err := m.store.Enabled(ctx)
	if err != nil {
		return nil, err
	}
	results := make([]UsageSyncResult, 0, len(list))
	for _, account := range list {
		if ctx.Err() != nil {
			return results, ctx.Err()
		}
		_, err := m.RefreshUsage(ctx, account.ID)
		results = append(results, UsageSyncResult{ID: account.ID, OK: err == nil, ErrorCode: UsageErrorCode(err)})
	}
	return results, nil
}
