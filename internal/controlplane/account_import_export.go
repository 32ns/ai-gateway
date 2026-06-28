package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/storage"
)

const AccountPoolExportVersion = 1

type AccountPoolExportInput struct {
	Group      string
	AccountIDs []string
	AllGroups  bool
}

type AccountPoolImportOptions struct {
	GroupOverride   *string
	ReplaceExisting bool
}

type AccountPoolExport struct {
	Version       int                 `json:"version"`
	ExportedAt    time.Time           `json:"exported_at"`
	AccountGroups []core.AccountGroup `json:"account_groups,omitempty"`
	Accounts      []core.Account      `json:"accounts"`
}

type AccountPoolImportResult struct {
	Total          int
	Imported       int
	Skipped        int
	Failed         int
	GroupsImported int
	Items          []AccountPoolImportItem
}

type AccountPoolImportItem struct {
	AccountID string `json:"account_id,omitempty"`
	Label     string `json:"label,omitempty"`
	Status    string `json:"status"`
	Message   string `json:"message,omitempty"`
}

func (s *Service) ExportAccountPool(input AccountPoolExportInput) AccountPoolExport {
	group := normalizeAccountGroup(input.Group)
	selectedIDs := map[string]struct{}{}
	for _, accountID := range uniqueAccountBatchIDs(input.AccountIDs) {
		selectedIDs[strings.ToLower(strings.TrimSpace(accountID))] = struct{}{}
	}
	accounts := s.repo.ListAccounts()
	out := make([]core.Account, 0, len(accounts))
	referencedGroups := map[string]struct{}{}
	for _, account := range accounts {
		account.Group = normalizeStoredAccountGroup(account.Group)
		include := input.AllGroups
		if !include {
			if len(selectedIDs) > 0 {
				_, include = selectedIDs[strings.ToLower(strings.TrimSpace(account.ID))]
			} else if group != "" {
				include = strings.EqualFold(account.Group, group)
			}
		}
		if !include {
			continue
		}
		out = append(out, account)
		referencedGroups[strings.ToLower(account.Group)] = struct{}{}
	}

	return AccountPoolExport{
		Version:       AccountPoolExportVersion,
		ExportedAt:    time.Now().UTC(),
		AccountGroups: s.accountGroupsForExport(input.AllGroups, referencedGroups),
		Accounts:      out,
	}
}

func (s *Service) accountGroupsForExport(all bool, referenced map[string]struct{}) []core.AccountGroup {
	groups := []core.AccountGroup{s.defaultAccountGroupSettings()}
	for _, group := range s.repo.ListAccountGroups() {
		if isDefaultAccountGroupID(group.ID) {
			continue
		}
		groups = append(groups, group)
	}
	if all {
		return groups
	}
	out := make([]core.AccountGroup, 0, len(groups))
	for _, group := range groups {
		key := strings.ToLower(normalizeAccountGroup(group.Name))
		if _, ok := referenced[key]; ok {
			out = append(out, group)
		}
	}
	return out
}

func (s *Service) ImportAccountPoolPayload(payload []byte, options AccountPoolImportOptions) (AccountPoolImportResult, error) {
	exported, err := decodeAccountPoolExport(payload)
	if err != nil {
		return AccountPoolImportResult{}, err
	}
	if len(exported.Accounts) == 0 {
		return AccountPoolImportResult{}, fmt.Errorf("account import file does not contain accounts")
	}

	result := AccountPoolImportResult{
		Total: len(exported.Accounts),
		Items: make([]AccountPoolImportItem, 0, len(exported.Accounts)),
	}
	if options.GroupOverride == nil {
		for _, group := range exported.AccountGroups {
			imported, err := s.importAccountPoolGroup(group)
			if err != nil {
				return AccountPoolImportResult{}, err
			}
			if imported {
				result.GroupsImported++
			}
		}
	}

	for _, record := range exported.Accounts {
		item := s.importAccountPoolAccount(record, options)
		switch item.Status {
		case "ok":
			result.Imported++
		case "skipped":
			result.Skipped++
		default:
			result.Failed++
		}
		result.Items = append(result.Items, item)
	}
	return result, nil
}

