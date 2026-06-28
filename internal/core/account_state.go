package core

import (
	"encoding/json"
	"strings"
	"time"
)

const (
	AccountRuntimeStatusTimeLimit = "time_limit"
	AccountRuntimeStatusWeekLimit = "week_limit"
)

type AccountPoolState string

const (
	AccountPoolStateNormal   AccountPoolState = "normal"
	AccountPoolStateCooling  AccountPoolState = "cooling"
	AccountPoolStateAbnormal AccountPoolState = "abnormal"
)

func NormalizeAccountRuntimeState(account Account, now time.Time) Account {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	switch account.Status {
	case AccountStatusBlocked,
		AccountStatusProviderBanned,
		AccountStatusExpired:
		account.CooldownUntil = nil
		return account
	}
	if account.CooldownUntil == nil {
		if account.Status == AccountStatusCooling || account.Status == AccountStatusRefreshing {
			account.Status = AccountStatusActive
		}
		return account
	}
	if account.CooldownUntil.After(now) {
		if account.Status != AccountStatusRefreshing {
			account.Status = AccountStatusCooling
		}
		return account
	}

	account.CooldownUntil = nil
	account.ConsecutiveFails = 0
	if account.Status == AccountStatusCooling || account.Status == AccountStatusRefreshing {
		account.Status = AccountStatusActive
	}
	return account
}

func NormalizeAccountsRuntimeState(accounts []Account, now time.Time) []Account {
	if len(accounts) == 0 {
		return accounts
	}
	out := make([]Account, len(accounts))
	for i, account := range accounts {
		out[i] = NormalizeAccountRuntimeState(account, now)
	}
	return out
}

func AccountControlDisabled(account Account) bool {
	return account.ControlDisabled
}

func PreserveAccountControlState(account Account, current Account) Account {
	account.ControlDisabled = current.ControlDisabled
	account.Backup = current.Backup
	return account
}

func AccountRuntimeAvailable(account Account, now time.Time) bool {
	if !AccountRuntimeBaseAvailable(account, now) {
		return false
	}
	return AccountQuotaLimitStatus(account, now) == "" && !AccountScopedRateLimitActive(account, now)
}

func AccountPoolStateFor(account Account, now time.Time) AccountPoolState {
	account = NormalizeAccountRuntimeState(account, now)
	if account.ControlDisabled {
		return AccountPoolStateAbnormal
	}
	switch account.Status {
	case AccountStatusBlocked,
		AccountStatusProviderBanned,
		AccountStatusExpired:
		return AccountPoolStateAbnormal
	case AccountStatusCooling,
		AccountStatusRefreshing:
		return AccountPoolStateCooling
	}
	if AccountQuotaLimitStatus(account, now) != "" {
		return AccountPoolStateCooling
	}
	if AccountScopedRateLimitActive(account, now) {
		return AccountPoolStateCooling
	}
	if AccountScopedImageRateLimitActive(account, now) {
		return AccountPoolStateCooling
	}
	if !AccountImageQuotaAvailable(account, now) {
		return AccountPoolStateCooling
	}
	return AccountPoolStateNormal
}

func AccountRuntimeBaseAvailable(account Account, now time.Time) bool {
	account = NormalizeAccountRuntimeState(account, now)
	switch account.Status {
	case AccountStatusBlocked,
		AccountStatusProviderBanned,
		AccountStatusExpired,
		AccountStatusRefreshing,
		AccountStatusCooling:
		return false
	default:
		return true
	}
}

func AccountAvailableForRouting(account Account, now time.Time) bool {
	if account.ControlDisabled {
		return false
	}
	return AccountRuntimeAvailable(account, now)
}

func AccountAvailableForImageRouting(account Account, now time.Time) bool {
	if account.ControlDisabled {
		return false
	}
	if !AccountRuntimeBaseAvailable(account, now) {
		return false
	}
	if AccountScopedImageRateLimitActive(account, now) {
		return false
	}
	if !AccountImageQuotaAvailable(account, now) {
		return false
	}
	if quota := ReadAccountQuota(account); imageQuotaIndependent(quota) {
		return true
	}
	return AccountQuotaLimitStatus(account, now) == ""
}

func AccountImageQuotaAvailable(account Account, now time.Time) bool {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	quota := ReadAccountQuota(account)
	if quota == nil || quota.Image == nil || quota.Image.Unknown {
		return true
	}
	if quota.Image.Remaining > 0 {
		return true
	}
	if quota.Image.ResetsAt != nil && !quota.Image.ResetsAt.IsZero() && !quota.Image.ResetsAt.UTC().After(now) {
		return true
	}
	return false
}

