package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/payments"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

type Service struct {
	repo                    storage.Repository
	providers               *providers.Registry
	payments                paymentClient
	oauthMu                 sync.Mutex
	clientAuthMu            sync.Mutex
	clientAuth              atomic.Value
	modelListMu             sync.Mutex
	modelList               atomic.Value
	systemSettings          atomic.Value
	settingsMu              sync.RWMutex
	settingsHook            func(core.SystemSettings)
	gatewayAuditRetention   atomic.Bool
	openAIPending           map[string]openAIOAuthPending
	openAIImports           map[string]oauthConnectImport
	claudePending           map[string]claudeOAuthPending
	claudeImports           map[string]oauthConnectImport
	accountBatchMu          sync.Mutex
	accountBatchJobs        map[string]*accountBatchJob
	activeAccountBatchJobID string
	monitorRunMu            sync.Mutex
	monitorRunning          map[string]time.Time
}

type configRevisionProvider interface {
	ConfigRevision() uint64
}

type clientRevisionProvider interface {
	ClientRevision() uint64
}

type userRevisionProvider interface {
	UserRevision() uint64
}

type modelRevisionProvider interface {
	ModelRevision() uint64
}

type dashboardClientSummaryPager interface {
	ListClientSummariesPage(offset, limit int) ([]core.APIClient, int)
}

type clientSummaryOwnerPager interface {
	ListClientSummariesByOwnerPage(ownerUserID string, offset, limit int) ([]core.APIClient, int)
}

type dashboardClientSummaryLister interface {
	ListClientSummaries() []core.APIClient
}

type paymentClient interface {
	CreateOrder(context.Context, payments.CreateOrderInput) (payments.CreateOrderResult, error)
	QueryOrder(context.Context, payments.QueryOrderInput) (payments.QueryOrderResult, error)
	CancelOrder(context.Context, payments.CancelOrderInput) (payments.CancelOrderResult, error)
	PersonalPayRuntime(context.Context) payments.PersonalPayRuntime
	DeletePersonalPayDevice(context.Context, string) error
}

type clientAuthCache struct {
	clientRevision uint64
	userRevision   uint64
	byKey          map[string]clientAuthEntry
}

type clientAuthEntry struct {
	client       core.APIClient
	ownerExists  bool
	ownerEnabled bool
}

type userLastUsedToucher interface {
	TouchUserLastUsedAt(userID string, usedAt time.Time) error
}

type userMetadataUpdater interface {
	UpdateUserMetadata(user core.User) error
}

type modelListCache struct {
	modelRevision uint64
	models        []core.ModelSpec
}

const dashboardClientPreviewLimit = 6

var ErrOAuthPending = errors.New("oauth authorization pending")

var editableAccountStatuses = []core.AccountStatus{
	core.AccountStatusActive,
	core.AccountStatusBlocked,
	core.AccountStatusProviderBanned,
	core.AccountStatusExpired,
}

func New(repo storage.Repository, registry *providers.Registry) *Service {
	service := &Service{
		repo:             repo,
		providers:        registry,
		payments:         payments.NewClient(nil),
		accountBatchJobs: make(map[string]*accountBatchJob),
		monitorRunning:   make(map[string]time.Time),
	}
	service.gatewayAuditRetention.Store(true)
	return service
}

func (s *Service) SetPaymentClient(client paymentClient) {
	if client == nil {
		client = payments.NewClient(nil)
	}
	s.payments = client
}

func (s *Service) SetSystemSettingsHook(hook func(core.SystemSettings)) {
	s.settingsMu.Lock()
	s.settingsHook = hook
	s.settingsMu.Unlock()
}

func (s *Service) SetGatewayAuditRetentionEnabled(enabled bool) {
	if s == nil {
		return
	}
	s.gatewayAuditRetention.Store(enabled)
}

func (s *Service) notifySystemSettingsHook(settings core.SystemSettings) {
	s.settingsMu.RLock()
	hook := s.settingsHook
	s.settingsMu.RUnlock()
	if hook != nil {
		hook(core.NormalizeSystemSettings(settings))
	}
}

func (s *Service) SeedDefaults() error {
	_, err := s.ensureDefaultAccountGroup()
	return err
}

func (s *Service) EnsureProtocolClient(seedKey string) (core.APIClient, bool, error) {
	if repositoryHasAnyClient(s.repo) {
		return core.APIClient{}, false, nil
	}

	key := strings.TrimSpace(seedKey)
	if key == "" {
		generated, err := generateClientKey()
		if err != nil {
			return core.APIClient{}, false, err
		}
		key = generated
	}

	client := core.APIClient{
		ID:                "client_default",
		Name:              "Default Client",
		APIKey:            key,
		OwnerUserID:       s.defaultAdminUserID(),
		Enabled:           true,
		SpendLimitNanoUSD: 0,
		RoutePolicy:       core.DefaultRoutePolicy(),
		AccountGroup:      core.DefaultAccountGroupName,
	}
	if err := s.repo.UpsertClient(client); err != nil {
		return core.APIClient{}, false, err
	}
	client, err := s.repo.GetClient(client.ID)
	if err != nil {
		return core.APIClient{}, false, err
	}
	return client, true, nil
}

func repositoryHasAnyClient(repo storage.Repository) bool {
	if pager, ok := repo.(dashboardClientSummaryPager); ok {
		_, total := pager.ListClientSummariesPage(0, 1)
		return total > 0
	}
	return len(repo.ListClients()) > 0
}

func (s *Service) Dashboard(ctx context.Context) Dashboard {
	_ = ctx
	accounts := s.ListAccounts()
	now := time.Now().UTC()
	accountGroups := accountGroupsWithDefault(s.repo.ListAccountGroups())
	audits := s.repo.ListAuditSummaries(64)
	providerHealth := buildProviderSummaries(accounts)
	stats := buildDashboardStats(accounts, providerHealth)
	stats.DroppedAuditEvents = droppedAuditEvents(s.repo)
	settings := s.currentSystemSettings()
	clientPreview, totalClients := dashboardClientSummaryPreview(s.repo, dashboardClientPreviewLimit)
	return Dashboard{
		Accounts:       accounts,
		AccountGroups:  buildAccountGroupSections(accounts, accountGroups),
		AccountPools:   buildAccountPoolViews(accounts, now),
		Clients:        clientPreview,
		TotalUsers:     dashboardCurrentUserCount(s.repo),
		TotalClients:   totalClients,
		Audit:          audits,
		GatewayAudit:   filterAuditByKind(audits, core.AuditKindGateway, 12),
		AdminAudit:     filterAuditByKind(audits, core.AuditKindAdmin, 12),
		Providers:      providerHealth,
		Stats:          stats,
		SystemProxyURL: settings.Network.SystemProxyURL,
	}
}

func (s *Service) AccountDashboard(ctx context.Context) Dashboard {
	_ = ctx
	accounts := s.ListAccounts()
	settings := s.currentSystemSettings()
	accountGroups := accountGroupsWithDefault(s.repo.ListAccountGroups())
	now := time.Now()
	return Dashboard{
		Accounts:               accounts,
		AccountGroups:          buildAccountGroupSections(accounts, accountGroups),
		AccountPools:           buildAccountPoolViews(accounts, now.UTC()),
		Users:                  s.accountGroupVisibleUsers(accountGroups),
		AccountDailyCallCounts: s.accountDailyCallCounts(now),
		SystemProxyURL:         settings.Network.SystemProxyURL,
	}
}

func (s *Service) accountDailyCallCounts(now time.Time) map[string]int {
	if s == nil || s.repo == nil {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	start := time.Date(now.Local().Year(), now.Local().Month(), now.Local().Day(), 0, 0, 0, 0, now.Local().Location())
	end := start.AddDate(0, 0, 1)
	requests := s.repo.ListBillingRequests(storage.BillingRequestQuery{StartedAt: start, EndedAt: end})
	counts := make(map[string]int)
	for _, request := range requests {
		accountID := strings.TrimSpace(request.AccountID)
		if accountID == "" {
			continue
		}
		counts[accountID]++
	}
	return counts
}

func (s *Service) accountGroupVisibleUsers(groups []core.AccountGroup) []core.User {
	seen := map[string]struct{}{}
	out := make([]core.User, 0)
	for _, group := range groups {
		for _, userID := range group.VisibleUserIDs {
			userID = strings.TrimSpace(userID)
			if userID == "" {
				continue
			}
			key := strings.ToLower(userID)
			if _, ok := seen[key]; ok {
				continue
			}
			user, err := s.repo.GetUser(userID)
			if err != nil {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, user)
		}
	}
	slices.SortFunc(out, func(a, b core.User) int {
		if strings.EqualFold(a.Username, b.Username) {
			return strings.Compare(strings.ToLower(a.ID), strings.ToLower(b.ID))
		}
		return strings.Compare(strings.ToLower(a.Username), strings.ToLower(b.Username))
	})
	return out
}

func (s *Service) GatewayDashboard(ctx context.Context) Dashboard {
	_ = ctx
	audits := s.repo.ListAuditSummaries(64)
	return Dashboard{
		GatewayAudit: filterAuditByKind(audits, core.AuditKindGateway, 12),
	}
}

func (s *Service) ProviderSummaries(ctx context.Context) []ProviderSummary {
	_ = ctx
	return buildProviderSummaries(s.ListAccounts())
}

func dashboardCurrentUserCount(repo storage.Repository) int {
	_, _, total := repo.ListUsersPage(storage.UserListQuery{Limit: 1})
	return total
}

func dashboardClientSummaryPreview(repo storage.Repository, limit int) ([]core.APIClient, int) {
	if limit <= 0 {
		limit = dashboardClientPreviewLimit
	}
	if pager, ok := repo.(dashboardClientSummaryPager); ok {
		clients, total := pager.ListClientSummariesPage(0, limit)
		return dashboardClientSummaries(clients), total
	}
	clients := dashboardListClientSummaries(repo)
	total := len(clients)
	if len(clients) > limit {
		clients = clients[:limit]
	}
	return dashboardClientSummaries(clients), total
}

func dashboardListClientSummaries(repo storage.Repository) []core.APIClient {
	if lister, ok := repo.(dashboardClientSummaryLister); ok {
		return lister.ListClientSummaries()
	}
	return repo.ListClients()
}

func dashboardClientSummaries(clients []core.APIClient) []core.APIClient {
	out := make([]core.APIClient, 0, len(clients))
	for _, client := range clients {
		out = append(out, dashboardClientSummary(client))
	}
	return out
}

func dashboardClientSummary(client core.APIClient) core.APIClient {
	return core.APIClient{
		ID:                strings.TrimSpace(client.ID),
		Name:              strings.TrimSpace(client.Name),
		OwnerUserID:       strings.TrimSpace(client.OwnerUserID),
		Enabled:           client.Enabled,
		SpendLimitNanoUSD: client.SpendLimitNanoUSD,
		AccountGroup:      core.NormalizeAccountGroupName(client.AccountGroup),
		LastUsedAt:        cloneTimePtr(client.LastUsedAt),
		CreatedAt:         client.CreatedAt,
		UpdatedAt:         client.UpdatedAt,
	}
}

func (s *Service) HealthReport(ctx context.Context) HealthReport {
	_ = ctx
	accounts := s.ListAccounts()
	providerHealth := buildProviderSummaries(accounts)
	stats := buildDashboardStats(accounts, providerHealth)
	return HealthReport{
		Status:                stats.Status,
		Reason:                stats.Reason,
		TotalAccounts:         stats.TotalAccounts,
		AvailableAccounts:     stats.AvailableAccounts,
		ExpiringSoonCount:     stats.ExpiringSoonCount,
		HealthyProviderCount:  stats.HealthyProviderCount,
		DegradedProviderCount: stats.DegradedProviderCount,
		DroppedAuditEvents:    droppedAuditEvents(s.repo),
		GeneratedAt:           time.Now().UTC(),
		Providers:             providerHealth,
	}
}

type auditDropCounter interface {
	DroppedAuditEvents() uint64
}

func droppedAuditEvents(repo storage.Repository) uint64 {
	counter, ok := repo.(auditDropCounter)
	if !ok {
		return 0
	}
	return counter.DroppedAuditEvents()
}

func (s *Service) AuditPage(ctx context.Context, filter AuditFilter) AuditPage {
	_ = ctx

	if filter.Page < 1 {
		filter.Page = 1
	}
	if filter.PageSize <= 0 {
		filter.PageSize = 25
	}
	if filter.PageSize > 100 {
		filter.PageSize = 100
	}
	filter.Kind = core.AuditKind(strings.TrimSpace(string(filter.Kind)))
	filter.Status = strings.TrimSpace(filter.Status)
	filter.Actor = strings.TrimSpace(filter.Actor)
	filter.Resource = strings.TrimSpace(filter.Resource)

	start := (filter.Page - 1) * filter.PageSize
	items, total := s.repo.ListAuditSummariesPage(storage.AuditQuery{
		Kind:     filter.Kind,
		Status:   filter.Status,
		Actor:    filter.Actor,
		Resource: filter.Resource,
		Offset:   start,
		Limit:    filter.PageSize,
	})
	end := start + len(items)

	page := AuditPage{
		Items:    items,
		Filter:   filter,
		Total:    total,
		Page:     filter.Page,
		PageSize: filter.PageSize,
	}
	if filter.Page > 1 && total > 0 {
		page.HasPrev = true
		page.PrevPage = filter.Page - 1
	}
	if end < total {
		page.HasNext = true
		page.NextPage = filter.Page + 1
	}
	return page
}

func (s *Service) UsageLogPage(ctx context.Context, user core.User, filter UsageLogFilter) UsageLogPage {
	_ = ctx

	if filter.Page < 1 {
		filter.Page = 1
	}
	if filter.PageSize <= 0 {
		filter.PageSize = 25
	}
	if filter.PageSize > 100 {
		filter.PageSize = 100
	}
	filter.ClientID = strings.TrimSpace(filter.ClientID)
	filter.Model = strings.TrimSpace(filter.Model)
	filter.Status = core.BillingRequestStatus(strings.TrimSpace(string(filter.Status)))

	userID := strings.TrimSpace(filter.UserID)
	if !user.IsAdmin() {
		userID = user.ID
		filter.UserID = user.ID
	} else {
		userID = s.resolveUsageLogUserFilter(userID)
	}

	start := (filter.Page - 1) * filter.PageSize
	items, total := s.repo.ListBillingRequestsPage(storage.BillingRequestQuery{
		UserID:    userID,
		ClientID:  filter.ClientID,
		Model:     filter.Model,
		Status:    filter.Status,
		StartedAt: filter.StartedAt,
		EndedAt:   filter.EndedAt,
		Offset:    start,
		Limit:     filter.PageSize,
	})
	end := start + len(items)

	accountLabels := map[string]string{}
	if user.IsAdmin() {
		for _, account := range s.repo.ListAccounts() {
			accountID := strings.TrimSpace(account.ID)
			if accountID == "" {
				continue
			}
			accountLabels[accountID] = strings.TrimSpace(account.Label)
		}
	}

	rows := make([]UsageLogRow, 0, len(items))
	for _, item := range items {
		accountID := strings.TrimSpace(item.AccountID)
		accountLabel := ""
		if user.IsAdmin() {
			accountLabel = strings.TrimSpace(item.AccountLabel)
			if accountLabel == "" {
				accountLabel = accountLabels[accountID]
			}
			if accountLabel == "" {
				accountLabel = accountID
			}
		}
		failedAccountLabels := []string(nil)
		if user.IsAdmin() {
			for _, failedLabel := range item.FailedAccountLabels {
				failedLabel = strings.TrimSpace(failedLabel)
				if failedLabel == "" || strings.EqualFold(failedLabel, accountLabel) {
					continue
				}
				failedAccountLabels = append(failedAccountLabels, failedLabel)
			}
		}
		rows = append(rows, UsageLogRow{
			Request:             item,
			ClientName:          strings.TrimSpace(item.ClientName),
			AccountGroup:        strings.TrimSpace(item.AccountGroup),
			AccountLabel:        accountLabel,
			FailedAccountLabels: failedAccountLabels,
		})
	}

	page := UsageLogPage{
		Rows:     rows,
		Filter:   filter,
		Total:    total,
		Page:     filter.Page,
		PageSize: filter.PageSize,
	}
	if filter.Page > 1 && total > 0 {
		page.HasPrev = true
		page.PrevPage = filter.Page - 1
	}
	if end < total {
		page.HasNext = true
		page.NextPage = filter.Page + 1
	}
	return page
}

func (s *Service) UsageCostChartForUser(ctx context.Context, user core.User, now time.Time) UsageCostChart {
	_ = ctx
	if strings.TrimSpace(user.ID) == "" {
		return emptyUsageCostChart()
	}
	if now.IsZero() {
		now = time.Now()
	}
	localNow := now.Local()
	start := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, localNow.Location())
	end := start.AddDate(0, 0, 1)

	points := make([]UsageCostPoint, 24)
	for hour := range points {
		points[hour] = UsageCostPoint{
			Hour:  hour,
			Label: fmt.Sprintf("%02d:00", hour),
		}
	}

	for _, summary := range s.repo.ListBillingUsageSpendByHourForUser(user.ID, start, end) {
		if summary.Hour < 0 || summary.Hour >= len(points) || summary.SpendNanoUSD <= 0 {
			continue
		}
		points[summary.Hour].NanoUSD = addNanoUSDSaturating(points[summary.Hour].NanoUSD, summary.SpendNanoUSD)
	}

	var max int64
	var total int64
	for _, point := range points {
		total = addNanoUSDSaturating(total, point.NanoUSD)
		if point.NanoUSD > max {
			max = point.NanoUSD
		}
	}
	for i := range points {
		if max > 0 {
			points[i].Percent = usageChartPercent(points[i].NanoUSD, max)
			if points[i].NanoUSD > 0 && points[i].Percent < 2 {
				points[i].Percent = 2
			}
		}
	}

	return UsageCostChart{
		Points: points,
		Total:  total,
		Max:    max,
	}
}