func decodeAccountPoolExport(payload []byte) (AccountPoolExport, error) {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return AccountPoolExport{}, fmt.Errorf("account import file is empty")
	}
	if payload[0] == '[' {
		var accounts []core.Account
		if err := json.Unmarshal(payload, &accounts); err != nil {
			return AccountPoolExport{}, fmt.Errorf("parse account import file: %w", err)
		}
		return AccountPoolExport{Version: AccountPoolExportVersion, Accounts: accounts}, nil
	}
	var exported AccountPoolExport
	if err := json.Unmarshal(payload, &exported); err != nil {
		return AccountPoolExport{}, fmt.Errorf("parse account import file: %w", err)
	}
	if exported.Version != 0 && exported.Version > AccountPoolExportVersion {
		return AccountPoolExport{}, fmt.Errorf("account import file version %d is newer than supported version %d", exported.Version, AccountPoolExportVersion)
	}
	return exported, nil
}

func (s *Service) importAccountPoolGroup(group core.AccountGroup) (bool, error) {
	group.ID = strings.TrimSpace(group.ID)
	group.Name = normalizeAccountGroup(group.Name)
	group.Remark = normalizeAccountGroupRemark(group.Remark)
	group.Type = core.NormalizeAccountGroupType(group.Type)
	group.ProxyURL = normalizeProxyURL(group.ProxyURL)
	if err := validateProxyURL(group.ProxyURL); err != nil {
		return false, fmt.Errorf("account group %q: %w", group.Name, err)
	}
	if group.Name == "" {
		return false, fmt.Errorf("account group is required")
	}
	if group.BillingMultiplierBps == 0 {
		group.BillingMultiplierBps = core.AccountGroupDefaultMultiplierBps
	}
	if group.PlanBillingMultiplierBps == 0 {
		group.PlanBillingMultiplierBps = group.BillingMultiplierBps
	}
	if group.PlanBillingEnabled == nil {
		group.PlanBillingEnabled = boolPtr(true)
	}
	if isDefaultAccountGroupID(group.ID) || strings.EqualFold(group.Name, core.DefaultAccountGroupName) {
		group.ID = defaultAccountGroupID
		group.Name = core.DefaultAccountGroupName
		if err := s.repo.UpsertAccountGroup(group); err != nil {
			return false, err
		}
		return true, nil
	}
	if reservedAccountGroupName(group.Name) {
		return false, fmt.Errorf("account group name %q is reserved", group.Name)
	}
	for _, existing := range s.repo.ListAccountGroups() {
		if strings.EqualFold(existing.Name, group.Name) {
			group.ID = existing.ID
			group.CreatedAt = existing.CreatedAt
			if err := s.repo.UpsertAccountGroup(group); err != nil {
				return false, err
			}
			return false, nil
		}
	}
	if group.ID == "" || isDefaultAccountGroupID(group.ID) {
		group.ID = fmt.Sprintf("group_%d", time.Now().UTC().UnixNano())
	}
	if err := s.repo.UpsertAccountGroup(group); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) importAccountPoolAccount(record core.Account, options AccountPoolImportOptions) AccountPoolImportItem {
	account, err := s.normalizeImportedAccount(record, options)
	if err != nil {
		return AccountPoolImportItem{
			AccountID: strings.TrimSpace(record.ID),
			Label:     strings.TrimSpace(record.Label),
			Status:    "failed",
			Message:   err.Error(),
		}
	}
	if existing, err := s.repo.GetAccount(account.ID); err == nil && !options.ReplaceExisting {
		return AccountPoolImportItem{
			AccountID: account.ID,
			Label:     firstNonEmpty(account.Label, existing.Label),
			Status:    "skipped",
			Message:   "account id already exists",
		}
	} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return AccountPoolImportItem{
			AccountID: account.ID,
			Label:     account.Label,
			Status:    "failed",
			Message:   err.Error(),
		}
	}
	if err := s.repo.UpsertAccount(account); err != nil {
		return AccountPoolImportItem{
			AccountID: account.ID,
			Label:     account.Label,
			Status:    "failed",
			Message:   err.Error(),
		}
	}
	return AccountPoolImportItem{
		AccountID: account.ID,
		Label:     account.Label,
		Status:    "ok",
		Message:   "imported",
	}
}