func imageQuotaIndependent(quota *AccountQuotaSnapshot) bool {
	if quota == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(quota.Source)) {
	case "openai_chatgpt_usage":
		return true
	case "new_api_key_billing", "sub2api_profile", "sub2api_usage", "ai_gateway_quota":
		return false
	}
	return true
}

func AccountQuotaRefreshFailureStatus(account Account) AccountStatus {
	metadata := account.Credential.Metadata
	if len(metadata) == 0 {
		return ""
	}
	if status := AccountStatus(strings.TrimSpace(metadata[AccountQuotaErrorStatusMetadataKey])); isQuotaRefreshFailureStatus(status) {
		return status
	}
	return ""
}

func isQuotaRefreshFailureStatus(status AccountStatus) bool {
	switch status {
	case AccountStatusProviderBanned,
		AccountStatusExpired:
		return true
	default:
		return false
	}
}

func AccountQuotaLimitStatus(account Account, now time.Time) string {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	quota := ReadAccountQuota(account)
	if quota == nil {
		return ""
	}
	reachedType := strings.ToLower(strings.TrimSpace(quota.ReachedType))
	if quotaProviderWindowLimitActive(quota.Primary, now) || quotaProviderReachedTypeActive(reachedType, "primary", quota.Primary, now) {
		return AccountRuntimeStatusTimeLimit
	}
	if quotaProviderWindowLimitActive(quota.Secondary, now) || quotaProviderReachedTypeActive(reachedType, "secondary", quota.Secondary, now) {
		return AccountRuntimeStatusWeekLimit
	}
	return ""
}

func AccountScopedRateLimitActive(account Account, now time.Time) bool {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	quota := ReadAccountQuota(account)
	if quota == nil {
		return false
	}
	return runtimeQuotaSnapshotActive(quota.Additional[AccountQuotaRuntimeChatLimitID], now) ||
		legacyScopedRateLimitActive(quota, now)
}

func AccountScopedImageRateLimitActive(account Account, now time.Time) bool {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	quota := ReadAccountQuota(account)
	if quota == nil {
		return false
	}
	return runtimeImageQuotaSnapshotActive(quota.Additional[AccountQuotaRuntimeImageLimitID], now) ||
		legacyScopedImageRateLimitActive(quota, now)
}

func legacyScopedRateLimitActive(quota *AccountQuotaSnapshot, now time.Time) bool {
	if quota == nil {
		return false
	}
	reachedType := strings.ToLower(strings.TrimSpace(quota.ReachedType))
	return scopedRateLimitWindowActive(reachedType, "primary", quota.Primary, now) ||
		scopedRateLimitWindowActive(reachedType, "secondary", quota.Secondary, now)
}

func legacyScopedImageRateLimitActive(quota *AccountQuotaSnapshot, now time.Time) bool {
	return quota != nil &&
		strings.TrimSpace(quota.Source) == "" &&
		imageQuotaLimitActive(quota.Image, now)
}

func runtimeQuotaSnapshotActive(quota AccountQuotaSnapshot, now time.Time) bool {
	if strings.TrimSpace(quota.Source) != AccountQuotaRuntimeSource {
		return false
	}
	reachedType := strings.ToLower(strings.TrimSpace(quota.ReachedType))
	return scopedRateLimitWindowActive(reachedType, "primary", quota.Primary, now) ||
		scopedRateLimitWindowActive(reachedType, "secondary", quota.Secondary, now)
}

func runtimeImageQuotaSnapshotActive(quota AccountQuotaSnapshot, now time.Time) bool {
	if strings.TrimSpace(quota.Source) != AccountQuotaRuntimeSource {
		return false
	}
	return imageQuotaLimitActive(quota.Image, now)
}

func quotaProviderWindowLimitActive(window *AccountQuotaWindow, now time.Time) bool {
	return !isScopedRateLimitWindow(window) && quotaWindowLimitActive(window, now)
}

func quotaProviderReachedTypeActive(reachedType, name string, window *AccountQuotaWindow, now time.Time) bool {
	return !isScopedRateLimitWindow(window) && quotaReachedTypeActive(reachedType, name, window, now)
}