func addNanoUSDSaturating(a, b int64) int64 {
	if a <= 0 {
		if b < 0 {
			return 0
		}
		return b
	}
	if b <= 0 {
		return a
	}
	const maxInt64 = int64(^uint64(0) >> 1)
	if a > maxInt64-b {
		return maxInt64
	}
	return a + b
}

func usageChartPercent(value, max int64) int {
	if value <= 0 || max <= 0 {
		return 0
	}
	if value > max/100 {
		return 100
	}
	return int(value * 100 / max)
}

func emptyUsageCostChart() UsageCostChart {
	points := make([]UsageCostPoint, 24)
	for hour := range points {
		points[hour] = UsageCostPoint{Hour: hour, Label: fmt.Sprintf("%02d:00", hour)}
	}
	return UsageCostChart{Points: points}
}

func (s *Service) resolveUsageLogUserFilter(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if user, err := s.repo.GetUser(raw); err == nil {
		return user.ID
	}
	if user, err := s.repo.FindUserByUsername(raw); err == nil {
		return user.ID
	}
	return raw
}

func (s *Service) ListModels(ctx context.Context) []core.ModelSpec {
	_ = ctx
	modelRevision, cacheable := modelListRevisions(s.repo)
	if cacheable {
		if cache, ok := s.loadModelListCache(); ok && cache.modelRevision == modelRevision {
			return cloneModelSpecs(cache.models)
		}
	}

	models := s.buildModelSpecs()
	if !cacheable {
		return models
	}

	s.modelListMu.Lock()
	defer s.modelListMu.Unlock()
	if cache, ok := s.loadModelListCache(); ok && cache.modelRevision == modelRevision {
		return cloneModelSpecs(cache.models)
	}
	s.modelList.Store(modelListCache{
		modelRevision: modelRevision,
		models:        cloneModelSpecs(models),
	})
	return models
}

func (s *Service) ListModelsForClient(ctx context.Context, client *core.APIClient) []core.ModelSpec {
	_ = ctx
	if !s.clientCanUseAccountGroup(client) {
		return nil
	}
	return s.buildModelSpecsForGroup(clientAccountGroupName(client))
}

func (s *Service) clientCanUseAccountGroup(client *core.APIClient) bool {
	if s == nil || s.repo == nil || client == nil {
		return true
	}
	groupName := clientAccountGroupName(client)
	if strings.EqualFold(groupName, core.DefaultAccountGroupName) {
		return true
	}
	ownerID := strings.TrimSpace(client.OwnerUserID)
	if ownerID == "" {
		return true
	}
	for _, group := range accountGroupsWithDefault(s.repo.ListAccountGroups()) {
		if strings.EqualFold(normalizeStoredAccountGroup(group.Name), groupName) {
			return core.AccountGroupVisibleInClientEditorForUser(group, ownerID)
		}
	}
	return true
}

func (s *Service) buildModelSpecs() []core.ModelSpec {
	configs := s.repo.ListModels()
	models := make([]core.ModelSpec, 0, len(configs))
	for _, config := range configs {
		if !config.Enabled {
			continue
		}
		models = append(models, core.ModelSpec{
			Name:      config.ID,
			Provider:  config.Provider,
			Type:      core.NormalizeModelType(config.Type, config.ID),
			CreatedAt: config.CreatedAt,
		})
	}
	return models
}

func (s *Service) buildModelSpecsForGroup(groupName string) []core.ModelSpec {
	visibleGroups := s.modelVisibilityGroupsForGroup(groupName)
	configs := s.repo.ListModels()
	models := make([]core.ModelSpec, 0, len(configs))
	for _, config := range configs {
		if !config.Enabled || !modelVisibleToAnyGroup(config, visibleGroups) {
			continue
		}
		models = append(models, core.ModelSpec{
			Name:      config.ID,
			Provider:  config.Provider,
			Type:      core.NormalizeModelType(config.Type, config.ID),
			CreatedAt: config.CreatedAt,
		})
	}
	return models
}

func modelVisibleToGroup(model core.ModelConfig, groupName string) bool {
	if len(model.VisibleGroups) == 0 {
		return false
	}
	groupName = normalizeStoredAccountGroup(groupName)
	for _, group := range model.VisibleGroups {
		if strings.EqualFold(normalizeStoredAccountGroup(group), groupName) {
			return true
		}
	}
	return false
}

func (s *Service) modelVisibilityGroupsForGroup(groupName string) []string {
	return []string{core.NormalizeAccountGroupName(groupName)}
}

func modelVisibleToAnyGroup(model core.ModelConfig, groups []string) bool {
	if len(model.VisibleGroups) == 0 {
		return false
	}
	for _, group := range groups {
		if modelVisibleToGroup(model, group) {
			return true
		}
	}
	return false
}

func clientAccountGroupName(client *core.APIClient) string {
	if client == nil {
		return ""
	}
	return normalizeStoredAccountGroup(client.AccountGroup)
}

func modelListRevisions(repo storage.Repository) (uint64, bool) {
	modelRevision := uint64(0)
	if revisioned, ok := repo.(modelRevisionProvider); ok {
		modelRevision = revisioned.ModelRevision()
	} else if revisioned, ok := repo.(configRevisionProvider); ok {
		modelRevision = revisioned.ConfigRevision()
	}
	return modelRevision, modelRevision != 0
}

func (s *Service) loadModelListCache() (modelListCache, bool) {
	value := s.modelList.Load()
	if value == nil {
		return modelListCache{}, false
	}
	cache, ok := value.(modelListCache)
	return cache, ok
}

func cloneModelSpecs(models []core.ModelSpec) []core.ModelSpec {
	out := make([]core.ModelSpec, 0, len(models))
	for _, model := range models {
		model.Aliases = append([]string(nil), model.Aliases...)
		out = append(out, model)
	}
	return out
}

func (s *Service) StartConnect(provider core.ProviderKind, oauthImportID string, preferredGroup ...string) (ConnectStart, error) {
	provider = core.ProviderKind(strings.TrimSpace(string(provider)))
	if _, ok := s.providers.Get(provider); !ok {
		return ConnectStart{}, fmt.Errorf("provider %q is not registered", provider)
	}
	availableGroups := s.visibleNamedAccountGroupsForProvider("", provider)
	if len(availableGroups) == 0 {
		return ConnectStart{}, fmt.Errorf("no account group allows %s accounts", providerLabel(provider))
	}
	group := availableGroups[0].Name
	if len(preferredGroup) > 0 {
		if selected := normalizeAccountGroup(preferredGroup[0]); selected != "" {
			for _, candidate := range availableGroups {
				if strings.EqualFold(normalizeAccountGroup(candidate.Name), selected) {
					group = candidate.Name
					break
				}
			}
		}
	}
	start := ConnectStart{
		Provider:        provider,
		ProviderLabel:   providerLabel(provider),
		Group:           group,
		AvailableGroups: availableGroups,
		Priority:        100,
		Weight:          100,
	}
	if prefill, includeCodexAuthPath, ok := s.oauthImportForProvider(provider, oauthImportID); ok {
		applyConnectImport(&start, prefill, includeCodexAuthPath)
	}
	return start, nil
}

func (s *Service) CompleteManualConnect(input ManualConnectInput) error {
	input.Provider = core.ProviderKind(strings.TrimSpace(string(input.Provider)))
	if _, ok := s.providers.Get(input.Provider); !ok {
		return fmt.Errorf("provider %q is not registered", input.Provider)
	}
	input.Label = strings.TrimSpace(input.Label)
	if input.Label == "" {
		return fmt.Errorf("label is required")
	}
	input.Group = normalizeAccountGroup(input.Group)
	if input.Group == "" {
		return fmt.Errorf("account group is required")
	}
	if err := s.ensureAccountGroupExists(input.Group); err != nil {
		return err
	}
	if err := s.ensureAccountGroupAllowsProvider(input.Group, input.Provider); err != nil {
		return err
	}
	if err := validateProxyURL(input.ProxyURL); err != nil {
		return err
	}
	if strings.TrimSpace(input.AccessToken) == "" {
		return fmt.Errorf("access token is required")
	}

	metadata := map[string]string{}
	if trimmedBaseURL := strings.TrimSpace(input.BaseURL); trimmedBaseURL != "" {
		metadata["base_url"] = trimmedBaseURL
	}
	if trimmedTokenSource := strings.TrimSpace(input.TokenSource); trimmedTokenSource != "" {
		metadata["token_source"] = trimmedTokenSource
	}
	if trimmedAccountID := strings.TrimSpace(input.OAuthAccountID); trimmedAccountID != "" {
		metadata["oauth_account_id"] = trimmedAccountID
	}
	if trimmedEmail := strings.TrimSpace(input.OAuthEmail); trimmedEmail != "" {
		metadata["email"] = trimmedEmail
	}
	if trimmedCodexAuthPath := strings.TrimSpace(input.CodexAuthPath); trimmedCodexAuthPath != "" {
		metadata[providers.OpenAICodexAuthPathMetadataKey] = trimmedCodexAuthPath
	}
	loginMethod := strings.TrimSpace(input.AccountLoginMethod)
	if loginMethod == "" {
		loginMethod = "api_key"
	}
	if loginMethod != "token" && loginMethod != "api_key" {
		return fmt.Errorf("account login method must be token or api_key")
	}
	metadata["account_login_method"] = loginMethod
	if loginMethod == "api_key" {
		metadata["account_type"] = "api_key"
		quotaProvider := strings.TrimSpace(input.APIKeyQuotaProvider)
		if quotaProvider != "" {
			if err := validateAPIKeyQuotaProvider(input.Provider, quotaProvider); err != nil {
				return err
			}
			metadata["api_key_quota_provider"] = quotaProvider
		}
	} else {
		metadata["account_type"] = "official"
	}

	mode := "manual-token"
	tags := []string{"manual"}
	if trimmedMode := strings.TrimSpace(input.CredentialMode); trimmedMode != "" {
		mode = trimmedMode
	}
	if providers.IsOpenAIOAuthTokenSource(input.TokenSource) {
		mode = providers.OpenAIOAuthModeValue()
		tags = []string{"oauth", "chatgpt"}
		metadata["account_login_method"] = "token"
		delete(metadata, "api_key_quota_provider")
		if strings.EqualFold(strings.TrimSpace(input.TokenSource), providers.OpenAICodexAuthTokenSourceValue()) {
			metadata["account_type"] = "free"
		} else {
			metadata["account_type"] = "official"
		}
	}
	if strings.TrimSpace(input.TokenSource) == providers.ClaudeOAuthTokenSourceValue() {
		mode = providers.ClaudeOAuthModeValue()
		tags = []string{"oauth", "claude"}
		metadata["account_type"] = "official"
		metadata["account_login_method"] = "token"
		delete(metadata, "api_key_quota_provider")
	}

	for i := 0; i < 5; i++ {
		accountID, err := generateAccountID(input.Provider)
		if err != nil {
			return err
		}
		account := core.Account{
			ID:       accountID,
			Provider: input.Provider,
			Label:    input.Label,
			Group:    input.Group,
			ProxyURL: normalizeProxyURL(input.ProxyURL),
			Status:   core.AccountStatusActive,
			Backup:   input.Backup,
			Priority: input.Priority,
			Weight:   input.Weight,
			Tags:     tags,
			Credential: core.Credential{
				Mode:         mode,
				AccessToken:  input.AccessToken,
				RefreshToken: strings.TrimSpace(input.RefreshToken),
				SessionToken: strings.TrimSpace(input.SessionToken),
				ExpiresAt:    cloneTimePtr(input.ExpiresAt),
				Metadata:     metadata,
			},
		}
		if _, err := s.repo.GetAccount(account.ID); err == nil {
			continue
		}
		return s.repo.UpsertAccount(account)
	}
	return fmt.Errorf("generate account id conflict")
}

func validateAPIKeyQuotaProvider(provider core.ProviderKind, quotaProvider string) error {
	quotaProvider = strings.TrimSpace(quotaProvider)
	if quotaProvider == "" {
		return nil
	}
	switch provider {
	case core.ProviderOpenAI:
		switch quotaProvider {
		case "new-api", "sub2api", "gateway":
			return nil
		}
		return fmt.Errorf("api key quota provider must be empty, new-api, sub2api, or gateway")
	case core.ProviderClaude:
		switch quotaProvider {
		case "sub2api", "gateway":
			return nil
		}
		return fmt.Errorf("claude api key quota provider must be empty, sub2api, or gateway")
	default:
		return fmt.Errorf("api key quota provider is not supported for provider %q", provider)
	}
}

func (s *Service) StartOpenAIOAuth(ctx context.Context) (OpenAIOAuthStart, error) {
	return s.startOpenAIOAuth(ctx, "")
}

func (s *Service) StartOpenAIOAuthForAccount(ctx context.Context, accountID string) (OpenAIOAuthStart, error) {
	account, err := s.oauthUpdateAccount(accountID, core.ProviderOpenAI)
	if err != nil {
		return OpenAIOAuthStart{}, err
	}
	start, err := s.startOpenAIOAuth(ctx, account.ID)
	if err != nil {
		return OpenAIOAuthStart{}, err
	}
	start.SuggestedLabel = account.Label
	return start, nil
}

func (s *Service) startOpenAIOAuth(ctx context.Context, targetAccountID string) (OpenAIOAuthStart, error) {
	if _, ok := s.providers.Get(core.ProviderOpenAI); !ok {
		return OpenAIOAuthStart{}, fmt.Errorf("provider %q is not registered", core.ProviderOpenAI)
	}
	if !s.OAuthEnabled(core.ProviderOpenAI) {
		return OpenAIOAuthStart{}, fmt.Errorf("openai oauth is disabled in system settings")
	}
	ctx = s.WithSystemProxy(ctx)
	device, err := providers.StartOpenAIDeviceFlow(ctx)
	if err != nil {
		return OpenAIOAuthStart{}, err
	}
	expiresAt := time.Now().UTC().Add(time.Duration(device.ExpiresIn) * time.Second)

	s.oauthMu.Lock()
	storeMapValue(&s.openAIPending, device.DeviceCode, openAIOAuthPending{
		UserCode:        device.UserCode,
		TargetAccountID: strings.TrimSpace(targetAccountID),
		ExpiresAt:       expiresAt,
	})
	s.oauthMu.Unlock()

	return OpenAIOAuthStart{
		DeviceCode:      device.DeviceCode,
		UserCode:        device.UserCode,
		VerificationURI: device.VerificationURI,
		ExpiresAt:       expiresAt,
		Interval:        device.Interval,
		SuggestedLabel:  "OpenAI OAuth",
	}, nil
}

func (s *Service) CompleteOpenAIOAuth(ctx context.Context, deviceCode string) (OpenAIOAuthConnectResult, error) {
	deviceCode = strings.TrimSpace(deviceCode)
	if deviceCode == "" {
		return OpenAIOAuthConnectResult{}, fmt.Errorf("device code is required")
	}

	s.oauthMu.Lock()
	pending, ok := s.openAIPending[deviceCode]
	s.oauthMu.Unlock()
	if !ok {
		return OpenAIOAuthConnectResult{}, fmt.Errorf("oauth session was not found")
	}
	if time.Now().UTC().After(pending.ExpiresAt) {
		s.oauthMu.Lock()
		delete(s.openAIPending, deviceCode)
		s.oauthMu.Unlock()
		return OpenAIOAuthConnectResult{}, providers.ErrOpenAIOAuthExpired
	}

	if !s.OAuthEnabled(core.ProviderOpenAI) {
		return OpenAIOAuthConnectResult{}, fmt.Errorf("openai oauth is disabled in system settings")
	}
	ctx = s.WithSystemProxy(ctx)
	tokens, err := providers.PollOpenAIDeviceFlow(ctx, deviceCode, pending.UserCode)
	if errors.Is(err, providers.ErrOpenAIOAuthPending) {
		return OpenAIOAuthConnectResult{}, ErrOAuthPending
	}
	if err != nil {
		return OpenAIOAuthConnectResult{}, err
	}
	if strings.TrimSpace(tokens.RefreshToken) == "" {
		return OpenAIOAuthConnectResult{}, fmt.Errorf("openai oauth response did not include a refresh token")
	}

	accountID, email := providers.ExtractOpenAIIdentity(tokens)
	label := oauthImportLabel(core.ProviderOpenAI, email, accountID)
	importID, err := generateConnectImportID()
	if err != nil {
		return OpenAIOAuthConnectResult{}, err
	}

	imported := newOAuthConnectImport(
		label,
		tokens.AccessToken,
		tokens.RefreshToken,
		providers.OpenAITokenExpiry(tokens.ExpiresIn),
		providers.OpenAIOAuthModeValue(),
		providers.OpenAIDeviceCodeTokenSourceValue(),
		accountID,
		email,
		"",
	)
	if pending.TargetAccountID != "" {
		if _, err := s.updateAccountOAuthCredential(pending.TargetAccountID, core.ProviderOpenAI, imported); err != nil {
			return OpenAIOAuthConnectResult{}, err
		}
		s.oauthMu.Lock()
		delete(s.openAIPending, deviceCode)
		s.oauthMu.Unlock()
		return OpenAIOAuthConnectResult{
			ImportID:        importID,
			Label:           label,
			Email:           email,
			TargetAccountID: pending.TargetAccountID,
		}, nil
	}

	s.oauthMu.Lock()
	delete(s.openAIPending, deviceCode)
	storeMapValue(&s.openAIImports, importID, imported)
	s.oauthMu.Unlock()

	return OpenAIOAuthConnectResult{
		ImportID: importID,
		Label:    label,
		Email:    email,
	}, nil
}

