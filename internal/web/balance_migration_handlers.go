package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/storage"
)

const balanceMigrationRequestLimit = 8 * 1024

type balanceMigrationClaimRequest struct {
	Code         string `json:"code"`
	TargetUserID string `json:"target_user_id"`
}

func (s *Server) handleBalanceMigrationCode(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/profile/balance-migration/code" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, ok := currentUserFromContext(r.Context())
	if !ok {
		s.writeConsoleAuthRequired(w, r)
		return
	}
	if s.balanceMigrationLimiter != nil && !s.balanceMigrationLimiter.allow(
		"balance-migration-generate",
		clientIP(r),
		4,
		time.Hour,
	) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{
			"status":  "error",
			"message": "Too many migration codes have been generated. Please try again later.",
		})
		return
	}
	result, err := s.control.CreateBalanceMigrationCode(user)
	if err != nil {
		writeBalanceMigrationError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{
		"status":     "ok",
		"code":       result.Code,
		"expires_at": result.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleBalanceMigrationClaim(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/internal/balance-migrations/claim" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.balanceMigrationLimiter != nil && !s.balanceMigrationLimiter.allow(
		"balance-migration-claim",
		clientIP(r),
		30,
		15*time.Minute,
	) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{
			"status":  "error",
			"code":    "rate_limited",
			"message": "Too many migration attempts. Please try again later.",
		})
		return
	}
	var request balanceMigrationClaimRequest
	if err := decodeStrictJSONBody(w, r, balanceMigrationRequestLimit, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"status":  "error",
			"code":    "invalid_request",
			"message": "Migration request is invalid.",
		})
		return
	}
	result, err := s.control.ClaimBalanceMigrationCode(request.Code, request.TargetUserID)
	if err != nil {
		if errors.Is(err, storage.ErrBalanceMigrationDraining) {
			w.Header().Set("Retry-After", "2")
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusAccepted, map[string]any{
				"status":              "draining",
				"retry_after_seconds": 2,
			})
			return
		}
		writeBalanceMigrationError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{
		"status":          "claimed",
		"claim_id":        result.ClaimID,
		"amount_nano_usd": strconv.FormatInt(result.AmountNanoUSD, 10),
		"currency":        "USD",
	})
}

func writeBalanceMigrationError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	code := "migration_failed"
	message := "Migration code cannot be used."
	switch {
	case errors.Is(err, storage.ErrBalanceMigrationInvalid):
		status, code = http.StatusNotFound, "invalid_code"
	case errors.Is(err, storage.ErrBalanceMigrationExpired):
		status, code, message = http.StatusGone, "expired_code", "Migration code has expired."
	case errors.Is(err, storage.ErrBalanceMigrationClaimed):
		status, code, message = http.StatusConflict, "already_migrated", "This balance has already been migrated."
	case errors.Is(err, storage.ErrBalanceMigrationTargetMismatch):
		status, code, message = http.StatusConflict, "target_mismatch", "This migration code belongs to another user."
	case errors.Is(err, storage.ErrBalanceMigrationBlocked):
		status, code, message = http.StatusConflict, "unfinished_finance", "Resolve every unpaid payment and pending refund before migrating."
	case errors.Is(err, storage.ErrBalanceMigrationNoBalance):
		status, code, message = http.StatusConflict, "no_balance", "A positive balance is required to migrate."
	case errors.Is(err, storage.ErrBalanceMigrationUnsupported):
		status, code, message = http.StatusServiceUnavailable, "unavailable", "Balance migration is unavailable."
	default:
		if strings.TrimSpace(err.Error()) != "" {
			message = "Balance migration is temporarily unavailable."
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, map[string]string{
		"status":  "error",
		"code":    code,
		"message": message,
	})
}