func scopedRateLimitWindowActive(reachedType, name string, window *AccountQuotaWindow, now time.Time) bool {
	if !isScopedRateLimitWindow(window) {
		return false
	}
	return quotaWindowLimitActive(window, now) || quotaReachedTypeActive(reachedType, name, window, now)
}

func isScopedRateLimitWindow(window *AccountQuotaWindow) bool {
	return window != nil && strings.TrimSpace(window.Name) == "rate_limit"
}

func quotaWindowLimitActive(window *AccountQuotaWindow, now time.Time) bool {
	if window == nil || window.UsedPercent < 100 {
		return false
	}
	if window.ResetsAt == nil || window.ResetsAt.IsZero() {
		return true
	}
	return window.ResetsAt.UTC().After(now)
}

func imageQuotaLimitActive(quota *AccountImageQuota, now time.Time) bool {
	if quota == nil || quota.Unknown || quota.Remaining > 0 {
		return false
	}
	if quota.ResetsAt == nil || quota.ResetsAt.IsZero() {
		return true
	}
	return quota.ResetsAt.UTC().After(now)
}

func AccountQuotaWindowUsedPercent(window *AccountQuotaWindow, now time.Time) float64 {
	if window == nil {
		return 0
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if window.ResetsAt != nil && !window.ResetsAt.IsZero() && !window.ResetsAt.UTC().After(now) {
		return 0
	}
	return window.UsedPercent
}

func AccountQuotaWindowResetActive(window *AccountQuotaWindow, now time.Time) bool {
	if window == nil || window.ResetsAt == nil || window.ResetsAt.IsZero() {
		return false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	return window.ResetsAt.UTC().After(now)
}

func ClearExpiredAccountQuotaCooldowns(account Account, now time.Time) (Account, bool) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	quota := ReadAccountQuota(account)
	if quota == nil {
		return account, false
	}

	changed := false
	if scopedQuotaWindowExpired(quota.Primary, now) {
		quota.Primary = nil
		changed = true
		if quotaReachedTypeMatches(quota.ReachedType, "primary") {
			quota.ReachedType = ""
		}
	}
	if scopedQuotaWindowExpired(quota.Secondary, now) {
		quota.Secondary = nil
		changed = true
		if quotaReachedTypeMatches(quota.ReachedType, "secondary") {
			quota.ReachedType = ""
		}
	}
	if quota.Image != nil &&
		quota.Source == "" &&
		quota.Image.Remaining <= 0 &&
		quota.Image.ResetsAt != nil &&
		!quota.Image.ResetsAt.IsZero() &&
		!quota.Image.ResetsAt.UTC().After(now) {
		quota.Image = nil
		changed = true
	}
	for key, scoped := range quota.Additional {
		if strings.TrimSpace(scoped.Source) != AccountQuotaRuntimeSource {
			continue
		}
		if !runtimeQuotaSnapshotActive(scoped, now) && !runtimeImageQuotaSnapshotActive(scoped, now) {
			delete(quota.Additional, key)
			changed = true
		}
	}
	if len(quota.Additional) == 0 {
		quota.Additional = nil
	}
	if accountQuotaSnapshotOnlyRuntimeTimestamp(quota) {
		quota.RefreshedAt = nil
		changed = true
	}
	if !changed {
		return account, false
	}

	metadata := cloneCredentialMetadata(account.Credential.Metadata)
	if accountQuotaSnapshotEmpty(quota) {
		delete(metadata, AccountQuotaMetadataKey)
	} else if encoded, err := json.Marshal(quota); err == nil {
		metadata[AccountQuotaMetadataKey] = string(encoded)
	} else {
		return account, false
	}
	account.Credential.Metadata = metadata
	return account, true
}

func ClearAccountScopedRateLimits(account Account) Account {
	quota := ReadAccountQuota(account)
	if quota == nil {
		return account
	}

	changed := false
	if isScopedRateLimitWindow(quota.Primary) {
		quota.Primary = nil
		changed = true
		if quotaReachedTypeMatches(quota.ReachedType, "primary") {
			quota.ReachedType = ""
		}
	}
	if isScopedRateLimitWindow(quota.Secondary) {
		quota.Secondary = nil
		changed = true
		if quotaReachedTypeMatches(quota.ReachedType, "secondary") {
			quota.ReachedType = ""
		}
	}
	if quota.Image != nil &&
		quota.Source == "" &&
		quota.Image.Remaining <= 0 {
		quota.Image = nil
		changed = true
	}
	for key, scoped := range quota.Additional {
		if strings.TrimSpace(scoped.Source) == AccountQuotaRuntimeSource {
			delete(quota.Additional, key)
			changed = true
		}
	}
	if len(quota.Additional) == 0 {
		quota.Additional = nil
	}
	if accountQuotaSnapshotOnlyRuntimeTimestamp(quota) {
		quota.RefreshedAt = nil
		changed = true
	}
	if !changed {
		return account
	}

	metadata := cloneCredentialMetadata(account.Credential.Metadata)
	if accountQuotaSnapshotEmpty(quota) {
		delete(metadata, AccountQuotaMetadataKey)
	} else if encoded, err := json.Marshal(quota); err == nil {
		metadata[AccountQuotaMetadataKey] = string(encoded)
	} else {
		return account
	}
	account.Credential.Metadata = metadata
	return account
}

func scopedQuotaWindowExpired(window *AccountQuotaWindow, now time.Time) bool {
	if !isScopedRateLimitWindow(window) {
		return false
	}
	if window.ResetsAt == nil || window.ResetsAt.IsZero() {
		return false
	}
	return !window.ResetsAt.UTC().After(now)
}

func quotaReachedTypeMatches(reachedType, name string) bool {
	reachedType = strings.ToLower(strings.TrimSpace(reachedType))
	return reachedType == name || reachedType == name+"_window"
}

func accountQuotaSnapshotEmpty(quota *AccountQuotaSnapshot) bool {
	if quota == nil {
		return true
	}
	return strings.TrimSpace(quota.Source) == "" &&
		strings.TrimSpace(quota.LimitID) == "" &&
		strings.TrimSpace(quota.LimitName) == "" &&
		strings.TrimSpace(quota.Plan) == "" &&
		quota.Primary == nil &&
		quota.Secondary == nil &&
		quota.Credits == nil &&
		quota.Image == nil &&
		strings.TrimSpace(quota.ReachedType) == "" &&
		len(quota.Additional) == 0 &&
		quota.RefreshedAt == nil
}

func accountQuotaSnapshotOnlyRuntimeTimestamp(quota *AccountQuotaSnapshot) bool {
	if quota == nil || quota.RefreshedAt == nil {
		return false
	}
	return strings.TrimSpace(quota.Source) == "" &&
		strings.TrimSpace(quota.LimitID) == "" &&
		strings.TrimSpace(quota.LimitName) == "" &&
		strings.TrimSpace(quota.Plan) == "" &&
		quota.Primary == nil &&
		quota.Secondary == nil &&
		quota.Credits == nil &&
		quota.Image == nil &&
		strings.TrimSpace(quota.ReachedType) == "" &&
		len(quota.Additional) == 0
}

func cloneCredentialMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(metadata))
	for key, value := range metadata {
		out[key] = value
	}
	return out
}