func (s *Service) StartClaudeOAuth(ctx context.Context, redirectURI string) (ClaudeOAuthStart, error) {
	return s.startClaudeOAuth(ctx, redirectURI, "")
}

func (s *Service) StartClaudeOAuthForAccount(ctx context.Context, redirectURI, accountID string) (ClaudeOAuthStart, error) {
	account, err := s.oauthUpdateAccount(accountID, core.ProviderClaude)
	if err != nil {
		return ClaudeOAuthStart{}, err
	}
	start, err := s.startClaudeOAuth(ctx, redirectURI, account.ID)
	if err != nil {
		return ClaudeOAuthStart{}, err
	}
	start.SuggestedLabel = account.Label
	return start, nil
}

func (s *Service) startClaudeOAuth(ctx context.Context, redirectURI, targetAccountID string) (ClaudeOAuthStart, error) {
	if _, ok := s.providers.Get(core.ProviderClaude); !ok {
		return ClaudeOAuthStart{}, fmt.Errorf("provider %q is not registered", core.ProviderClaude)
	}
	if !s.OAuthEnabled(core.ProviderClaude) {
		return ClaudeOAuthStart{}, fmt.Errorf("claude oauth is disabled in system settings")
	}
	redirectURI = strings.TrimSpace(redirectURI)
	if redirectURI == "" {
		return ClaudeOAuthStart{}, fmt.Errorf("redirect uri is required")
	}

	start, err := providers.StartClaudeAuthorization(ctx, redirectURI)
	if err != nil {
		return ClaudeOAuthStart{}, err
	}
	expiresAt := time.Now().UTC().Add(time.Duration(start.ExpiresIn) * time.Second)

	s.oauthMu.Lock()
	storeMapValue(&s.claudePending, start.State, claudeOAuthPending{
		RedirectURI:     redirectURI,
		CodeVerifier:    start.CodeVerifier,
		ExpectedState:   start.State,
		TargetAccountID: strings.TrimSpace(targetAccountID),
		ExpiresAt:       expiresAt,
	})
	s.oauthMu.Unlock()

	return ClaudeOAuthStart{
		AuthorizationURL: start.AuthorizationURL,
		ExpiresAt:        expiresAt,
		SuggestedLabel:   "Claude OAuth",
	}, nil
}

func (s *Service) CompleteClaudeOAuth(ctx context.Context, state, code string) (ClaudeOAuthConnectResult, error) {
	state = strings.TrimSpace(state)
	code = strings.TrimSpace(code)
	if state == "" {
		return ClaudeOAuthConnectResult{}, fmt.Errorf("state is required")
	}
	if code == "" {
		return ClaudeOAuthConnectResult{}, fmt.Errorf("authorization code is required")
	}

	s.oauthMu.Lock()
	pending, ok := s.claudePending[state]
	s.oauthMu.Unlock()
	if !ok {
		return ClaudeOAuthConnectResult{}, fmt.Errorf("oauth session was not found")
	}
	if time.Now().UTC().After(pending.ExpiresAt) {
		s.oauthMu.Lock()
		delete(s.claudePending, state)
		s.oauthMu.Unlock()
		return ClaudeOAuthConnectResult{}, fmt.Errorf("claude oauth session expired")
	}

	if !s.OAuthEnabled(core.ProviderClaude) {
		return ClaudeOAuthConnectResult{}, fmt.Errorf("claude oauth is disabled in system settings")
	}
	ctx = s.WithSystemProxy(ctx)
	tokens, err := providers.ExchangeClaudeAuthorizationCode(ctx, code, state, pending.ExpectedState, pending.RedirectURI, pending.CodeVerifier)
	if err != nil {
		return ClaudeOAuthConnectResult{}, err
	}
	if strings.TrimSpace(tokens.RefreshToken) == "" {
		return ClaudeOAuthConnectResult{}, fmt.Errorf("claude oauth response did not include a refresh token")
	}

	accountID, email := providers.ExtractClaudeIdentity(tokens)
	label := oauthImportLabel(core.ProviderClaude, email, accountID)

	importID, err := generateConnectImportID()
	if err != nil {
		return ClaudeOAuthConnectResult{}, err
	}

	imported := newOAuthConnectImport(
		label,
		tokens.AccessToken,
		tokens.RefreshToken,
		providers.ClaudeTokenExpiry(tokens.ExpiresIn),
		providers.ClaudeOAuthModeValue(),
		providers.ClaudeOAuthTokenSourceValue(),
		accountID,
		email,
		"",
	)
	if pending.TargetAccountID != "" {
		if _, err := s.updateAccountOAuthCredential(pending.TargetAccountID, core.ProviderClaude, imported); err != nil {
			return ClaudeOAuthConnectResult{}, err
		}
		s.oauthMu.Lock()
		delete(s.claudePending, state)
		s.oauthMu.Unlock()
		return ClaudeOAuthConnectResult{
			ImportID:        importID,
			Label:           label,
			Email:           email,
			TargetAccountID: pending.TargetAccountID,
		}, nil
	}

	s.oauthMu.Lock()
	delete(s.claudePending, state)
	storeMapValue(&s.claudeImports, importID, imported)
	s.oauthMu.Unlock()

	return ClaudeOAuthConnectResult{
		ImportID: importID,
		Label:    label,
		Email:    email,
	}, nil
}

func (s *Service) oauthUpdateAccount(accountID string, provider core.ProviderKind) (core.Account, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return core.Account{}, fmt.Errorf("account id is required")
	}
	account, err := s.repo.GetAccount(accountID)
	if err != nil {
		return core.Account{}, err
	}
	if account.Provider != provider {
		return core.Account{}, fmt.Errorf("account provider mismatch")
	}
	if !providers.CredentialRefreshable(account) {
		return core.Account{}, fmt.Errorf("account is not an OAuth credential")
	}
	return account, nil
}

func (s *Service) updateAccountOAuthCredential(accountID string, provider core.ProviderKind, imported oauthConnectImport) (core.Account, error) {
	account, err := s.oauthUpdateAccount(accountID, provider)
	if err != nil {
		return core.Account{}, err
	}
	if strings.TrimSpace(imported.AccessToken) == "" {
		return core.Account{}, fmt.Errorf("access token is required")
	}
	if strings.TrimSpace(imported.RefreshToken) == "" {
		return core.Account{}, fmt.Errorf("refresh token is required")
	}

	account.Credential.AccessToken = strings.TrimSpace(imported.AccessToken)
	account.Credential.RefreshToken = strings.TrimSpace(imported.RefreshToken)
	account.Credential.SessionToken = strings.TrimSpace(imported.SessionToken)
	account.Credential.ExpiresAt = cloneTimePtr(imported.CredentialExpiresAt)
	if mode := strings.TrimSpace(imported.CredentialMode); mode != "" {
		account.Credential.Mode = mode
	}

	metadata := cloneStringMap(account.Credential.Metadata)
	if tokenSource := strings.TrimSpace(imported.TokenSource); tokenSource != "" {
		metadata["token_source"] = tokenSource
	}
	if oauthAccountID := strings.TrimSpace(imported.OAuthAccountID); oauthAccountID != "" {
		metadata["oauth_account_id"] = oauthAccountID
	} else {
		delete(metadata, "oauth_account_id")
	}
	if email := strings.TrimSpace(imported.OAuthEmail); email != "" {
		metadata["email"] = email
	} else {
		delete(metadata, "email")
	}
	if baseURL := strings.TrimSpace(imported.BaseURL); baseURL != "" {
		metadata["base_url"] = baseURL
	}
	if codexAuthPath := strings.TrimSpace(imported.CodexAuthPath); codexAuthPath != "" {
		metadata[providers.OpenAICodexAuthPathMetadataKey] = codexAuthPath
	} else if strings.TrimSpace(imported.TokenSource) != providers.OpenAICodexAuthTokenSourceValue() {
		delete(metadata, providers.OpenAICodexAuthPathMetadataKey)
	}
	metadata["account_login_method"] = "token"
	if strings.EqualFold(strings.TrimSpace(imported.TokenSource), providers.OpenAICodexAuthTokenSourceValue()) {
		metadata["account_type"] = "free"
	} else {
		metadata["account_type"] = "official"
	}
	delete(metadata, "api_key_quota_provider")
	delete(metadata, "new_api_user_access_token")
	delete(metadata, "new_api_user_id")
	account.Credential.Metadata = metadata

	account = core.ClearAccountQuotaRefreshFailureMetadata(account)
	account.Status = core.AccountStatusActive
	account.CooldownUntil = nil
	account.ConsecutiveFails = 0
	if err := s.repo.UpsertAccount(account); err != nil {
		return core.Account{}, err
	}
	return s.repo.GetAccount(account.ID)
}

func (s *Service) GetAccountEditor(id string) (AccountEditor, error) {
	account, err := s.repo.GetAccount(id)
	if err != nil {
		return AccountEditor{}, err
	}

	return AccountEditor{
		Account:         account,
		BaseURL:         strings.TrimSpace(account.Credential.Metadata["base_url"]),
		ProxyURL:        strings.TrimSpace(account.ProxyURL),
		ExpiresAtText:   formatOptionalTime(account.Credential.ExpiresAt),
		StatusOptions:   append([]core.AccountStatus(nil), editableAccountStatuses...),
		AvailableGroups: availableAccountGroupsWithCurrent(accountGroupsWithDefault(s.repo.ListAccountGroups()), account.Group),
	}, nil
}

func (s *Service) GetAccount(id string) (core.Account, error) {
	return s.repo.GetAccount(id)
}

func (s *Service) UpsertOpenAIResponseBinding(responseID, accountID string, client *core.APIClient) error {
	clientID := ""
	if client != nil {
		clientID = strings.TrimSpace(client.ID)
	}
	return s.repo.UpsertOpenAIResponseBinding(core.OpenAIResponseBinding{
		ResponseID: strings.TrimSpace(responseID),
		AccountID:  strings.TrimSpace(accountID),
		ClientID:   clientID,
	})
}

func (s *Service) GetOpenAIResponseBinding(responseID string) (core.OpenAIResponseBinding, error) {
	return s.repo.GetOpenAIResponseBinding(strings.TrimSpace(responseID))
}

func (s *Service) DetectAccountStatus(ctx context.Context, id string) (core.Account, error) {
	accountID := strings.TrimSpace(id)
	account, err := s.repo.GetAccount(accountID)
	if err != nil {
		return core.Account{}, err
	}
	if s.providers == nil {
		return core.Account{}, fmt.Errorf("provider registry is not configured")
	}
	adapter, ok := s.providers.Get(account.Provider)
	if !ok {
		return core.Account{}, fmt.Errorf("provider %q is not registered", account.Provider)
	}
	account = core.NormalizeAccountRuntimeState(account, time.Now().UTC())
	account.EffectiveProxyURL = s.effectiveProxyURLForAccount(account)
	if refresher, ok := adapter.(providers.RefreshingAdapter); ok && providers.CredentialNeedsRefresh(account) {
		credential, err := refresher.Refresh(ctx, account)
		if err != nil {
			if persistErr := s.persistCredentialRefreshFailure(account, err); persistErr != nil {
				return core.Account{}, persistErr
			}
			saved, savedErr := s.repo.GetAccount(accountID)
			if savedErr != nil {
				return core.Account{}, savedErr
			}
			return saved, err
		}
		account.Credential = credential
		if current, err := s.repo.GetAccount(account.ID); err == nil {
			account = core.PreserveAccountControlState(account, current)
		}
		if err := s.repo.UpsertAccount(account); err != nil {
			return core.Account{}, err
		}
	}
	decision := core.RouteDecision{
		Provider: account.Provider,
		Account:  account,
		Model:    s.accountDetectionModel(account),
		Reason:   "account_detection",
	}
	err = s.streamCheckAccount(ctx, adapter, decision, accountDetectionRequest(account))
	if err != nil {
		if persistErr := s.persistPingAccountFailure(account, err); persistErr != nil {
			return core.Account{}, persistErr
		}
		saved, savedErr := s.repo.GetAccount(accountID)
		if savedErr != nil {
			return core.Account{}, savedErr
		}
		return saved, err
	}
	if err := s.persistPingAccountSuccess(account); err != nil {
		return core.Account{}, err
	}
	saved, err := s.repo.GetAccount(accountID)
	if err != nil {
		return core.Account{}, err
	}
	return saved, nil
}

func (s *Service) streamCheckAccount(ctx context.Context, adapter providers.Adapter, decision core.RouteDecision, req *core.GatewayRequest) error {
	streaming, ok := adapter.(providers.StreamingAdapter)
	if !ok {
		return fmt.Errorf("provider %q does not support stream detection", decision.Provider)
	}
	session, err := streaming.OpenStream(ctx, decision, req)
	if err != nil {
		return err
	}
	if session == nil || session.Stream == nil {
		return fmt.Errorf("upstream returned no detection stream")
	}
	defer session.Stream.Close()

	for {
		event, err := session.Stream.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("upstream detection stream ended before any usable event")
			}
			return err
		}
		usable, eventErr := streamDetectionEventUsable(decision.Provider, event)
		if eventErr != nil {
			return eventErr
		}
		if usable {
			return nil
		}
	}
}

func streamDetectionEventUsable(provider core.ProviderKind, event *core.StreamEvent) (bool, error) {
	if event == nil {
		return false, nil
	}
	rawEvent := strings.TrimSpace(event.RawEvent)
	finishReason := strings.TrimSpace(event.FinishReason)
	switch provider {
	case core.ProviderOpenAI:
		switch rawEvent {
		case "response.completed", "response.done":
			return true, nil
		case "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
			if finishReason == "" {
				finishReason = strings.TrimPrefix(strings.TrimPrefix(rawEvent, "response."), "response_")
			}
			return false, fmt.Errorf("upstream detection ended with %s", finishReason)
		}
		return event.Done && finishReason != "", nil
	case core.ProviderClaude:
		if rawEvent == "message_stop" {
			return true, nil
		}
		return event.Done && finishReason != "", nil
	default:
		return event.Done && finishReason != "", nil
	}
}

func (s *Service) accountDetectionModel(account core.Account) string {
	if account.Provider == core.ProviderOpenAI {
		return "gpt-5.5"
	}
	for _, model := range s.repo.ListModels() {
		if model.Provider != account.Provider || !model.Enabled {
			continue
		}
		if core.NormalizeModelType(model.Type, model.ID) != core.ModelTypeText {
			continue
		}
		if upstreamID := strings.TrimSpace(model.UpstreamID); upstreamID != "" {
			return upstreamID
		}
		if id := strings.TrimSpace(model.ID); id != "" {
			return id
		}
	}
	switch account.Provider {
	case core.ProviderClaude:
		return "claude-sonnet-4-6"
	case core.ProviderOpenAI:
		return "gpt-5.5"
	default:
		return "gpt-5.5"
	}
}

func accountDetectionRequest(account core.Account) *core.GatewayRequest {
	if account.Provider == core.ProviderOpenAI && openAIAccountDetectionLoginMethod(account) != "token" {
		return &core.GatewayRequest{
			Messages: []core.Message{
				{Role: "user", Content: "hi"},
			},
			Metadata: map[string]string{
				"purpose": "account_detection",
			},
		}
	}
	maxTokens := 8
	temperature := 0.0
	var metadata map[string]string
	if account.Provider == core.ProviderOpenAI {
		metadata = map[string]string{
			"purpose": "account_detection",
		}
	}
	request := &core.GatewayRequest{
		Messages: []core.Message{
			{Role: "user", Content: "Reply with exactly this word: pong"},
		},
		MaxTokens:   &maxTokens,
		Temperature: &temperature,
		Metadata:    metadata,
	}
	return request
}

func openAIAccountDetectionLoginMethod(account core.Account) string {
	if account.Provider != core.ProviderOpenAI {
		return ""
	}
	if providers.IsOpenAIOAuthTokenSource(account.Credential.Metadata["token_source"]) ||
		strings.TrimSpace(account.Credential.Mode) == providers.OpenAIOAuthModeValue() {
		return "token"
	}
	method := strings.TrimSpace(account.Credential.Metadata["account_login_method"])
	if method == "token" {
		return "token"
	}
	return "api_key"
}

func (s *Service) persistPingAccountSuccess(account core.Account) error {
	account = core.ClearAccountQuotaRefreshFailureMetadata(account)
	account.Status = core.AccountStatusActive
	account.CooldownUntil = nil
	account.ConsecutiveFails = 0
	return s.upsertAccountRuntimeState(account)
}