func (s *Service) normalizeImportedAccount(account core.Account, options AccountPoolImportOptions) (core.Account, error) {
	account.ID = strings.TrimSpace(account.ID)
	account.Provider = core.ProviderKind(strings.TrimSpace(string(account.Provider)))
	account.Label = strings.TrimSpace(account.Label)
	account.Remark = strings.TrimSpace(account.Remark)
	account.Group = normalizeAccountGroup(account.Group)
	account.ProxyURL = normalizeProxyURL(account.ProxyURL)
	account.Credential.Mode = strings.TrimSpace(account.Credential.Mode)
	account.Credential.AccessToken = strings.TrimSpace(account.Credential.AccessToken)
	account.Credential.RefreshToken = strings.TrimSpace(account.Credential.RefreshToken)
	account.Credential.SessionToken = strings.TrimSpace(account.Credential.SessionToken)
	account.Credential.Metadata = cloneStringMap(account.Credential.Metadata)
	account.Tags = append([]string(nil), account.Tags...)
	account.EffectiveProxyURL = ""

	if options.GroupOverride != nil {
		account.Group = normalizeAccountGroup(*options.GroupOverride)
	}
	if account.ID == "" {
		generated, err := s.nextImportedAccountID(account.Provider)
		if err != nil {
			return core.Account{}, err
		}
		account.ID = generated
	}
	if account.Provider == "" {
		return core.Account{}, fmt.Errorf("provider is required")
	}
	if s.providers != nil {
		if _, ok := s.providers.Get(account.Provider); !ok {
			return core.Account{}, fmt.Errorf("provider %q is not registered", account.Provider)
		}
	}
	if account.Label == "" {
		account.Label = account.ID
	}
	if account.Group == "" {
		return core.Account{}, fmt.Errorf("account group is required")
	}
	if err := validateProxyURL(account.ProxyURL); err != nil {
		return core.Account{}, err
	}
	if account.Credential.AccessToken == "" {
		return core.Account{}, fmt.Errorf("access token is required")
	}
	if account.Credential.Mode == "" {
		account.Credential.Mode = "manual-token"
	}
	if account.Status == "" {
		account.Status = core.AccountStatusActive
	}
	if !validImportedAccountStatus(account.Status) {
		return core.Account{}, fmt.Errorf("account status %q is not supported", account.Status)
	}
	if account.Priority <= 0 {
		account.Priority = 100
	}
	if account.Weight <= 0 {
		account.Weight = 100
	}
	if err := s.ensureImportedAccountGroup(account.Group); err != nil {
		return core.Account{}, err
	}
	return account, nil
}

func (s *Service) ensureImportedAccountGroup(name string) error {
	name = normalizeAccountGroup(name)
	if name == "" {
		return fmt.Errorf("account group is required")
	}
	if strings.EqualFold(name, core.DefaultAccountGroupName) {
		return nil
	}
	if reservedAccountGroupName(name) {
		return fmt.Errorf("account group name %q is reserved", name)
	}
	for _, group := range s.repo.ListAccountGroups() {
		if strings.EqualFold(group.Name, name) {
			return nil
		}
	}
	return s.repo.UpsertAccountGroup(core.AccountGroup{
		ID:                       fmt.Sprintf("group_%d", time.Now().UTC().UnixNano()),
		Name:                     name,
		BillingMultiplierBps:     core.AccountGroupDefaultMultiplierBps,
		PlanBillingMultiplierBps: core.AccountGroupDefaultMultiplierBps,
	})
}

func (s *Service) nextImportedAccountID(provider core.ProviderKind) (string, error) {
	for i := 0; i < 5; i++ {
		accountID, err := generateAccountID(provider)
		if err != nil {
			return "", err
		}
		if _, err := s.repo.GetAccount(accountID); errors.Is(err, storage.ErrNotFound) {
			return accountID, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("generate account id conflict")
}

func validImportedAccountStatus(status core.AccountStatus) bool {
	switch status {
	case core.AccountStatusActive,
		core.AccountStatusCooling,
		core.AccountStatusExpired,
		core.AccountStatusBlocked,
		core.AccountStatusProviderBanned,
		core.AccountStatusRefreshing:
		return true
	default:
		return false
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
