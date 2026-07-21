package controlplane

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/storage"
)

const balanceMigrationCodeTTL = 30 * time.Minute

type BalanceMigrationCodeResult struct {
	Code      string
	ExpiresAt time.Time
}

type BalanceMigrationClaimResult struct {
	ClaimID       string
	AmountNanoUSD int64
}

func (s *Service) CreateBalanceMigrationCode(user core.User) (BalanceMigrationCodeResult, error) {
	if user.ID == "" || !user.Enabled || user.IsAdmin() {
		return BalanceMigrationCodeResult{}, storage.ErrBalanceMigrationInvalid
	}
	store, ok := s.repo.(storage.BalanceMigrationStore)
	if !ok {
		return BalanceMigrationCodeResult{}, storage.ErrBalanceMigrationUnsupported
	}
	rawCode, err := randomHex(20)
	if err != nil {
		return BalanceMigrationCodeResult{}, err
	}
	now := time.Now().UTC()
	expiresAt := now.Add(balanceMigrationCodeTTL)
	if err := store.CreateBalanceMigrationCode(core.BalanceMigrationCode{
		ID:          "migration_" + rawCode[:16],
		UserID:      user.ID,
		CodeHash:    balanceMigrationCodeHash(rawCode),
		Status:      core.BalanceMigrationPending,
		ExpiresAt:   expiresAt,
		GeneratedAt: now,
		UpdatedAt:   now,
	}); err != nil {
		return BalanceMigrationCodeResult{}, err
	}
	return BalanceMigrationCodeResult{
		Code:      formatBalanceMigrationCode(rawCode),
		ExpiresAt: expiresAt,
	}, nil
}

func (s *Service) ClaimBalanceMigrationCode(code, targetUserID string) (BalanceMigrationClaimResult, error) {
	store, ok := s.repo.(storage.BalanceMigrationStore)
	if !ok {
		return BalanceMigrationClaimResult{}, storage.ErrBalanceMigrationUnsupported
	}
	claimed, err := store.ClaimBalanceMigrationCode(
		balanceMigrationCodeHash(code),
		strings.TrimSpace(targetUserID),
	)
	if err != nil {
		return BalanceMigrationClaimResult{}, err
	}
	return BalanceMigrationClaimResult{
		ClaimID:       claimed.ID,
		AmountNanoUSD: claimed.AmountNanoUSD,
	}, nil
}

func balanceMigrationCodeHash(value string) string {
	normalized := strings.ToLower(strings.Map(func(r rune) rune {
		switch r {
		case '-', ' ', '\t', '\r', '\n':
			return -1
		default:
			return r
		}
	}, value))
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

func formatBalanceMigrationCode(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	parts := make([]string, 0, (len(value)+3)/4)
	for len(value) > 0 {
		width := min(4, len(value))
		parts = append(parts, value[:width])
		value = value[width:]
	}
	return strings.Join(parts, "-")
}