func (s *Service) persistPingAccountFailure(account core.Account, err error) error {
	// Model-listing probes are diagnostic only. A /models failure is not proof
	// that the credential is expired or the account should leave the pool.
	_ = account
	_ = err
	return nil
}

func (s *Service) persistCredentialRefreshFailure(account core.Account, err error) error {
	account = providers.ApplyCredentialRefreshFailureStatus(account, err, time.Now().UTC())
	return s.upsertAccountRuntimeState(account)
}

func (s *Service) UpdateAccount(id, label, remark, group, proxyURL, accessToken, refreshToken, sessionToken, baseURL string, expiresAt *time.Time, priority, weight int, status core.AccountStatus, controlDisabled, backup bool) error {
	account, err := s.repo.GetAccount(id)
	if err != nil {
		return err
	}
	label = strings.TrimSpace(label)
	if label == "" {
		return fmt.Errorf("label is required")
	}

	if status != "" && !isEditableAccountStatus(status) {
		return fmt.Errorf("account status %q is not supported", status)
	}
	if status == "" {
		status = account.Status
	}
	if err := validateProxyURL(proxyURL); err != nil {
		return err
	}

	targetGroup := normalizeAccountGroup(group)
	if targetGroup == "" {
		return fmt.Errorf("account group is required")
	}
	account.Label = label
	account.Remark = strings.TrimSpace(remark)
	account.Group = targetGroup
	account.ProxyURL = normalizeProxyURL(proxyURL)
	if err := s.ensureAccountGroupExists(account.Group); err != nil {
		return err
	}
	if err := s.ensureAccountGroupAllowsProvider(account.Group, account.Provider); err != nil {
		return err
	}
	account.Priority = priority
	account.Weight = weight
	account.Status = status
	account.ControlDisabled = controlDisabled
	account.Backup = backup
	account.CooldownUntil = nil
	if status != "" {
		account = core.ClearAccountQuotaRefreshFailureMetadata(account)
	}

	if strings.TrimSpace(accessToken) != "" {
		account.Credential.AccessToken = accessToken
	}
	if strings.TrimSpace(refreshToken) != "" {
		account.Credential.RefreshToken = strings.TrimSpace(refreshToken)
	}
	if strings.TrimSpace(sessionToken) != "" {
		account.Credential.SessionToken = strings.TrimSpace(sessionToken)
	}
	account.Credential.ExpiresAt = cloneTimePtr(expiresAt)

	metadata := cloneStringMap(account.Credential.Metadata)
	if trimmedBaseURL := strings.TrimSpace(baseURL); trimmedBaseURL != "" {
		metadata["base_url"] = trimmedBaseURL
	} else {
		delete(metadata, "base_url")
	}
	delete(metadata, "new_api_user_access_token")
	delete(metadata, "new_api_user_id")
	account.Credential.Metadata = metadata

	return s.repo.UpsertAccount(account)
}

func (s *Service) ToggleAccount(id string) error {
	account, err := s.repo.GetAccount(id)
	if err != nil {
		return err
	}
	account.ControlDisabled = !account.ControlDisabled
	return s.repo.UpsertAccount(account)
}

func (s *Service) RecoverAccount(id string) (core.Account, error) {
	account, err := s.repo.GetAccount(id)
	if err != nil {
		return core.Account{}, err
	}
	account = core.ClearAccountQuotaRefreshFailureMetadata(account)
	account = core.ClearAccountScopedRateLimits(account)
	account.Status = core.AccountStatusActive
	account.CooldownUntil = nil
	account.ConsecutiveFails = 0
	if err := s.repo.UpsertAccount(account); err != nil {
		return core.Account{}, err
	}
	return s.repo.GetAccount(id)
}

func (s *Service) DeleteAccount(id string) (core.Account, error) {
	account, err := s.repo.GetAccount(id)
	if err != nil {
		return core.Account{}, err
	}
	if err := s.repo.DeleteAccount(id); err != nil {
		return core.Account{}, err
	}
	return account, nil
}

func (s *Service) CreateAccountGroup(name string, groupType ...string) (core.AccountGroup, error) {
	name = normalizeAccountGroup(name)
	if name == "" {
		return core.AccountGroup{}, fmt.Errorf("group name is required")
	}
	if reservedAccountGroupName(name) {
		return core.AccountGroup{}, fmt.Errorf("account group name %q is reserved", name)
	}
	for _, existing := range s.repo.ListAccountGroups() {
		if strings.EqualFold(existing.Name, name) {
			return core.AccountGroup{}, fmt.Errorf("account group %q already exists", existing.Name)
		}
	}
	normalizedType := core.AccountGroupTypeMixed
	if len(groupType) > 0 {
		normalizedType = core.NormalizeAccountGroupType(groupType[0])
	}
	remark := ""
	if len(groupType) > 1 {
		remark = normalizeAccountGroupRemark(groupType[1])
	}
	group := core.AccountGroup{
		ID:                       fmt.Sprintf("group_%d", time.Now().UnixNano()),
		Name:                     name,
		Type:                     normalizedType,
		Remark:                   remark,
		ProxyURL:                 "",
		ShowInClientEditor:       nil,
		BillingMultiplierBps:     core.AccountGroupDefaultMultiplierBps,
		PlanBillingMultiplierBps: core.AccountGroupDefaultMultiplierBps,
		PlanBillingEnabled:       boolPtr(true),
	}
	if err := s.repo.UpsertAccountGroup(group); err != nil {
		return core.AccountGroup{}, err
	}
	groups := s.repo.ListAccountGroups()
	for _, saved := range groups {
		if saved.ID == group.ID {
			return saved, nil
		}
	}
	return group, nil
}

func (s *Service) UpdateAccountGroupName(id, name string) (core.AccountGroup, error) {
	name = normalizeAccountGroup(name)
	if name == "" {
		return core.AccountGroup{}, fmt.Errorf("group name is required")
	}
	if isDefaultAccountGroupID(id) {
		return core.AccountGroup{}, fmt.Errorf("default account group cannot be renamed")
	}
	if reservedAccountGroupName(name) {
		return core.AccountGroup{}, fmt.Errorf("account group name %q is reserved", name)
	}
	groups := s.repo.ListAccountGroups()
	var target core.AccountGroup
	found := false
	for _, group := range groups {
		if group.ID == id {
			target = group
			found = true
			continue
		}
		if strings.EqualFold(normalizeAccountGroup(group.Name), name) {
			return core.AccountGroup{}, fmt.Errorf("account group %q already exists", group.Name)
		}
	}
	if !found {
		return core.AccountGroup{}, storage.ErrNotFound
	}
	oldName := normalizeStoredAccountGroup(target.Name)
	if oldName == "" {
		oldName = target.Name
	}
	target.Name = name
	if err := s.repo.UpsertAccountGroup(target); err != nil {
		return core.AccountGroup{}, err
	}
	if strings.EqualFold(oldName, name) && oldName == name {
		return s.accountGroupByID(target.ID, target)
	}
	if err := s.renameAccountGroupReferences(oldName, name); err != nil {
		return core.AccountGroup{}, err
	}
	return s.accountGroupByID(target.ID, target)
}

func (s *Service) UpdateAccountGroupProfile(id, name, groupType, remark string) (core.AccountGroup, error) {
	if isDefaultAccountGroupID(id) {
		target := s.defaultAccountGroupSettings()
		normalizedType := core.NormalizeAccountGroupType(groupType)
		if err := s.ensureExistingGroupAccountsAllowed(target.Name, normalizedType); err != nil {
			return core.AccountGroup{}, err
		}
		target.Type = normalizedType
		target.Remark = normalizeAccountGroupRemark(remark)
		if err := s.repo.UpsertAccountGroup(target); err != nil {
			return core.AccountGroup{}, err
		}
		return target, nil
	}
	name = normalizeAccountGroup(name)
	if name == "" {
		return core.AccountGroup{}, fmt.Errorf("group name is required")
	}
	if reservedAccountGroupName(name) {
		return core.AccountGroup{}, fmt.Errorf("account group name %q is reserved", name)
	}
	normalizedType := core.NormalizeAccountGroupType(groupType)
	normalizedRemark := normalizeAccountGroupRemark(remark)
	groups := s.repo.ListAccountGroups()
	var target core.AccountGroup
	found := false
	for _, group := range groups {
		if group.ID == id {
			target = group
			found = true
			continue
		}
		if strings.EqualFold(normalizeAccountGroup(group.Name), name) {
			return core.AccountGroup{}, fmt.Errorf("account group %q already exists", group.Name)
		}
	}
	if !found {
		return core.AccountGroup{}, storage.ErrNotFound
	}
	oldName := normalizeStoredAccountGroup(target.Name)
	if oldName == "" {
		oldName = target.Name
	}
	if err := s.ensureExistingGroupAccountsAllowed(oldName, normalizedType); err != nil {
		return core.AccountGroup{}, err
	}
	target.Name = name
	target.Type = normalizedType
	target.Remark = normalizedRemark
	if err := s.repo.UpsertAccountGroup(target); err != nil {
		return core.AccountGroup{}, err
	}
	if !strings.EqualFold(oldName, name) || oldName != name {
		if err := s.renameAccountGroupReferences(oldName, name); err != nil {
			return core.AccountGroup{}, err
		}
	}
	return s.accountGroupByID(target.ID, target)
}

func (s *Service) renameAccountGroupReferences(oldName, newName string) error {
	oldName = normalizeStoredAccountGroup(oldName)
	newName = normalizeStoredAccountGroup(newName)
	for _, account := range s.repo.ListAccounts() {
		if !strings.EqualFold(normalizeStoredAccountGroup(account.Group), oldName) {
			continue
		}
		account.Group = newName
		if err := s.repo.UpsertAccount(account); err != nil {
			return err
		}
	}
	for _, client := range s.repo.ListClients() {
		if !strings.EqualFold(normalizeStoredAccountGroup(client.AccountGroup), oldName) {
			continue
		}
		fullClient, err := s.repo.GetClient(client.ID)
		if err != nil {
			return err
		}
		fullClient.AccountGroup = newName
		if err := s.repo.UpsertClient(fullClient); err != nil {
			return err
		}
	}
	for _, model := range s.repo.ListModels() {
		visibleGroups, changed := replaceAccountGroupName(model.VisibleGroups, oldName, newName)
		if !changed {
			continue
		}
		model.VisibleGroups = visibleGroups
		if err := s.repo.UpsertModel(model); err != nil {
			return err
		}
	}
	for _, message := range s.repo.ListSiteMessages() {
		targetGroups, changed := replaceAccountGroupName(message.TargetAccountGroups, oldName, newName)
		if !changed {
			continue
		}
		message.TargetAccountGroups = targetGroups
		if err := s.repo.UpdateSiteMessage(message); err != nil {
			return err
		}
	}
	for _, target := range s.repo.ListMonitorTargets() {
		if !strings.EqualFold(normalizeStoredAccountGroup(target.AccountGroup), oldName) {
			continue
		}
		defaultName := oldName + " / " + strings.TrimSpace(target.Model)
		if strings.EqualFold(strings.TrimSpace(target.Name), defaultName) {
			target.Name = newName + " / " + strings.TrimSpace(target.Model)
		}
		target.AccountGroup = newName
		if err := s.repo.UpsertMonitorTarget(target); err != nil {
			return err
		}
	}
	return nil
}

func replaceAccountGroupName(values []string, oldName, newName string) ([]string, bool) {
	if len(values) == 0 {
		return values, false
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	changed := false
	for _, value := range values {
		trimmed := normalizeStoredAccountGroup(value)
		if strings.EqualFold(trimmed, oldName) {
			trimmed = newName
			changed = true
		}
		key := strings.ToLower(trimmed)
		if _, ok := seen[key]; ok {
			changed = true
			continue
		}
		seen[key] = struct{}{}
		out = append(out, trimmed)
	}
	return out, changed
}

func (s *Service) UpdateAccountGroupType(id, groupType string) (core.AccountGroup, error) {
	normalizedType := core.NormalizeAccountGroupType(groupType)
	if isDefaultAccountGroupID(id) {
		target := s.defaultAccountGroupSettings()
		target.Type = normalizedType
		if err := s.ensureExistingGroupAccountsAllowed(target.Name, normalizedType); err != nil {
			return core.AccountGroup{}, err
		}
		if err := s.repo.UpsertAccountGroup(target); err != nil {
			return core.AccountGroup{}, err
		}
		return target, nil
	}
	groups := s.repo.ListAccountGroups()
	var target core.AccountGroup
	found := false
	for _, group := range groups {
		if group.ID == id {
			target = group
			found = true
			break
		}
	}
	if !found {
		return core.AccountGroup{}, storage.ErrNotFound
	}
	if err := s.ensureExistingGroupAccountsAllowed(target.Name, normalizedType); err != nil {
		return core.AccountGroup{}, err
	}
	target.Type = normalizedType
	if err := s.repo.UpsertAccountGroup(target); err != nil {
		return core.AccountGroup{}, err
	}
	return s.accountGroupByID(target.ID, target)
}

func (s *Service) UpdateAccountGroupRemark(id, remark string) (core.AccountGroup, error) {
	remark = normalizeAccountGroupRemark(remark)
	if isDefaultAccountGroupID(id) {
		target := s.defaultAccountGroupSettings()
		target.Remark = remark
		if err := s.repo.UpsertAccountGroup(target); err != nil {
			return core.AccountGroup{}, err
		}
		return target, nil
	}
	groups := s.repo.ListAccountGroups()
	var target core.AccountGroup
	found := false
	for _, group := range groups {
		if group.ID == id {
			target = group
			found = true
			break
		}
	}
	if !found {
		return core.AccountGroup{}, storage.ErrNotFound
	}
	target.Remark = remark
	if err := s.repo.UpsertAccountGroup(target); err != nil {
		return core.AccountGroup{}, err
	}
	return s.accountGroupByID(target.ID, target)
}

func (s *Service) UpdateAccountGroupProxy(id, proxyURL string) (core.AccountGroup, error) {
	if err := validateProxyURL(proxyURL); err != nil {
		return core.AccountGroup{}, err
	}
	if isDefaultAccountGroupID(id) {
		target := s.defaultAccountGroupSettings()
		target.ProxyURL = normalizeProxyURL(proxyURL)
		if err := s.repo.UpsertAccountGroup(target); err != nil {
			return core.AccountGroup{}, err
		}
		return target, nil
	}
	groups := s.repo.ListAccountGroups()
	var target core.AccountGroup
	found := false
	for _, group := range groups {
		if group.ID == id {
			target = group
			found = true
			break
		}
	}
	if !found {
		return core.AccountGroup{}, storage.ErrNotFound
	}
	target.ProxyURL = normalizeProxyURL(proxyURL)
	if err := s.repo.UpsertAccountGroup(target); err != nil {
		return core.AccountGroup{}, err
	}
	return target, nil
}

func (s *Service) UpdateAccountGroupVisibility(id string, showInClientEditor bool) (core.AccountGroup, error) {
	return s.UpdateAccountGroupVisibilitySettings(id, showInClientEditor)
}

func (s *Service) UpdateAccountGroupVisibilitySettings(id string, showInClientEditor bool, visibleUserIDs ...[]string) (core.AccountGroup, error) {
	normalizedVisibleUserIDs, err := s.normalizeAccountGroupVisibleUserIDs(firstStringSliceArg(visibleUserIDs))
	if err != nil {
		return core.AccountGroup{}, err
	}
	if isDefaultAccountGroupID(id) {
		target := s.defaultAccountGroupSettings()
		target.ShowInClientEditor = boolPtr(true)
		target.VisibleUserIDs = nil
		if err := s.repo.UpsertAccountGroup(target); err != nil {
			return core.AccountGroup{}, err
		}
		return target, nil
	}
	groups := s.repo.ListAccountGroups()
	var target core.AccountGroup
	found := false
	for _, group := range groups {
		if group.ID == id {
			target = group
			found = true
			break
		}
	}
	if !found {
		return core.AccountGroup{}, storage.ErrNotFound
	}
	target.ShowInClientEditor = boolPtr(showInClientEditor)
	target.VisibleUserIDs = normalizedVisibleUserIDs
	if err := s.repo.UpsertAccountGroup(target); err != nil {
		return core.AccountGroup{}, err
	}
	return target, nil
}

func (s *Service) normalizeAccountGroupVisibleUserIDs(values []string) ([]string, error) {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		userID := strings.TrimSpace(value)
		if userID == "" {
			continue
		}
		user, err := s.repo.GetUser(userID)
		if err != nil {
			user, err = s.repo.FindUserByUsername(userID)
		}
		if err != nil {
			return nil, fmt.Errorf("visible user %q: %w", userID, err)
		}
		userID = strings.TrimSpace(user.ID)
		key := strings.ToLower(userID)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, userID)
	}
	slices.SortFunc(out, func(a, b string) int {
		return strings.Compare(strings.ToLower(a), strings.ToLower(b))
	})
	return out, nil
}

func firstStringSliceArg(values [][]string) []string {
	if len(values) == 0 {
		return nil
	}
	return values[0]
}

