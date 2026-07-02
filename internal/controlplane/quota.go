package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
)

type runtimeAccountUpdater interface {
	UpsertRuntimeAccount(core.Account) error
}

func ReadAccountQuota(account core.Account) *core.AccountQuotaSnapshot {
	return core.ReadAccountQuota(account)
}

func ReadAccountQuotaError(account core.Account) (string, *time.Time) {
	return core.ReadAccountQuotaError(account)
}

func (s *Service) SupportsQuotaRefresh(account core.Account) bool {
	if s == nil || s.providers == nil {
		return false
	}
	if account.ControlDisabled {
		return false
	}
	adapter, ok := s.providers.Get(account.Provider)
	if !ok {
		return false
	}
	if _, ok := adapter.(providers.QuotaFetchingAdapter); !ok {
		return false
	}

	switch account.Provider {
	case core.ProviderOpenAI:
		if providers.IsOpenAIOAuthTokenSource(account.Credential.Metadata["token_source"]) {
			return true
		}
		if strings.TrimSpace(account.Credential.Mode) != "manual-token" {
			return false
		}
		return providers.OpenAIManualTokenUsesChatGPTBackend(account) ||
			providers.OpenAIAPIKeyQuotaRefreshConfigured(account)
	case core.ProviderClaude:
		return strings.TrimSpace(account.Credential.Metadata["token_source"]) == providers.ClaudeOAuthTokenSourceValue() ||
			providers.APIKeyQuotaRefreshConfigured(account)
	default:
		return false
	}
}

func (s *Service) RefreshAccountQuota(ctx context.Context, id string) (core.Account, core.AccountQuotaSnapshot, error) {
	account, err := s.repo.GetAccount(id)
	if err != nil {
		return core.Account{}, core.AccountQuotaSnapshot{}, err
	}
	account = core.NormalizeAccountRuntimeState(account, time.Now().UTC())
	if s.providers == nil {
		return core.Account{}, core.AccountQuotaSnapshot{}, fmt.Errorf("provider registry is not configured")
	}
	account.EffectiveProxyURL = s.effectiveProxyURLForAccount(account)

	adapter, ok := s.providers.Get(account.Provider)
	if !ok {
		return core.Account{}, core.AccountQuotaSnapshot{}, fmt.Errorf("provider %q is not registered", account.Provider)
	}
	fetcher, ok := adapter.(providers.QuotaFetchingAdapter)
	if !ok || !s.SupportsQuotaRefresh(account) {
		return core.Account{}, core.AccountQuotaSnapshot{}, fmt.Errorf("provider %q does not support quota refresh for this account", account.Provider)
	}

	if refresher, ok := adapter.(providers.RefreshingAdapter); ok && providers.CredentialNeedsRefresh(account) {
		credential, err := refresher.Refresh(ctx, account)
		if err != nil {
			if persistErr := s.persistQuotaCredentialRefreshError(account, err); persistErr != nil {
				return core.Account{}, core.AccountQuotaSnapshot{}, persistErr
			}
			return core.Account{}, core.AccountQuotaSnapshot{}, err
		}
		account.Credential = credential
		if current, err := s.repo.GetAccount(account.ID); err == nil {
			account = core.PreserveAccountControlState(account, current)
		}
		if err := s.repo.UpsertAccount(account); err != nil {
			return core.Account{}, core.AccountQuotaSnapshot{}, err
		}
	}

	snapshot, err := fetcher.FetchQuota(ctx, account)
	if err != nil {
		if persistErr := s.persistQuotaFetchError(account, err); persistErr != nil {
			return core.Account{}, core.AccountQuotaSnapshot{}, persistErr
		}
		return core.Account{}, core.AccountQuotaSnapshot{}, err
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return core.Account{}, core.AccountQuotaSnapshot{}, err
	}

	metadata := cloneStringMap(account.Credential.Metadata)
	metadata[core.AccountQuotaMetadataKey] = string(encoded)
	account.Credential.Metadata = metadata
	account = core.ClearAccountQuotaRefreshFailureMetadata(account)
	account.Status = core.AccountStatusActive
	account.CooldownUntil = nil
	account.ConsecutiveFails = 0
	if err := s.upsertAccountRuntimeState(account); err != nil {
		return core.Account{}, core.AccountQuotaSnapshot{}, err
	}

	saved, err := s.repo.GetAccount(id)
	if err != nil {
		return core.Account{}, core.AccountQuotaSnapshot{}, err
	}
	return saved, snapshot, nil
}

func (s *Service) persistQuotaFetchError(account core.Account, err error) error {
	account = preserveQuotaFetchErrorRuntimeState(account, s.repo)
	account.Credential.Metadata = quotaRefreshErrorMetadata(account, err)
	return s.upsertAccountRuntimeState(account)
}

func (s *Service) persistQuotaCredentialRefreshError(account core.Account, err error) error {
	metadata := quotaRefreshErrorMetadata(account, err)
	account = providers.ApplyCredentialRefreshFailureStatus(account, err, time.Now().UTC())
	if account.Status == core.AccountStatusExpired || account.Status == core.AccountStatusProviderBanned {
		metadata[core.AccountQuotaErrorStatusMetadataKey] = string(account.Status)
	} else {
		delete(metadata, core.AccountQuotaErrorStatusMetadataKey)
	}
	account.Credential.Metadata = metadata
	return s.upsertAccountRuntimeState(account)
}

func quotaRefreshErrorMetadata(account core.Account, err error) map[string]string {
	metadata := cloneStringMap(account.Credential.Metadata)
	metadata[core.AccountQuotaErrorMetadataKey] = strings.TrimSpace(err.Error())
	metadata[core.AccountQuotaErrorAtMetadataKey] = time.Now().UTC().Format(time.RFC3339)
	if code := providers.ErrorCode(err); code != "" {
		metadata[core.AccountQuotaErrorCodeMetadataKey] = code
	} else {
		delete(metadata, core.AccountQuotaErrorCodeMetadataKey)
	}
	delete(metadata, core.AccountQuotaErrorStatusMetadataKey)
	return metadata
}

func preserveQuotaFetchErrorRuntimeState(account core.Account, repo interface {
	GetAccount(string) (core.Account, error)
}) core.Account {
	if repo == nil || strings.TrimSpace(account.ID) == "" {
		return account
	}
	current, err := repo.GetAccount(account.ID)
	if err != nil {
		return account
	}
	account.Status = current.Status
	account.CooldownUntil = cloneTimePtr(current.CooldownUntil)
	account.ConsecutiveFails = current.ConsecutiveFails
	account.TotalFails = current.TotalFails
	return account
}

func (s *Service) upsertAccountRuntimeState(account core.Account) error {
	if current, err := s.repo.GetAccount(account.ID); err == nil {
		account = core.PreserveAccountControlState(account, current)
	}
	if repo, ok := s.repo.(runtimeAccountUpdater); ok {
		return repo.UpsertRuntimeAccount(account)
	}
	return s.repo.UpsertAccount(account)
}
