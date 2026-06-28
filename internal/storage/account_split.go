package storage

import (
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

type storedAccountConfig struct {
	ID              string
	Provider        core.ProviderKind
	Label           string
	Remark          string
	Group           string
	ControlDisabled bool
	Backup          bool
	Weight          int
	Priority        int
	Tags            []string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type storedAccountCredentialState struct {
	AccountID  string
	ProxyURL   string
	Credential core.Credential
	UpdatedAt  time.Time
}

type storedAccountRuntimeState struct {
	AccountID        string
	Status           core.AccountStatus
	LastUsedAt       *time.Time
	CooldownUntil    *time.Time
	ConsecutiveFails int
	TotalFails       int
	Metadata         map[string]string
	UpdatedAt        time.Time
}

func splitAccountForStorage(account core.Account) (storedAccountConfig, storedAccountCredentialState, storedAccountRuntimeState) {
	account.Group = core.NormalizeAccountGroupName(account.Group)

	credentialMetadata, runtimeMetadata := splitAccountMetadata(account.Credential.Metadata)
	credential := account.Credential
	credential.ExpiresAt = cloneTimePtr(account.Credential.ExpiresAt)
	credential.Metadata = credentialMetadata

	config := storedAccountConfig{
		ID:              account.ID,
		Provider:        account.Provider,
		Label:           account.Label,
		Remark:          account.Remark,
		Group:           account.Group,
		ControlDisabled: account.ControlDisabled,
		Backup:          account.Backup,
		Weight:          account.Weight,
		Priority:        account.Priority,
		Tags:            append([]string(nil), account.Tags...),
		CreatedAt:       account.CreatedAt,
		UpdatedAt:       account.UpdatedAt,
	}
	credentialState := storedAccountCredentialState{
		AccountID:  account.ID,
		ProxyURL:   account.ProxyURL,
		Credential: credential,
		UpdatedAt:  account.UpdatedAt,
	}
	runtimeState := storedAccountRuntimeState{
		AccountID:        account.ID,
		Status:           normalizeStoredAccountStatus(account.Status),
		LastUsedAt:       cloneTimePtr(account.LastUsedAt),
		CooldownUntil:    cloneTimePtr(account.CooldownUntil),
		ConsecutiveFails: account.ConsecutiveFails,
		TotalFails:       account.TotalFails,
		Metadata:         runtimeMetadata,
		UpdatedAt:        account.UpdatedAt,
	}
	return config, credentialState, runtimeState
}

func cloneCredential(credential core.Credential) core.Credential {
	out := credential
	out.ExpiresAt = cloneTimePtr(credential.ExpiresAt)
	out.Metadata = cloneMap(credential.Metadata)
	return out
}

func mergeStoredAccount(config storedAccountConfig, credentialState storedAccountCredentialState, runtimeState storedAccountRuntimeState) core.Account {
	account := core.Account{
		ID:               firstNonEmpty(config.ID, credentialState.AccountID, runtimeState.AccountID),
		Provider:         config.Provider,
		Label:            config.Label,
		Remark:           config.Remark,
		Group:            core.NormalizeAccountGroupName(config.Group),
		ProxyURL:         credentialState.ProxyURL,
		Status:           normalizeStoredAccountStatus(runtimeState.Status),
		ControlDisabled:  config.ControlDisabled,
		Backup:           config.Backup,
		Weight:           config.Weight,
		Priority:         config.Priority,
		Tags:             append([]string(nil), config.Tags...),
		Credential:       credentialState.Credential,
		LastUsedAt:       cloneTimePtr(runtimeState.LastUsedAt),
		CooldownUntil:    cloneTimePtr(runtimeState.CooldownUntil),
		ConsecutiveFails: runtimeState.ConsecutiveFails,
		TotalFails:       runtimeState.TotalFails,
		CreatedAt:        config.CreatedAt,
		UpdatedAt:        latestTime(config.UpdatedAt, credentialState.UpdatedAt, runtimeState.UpdatedAt),
	}
	account.Credential.ExpiresAt = cloneTimePtr(credentialState.Credential.ExpiresAt)
	account.Credential.Metadata = mergeAccountMetadata(credentialState.Credential.Metadata, runtimeState.Metadata)
	return account
}

func applyRuntimeStateToAccount(account core.Account, runtimeState storedAccountRuntimeState) core.Account {
	account.Status = normalizeStoredAccountStatus(runtimeState.Status)
	account.LastUsedAt = cloneTimePtr(runtimeState.LastUsedAt)
	account.CooldownUntil = cloneTimePtr(runtimeState.CooldownUntil)
	account.ConsecutiveFails = runtimeState.ConsecutiveFails
	account.TotalFails = runtimeState.TotalFails
	account.UpdatedAt = latestTime(account.UpdatedAt, runtimeState.UpdatedAt)
	account.Credential.Metadata = mergeAccountMetadata(credentialMetadataOnly(account.Credential.Metadata), runtimeState.Metadata)
	return account
}

func splitAccountMetadata(metadata map[string]string) (map[string]string, map[string]string) {
	credential := map[string]string{}
	runtime := map[string]string{}
	for key, value := range metadata {
		if accountRuntimeMetadataKey(key) {
			runtime[key] = value
			continue
		}
		credential[key] = value
	}
	return credential, runtime
}

func credentialMetadataOnly(metadata map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range metadata {
		if !accountRuntimeMetadataKey(key) {
			out[key] = value
		}
	}
	return out
}

func runtimeMetadataOnly(metadata map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range metadata {
		if accountRuntimeMetadataKey(key) {
			out[key] = value
		}
	}
	return out
}

func mergeAccountMetadata(credential, runtime map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range credential {
		if !accountRuntimeMetadataKey(key) {
			out[key] = value
		}
	}
	for key, value := range runtime {
		if accountRuntimeMetadataKey(key) {
			out[key] = value
		}
	}
	return out
}

func accountRuntimeMetadataKey(key string) bool {
	switch key {
	case core.AccountQuotaMetadataKey,
		core.AccountQuotaErrorMetadataKey,
		core.AccountQuotaErrorAtMetadataKey,
		core.AccountQuotaErrorCodeMetadataKey,
		core.AccountQuotaErrorStatusMetadataKey,
		"force_oauth_refresh":
		return true
	default:
		return false
	}
}

func normalizeStoredAccountStatus(status core.AccountStatus) core.AccountStatus {
	if status == "" {
		return core.AccountStatusActive
	}
	return status
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func latestTime(values ...time.Time) time.Time {
	var latest time.Time
	for _, value := range values {
		if value.IsZero() {
			continue
		}
		value = value.UTC()
		if latest.IsZero() || value.After(latest) {
			latest = value
		}
	}
	return latest
}