func (s *Service) UpdateAccountGroupBilling(id string, multiplierBps, inputPriceNanoUSDPer1M, cachedInputPriceNanoUSDPer1M, outputPriceNanoUSDPer1M int64, timedMultipliers ...[]core.AccountGroupTimedMultiplier) (core.AccountGroup, error) {
	return s.UpdateAccountGroupBillingWithPlanMultiplier(id, multiplierBps, multiplierBps, inputPriceNanoUSDPer1M, cachedInputPriceNanoUSDPer1M, outputPriceNanoUSDPer1M, timedMultipliers...)
}

func (s *Service) UpdateAccountGroupBillingWithPlanMultiplier(id string, multiplierBps, planMultiplierBps, inputPriceNanoUSDPer1M, cachedInputPriceNanoUSDPer1M, outputPriceNanoUSDPer1M int64, timedMultipliers ...[]core.AccountGroupTimedMultiplier) (core.AccountGroup, error) {
	if multiplierBps < 0 || planMultiplierBps < 0 || inputPriceNanoUSDPer1M < 0 || cachedInputPriceNanoUSDPer1M < 0 || outputPriceNanoUSDPer1M < 0 {
		return core.AccountGroup{}, fmt.Errorf("group billing values must be zero or greater")
	}
	var normalizedTimedMultipliers []core.AccountGroupTimedMultiplier
	timedMultipliersProvided := len(timedMultipliers) > 0
	if timedMultipliersProvided {
		var err error
		normalizedTimedMultipliers, err = normalizeAccountGroupTimedMultipliers(timedMultipliers[0])
		if err != nil {
			return core.AccountGroup{}, err
		}
	}
	if isDefaultAccountGroupID(id) {
		target := s.defaultAccountGroupSettings()
		target.BillingMultiplierBps = multiplierBps
		target.PlanBillingMultiplierBps = planMultiplierBps
		target.InputPriceNanoUSDPer1M = inputPriceNanoUSDPer1M
		target.CachedInputPriceNanoUSDPer1M = cachedInputPriceNanoUSDPer1M
		target.OutputPriceNanoUSDPer1M = outputPriceNanoUSDPer1M
		if timedMultipliersProvided {
			target.TimedMultipliers = normalizedTimedMultipliers
		}
		if err := s.repo.UpsertAccountGroup(target); err != nil {
			return core.AccountGroup{}, err
		}
		return target, nil
	}
	groups := s.repo.ListAccountGroups()
	var target core.AccountGroup
	found := false
	for _, group := range groups {
		if group.ID == id {
			target = group
			found = true
			break
		}
	}
	if !found {
		return core.AccountGroup{}, storage.ErrNotFound
	}
	target.BillingMultiplierBps = multiplierBps
	target.PlanBillingMultiplierBps = planMultiplierBps
	target.InputPriceNanoUSDPer1M = inputPriceNanoUSDPer1M
	target.CachedInputPriceNanoUSDPer1M = cachedInputPriceNanoUSDPer1M
	target.OutputPriceNanoUSDPer1M = outputPriceNanoUSDPer1M
	if timedMultipliersProvided {
		target.TimedMultipliers = normalizedTimedMultipliers
	}
	if err := s.repo.UpsertAccountGroup(target); err != nil {
		return core.AccountGroup{}, err
	}
	return target, nil
}

func (s *Service) UpdateAccountGroupBillingFull(id string, multiplierBps, planMultiplierBps, inputPriceNanoUSDPer1M, cachedInputPriceNanoUSDPer1M, cacheWritePriceNanoUSDPer1M, cacheWrite5mPriceNanoUSDPer1M, cacheWrite1hPriceNanoUSDPer1M, outputPriceNanoUSDPer1M, imageOutputPriceNanoUSDPer1M int64, timedMultipliers []core.AccountGroupTimedMultiplier, planBillingEnabled ...bool) (core.AccountGroup, error) {
	if multiplierBps < 0 || planMultiplierBps < 0 || inputPriceNanoUSDPer1M < 0 || cachedInputPriceNanoUSDPer1M < 0 || cacheWritePriceNanoUSDPer1M < 0 || cacheWrite5mPriceNanoUSDPer1M < 0 || cacheWrite1hPriceNanoUSDPer1M < 0 || outputPriceNanoUSDPer1M < 0 || imageOutputPriceNanoUSDPer1M < 0 {
		return core.AccountGroup{}, fmt.Errorf("group billing values must be zero or greater")
	}
	normalizedTimedMultipliers, err := normalizeAccountGroupTimedMultipliers(timedMultipliers)
	if err != nil {
		return core.AccountGroup{}, err
	}
	planEnabledProvided := len(planBillingEnabled) > 0
	planEnabled := true
	if planEnabledProvided {
		planEnabled = planBillingEnabled[0]
	}
	if isDefaultAccountGroupID(id) {
		target := s.defaultAccountGroupSettings()
		target.BillingMultiplierBps = multiplierBps
		target.PlanBillingMultiplierBps = planMultiplierBps
		if planEnabledProvided {
			target.PlanBillingEnabled = boolPtr(planEnabled)
		}
		target.InputPriceNanoUSDPer1M = inputPriceNanoUSDPer1M
		target.CachedInputPriceNanoUSDPer1M = cachedInputPriceNanoUSDPer1M
		target.CacheWritePriceNanoUSDPer1M = cacheWritePriceNanoUSDPer1M
		target.CacheWrite5mPriceNanoUSDPer1M = cacheWrite5mPriceNanoUSDPer1M
		target.CacheWrite1hPriceNanoUSDPer1M = cacheWrite1hPriceNanoUSDPer1M
		target.OutputPriceNanoUSDPer1M = outputPriceNanoUSDPer1M
		target.ImageOutputPriceNanoUSDPer1M = imageOutputPriceNanoUSDPer1M
		target.TimedMultipliers = normalizedTimedMultipliers
		if err := s.repo.UpsertAccountGroup(target); err != nil {
			return core.AccountGroup{}, err
		}
		return target, nil
	}
	groups := s.repo.ListAccountGroups()
	var target core.AccountGroup
	found := false
	for _, group := range groups {
		if group.ID == id {
			target = group
			found = true
			break
		}
	}
	if !found {
		return core.AccountGroup{}, storage.ErrNotFound
	}
	target.BillingMultiplierBps = multiplierBps
	target.PlanBillingMultiplierBps = planMultiplierBps
	if planEnabledProvided {
		target.PlanBillingEnabled = boolPtr(planEnabled)
	}
	target.InputPriceNanoUSDPer1M = inputPriceNanoUSDPer1M
	target.CachedInputPriceNanoUSDPer1M = cachedInputPriceNanoUSDPer1M
	target.CacheWritePriceNanoUSDPer1M = cacheWritePriceNanoUSDPer1M
	target.CacheWrite5mPriceNanoUSDPer1M = cacheWrite5mPriceNanoUSDPer1M
	target.CacheWrite1hPriceNanoUSDPer1M = cacheWrite1hPriceNanoUSDPer1M
	target.OutputPriceNanoUSDPer1M = outputPriceNanoUSDPer1M
	target.ImageOutputPriceNanoUSDPer1M = imageOutputPriceNanoUSDPer1M
	target.TimedMultipliers = normalizedTimedMultipliers
	if err := s.repo.UpsertAccountGroup(target); err != nil {
		return core.AccountGroup{}, err
	}
	return target, nil
}

func (s *Service) TestProxy(ctx context.Context, proxyURL string) (providers.ProxyTestResult, error) {
	if err := validateProxyURL(proxyURL); err != nil {
		return providers.ProxyTestResult{}, err
	}
	return providers.TestProxy(ctx, normalizeProxyURL(proxyURL))
}

func (s *Service) DeleteAccountGroup(id string) (core.AccountGroup, error) {
	if isDefaultAccountGroupID(id) {
		return core.AccountGroup{}, fmt.Errorf("default account group cannot be deleted")
	}
	groups := s.repo.ListAccountGroups()
	var target core.AccountGroup
	found := false
	for _, group := range groups {
		if group.ID == id {
			target = group
			found = true
			break
		}
	}
	if !found {
		return core.AccountGroup{}, storage.ErrNotFound
	}
	movedClients := make([]core.APIClient, 0)
	notifyUserIDs := make(map[string]struct{})
	for _, client := range s.repo.ListClients() {
		if strings.EqualFold(normalizeAccountGroup(client.AccountGroup), target.Name) {
			fullClient, err := s.repo.GetClient(client.ID)
			if err != nil {
				return core.AccountGroup{}, err
			}
			client = fullClient
			client.AccountGroup = core.DefaultAccountGroupName
			if err := s.repo.UpsertClient(client); err != nil {
				return core.AccountGroup{}, err
			}
			movedClients = append(movedClients, client)
			if ownerID := strings.TrimSpace(client.OwnerUserID); ownerID != "" {
				notifyUserIDs[ownerID] = struct{}{}
			}
		}
	}
	for _, account := range s.repo.ListAccounts() {
		if !strings.EqualFold(normalizeAccountGroup(account.Group), target.Name) {
			continue
		}
		account.Group = core.DefaultAccountGroupName
		if err := s.repo.UpsertAccount(account); err != nil {
			return core.AccountGroup{}, err
		}
	}
	if err := s.repo.DeleteAccountGroup(id); err != nil {
		return core.AccountGroup{}, err
	}
	if len(notifyUserIDs) > 0 {
		_ = s.createAccountGroupDeletedNotice(target, movedClients, notifyUserIDs)
	}
	return target, nil
}