func ClearAccountQuotaRefreshFailureMetadata(account Account) Account {
	metadata := cloneCredentialMetadata(account.Credential.Metadata)
	delete(metadata, AccountQuotaErrorMetadataKey)
	delete(metadata, AccountQuotaErrorAtMetadataKey)
	delete(metadata, AccountQuotaErrorCodeMetadataKey)
	delete(metadata, AccountQuotaErrorStatusMetadataKey)
	account.Credential.Metadata = metadata
	return account
}

func quotaReachedTypeActive(reachedType, name string, window *AccountQuotaWindow, now time.Time) bool {
	if reachedType != name && reachedType != name+"_window" {
		return false
	}
	if window == nil || window.ResetsAt == nil || window.ResetsAt.IsZero() {
		return true
	}
	return window.ResetsAt.UTC().After(now)
}

func ReadAccountQuota(account Account) *AccountQuotaSnapshot {
	raw := strings.TrimSpace(account.Credential.Metadata[AccountQuotaMetadataKey])
	if raw == "" {
		return nil
	}
	var snapshot AccountQuotaSnapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return nil
	}
	return &snapshot
}

func ReadAccountQuotaError(account Account) (string, *time.Time) {
	message := strings.TrimSpace(account.Credential.Metadata[AccountQuotaErrorMetadataKey])
	if message == "" {
		return "", nil
	}
	rawTime := strings.TrimSpace(account.Credential.Metadata[AccountQuotaErrorAtMetadataKey])
	if rawTime == "" {
		return message, nil
	}
	parsed, err := time.Parse(time.RFC3339, rawTime)
	if err != nil {
		return message, nil
	}
	utc := parsed.UTC()
	return message, &utc
}
