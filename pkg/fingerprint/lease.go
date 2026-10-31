package fingerprint

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrLeaseHeld = errors.New("profile lease is held by another holder")
	ErrLeaseLost = errors.New("profile lease is not owned or has expired")
	// ErrLeaseGateBusy means only the tiny filesystem mutation gate is busy;
	// callers may retry without treating it as a competing lease holder.
	ErrLeaseGateBusy = errors.New("profile lease mutation gate is busy")
)

// LeaseBackend is the consistency boundary used by browser processes. The
// filesystem implementation remains useful for one host or a true shared POSIX
// volume; a remote deployment must provide this same token-based contract from
// one strongly consistent authority.
type LeaseBackend interface {
	Acquire(profileID, holderID string, ttl time.Duration) (*Lease, error)
	Renew(lease *Lease, ttl time.Duration) (*Lease, error)
	Release(lease *Lease) error
}

// Lease is an opaque, renewable exclusive ownership claim. Token is never
// logged or printed by this package; possession is required for renewal and
// release so a delayed old process cannot release a new owner's lease.
type Lease struct {
	ProfileID string    `json:"profile_id"`
	HolderID  string    `json:"holder_id"`
	Token     string    `json:"token"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// LeaseStore is a fail-closed lease backend for one filesystem with atomic
// O_EXCL creation semantics (local disk or a correctly configured shared POSIX
// filesystem). It is intentionally not advertised as a multi-region service:
// eventually-consistent sync folders are unsafe and must not use this backend.
type LeaseStore struct {
	Root string
	Now  func() time.Time
}

func OpenLeaseStore(root string) (*LeaseStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("lease: root 不能为空")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("lease: 创建目录: %w", err)
	}
	return &LeaseStore{Root: root, Now: time.Now}, nil
}

func (s *LeaseStore) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *LeaseStore) paths(profileID string) (leasePath, gatePath string, err error) {
	if s == nil || strings.TrimSpace(s.Root) == "" {
		return "", "", fmt.Errorf("lease: store 未初始化")
	}
	if err := ValidIdentity(profileID); err != nil {
		return "", "", fmt.Errorf("lease: profile_id 非法: %w", err)
	}
	leasePath = filepath.Join(s.Root, profileID+".lease.json")
	return leasePath, leasePath + ".gate", nil
}

func randomLeaseToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// withGate serializes expiry/replacement races. A stale gate is only recovered
// after 30 seconds; failing closed for that short interval is safer than two
// active writers after a crash on a flaky share.
func (s *LeaseStore) withGate(profileID string, fn func(string) error) error {
	leasePath, gatePath, err := s.paths(profileID)
	if err != nil {
		return err
	}
	gate, err := os.OpenFile(gatePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			if info, statErr := os.Stat(gatePath); statErr == nil && s.now().Sub(info.ModTime()) > 30*time.Second {
				// It is a crash-recovery path, never a normal contention strategy.
				if removeErr := os.Remove(gatePath); removeErr == nil {
					return s.withGate(profileID, fn)
				}
			}
			return ErrLeaseGateBusy
		}
		return fmt.Errorf("lease: 取得互斥门失败: %w", err)
	}
	_ = gate.Close()
	defer func() { _ = os.Remove(gatePath) }()
	return fn(leasePath)
}

func readLease(path string) (*Lease, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lease Lease
	if err := json.Unmarshal(b, &lease); err != nil {
		return nil, fmt.Errorf("lease: 已有记录损坏，拒绝接管: %w", err)
	}
	if err := ValidIdentity(lease.ProfileID); err != nil || strings.TrimSpace(lease.HolderID) == "" || strings.TrimSpace(lease.Token) == "" || lease.ExpiresAt.IsZero() {
		return nil, fmt.Errorf("lease: 已有记录字段非法，拒绝接管")
	}
	return &lease, nil
}

func writeLease(path string, lease *Lease) error {
	b, err := json.Marshal(lease)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".lease-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func (s *LeaseStore) Acquire(profileID, holderID string, ttl time.Duration) (*Lease, error) {
	if strings.TrimSpace(holderID) == "" || ttl <= 0 {
		return nil, fmt.Errorf("lease: holder_id 不能为空且 ttl 必须为正")
	}
	var acquired *Lease
	err := s.withGate(profileID, func(path string) error {
		now := s.now()
		if current, err := readLease(path); err == nil {
			if current.ExpiresAt.After(now) {
				return ErrLeaseHeld
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		token, err := randomLeaseToken()
		if err != nil {
			return fmt.Errorf("lease: 生成 token: %w", err)
		}
		acquired = &Lease{ProfileID: profileID, HolderID: holderID, Token: token, IssuedAt: now, ExpiresAt: now.Add(ttl)}
		return writeLease(path, acquired)
	})
	if err != nil {
		return nil, err
	}
	return acquired, nil
}

func (s *LeaseStore) Renew(lease *Lease, ttl time.Duration) (*Lease, error) {
	if lease == nil || ttl <= 0 {
		return nil, fmt.Errorf("lease: lease 不能为空且 ttl 必须为正")
	}
	var renewed *Lease
	err := s.withGate(lease.ProfileID, func(path string) error {
		current, err := readLease(path)
		if err != nil || current.Token != lease.Token || current.HolderID != lease.HolderID || !current.ExpiresAt.After(s.now()) {
			return ErrLeaseLost
		}
		renewed = &Lease{ProfileID: current.ProfileID, HolderID: current.HolderID, Token: current.Token, IssuedAt: current.IssuedAt, ExpiresAt: s.now().Add(ttl)}
		return writeLease(path, renewed)
	})
	if err != nil {
		return nil, err
	}
	return renewed, nil
}

func (s *LeaseStore) Release(lease *Lease) error {
	if lease == nil {
		return fmt.Errorf("lease: lease 不能为空")
	}
	return s.withGate(lease.ProfileID, func(path string) error {
		current, err := readLease(path)
		if err != nil || current.Token != lease.Token || current.HolderID != lease.HolderID {
			return ErrLeaseLost
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("lease: 释放失败: %w", err)
		}
		return nil
	})
}