func (s *Service) createAccountGroupDeletedNotice(group core.AccountGroup, movedClients []core.APIClient, userIDs map[string]struct{}) error {
	targetUserIDs := make([]string, 0, len(userIDs))
	for userID := range userIDs {
		targetUserIDs = append(targetUserIDs, userID)
	}
	slices.Sort(targetUserIDs)
	clientNames := make([]string, 0, len(movedClients))
	for _, client := range movedClients {
		if strings.TrimSpace(client.OwnerUserID) == "" {
			continue
		}
		name := strings.TrimSpace(client.Name)
		if name == "" {
			name = strings.TrimSpace(client.ID)
		}
		if name != "" {
			clientNames = append(clientNames, name)
		}
	}
	slices.Sort(clientNames)
	body := fmt.Sprintf("账号组 %q 已被管理员删除，相关 API 密钥已自动迁移到默认账号组。", group.Name)
	if len(clientNames) > 0 {
		body += " 受影响的 API 密钥：" + strings.Join(clientNames, "、") + "。"
	}
	now := time.Now().UTC()
	for range 3 {
		message := core.SiteMessage{
			ID:            generateSiteMessageID(),
			Title:         "账号组已删除",
			Body:          body,
			CreatedBy:     "system",
			Enabled:       true,
			TargetUserIDs: targetUserIDs,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		if err := s.repo.CreateSiteMessage(message); err != nil {
			if errors.Is(err, storage.ErrBillingRequestConflict) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("message id conflict")
}

func (s *Service) defaultAccountGroupSettings() core.AccountGroup {
	for _, group := range s.repo.ListAccountGroups() {
		if isDefaultAccountGroupID(group.ID) {
			group.ID = core.DefaultAccountGroupID
			group.Name = core.DefaultAccountGroupName
			return core.NormalizeAccountGroupBilling(group)
		}
	}
	return core.AccountGroup{
		ID:                       core.DefaultAccountGroupID,
		Name:                     core.DefaultAccountGroupName,
		BillingMultiplierBps:     core.AccountGroupDefaultMultiplierBps,
		PlanBillingMultiplierBps: core.AccountGroupDefaultMultiplierBps,
		ShowInClientEditor:       nil,
		PlanBillingEnabled:       boolPtr(true),
	}
}

func (s *Service) accountGroupByID(id string, fallback core.AccountGroup) (core.AccountGroup, error) {
	for _, group := range s.repo.ListAccountGroups() {
		if group.ID == id {
			return core.NormalizeAccountGroupBilling(group), nil
		}
	}
	if id == fallback.ID {
		return core.NormalizeAccountGroupBilling(fallback), nil
	}
	return core.AccountGroup{}, storage.ErrNotFound
}

func (s *Service) accountGroupByName(name string) (core.AccountGroup, bool) {
	name = normalizeAccountGroup(name)
	for _, group := range accountGroupsWithDefault(s.repo.ListAccountGroups()) {
		if strings.EqualFold(normalizeAccountGroup(group.Name), name) {
			return core.NormalizeAccountGroupBilling(group), true
		}
	}
	return core.AccountGroup{}, false
}

func (s *Service) ensureAccountGroupAllowsProvider(groupName string, provider core.ProviderKind) error {
	group, ok := s.accountGroupByName(groupName)
	if !ok {
		return fmt.Errorf("account group %q does not exist", normalizeAccountGroup(groupName))
	}
	if core.AccountGroupTypeAllowsProvider(group, provider) {
		return nil
	}
	return fmt.Errorf("account group %q does not allow %s accounts", group.Name, providerLabel(provider))
}

func (s *Service) ensureAccountGroupAllowsBillingSource(groupName, billingSource string) error {
	if core.NormalizeClientBillingSource(billingSource) != core.ClientBillingSourcePlan {
		return nil
	}
	group, ok := s.accountGroupByName(groupName)
	if !ok {
		return fmt.Errorf("account group %q does not exist", normalizeAccountGroup(groupName))
	}
	if core.AccountGroupPlanBillingEnabled(group) {
		return nil
	}
	return fmt.Errorf("account group %q does not allow plan billing", group.Name)
}

func (s *Service) ensureExistingGroupAccountsAllowed(groupName, groupType string) error {
	group := core.AccountGroup{Name: normalizeAccountGroup(groupName), Type: groupType}
	for _, account := range s.repo.ListAccounts() {
		if !strings.EqualFold(normalizeStoredAccountGroup(account.Group), normalizeStoredAccountGroup(group.Name)) {
			continue
		}
		if !core.AccountGroupTypeAllowsProvider(group, account.Provider) {
			return fmt.Errorf("account group %q contains %s account %q", group.Name, providerLabel(account.Provider), account.Label)
		}
	}
	return nil
}

const defaultAccountGroupID = core.DefaultAccountGroupID

func isDefaultAccountGroupID(id string) bool {
	return strings.EqualFold(strings.TrimSpace(id), defaultAccountGroupID)
}

func (s *Service) ensureDefaultAccountGroup() (core.AccountGroup, error) {
	group := s.defaultAccountGroupSettings()
	if err := s.repo.UpsertAccountGroup(group); err != nil {
		return core.AccountGroup{}, err
	}
	return group, nil
}

func accountGroupsWithDefault(groups []core.AccountGroup) []core.AccountGroup {
	out := make([]core.AccountGroup, 0, len(groups)+1)
	foundDefault := false
	for _, group := range groups {
		if isDefaultAccountGroupID(group.ID) {
			group.ID = core.DefaultAccountGroupID
			group.Name = core.DefaultAccountGroupName
			out = append(out, core.NormalizeAccountGroupBilling(group))
			foundDefault = true
			continue
		}
		if strings.EqualFold(normalizeAccountGroup(group.Name), core.DefaultAccountGroupName) {
			continue
		}
		out = append(out, core.NormalizeAccountGroupBilling(group))
	}
	if !foundDefault {
		out = append(out, core.AccountGroup{
			ID:                       core.DefaultAccountGroupID,
			Name:                     core.DefaultAccountGroupName,
			BillingMultiplierBps:     core.AccountGroupDefaultMultiplierBps,
			PlanBillingMultiplierBps: core.AccountGroupDefaultMultiplierBps,
			PlanBillingEnabled:       boolPtr(true),
		})
	}
	slices.SortStableFunc(out, func(a, b core.AccountGroup) int {
		if isDefaultAccountGroupID(a.ID) {
			return -1
		}
		if isDefaultAccountGroupID(b.ID) {
			return 1
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out
}

func (s *Service) NewClientEditor() ClientEditor {
	return ClientEditor{
		Client: core.APIClient{
			Enabled:       true,
			RoutePolicy:   core.DefaultRoutePolicy(),
			AccountGroup:  core.DefaultAccountGroupName,
			BillingSource: core.ClientBillingSourceCash,
		},
		AvailableAccountGroups: s.clientEditorAccountGroups(""),
		SelectedAccountGroup:   core.DefaultAccountGroupName,
		SelectedBillingSource:  core.ClientBillingSourceCash,
	}
}

func (s *Service) ListAccounts() []core.Account {
	return core.NormalizeAccountsRuntimeState(s.repo.ListAccounts(), time.Now().UTC())
}

func (s *Service) ListAccountGroups() []core.AccountGroup {
	return accountGroupsWithDefault(s.repo.ListAccountGroups())
}

func (s *Service) NewClientEditorForUser(user core.User) ClientEditor {
	editor := s.NewClientEditor()
	editor.Client.OwnerUserID = user.ID
	editor.AvailableAccountGroups = s.clientEditorAccountGroupsForUser("", user)
	if s.userHasActivePlan(user.ID) {
		editor.Client.BillingSource = core.ClientBillingSourcePlan
		editor.SelectedBillingSource = core.ClientBillingSourcePlan
		editor.HasActivePlan = true
	}
	return editor
}

func (s *Service) GetClientEditor(id string) (ClientEditor, error) {
	client, err := s.repo.GetClient(id)
	if err != nil {
		return ClientEditor{}, err
	}
	return ClientEditor{
		Client:                 client,
		AvailableAccountGroups: s.clientEditorAccountGroups(client.AccountGroup),
		SelectedAccountGroup:   normalizeAccountGroup(client.AccountGroup),
		SelectedBillingSource:  core.NormalizeClientBillingSource(client.BillingSource),
		HasActivePlan:          s.userHasActivePlan(client.OwnerUserID),
	}, nil
}

func (s *Service) clientEditorAccountGroups(current string) []core.AccountGroup {
	current = normalizeAccountGroup(current)
	out := make([]core.AccountGroup, 0)
	for _, group := range accountGroupsWithDefault(s.repo.ListAccountGroups()) {
		if !core.AccountGroupVisibleInClientEditor(group) && !strings.EqualFold(normalizeAccountGroup(group.Name), current) {
			continue
		}
		out = append(out, group)
	}
	return availableAccountGroupsWithCurrent(out, current)
}

func (s *Service) clientEditorAccountGroupsForUser(current string, user core.User) []core.AccountGroup {
	out := make([]core.AccountGroup, 0)
	for _, group := range accountGroupsWithDefault(s.repo.ListAccountGroups()) {
		if !core.AccountGroupVisibleInClientEditorForUser(group, user.ID) {
			continue
		}
		out = append(out, group)
	}
	return sortAccountGroupsByName(out)
}

func (s *Service) visibleNamedAccountGroups(current string) []core.AccountGroup {
	current = normalizeAccountGroup(current)
	groups := accountGroupsWithDefault(s.repo.ListAccountGroups())
	out := make([]core.AccountGroup, 0, len(groups))
	for _, group := range groups {
		if !core.AccountGroupVisibleInClientEditor(group) && !strings.EqualFold(normalizeAccountGroup(group.Name), current) {
			continue
		}
		out = append(out, group)
	}
	return availableAccountGroupsWithCurrent(out, current)
}

func (s *Service) visibleNamedAccountGroupsForProvider(current string, provider core.ProviderKind) []core.AccountGroup {
	current = normalizeAccountGroup(current)
	groups := accountGroupsWithDefault(s.repo.ListAccountGroups())
	out := make([]core.AccountGroup, 0, len(groups))
	for _, group := range groups {
		if !core.AccountGroupVisibleInClientEditor(group) && !strings.EqualFold(normalizeAccountGroup(group.Name), current) {
			continue
		}
		if !core.AccountGroupTypeAllowsProvider(group, provider) && !strings.EqualFold(normalizeAccountGroup(group.Name), current) {
			continue
		}
		out = append(out, group)
	}
	return availableAccountGroupsWithCurrent(out, current)
}

func (s *Service) GetClientEditorForUser(id string, user core.User) (ClientEditor, error) {
	client, err := s.repo.GetClient(id)
	if err != nil {
		return ClientEditor{}, err
	}
	editor := ClientEditor{
		Client:                 client,
		AvailableAccountGroups: s.clientEditorAccountGroupsForUser(client.AccountGroup, user),
		SelectedAccountGroup:   normalizeAccountGroup(client.AccountGroup),
		SelectedBillingSource:  core.NormalizeClientBillingSource(client.BillingSource),
		HasActivePlan:          s.userHasActivePlan(client.OwnerUserID),
	}
	if !s.CanUserManageClient(user, editor.Client) {
		return ClientEditor{}, &AccessError{StatusCode: httpStatusForbidden, Code: ErrorCodeForbidden, Message: "client access denied"}
	}
	return editor, nil
}

func (s *Service) userHasActivePlan(userID string) bool {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false
	}
	_, err := s.repo.GetActiveUserPlanEntitlement(userID)
	return err == nil
}

func (s *Service) GetClient(id string) (core.APIClient, error) {
	return s.repo.GetClient(id)
}

func (s *Service) ListClients() []core.APIClient {
	return s.repo.ListClients()
}

func (s *Service) GetClientSpend(clientID string) (core.ClientSpend, error) {
	return s.repo.GetClientSpend(strings.TrimSpace(clientID))
}

func (s *Service) GetClientActualSpend(clientID string) (core.ClientSpend, error) {
	return s.repo.GetClientActualSpend(strings.TrimSpace(clientID))
}

type ClientQuota struct {
	ClientID          string `json:"client_id"`
	ClientName        string `json:"client_name"`
	UserID            string `json:"user_id,omitempty"`
	BalanceNanoUSD    int64  `json:"balance_nano_usd"`
	PlanQuotaNanoUSD  int64  `json:"plan_quota_nano_usd"`
	PlanName          string `json:"plan_name,omitempty"`
	SpendLimitNanoUSD int64  `json:"spend_limit_nano_usd"`
	SpendUsedNanoUSD  int64  `json:"spend_used_nano_usd"`
	RemainingNanoUSD  int64  `json:"remaining_nano_usd"`
	UpdatedAt         string `json:"updated_at,omitempty"`
}

func (s *Service) GetClientQuota(client core.APIClient) (ClientQuota, error) {
	clientID := strings.TrimSpace(client.ID)
	spend, err := s.repo.GetClientActualSpend(clientID)
	if err != nil {
		spend = core.ClientSpend{
			ClientID:          clientID,
			SpendLimitNanoUSD: client.SpendLimitNanoUSD,
		}
	}

	balance := int64(0)
	planQuota := int64(0)
	planName := ""
	hasOwner := false
	if ownerID := strings.TrimSpace(client.OwnerUserID); ownerID != "" {
		hasOwner = true
		owner, err := s.repo.GetUser(ownerID)
		if err != nil {
			return ClientQuota{}, err
		}
		balance = owner.BalanceNanoUSD
		if entitlement, err := s.repo.GetActiveUserPlanEntitlement(ownerID); err == nil {
			planQuota = entitlement.CurrentQuotaNanoUSD
			planName = entitlement.PlanName
		} else if err != storage.ErrNotFound {
			return ClientQuota{}, err
		}
	}

	cashAvailable := balance
	if cashAvailable < 0 {
		cashAvailable = 0
	}
	remaining := cashAvailable
	if core.NormalizeClientBillingSource(client.BillingSource) == core.ClientBillingSourcePlan {
		remaining = planQuota
	}
	if spend.SpendLimitNanoUSD > 0 {
		limitRemaining := spend.SpendLimitNanoUSD - spend.SpendUsedNanoUSD
		if limitRemaining < 0 {
			limitRemaining = 0
		}
		if (!hasOwner && remaining == 0) || limitRemaining < remaining {
			remaining = limitRemaining
		}
	}
	if remaining < 0 {
		remaining = 0
	}

	updatedAt := ""
	if !spend.UpdatedAt.IsZero() {
		updatedAt = spend.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return ClientQuota{
		ClientID:          clientID,
		ClientName:        strings.TrimSpace(client.Name),
		UserID:            strings.TrimSpace(client.OwnerUserID),
		BalanceNanoUSD:    balance,
		PlanQuotaNanoUSD:  planQuota,
		PlanName:          planName,
		SpendLimitNanoUSD: spend.SpendLimitNanoUSD,
		SpendUsedNanoUSD:  spend.SpendUsedNanoUSD,
		RemainingNanoUSD:  remaining,
		UpdatedAt:         updatedAt,
	}, nil
}

func (s *Service) ClientsForUser(user core.User) []core.APIClient {
	return s.repo.ListClientsByOwner(user.ID)
}

func (s *Service) ClientsForUserPage(user core.User, offset, limit int) ([]core.APIClient, int) {
	ownerID := strings.TrimSpace(user.ID)
	if ownerID == "" {
		return nil, 0
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = dashboardClientPreviewLimit
	}
	if pager, ok := s.repo.(clientSummaryOwnerPager); ok {
		clients, total := pager.ListClientSummariesByOwnerPage(ownerID, offset, limit)
		return dashboardClientSummaries(clients), total
	}
	clients := s.repo.ListClientsByOwner(ownerID)
	total := len(clients)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return dashboardClientSummaries(clients[offset:end]), total
}

func (s *Service) DashboardForUser(ctx context.Context, user core.User) Dashboard {
	clients, totalClients := s.ClientsForUserPage(user, 0, dashboardClientPreviewLimit)
	if !user.IsAdmin() {
		return Dashboard{
			Clients:      clients,
			Users:        []core.User{user},
			TotalUsers:   1,
			TotalClients: totalClients,
		}
	}
	dashboard := s.Dashboard(ctx)
	dashboard.Clients = clients
	dashboard.TotalClients = totalClients
	return dashboard
}

func (s *Service) CanUserManageClient(user core.User, client core.APIClient) bool {
	return user.ID != "" && client.OwnerUserID == user.ID
}

func (s *Service) CreateClientInAccountGroupForUser(user core.User, name string, spendLimitNanoUSD int64, enabled bool, accountGroup string, defaultProvider core.ProviderKind, fallback []core.ProviderKind) (core.APIClient, error) {
	return s.CreateClientInBillingSourceForUser(user, name, spendLimitNanoUSD, enabled, accountGroup, core.ClientBillingSourceCash, defaultProvider, fallback)
}

func (s *Service) CreateClientInBillingSourceForUser(user core.User, name string, spendLimitNanoUSD int64, enabled bool, accountGroup, billingSource string, defaultProvider core.ProviderKind, fallback []core.ProviderKind) (core.APIClient, error) {
	ownerUserID := strings.TrimSpace(user.ID)
	if ownerUserID == "" {
		return core.APIClient{}, fmt.Errorf("user is required")
	}
	billingSource = core.NormalizeClientBillingSource(billingSource)
	if billingSource == core.ClientBillingSourcePlan && !s.userHasActivePlan(ownerUserID) {
		return core.APIClient{}, fmt.Errorf("active plan is required for plan billing")
	}
	normalizedGroup, err := s.normalizeClientAccountGroupForUser(accountGroup, "", user)
	if err != nil {
		return core.APIClient{}, err
	}
	if err := s.ensureAccountGroupAllowsBillingSource(normalizedGroup, billingSource); err != nil {
		return core.APIClient{}, err
	}
	return s.createClientWithOwner(ownerUserID, name, "", spendLimitNanoUSD, enabled, defaultProvider, fallback, normalizedGroup, billingSource)
}

func (s *Service) createClientWithOwner(ownerUserID, name, apiKey string, spendLimitNanoUSD int64, enabled bool, defaultProvider core.ProviderKind, fallback []core.ProviderKind, accountGroup, billingSource string) (core.APIClient, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return core.APIClient{}, fmt.Errorf("client name is required")
	}
	if spendLimitNanoUSD < 0 {
		return core.APIClient{}, fmt.Errorf("spend limit must be zero or greater")
	}

	key := strings.TrimSpace(apiKey)
	if key == "" {
		var err error
		key, err = generateClientKey()
		if err != nil {
			return core.APIClient{}, err
		}
	}
	if err := s.ensureClientKeyUnique("", key); err != nil {
		return core.APIClient{}, err
	}
	accountGroup = normalizeAccountGroup(accountGroup)
	if err := s.ensureAccountGroupExists(accountGroup); err != nil {
		return core.APIClient{}, err
	}
	if !s.accountGroupHasAccounts([]string{accountGroup}) {
		return core.APIClient{}, fmt.Errorf("account group %q has no accounts", accountGroup)
	}
	billingSource = core.NormalizeClientBillingSource(billingSource)

	client := core.APIClient{
		ID:                fmt.Sprintf("client_%d", time.Now().UnixNano()),
		Name:              name,
		APIKey:            key,
		OwnerUserID:       strings.TrimSpace(ownerUserID),
		Enabled:           enabled,
		SpendLimitNanoUSD: spendLimitNanoUSD,
		RoutePolicy:       normalizeRoutePolicy(defaultProvider, fallback),
		AccountGroup:      accountGroup,
		BillingSource:     billingSource,
	}
	if err := s.repo.UpsertClient(client); err != nil {
		return core.APIClient{}, err
	}
	client, err := s.repo.GetClient(client.ID)
	if err != nil {
		return core.APIClient{}, err
	}
	return client, nil
}

func (s *Service) UpdateClientAccountGroupForUser(user core.User, id, name string, spendLimitNanoUSD int64, enabled bool, accountGroup string, defaultProvider core.ProviderKind, fallback []core.ProviderKind) error {
	client, err := s.repo.GetClient(strings.TrimSpace(id))
	if err != nil {
		return err
	}
	return s.UpdateClientBillingSourceForUser(user, id, name, spendLimitNanoUSD, enabled, accountGroup, client.BillingSource, defaultProvider, fallback)
}

func (s *Service) UpdateClientBillingSourceForUser(user core.User, id, name string, spendLimitNanoUSD int64, enabled bool, accountGroup, billingSource string, defaultProvider core.ProviderKind, fallback []core.ProviderKind) error {
	client, err := s.repo.GetClient(strings.TrimSpace(id))
	if err != nil {
		return err
	}
	if !s.CanUserManageClient(user, client) {
		return &AccessError{StatusCode: httpStatusForbidden, Code: ErrorCodeForbidden, Message: "client access denied"}
	}
	billingSource = core.NormalizeClientBillingSource(billingSource)
	if billingSource == core.ClientBillingSourcePlan && !s.userHasActivePlan(client.OwnerUserID) {
		return fmt.Errorf("active plan is required for plan billing")
	}
	normalizedGroup, err := s.normalizeClientAccountGroupForUser(accountGroup, client.AccountGroup, user)
	if err != nil {
		return err
	}
	if err := s.ensureAccountGroupAllowsBillingSource(normalizedGroup, billingSource); err != nil {
		return err
	}
	if !user.IsAdmin() {
		return s.updateClientWithOwner(id, client.OwnerUserID, true, name, "", spendLimitNanoUSD, enabled, defaultProvider, fallback, normalizedGroup, billingSource)
	}
	return s.updateClientWithOwner(id, "", false, name, "", spendLimitNanoUSD, enabled, defaultProvider, fallback, normalizedGroup, billingSource)
}

func (s *Service) updateClientWithOwner(id, ownerUserID string, updateOwner bool, name, apiKey string, spendLimitNanoUSD int64, enabled bool, defaultProvider core.ProviderKind, fallback []core.ProviderKind, accountGroup, billingSource string) error {
	client, err := s.repo.GetClient(id)
	if err != nil {
		return err
	}

	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("client name is required")
	}
	if spendLimitNanoUSD < 0 {
		return fmt.Errorf("spend limit must be zero or greater")
	}

	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		apiKey = client.APIKey
	}
	if err := s.ensureClientKeyUnique(id, apiKey); err != nil {
		return err
	}
	accountGroup = normalizeAccountGroup(accountGroup)
	if err := s.ensureAccountGroupExists(accountGroup); err != nil {
		return err
	}
	if !s.accountGroupHasAccounts([]string{accountGroup}) {
		return fmt.Errorf("account group %q has no accounts", accountGroup)
	}
	billingSource = core.NormalizeClientBillingSource(billingSource)

	client.Name = name
	client.APIKey = apiKey
	if updateOwner {
		client.OwnerUserID = strings.TrimSpace(ownerUserID)
	}
	client.Enabled = enabled
	client.SpendLimitNanoUSD = spendLimitNanoUSD
	client.RoutePolicy = mergeRoutePolicy(client.RoutePolicy, normalizeRoutePolicy(defaultProvider, fallback))
	client.AccountGroup = accountGroup
	client.BillingSource = billingSource
	if err := s.repo.UpsertClient(client); err != nil {
		return err
	}
	return nil
}

func (s *Service) ToggleClient(id string) error {
	client, err := s.repo.GetClient(id)
	if err != nil {
		return err
	}
	client.Enabled = !client.Enabled
	if err := s.repo.UpsertClient(client); err != nil {
		return err
	}
	return nil
}

func (s *Service) ToggleClientForUser(user core.User, id string) error {
	client, err := s.repo.GetClient(id)
	if err != nil {
		return err
	}
	if !s.CanUserManageClient(user, client) {
		return &AccessError{StatusCode: httpStatusForbidden, Code: ErrorCodeForbidden, Message: "client access denied"}
	}
	client.Enabled = !client.Enabled
	if err := s.repo.UpsertClient(client); err != nil {
		return err
	}
	return nil
}

func (s *Service) DeleteClient(id string) (core.APIClient, error) {
	client, err := s.repo.GetClient(id)
	if err != nil {
		return core.APIClient{}, err
	}
	if err := s.repo.DeleteClient(id); err != nil {
		return core.APIClient{}, err
	}
	return client, nil
}

func (s *Service) DeleteClientForUser(user core.User, id string) (core.APIClient, error) {
	client, err := s.repo.GetClient(id)
	if err != nil {
		return core.APIClient{}, err
	}
	if !s.CanUserManageClient(user, client) {
		return core.APIClient{}, &AccessError{StatusCode: httpStatusForbidden, Code: ErrorCodeForbidden, Message: "client access denied"}
	}
	if err := s.repo.DeleteClient(id); err != nil {
		return core.APIClient{}, err
	}
	return client, nil
}

func (s *Service) AuthorizeProtocolKey(token string) (core.APIClient, error) {
	client, err := s.AuthorizeProtocolKeyPointer(token)
	if err != nil {
		return core.APIClient{}, err
	}
	return *client, nil
}

func (s *Service) AuthorizeProtocolKeyPointer(token string) (*core.APIClient, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, &AccessError{
			StatusCode: httpStatusUnauthorized,
			Code:       ErrorCodeAuthError,
			Message:    "missing api key",
		}
	}

	client, err := s.clientByAPIKey(token)
	if err != nil {
		var accessErr *AccessError
		if errors.As(err, &accessErr) {
			return nil, accessErr
		}
		return nil, &AccessError{
			StatusCode: httpStatusUnauthorized,
			Code:       ErrorCodeAuthError,
			Message:    "missing or invalid api key",
		}
	}
	if !client.Enabled {
		return nil, &AccessError{
			StatusCode: httpStatusForbidden,
			Code:       ErrorCodeAuthError,
			Message:    "api client is disabled",
		}
	}
	s.recordProtocolClientUse(client)
	return client, nil
}

func (s *Service) recordProtocolClientUse(client *core.APIClient) {
	if s == nil || s.repo == nil || client == nil {
		return
	}
	ownerID := strings.TrimSpace(client.OwnerUserID)
	if ownerID == "" {
		return
	}
	toucher, ok := s.repo.(userLastUsedToucher)
	if !ok {
		return
	}
	_ = toucher.TouchUserLastUsedAt(ownerID, time.Now().UTC())
}

func (s *Service) clientByAPIKey(apiKey string) (*core.APIClient, error) {
	clientRev, userRev := clientAuthRevision(s.repo)
	if clientRev == 0 {
		client, err := s.repo.FindClientByAPIKey(apiKey)
		if err != nil {
			return nil, err
		}
		client = authCacheClient(client)
		return s.validateLoadedAuthEntry(clientAuthEntry{client: client})
	}
	if cache, ok := s.loadClientAuthCache(); ok && cache.clientRevision == clientRev && cache.userRevision == userRev {
		if entry, ok := cache.byKey[strings.TrimSpace(apiKey)]; ok {
			return validateAuthCacheEntry(entry)
		}
	}
	client, err := s.repo.FindClientByAPIKey(apiKey)
	if err != nil {
		return nil, err
	}
	client = authCacheClient(client)
	entry := clientAuthEntry{client: client}
	entry, err = s.loadClientAuthEntry(entry)
	if err != nil {
		return nil, err
	}
	validated, err := validateAuthCacheEntry(entry)
	if err != nil {
		return nil, err
	}

	s.clientAuthMu.Lock()
	defer s.clientAuthMu.Unlock()
	cache, ok := s.loadClientAuthCache()
	if !ok || cache.clientRevision != clientRev || cache.userRevision != userRev {
		cache = clientAuthCache{
			clientRevision: clientRev,
			userRevision:   userRev,
			byKey:          map[string]clientAuthEntry{},
		}
	}
	cache.byKey[strings.TrimSpace(apiKey)] = entry
	s.clientAuth.Store(cache)
	return validated, nil
}

func clientAuthRevision(repo storage.Repository) (uint64, uint64) {
	if revisioned, ok := repo.(clientRevisionProvider); ok {
		clientRev := revisioned.ClientRevision()
		userRev := uint64(0)
		if userRevisioned, ok := repo.(userRevisionProvider); ok {
			userRev = userRevisioned.UserRevision()
		} else if configRevisioned, ok := repo.(configRevisionProvider); ok {
			userRev = configRevisioned.ConfigRevision()
		}
		return clientRev, userRev
	}
	if revisioned, ok := repo.(configRevisionProvider); ok {
		revision := revisioned.ConfigRevision()
		return revision, revision
	}
	return 0, 0
}

func (s *Service) loadClientAuthCache() (clientAuthCache, bool) {
	value := s.clientAuth.Load()
	if value == nil {
		return clientAuthCache{}, false
	}
	cache, ok := value.(clientAuthCache)
	return cache, ok
}

func (s *Service) loadClientAuthEntry(entry clientAuthEntry) (clientAuthEntry, error) {
	if ownerID := strings.TrimSpace(entry.client.OwnerUserID); ownerID != "" {
		owner, err := s.repo.GetUser(ownerID)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				entry.ownerExists = false
			} else {
				return clientAuthEntry{}, err
			}
		} else {
			entry.ownerExists = true
			entry.ownerEnabled = owner.Enabled
		}
	}
	return entry, nil
}

