package browser

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"uws/pkg/fingerprint"
)

// LeaseFactory decorates a SessionFactory with a renewable, process-local
// reference-counted lease. Recreating a BrowserContext session therefore keeps
// ownership until the old session has closed, rather than briefly releasing the
// profile to another machine during a graceful restart.
type LeaseFactory struct {
	Factory  SessionFactory
	Store    fingerprint.LeaseBackend
	HolderID string
	TTL      time.Duration

	mu      sync.Mutex
	entries map[string]*leaseFactoryEntry
}

type leaseFactoryEntry struct {
	lease    *fingerprint.Lease
	refs     int
	sessions map[*leasedSession]struct{}
	stop     chan struct{}
}

type leasedSession struct {
	CDPSession
	factory  *LeaseFactory
	profile  string
	entry    *leaseFactoryEntry
	once     sync.Once
	closeErr error
}

func (f *LeaseFactory) configured() error {
	if f == nil || f.Factory == nil || f.Store == nil || f.HolderID == "" {
		return fmt.Errorf("browser: LeaseFactory 需要 Factory、Store 与 HolderID")
	}
	if f.TTL <= 0 {
		f.TTL = 60 * time.Second
	}
	if f.entries == nil {
		f.entries = make(map[string]*leaseFactoryEntry)
	}
	return nil
}

func (f *LeaseFactory) Create(ctx context.Context, cfg Config) (CDPSession, error) {
	if cfg.ProfileID == "" {
		if f == nil || f.Factory == nil {
			return nil, fmt.Errorf("browser: LeaseFactory 缺少底层 Factory")
		}
		return f.Factory.Create(ctx, cfg)
	}
	if err := f.configured(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	entry := f.entries[cfg.ProfileID]
	if entry != nil {
		entry.refs++
		f.mu.Unlock()
	} else {
		f.mu.Unlock()
		lease, err := f.Store.Acquire(cfg.ProfileID, f.HolderID, f.TTL)
		if err != nil {
			return nil, fmt.Errorf("browser: acquire profile lease %q: %w", cfg.ProfileID, err)
		}
		entry = &leaseFactoryEntry{lease: lease, refs: 1, sessions: make(map[*leasedSession]struct{}), stop: make(chan struct{})}
		f.mu.Lock()
		// A concurrent Create may have acquired while this one waited. Keep the
		// lease we acquired only if we won the in-process publication race.
		if current := f.entries[cfg.ProfileID]; current != nil {
			current.refs++
			f.mu.Unlock()
			_ = f.Store.Release(lease)
			entry = current
		} else {
			f.entries[cfg.ProfileID] = entry
			f.mu.Unlock()
			go f.renewLoop(cfg.ProfileID, entry)
		}
	}

	inner, err := f.Factory.Create(ctx, cfg)
	if err != nil {
		f.release(cfg.ProfileID, entry, nil)
		return nil, err
	}
	wrapped := &leasedSession{CDPSession: inner, factory: f, profile: cfg.ProfileID, entry: entry}
	f.mu.Lock()
	entry.sessions[wrapped] = struct{}{}
	f.mu.Unlock()
	return wrapped, nil
}

func (f *LeaseFactory) renewLoop(profile string, entry *leaseFactoryEntry) {
	interval := f.TTL / 2
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-entry.stop:
			return
		case <-ticker.C:
			renewed, err := f.Store.Renew(entry.lease, f.TTL)
			if err == nil {
				f.mu.Lock()
				entry.lease = renewed
				f.mu.Unlock()
				continue
			}
			// A short-lived O_EXCL gate collision is not a lost ownership token.
			// A concurrent observer/acquire must not be mistaken for losing the
			// ownership token; retry on the next heartbeat while the current lease
			// remains valid.
			if err == fingerprint.ErrLeaseGateBusy && time.Until(entry.lease.ExpiresAt) > 0 {
				continue
			}
			// Continuing after ownership is lost would violate the one-writer
			// contract. Close every dependent browser rather than hoping callers
			// notice a log line.
			log.Printf("browser: profile %q 租约续期失败，关闭关联会话: %v", profile, err)
			f.mu.Lock()
			if f.entries[profile] == entry {
				delete(f.entries, profile)
			}
			sessions := make([]*leasedSession, 0, len(entry.sessions))
			for session := range entry.sessions {
				sessions = append(sessions, session)
			}
			f.mu.Unlock()
			for _, session := range sessions {
				_ = session.Close()
			}
			return
		}
	}
}

func (f *LeaseFactory) release(profile string, entry *leaseFactoryEntry, session *leasedSession) {
	if f == nil || entry == nil {
		return
	}
	f.mu.Lock()
	if session != nil {
		delete(entry.sessions, session)
	}
	entry.refs--
	if entry.refs > 0 || f.entries[profile] != entry {
		f.mu.Unlock()
		return
	}
	delete(f.entries, profile)
	close(entry.stop)
	lease := entry.lease
	f.mu.Unlock()
	if err := f.Store.Release(lease); err != nil && err != fingerprint.ErrLeaseLost {
		log.Printf("browser: release profile lease %q: %v", profile, err)
	}
}

func (s *leasedSession) Close() error {
	s.once.Do(func() {
		s.closeErr = s.CDPSession.Close()
		s.factory.release(s.profile, s.entry, s)
	})
	return s.closeErr
}
