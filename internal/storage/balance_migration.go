package storage

import (
	"errors"
	"strings"

	"github.com/32ns/ai-gateway/internal/core"
)

var (
	ErrBalanceMigrationInvalid        = errors.New("balance migration code is invalid")
	ErrBalanceMigrationExpired        = errors.New("balance migration code has expired")
	ErrBalanceMigrationClaimed        = errors.New("balance migration code has already been claimed")
	ErrBalanceMigrationTargetMismatch = errors.New("balance migration code belongs to another target user")
	ErrBalanceMigrationDraining       = errors.New("balance migration is waiting for active requests")
	ErrBalanceMigrationBlocked        = errors.New("balance migration is blocked by an unfinished payment or refund")
	ErrBalanceMigrationNoBalance      = errors.New("balance migration requires a positive balance")
	ErrBalanceMigrationUnsupported    = errors.New("balance migration is not supported by this repository")
)

// BalanceMigrationStore is intentionally separate from Repository. The feature
// is temporary and only the active SQLite-backed legacy deployment needs it.
type BalanceMigrationStore interface {
	CreateBalanceMigrationCode(core.BalanceMigrationCode) error
	ClaimBalanceMigrationCode(codeHash, targetUserID string) (core.BalanceMigrationCode, error)
}

func normalizeBalanceMigrationTarget(value string) string {
	return strings.TrimSpace(value)
}