func (s *Service) validateLoadedAuthEntry(entry clientAuthEntry) (*core.APIClient, error) {
	entry, err := s.loadClientAuthEntry(entry)
	if err != nil {
		return nil, err
	}
	return validateAuthCacheEntry(entry)
}

func validateAuthCacheEntry(entry clientAuthEntry) (*core.APIClient, error) {
	if ownerID := strings.TrimSpace(entry.client.OwnerUserID); ownerID != "" {
		if !entry.ownerExists {
			return nil, &AccessError{
				StatusCode: httpStatusForbidden,
				Code:       ErrorCodeAuthError,
				Message:    "api client owner is unavailable",
			}
		}
		if !entry.ownerEnabled {
			return nil, &AccessError{
				StatusCode: httpStatusForbidden,
				Code:       ErrorCodeAuthError,
				Message:    "api client owner is disabled",
			}
		}
	}
	return &entry.client, nil
}

func authCacheClient(client core.APIClient) core.APIClient {
	client.RouteAffinityKey = ""
	return client
}

func (s *Service) AppendAdminAudit(actor, action, resourceType, resourceID, resourceName, status, message string) error {
	return s.repo.AppendAudit(core.AuditEvent{
		ID:           fmt.Sprintf("audit_admin_%d", time.Now().UnixNano()),
		Kind:         core.AuditKindAdmin,
		Actor:        strings.TrimSpace(actor),
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		ResourceName: resourceName,
		Status:       status,
		Message:      message,
		CreatedAt:    time.Now().UTC(),
	})
}

func (s *Service) AppendProtocolAudit(client core.APIClient, action, resourceType, resourceID, resourceName, status, message, requestBody string) error {
	return s.repo.AppendAudit(core.AuditEvent{
		ID:           fmt.Sprintf("audit_gateway_%d", time.Now().UnixNano()),
		Kind:         core.AuditKindGateway,
		Action:       strings.TrimSpace(action),
		ResourceType: strings.TrimSpace(resourceType),
		ResourceID:   strings.TrimSpace(resourceID),
		ResourceName: strings.TrimSpace(resourceName),
		ClientID:     client.ID,
		ClientName:   client.Name,
		Status:       strings.TrimSpace(status),
		Message:      strings.TrimSpace(message),
		RequestBody:  strings.TrimSpace(requestBody),
		CreatedAt:    time.Now().UTC(),
	})
}

type ProviderSummary struct {
	Kind                   core.ProviderKind `json:"kind"`
	Label                  string            `json:"label"`
	TotalAccounts          int               `json:"total_accounts"`
	AvailableAccounts      int               `json:"available_accounts"`
	ActiveAccounts         int               `json:"active_accounts"`
	CoolingAccounts        int               `json:"cooling_accounts"`
	BlockedAccounts        int               `json:"blocked_accounts"`
	ProviderBannedAccounts int               `json:"provider_banned_accounts"`
	ExpiredAccounts        int               `json:"expired_accounts"`
	RefreshingAccounts     int               `json:"refreshing_accounts"`
	LastUsedAt             *time.Time        `json:"last_used_at,omitempty"`
}

type Dashboard struct {
	Accounts               []core.Account
	AccountGroups          []AccountGroupSection
	AccountPools           AccountPoolViews
	AccountDailyCallCounts map[string]int
	Clients                []core.APIClient
	Users                  []core.User
	TotalUsers             int
	TotalClients           int
	Audit                  []core.AuditEvent
	GatewayAudit           []core.AuditEvent
	AdminAudit             []core.AuditEvent
	Providers              []ProviderSummary
	Stats                  DashboardStats
	SystemProxyURL         string
}

type DashboardStats struct {
	Status                string `json:"status"`
	Reason                string `json:"reason"`
	TotalAccounts         int    `json:"total_accounts"`
	AvailableAccounts     int    `json:"available_accounts"`
	CoolingCount          int    `json:"cooling_count"`
	FailureCount          int    `json:"failure_count"`
	ExpiringSoonCount     int    `json:"expiring_soon_count"`
	HealthyProviderCount  int    `json:"healthy_provider_count"`
	DegradedProviderCount int    `json:"degraded_provider_count"`
	DroppedAuditEvents    uint64 `json:"dropped_audit_events"`
}

type AccountGroupSection struct {
	Key                           string
	ID                            string
	Label                         string
	Type                          string
	Remark                        string
	ProxyURL                      string
	ShowInClientEditor            *bool
	VisibleUserIDs                []string
	BillingMultiplierBps          int64
	PlanBillingMultiplierBps      int64
	PlanBillingEnabled            *bool
	InputPriceNanoUSDPer1M        int64
	CachedInputPriceNanoUSDPer1M  int64
	CacheWritePriceNanoUSDPer1M   int64
	CacheWrite5mPriceNanoUSDPer1M int64
	CacheWrite1hPriceNanoUSDPer1M int64
	OutputPriceNanoUSDPer1M       int64
	ImageOutputPriceNanoUSDPer1M  int64
	TimedMultipliers              []core.AccountGroupTimedMultiplier
	Accounts                      []core.Account
}

type HealthReport struct {
	Status                string            `json:"status"`
	Reason                string            `json:"reason"`
	TotalAccounts         int               `json:"total_accounts"`
	AvailableAccounts     int               `json:"available_accounts"`
	ExpiringSoonCount     int               `json:"expiring_soon_count"`
	HealthyProviderCount  int               `json:"healthy_provider_count"`
	DegradedProviderCount int               `json:"degraded_provider_count"`
	DroppedAuditEvents    uint64            `json:"dropped_audit_events"`
	GeneratedAt           time.Time         `json:"generated_at"`
	Providers             []ProviderSummary `json:"providers"`
}

type AuditFilter struct {
	Kind     core.AuditKind
	Status   string
	Actor    string
	Resource string
	Page     int
	PageSize int
}

type AuditPage struct {
	Items    []core.AuditEvent
	Filter   AuditFilter
	Total    int
	Page     int
	PageSize int
	HasPrev  bool
	PrevPage int
	HasNext  bool
	NextPage int
}

type UsageLogFilter struct {
	UserID    string
	ClientID  string
	Model     string
	Status    core.BillingRequestStatus
	StartedAt time.Time
	EndedAt   time.Time
	Page      int
	PageSize  int
}

type UsageLogPage struct {
	Rows     []UsageLogRow
	Filter   UsageLogFilter
	Total    int
	Page     int
	PageSize int
	HasPrev  bool
	PrevPage int
	HasNext  bool
	NextPage int
}

type UsageCostChart struct {
	Points []UsageCostPoint
	Total  int64
	Max    int64
}

type UsageCostPoint struct {
	Hour    int
	Label   string
	NanoUSD int64
	Percent int
}

type UsageLogRow struct {
	Request             core.BillingReservation
	ClientName          string
	AccountGroup        string
	AccountLabel        string
	FailedAccountLabels []string
}

type ConnectStart struct {
	Provider            core.ProviderKind
	ProviderLabel       string
	Label               string
	Group               string
	ProxyURL            string
	AvailableGroups     []core.AccountGroup
	AccessToken         string
	RefreshToken        string
	SessionToken        string
	ExpiresAtText       string
	BaseURL             string
	Backup              bool
	Priority            int
	Weight              int
	AccountLoginMethod  string
	APIKeyQuotaProvider string
	CredentialMode      string
	TokenSource         string
	OAuthAccountID      string
	OAuthEmail          string
	CodexAuthPath       string
}

type OpenAIOAuthStart struct {
	DeviceCode      string
	UserCode        string
	VerificationURI string
	ExpiresAt       time.Time
	Interval        int64
	SuggestedLabel  string
}

type ClaudeOAuthStart struct {
	AuthorizationURL string
	ExpiresAt        time.Time
	SuggestedLabel   string
}

type ManualConnectInput struct {
	Provider            core.ProviderKind
	Label               string
	Group               string
	ProxyURL            string
	AccessToken         string
	RefreshToken        string
	SessionToken        string
	BaseURL             string
	ExpiresAt           *time.Time
	Backup              bool
	Priority            int
	Weight              int
	AccountLoginMethod  string
	APIKeyQuotaProvider string
	CredentialMode      string
	TokenSource         string
	OAuthAccountID      string
	OAuthEmail          string
	CodexAuthPath       string
}

type OAuthConnectResult struct {
	ImportID        string
	Label           string
	Email           string
	TargetAccountID string
}

type OpenAIOAuthConnectResult = OAuthConnectResult
type ClaudeOAuthConnectResult = OAuthConnectResult

type CodexOpenAIAuthUploadResult struct {
	Path     string
	Imported int
	Skipped  int
	Failed   int
	Items    []CodexOpenAIAuthUploadItem
}

type CodexOpenAIAuthUploadItem struct {
	Path      string
	Label     string
	Email     string
	AccountID string
	Imported  bool
	Skipped   bool
	Error     string
}

type openAIOAuthPending struct {
	UserCode        string
	TargetAccountID string
	ExpiresAt       time.Time
}

type claudeOAuthPending struct {
	RedirectURI     string
	CodeVerifier    string
	ExpectedState   string
	TargetAccountID string
	ExpiresAt       time.Time
}

type oauthConnectImport struct {
	Label               string
	AccessToken         string
	RefreshToken        string
	SessionToken        string
	BaseURL             string
	CredentialExpiresAt *time.Time
	Backup              bool
	Priority            int
	Weight              int
	CredentialMode      string
	TokenSource         string
	OAuthAccountID      string
	OAuthEmail          string
	CodexAuthPath       string
	ExpiresAt           time.Time
}

type AccountEditor struct {
	Account         core.Account
	BaseURL         string
	ProxyURL        string
	ExpiresAtText   string
	StatusOptions   []core.AccountStatus
	AvailableGroups []core.AccountGroup
}

type ClientEditor struct {
	Client                 core.APIClient
	AvailableAccountGroups []core.AccountGroup
	SelectedAccountGroup   string
	SelectedBillingSource  string
	HasActivePlan          bool
}

type AccessError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *AccessError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

const (
	httpStatusUnauthorized    = 401
	httpStatusForbidden       = 403
	httpStatusTooManyRequests = 429
)

func isEditableAccountStatus(status core.AccountStatus) bool {
	for _, item := range editableAccountStatuses {
		if item == status {
			return true
		}
	}
	return false
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return map[string]string{}
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func cloneTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copyValue := value.UTC()
	return &copyValue
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func formatOptionalTime(value *time.Time) string {
	if value == nil || value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func normalizeRoutePolicy(defaultProvider core.ProviderKind, fallback []core.ProviderKind) core.RoutePolicy {
	policy := core.DefaultRoutePolicy()
	if defaultProvider != "" {
		policy.DefaultProvider = defaultProvider
	}
	policy.FallbackProviders = filterFallbackProviders(policy.DefaultProvider, fallback)
	return policy
}

func mergeRoutePolicy(base, override core.RoutePolicy) core.RoutePolicy {
	policy := base
	if policy.DefaultProvider == "" {
		policy = core.DefaultRoutePolicy()
	}
	if override.DefaultProvider != "" {
		policy.DefaultProvider = override.DefaultProvider
	}
	policy.FallbackProviders = filterFallbackProviders(policy.DefaultProvider, override.FallbackProviders)
	if len(policy.Rules) == 0 {
		policy.Rules = append([]core.RouteRule(nil), core.DefaultRoutePolicy().Rules...)
	}
	return policy
}

func filterFallbackProviders(defaultProvider core.ProviderKind, providers []core.ProviderKind) []core.ProviderKind {
	filtered := make([]core.ProviderKind, 0, len(providers))
	seen := map[core.ProviderKind]bool{}
	for _, provider := range providers {
		if provider == "" || provider == defaultProvider || seen[provider] {
			continue
		}
		seen[provider] = true
		filtered = append(filtered, provider)
	}
	return filtered
}

func (s *Service) normalizeClientAccountGroupForUser(groupName, current string, user core.User) (string, error) {
	groupName = normalizeAccountGroup(groupName)
	current = normalizeAccountGroup(current)
	if groupName == "" {
		return "", fmt.Errorf("account group is required")
	}
	available := s.clientEditorAccountGroupsForUser(current, user)
	matched := false
	for _, group := range available {
		if strings.EqualFold(normalizeAccountGroup(group.Name), groupName) {
			groupName = normalizeAccountGroup(group.Name)
			matched = true
			break
		}
	}
	if !matched {
		return "", fmt.Errorf("account group %q is not available", groupName)
	}
	if err := s.ensureAccountGroupExists(groupName); err != nil {
		return "", err
	}
	if !s.accountGroupHasAccounts([]string{groupName}) {
		return "", fmt.Errorf("account group %q has no accounts", groupName)
	}
	return groupName, nil
}

func (s *Service) accountGroupHasAccounts(groups []string) bool {
	allowed := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		allowed[strings.ToLower(normalizeStoredAccountGroup(group))] = struct{}{}
	}
	for _, account := range s.repo.ListAccounts() {
		if _, ok := allowed[strings.ToLower(normalizeStoredAccountGroup(account.Group))]; ok {
			return true
		}
	}
	return false
}

func (s *Service) ensureClientKeyUnique(currentID, apiKey string) error {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil
	}
	existing, err := s.repo.FindClientByAPIKey(apiKey)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if strings.TrimSpace(existing.ID) != strings.TrimSpace(currentID) {
		return fmt.Errorf("api key is already assigned to client %q", existing.Name)
	}
	return nil
}

func generateClientKey() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "sk-" + hex.EncodeToString(buf), nil
}

func shortID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 8 {
		return value
	}
	return value[:8]
}

func oauthImportLabel(provider core.ProviderKind, email, accountID string) string {
	label := providerLabel(provider)
	email = strings.TrimSpace(email)
	accountID = strings.TrimSpace(accountID)
	switch {
	case email != "":
		return label + " " + email
	case accountID != "":
		return label + " OAuth " + shortID(accountID)
	default:
		return label + " OAuth"
	}
}

func newOAuthConnectImport(label, accessToken, refreshToken string, credentialExpiresAt *time.Time, credentialMode, tokenSource, accountID, email, codexAuthPath string) oauthConnectImport {
	imported := oauthConnectImport{
		Label:               strings.TrimSpace(label),
		AccessToken:         strings.TrimSpace(accessToken),
		RefreshToken:        strings.TrimSpace(refreshToken),
		CredentialExpiresAt: cloneTimePtr(credentialExpiresAt),
		Priority:            100,
		Weight:              100,
		CredentialMode:      strings.TrimSpace(credentialMode),
		TokenSource:         strings.TrimSpace(tokenSource),
		OAuthAccountID:      strings.TrimSpace(accountID),
		OAuthEmail:          strings.TrimSpace(email),
		ExpiresAt:           time.Now().UTC().Add(15 * time.Minute),
	}
	if trimmed := strings.TrimSpace(codexAuthPath); trimmed != "" {
		imported.CodexAuthPath = trimmed
	}
	return imported
}

func applyConnectImport(start *ConnectStart, prefill oauthConnectImport, includeCodexAuthPath bool) {
	if start == nil {
		return
	}
	start.Label = prefill.Label
	start.AccessToken = prefill.AccessToken
	start.RefreshToken = prefill.RefreshToken
	start.SessionToken = prefill.SessionToken
	start.ExpiresAtText = formatOptionalTime(prefill.CredentialExpiresAt)
	start.BaseURL = prefill.BaseURL
	start.Backup = prefill.Backup
	start.Priority = prefill.Priority
	start.Weight = prefill.Weight
	start.CredentialMode = prefill.CredentialMode
	start.TokenSource = prefill.TokenSource
	start.OAuthAccountID = prefill.OAuthAccountID
	start.OAuthEmail = prefill.OAuthEmail
	if includeCodexAuthPath {
		start.CodexAuthPath = prefill.CodexAuthPath
	}
}

func (s *Service) oauthImportForProvider(provider core.ProviderKind, id string) (oauthConnectImport, bool, bool) {
	switch provider {
	case core.ProviderOpenAI:
		imported, ok := s.oauthImportByID(s.openAIImports, id)
		return imported, true, ok
	case core.ProviderClaude:
		imported, ok := s.oauthImportByID(s.claudeImports, id)
		return imported, false, ok
	default:
		return oauthConnectImport{}, false, false
	}
}

func (s *Service) oauthImportByID(imports map[string]oauthConnectImport, id string) (oauthConnectImport, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return oauthConnectImport{}, false
	}

	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()

	imported, ok := imports[id]
	if !ok {
		return oauthConnectImport{}, false
	}
	if time.Now().UTC().After(imported.ExpiresAt) {
		delete(imports, id)
		return oauthConnectImport{}, false
	}
	return imported, true
}

func storeMapValue[T any](target *map[string]T, key string, value T) {
	if *target == nil {
		*target = map[string]T{}
	}
	(*target)[key] = value
}

func generateConnectImportID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "oauth_" + hex.EncodeToString(buf), nil
}

func generateAccountID(provider core.ProviderKind) (string, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return fmt.Sprintf("acct_%s_%d_%s", provider, time.Now().UnixNano(), hex.EncodeToString(buf)), nil
}

func buildAccountGroupSections(accounts []core.Account, groups []core.AccountGroup) []AccountGroupSection {
	grouped := make(map[string][]core.Account)
	groupIDs := make(map[string]string)
	groupProxies := make(map[string]string)
	groupBilling := make(map[string]core.AccountGroup)
	order := make([]string, 0, len(groups)+1)
	for _, account := range accounts {
		key := normalizeStoredAccountGroup(account.Group)
		account.Group = key
		if _, ok := grouped[key]; !ok {
			order = append(order, key)
		}
		grouped[key] = append(grouped[key], account)
	}
	for _, group := range groups {
		key := normalizeAccountGroup(group.Name)
		if key == "" {
			continue
		}
		groupIDs[key] = group.ID
		groupProxies[key] = normalizeProxyURL(group.ProxyURL)
		groupBilling[key] = group
		if _, ok := grouped[key]; ok {
			continue
		}
		grouped[key] = nil
		order = append(order, key)
	}

	slices.SortFunc(order, func(a, b string) int {
		if strings.EqualFold(a, core.DefaultAccountGroupName) {
			return -1
		}
		if strings.EqualFold(b, core.DefaultAccountGroupName) {
			return 1
		}
		return strings.Compare(strings.ToLower(a), strings.ToLower(b))
	})

	sections := make([]AccountGroupSection, 0, len(order))
	for _, key := range order {
		accounts := append([]core.Account(nil), grouped[key]...)
		sortAccountsByDefaultStatus(accounts)
		label := key
		id := groupIDs[key]
		group := groupBilling[key]
		if strings.EqualFold(key, core.DefaultAccountGroupName) {
			id = core.DefaultAccountGroupID
		}
		group = core.NormalizeAccountGroupBilling(group)
		sections = append(sections, AccountGroupSection{
			Key:                           key,
			ID:                            id,
			Label:                         label,
			Type:                          core.NormalizeAccountGroupType(group.Type),
			Remark:                        group.Remark,
			ProxyURL:                      groupProxies[key],
			ShowInClientEditor:            group.ShowInClientEditor,
			VisibleUserIDs:                slices.Clone(group.VisibleUserIDs),
			BillingMultiplierBps:          group.BillingMultiplierBps,
			PlanBillingMultiplierBps:      group.PlanBillingMultiplierBps,
			PlanBillingEnabled:            group.PlanBillingEnabled,
			InputPriceNanoUSDPer1M:        group.InputPriceNanoUSDPer1M,
			CachedInputPriceNanoUSDPer1M:  group.CachedInputPriceNanoUSDPer1M,
			CacheWritePriceNanoUSDPer1M:   group.CacheWritePriceNanoUSDPer1M,
			CacheWrite5mPriceNanoUSDPer1M: group.CacheWrite5mPriceNanoUSDPer1M,
			CacheWrite1hPriceNanoUSDPer1M: group.CacheWrite1hPriceNanoUSDPer1M,
			OutputPriceNanoUSDPer1M:       group.OutputPriceNanoUSDPer1M,
			ImageOutputPriceNanoUSDPer1M:  group.ImageOutputPriceNanoUSDPer1M,
			TimedMultipliers:              group.TimedMultipliers,
			Accounts:                      accounts,
		})
	}
	return sections
}

func sortAccountsByDefaultStatus(accounts []core.Account) {
	slices.SortStableFunc(accounts, func(a, b core.Account) int {
		return accountDefaultStatusRank(a) - accountDefaultStatusRank(b)
	})
}

func accountDefaultStatusRank(account core.Account) int {
	switch AccountFilterStatus(account) {
	case "cooling":
		return 1
	case "time_limit":
		return 2
	case "week_limit":
		return 3
	case "exception":
		return 4
	default:
		return 0
	}
}

func AccountFilterStatus(account core.Account) string {
	now := time.Now().UTC()
	account = core.NormalizeAccountRuntimeState(account, now)
	switch account.Status {
	case core.AccountStatusActive:
	case core.AccountStatusCooling, core.AccountStatusRefreshing:
		return "cooling"
	default:
		return "exception"
	}
	if status := core.AccountQuotaLimitStatus(account, now); status != "" {
		return status
	}
	if core.AccountScopedRateLimitActive(account, now) {
		return "cooling"
	}
	if core.AccountScopedImageRateLimitActive(account, now) {
		return "cooling"
	}
	if !core.AccountImageQuotaAvailable(account, now) {
		return "cooling"
	}
	return "normal"
}

func AccountRuntimeStatus(account core.Account) string {
	now := time.Now().UTC()
	account = core.NormalizeAccountRuntimeState(account, now)
	if account.Status == core.AccountStatusActive {
		if status := core.AccountQuotaLimitStatus(account, now); status != "" {
			return status
		}
		if core.AccountScopedRateLimitActive(account, now) {
			return string(core.AccountStatusCooling)
		}
		if core.AccountScopedImageRateLimitActive(account, now) {
			return string(core.AccountStatusCooling)
		}
		if !core.AccountImageQuotaAvailable(account, now) {
			return string(core.AccountStatusCooling)
		}
	}
	return string(account.Status)
}

func AccountRecoverable(account core.Account) bool {
	switch core.AccountStatus(AccountRuntimeStatus(account)) {
	case core.AccountStatusCooling,
		core.AccountStatusBlocked,
		core.AccountStatusProviderBanned,
		core.AccountStatusExpired,
		core.AccountStatusRefreshing:
		return true
	default:
		return false
	}
}

func normalizeAccountGroup(value string) string {
	return strings.TrimSpace(value)
}

func normalizeAccountGroupRemark(value string) string {
	value = strings.TrimSpace(strings.Join(strings.Fields(value), " "))
	runes := []rune(value)
	if len(runes) <= 120 {
		return value
	}
	return string(runes[:120])
}

func normalizeStoredAccountGroup(value string) string {
	return core.NormalizeAccountGroupName(value)
}

func reservedAccountGroupName(value string) bool {
	switch strings.ToLower(normalizeAccountGroup(value)) {
	case strings.ToLower(core.DefaultAccountGroupName):
		return true
	default:
		return false
	}
}

func uniqueNormalizedAccountGroups(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = normalizeAccountGroup(value)
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func normalizeProxyURL(value string) string {
	return strings.TrimSpace(value)
}

func validateProxyURL(value string) error {
	value = normalizeProxyURL(value)
	if value == "" {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("proxy url must be a valid URL")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5", "socks5h":
		return nil
	default:
		return fmt.Errorf("proxy url scheme must be http, https, socks5, or socks5h")
	}
}

func availableAccountGroupsWithCurrent(groups []core.AccountGroup, current string) []core.AccountGroup {
	current = normalizeAccountGroup(current)
	if current == "" {
		return sortAccountGroupsByName(groups)
	}
	for _, group := range groups {
		if strings.EqualFold(group.Name, current) {
			return sortAccountGroupsByName(groups)
		}
	}
	out := append([]core.AccountGroup(nil), groups...)
	out = append(out, core.AccountGroup{Name: current})
	return sortAccountGroupsByName(out)
}

func sortAccountGroupsByName(groups []core.AccountGroup) []core.AccountGroup {
	out := append([]core.AccountGroup(nil), groups...)
	slices.SortFunc(out, func(a, b core.AccountGroup) int {
		if strings.ToLower(a.Name) < strings.ToLower(b.Name) {
			return -1
		}
		if strings.ToLower(a.Name) > strings.ToLower(b.Name) {
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

func boolPtr(value bool) *bool {
	return &value
}

func (s *Service) ensureAccountGroupExists(name string) error {
	name = normalizeAccountGroup(name)
	if name == "" {
		return fmt.Errorf("account group is required")
	}
	if strings.EqualFold(name, core.DefaultAccountGroupName) {
		return nil
	}
	for _, group := range accountGroupsWithDefault(s.repo.ListAccountGroups()) {
		if strings.EqualFold(group.Name, name) {
			return nil
		}
	}
	return fmt.Errorf("account group %q does not exist", name)
}

func (s *Service) effectiveProxyURLForAccount(account core.Account) string {
	if proxyURL := normalizeProxyURL(account.ProxyURL); proxyURL != "" {
		return proxyURL
	}
	systemProxyURL := s.SystemProxyURL()
	groupName := normalizeAccountGroup(account.Group)
	if groupName == "" {
		return systemProxyURL
	}
	for _, group := range s.repo.ListAccountGroups() {
		if strings.EqualFold(group.Name, groupName) {
			if proxyURL := normalizeProxyURL(group.ProxyURL); proxyURL != "" {
				return proxyURL
			}
			return systemProxyURL
		}
	}
	return systemProxyURL
}

func filterAuditByKind(audits []core.AuditEvent, kind core.AuditKind, limit int) []core.AuditEvent {
	filtered := make([]core.AuditEvent, 0, limit)
	for _, audit := range audits {
		if audit.EffectiveKind() != kind {
			continue
		}
		filtered = append(filtered, audit)
		if limit > 0 && len(filtered) >= limit {
			break
		}
	}
	return filtered
}

func buildProviderSummaries(accounts []core.Account) []ProviderSummary {
	summaries := make([]ProviderSummary, 0, len(accounts))
	indexByProvider := make(map[core.ProviderKind]int, len(accounts))

	appendProvider := func(kind core.ProviderKind) {
		if kind == "" {
			return
		}
		if _, ok := indexByProvider[kind]; ok {
			return
		}
		indexByProvider[kind] = len(summaries)
		summaries = append(summaries, ProviderSummary{
			Kind:  kind,
			Label: providerLabel(kind),
		})
	}

	for _, account := range accounts {
		appendProvider(account.Provider)
	}

	now := time.Now().UTC()
	for _, account := range accounts {
		account = core.NormalizeAccountRuntimeState(account, now)
		idx, ok := indexByProvider[account.Provider]
		if !ok {
			continue
		}
		summary := &summaries[idx]
		summary.TotalAccounts++
		poolState := core.AccountPoolStateFor(account, now)
		if isAccountAvailable(account, now) {
			summary.AvailableAccounts++
		}
		switch {
		case poolState == core.AccountPoolStateNormal:
			summary.ActiveAccounts++
		case poolState == core.AccountPoolStateCooling:
			summary.CoolingAccounts++
		default:
			switch account.Status {
			case core.AccountStatusBlocked:
				summary.BlockedAccounts++
			case core.AccountStatusProviderBanned:
				summary.ProviderBannedAccounts++
			case core.AccountStatusExpired:
				summary.ExpiredAccounts++
			case core.AccountStatusRefreshing:
				summary.RefreshingAccounts++
			default:
				summary.CoolingAccounts++
			}
		}
		if account.LastUsedAt != nil && (summary.LastUsedAt == nil || account.LastUsedAt.After(*summary.LastUsedAt)) {
			summary.LastUsedAt = cloneTimePtr(account.LastUsedAt)
		}
	}

	slices.SortFunc(summaries, compareProviderSummary)
	return summaries
}

func compareProviderSummary(a, b ProviderSummary) int {
	if diff := providerSortRank(a.Kind) - providerSortRank(b.Kind); diff != 0 {
		return diff
	}
	if a.Kind < b.Kind {
		return -1
	}
	if a.Kind > b.Kind {
		return 1
	}
	return 0
}

func buildDashboardStats(accounts []core.Account, providers []ProviderSummary) DashboardStats {
	now := time.Now().UTC()
	stats := DashboardStats{
		TotalAccounts: len(accounts),
	}
	for _, account := range accounts {
		account = core.NormalizeAccountRuntimeState(account, now)
		poolState := core.AccountPoolStateFor(account, now)
		if isAccountAvailable(account, now) {
			stats.AvailableAccounts++
		}
		if poolState == core.AccountPoolStateCooling {
			stats.CoolingCount++
		}
		if account.Status == core.AccountStatusBlocked || account.Status == core.AccountStatusProviderBanned || account.Status == core.AccountStatusExpired || account.Status == core.AccountStatusRefreshing {
			stats.FailureCount++
		}
		if accountExpiresWithin(account, 15*time.Minute) {
			stats.ExpiringSoonCount++
		}
	}
	for _, provider := range providers {
		if provider.AvailableAccounts > 0 {
			stats.HealthyProviderCount++
		} else {
			stats.DegradedProviderCount++
		}
	}

	switch {
	case stats.TotalAccounts == 0:
		stats.Status = "setup"
		stats.Reason = "no accounts configured"
	case stats.AvailableAccounts == 0:
		stats.Status = "error"
		stats.Reason = "no available accounts"
	case stats.DegradedProviderCount > 0:
		stats.Status = "degraded"
		stats.Reason = "one or more providers have no available accounts"
	case stats.ExpiringSoonCount > 0:
		stats.Status = "degraded"
		stats.Reason = "one or more accounts expire soon"
	default:
		stats.Status = "ok"
		stats.Reason = "accounts available"
	}

	return stats
}

func isAccountAvailable(account core.Account, now time.Time) bool {
	return core.AccountAvailableForRouting(account, now)
}

func accountExpiresWithin(account core.Account, window time.Duration) bool {
	if account.Credential.ExpiresAt == nil || account.Credential.ExpiresAt.IsZero() {
		return false
	}
	remaining := time.Until(account.Credential.ExpiresAt.UTC())
	return remaining > 0 && remaining <= window
}

func providerLabel(kind core.ProviderKind) string {
	switch kind {
	case core.ProviderOpenAI:
		return "OpenAI"
	case core.ProviderClaude:
		return "Claude"
	default:
		return string(kind)
	}
}

func providerSortRank(kind core.ProviderKind) int {
	switch kind {
	case core.ProviderOpenAI:
		return 0
	case core.ProviderClaude:
		return 1
	default:
		return 100
	}
}
