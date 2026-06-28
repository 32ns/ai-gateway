package storage

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

type CachedRepository struct {
	Repository

	mu              sync.RWMutex
	accounts        []core.Account
	accountsByID    map[string]core.Account
	accountsLoaded  bool
	groups          []core.AccountGroup
	groupsLoaded    bool
	models          []core.ModelConfig
	modelsByID      map[string]core.ModelConfig
	modelsLoaded    bool
	users           []core.User
	usersByID       map[string]core.User
	usersByName     map[string]core.User
	usersByOAuth    map[string]core.User
	usersByInvite   map[string]core.User
	usersByInviter  map[string][]core.User
	enabledAdminIDs map[string]struct{}
	clients         []core.APIClient
	clientsByID     map[string]core.APIClient
	clientsByKey    map[string]core.APIClient
	clientsByOwner  map[string][]core.APIClient
	settings        core.SystemSettings
	settingsLoaded  bool
	startupSettings core.SystemSettings
	startupLoaded   bool
	revision        atomic.Uint64
	userRev         atomic.Uint64
	accountRev      atomic.Uint64
	modelRev        atomic.Uint64
	clientRev       atomic.Uint64
}

func NewCachedRepository(base Repository) *CachedRepository {
	repo := &CachedRepository{Repository: base}
	repo.reloadStartup()
	return repo
}

func oauthIdentityCacheKey(provider, subject string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	subject = strings.TrimSpace(subject)
	if provider == "" || subject == "" {
		return ""
	}
	return provider + "\x00" + subject
}

func (r *CachedRepository) reload() {
	accounts := cloneAccounts(r.Repository.ListAccounts())
	groups := cloneAccountGroups(r.Repository.ListAccountGroups())
	models := cloneModels(r.Repository.ListModels())
	settings, err := r.Repository.GetSystemSettings()
	if err != nil {
		settings = core.DefaultSystemSettings()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.accounts = accounts
	r.accountsByID = map[string]core.Account{}
	for _, account := range r.accounts {
		r.accountsByID[account.ID] = cloneAccount(account)
	}
	r.accountsLoaded = true
	r.groups = groups
	r.groupsLoaded = true
	r.models = models
	r.modelsByID = map[string]core.ModelConfig{}
	for _, model := range r.models {
		r.modelsByID[model.ID] = cloneModel(model)
	}
	r.modelsLoaded = true
	r.users = nil
	r.usersByID = map[string]core.User{}
	r.usersByName = map[string]core.User{}
	r.usersByOAuth = map[string]core.User{}
	r.usersByInvite = map[string]core.User{}
	r.usersByInviter = map[string][]core.User{}
	r.enabledAdminIDs = map[string]struct{}{}
	r.clients = nil
	r.clientsByID = map[string]core.APIClient{}
	r.clientsByKey = map[string]core.APIClient{}
	r.clientsByOwner = map[string][]core.APIClient{}
	r.cacheFullSettingsLocked(settings)
	r.revision.Add(1)
	r.userRev.Add(1)
	r.accountRev.Add(1)
	r.modelRev.Add(1)
	r.clientRev.Add(1)
}

func (r *CachedRepository) reloadStartup() {
	settings, err := LoadStartupSystemSettings(r.Repository)
	if err != nil {
		settings = core.DefaultSystemSettings()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.accounts = nil
	r.accountsByID = map[string]core.Account{}
	r.accountsLoaded = false
	r.groups = nil
	r.groupsLoaded = false
	r.models = nil
	r.modelsByID = map[string]core.ModelConfig{}
	r.modelsLoaded = false
	r.users = nil
	r.usersByID = map[string]core.User{}
	r.usersByName = map[string]core.User{}
	r.usersByOAuth = map[string]core.User{}
	r.usersByInvite = map[string]core.User{}
	r.usersByInviter = map[string][]core.User{}
	r.enabledAdminIDs = map[string]struct{}{}
	r.clients = nil
	r.clientsByID = map[string]core.APIClient{}
	r.clientsByKey = map[string]core.APIClient{}
	r.clientsByOwner = map[string][]core.APIClient{}
	r.settings = core.SystemSettings{}
	r.settingsLoaded = false
	r.startupSettings = core.StartupSystemSettingsFrom(settings)
	r.startupLoaded = true
	r.revision.Add(1)
	r.userRev.Add(1)
	r.accountRev.Add(1)
	r.modelRev.Add(1)
	r.clientRev.Add(1)
}

func (r *CachedRepository) cacheFullSettingsLocked(settings core.SystemSettings) {
	settings = core.NormalizeSystemSettings(settings)
	r.settings = settings
	r.settingsLoaded = true
	r.startupSettings = core.StartupSystemSettingsFrom(settings)
	r.startupLoaded = true
}

func (r *CachedRepository) hydrateAccounts(accounts []core.Account) []core.Account {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.accountsLoaded {
		return cloneAccounts(r.accounts)
	}
	r.accounts = cloneAccounts(accounts)
	r.accountsByID = map[string]core.Account{}
	for _, account := range r.accounts {
		r.accountsByID[account.ID] = cloneAccount(account)
	}
	r.accountsLoaded = true
	return cloneAccounts(r.accounts)
}

func (r *CachedRepository) hydrateAccount(account core.Account) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cloned := cloneAccount(account)
	r.accountsByID[account.ID] = cloned
	if r.accountsLoaded {
		replaced := false
		for i, existing := range r.accounts {
			if existing.ID == account.ID {
				r.accounts[i] = cloned
				replaced = true
				break
			}
		}
		if !replaced {
			r.accounts = append(r.accounts, cloned)
		}
		r.accounts = sortAccounts(r.accounts)
	}
}

func (r *CachedRepository) hydrateAccountGroups(groups []core.AccountGroup) []core.AccountGroup {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.groupsLoaded {
		return cloneAccountGroups(r.groups)
	}
	r.groups = cloneAccountGroups(groups)
	r.groupsLoaded = true
	return cloneAccountGroups(r.groups)
}

func (r *CachedRepository) hydrateModels(models []core.ModelConfig) []core.ModelConfig {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.modelsLoaded {
		return cloneModels(r.models)
	}
	r.models = cloneModels(models)
	r.modelsByID = map[string]core.ModelConfig{}
	for _, model := range r.models {
		r.modelsByID[model.ID] = cloneModel(model)
	}
	r.modelsLoaded = true
	return cloneModels(r.models)
}

func (r *CachedRepository) hydrateModel(model core.ModelConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cloned := cloneModel(model)
	r.modelsByID[model.ID] = cloned
	if r.modelsLoaded {
		replaced := false
		for i, existing := range r.models {
			if existing.ID == model.ID {
				r.models[i] = cloned
				replaced = true
				break
			}
		}
		if !replaced {
			r.models = append(r.models, cloned)
		}
		r.models = sortModels(r.models)
	}
}

func (r *CachedRepository) ConfigRevision() uint64 {
	return r.revision.Load()
}

func (r *CachedRepository) AccountRevision() uint64 {
	return r.accountRev.Load()
}

func (r *CachedRepository) UserRevision() uint64 {
	return r.userRev.Load()
}

func (r *CachedRepository) ModelRevision() uint64 {
	return r.modelRev.Load()
}

func (r *CachedRepository) ClientRevision() uint64 {
	return r.clientRev.Load()
}

func (r *CachedRepository) ListUsers() []core.User {
	return cloneUsers(r.Repository.ListUsers())
}

func (r *CachedRepository) GetUser(id string) (core.User, error) {
	id = strings.TrimSpace(id)
	r.mu.RLock()
	user, ok := r.usersByID[id]
	r.mu.RUnlock()
	if !ok {
		stored, err := r.Repository.GetUser(id)
		if err != nil {
			return core.User{}, err
		}
		r.cacheUser(stored)
		return cloneUser(stored), nil
	}
	return cloneUser(user), nil
}

func (r *CachedRepository) FindUserByUsername(username string) (core.User, error) {
	key := usernameKey(username)
	r.mu.RLock()
	user, ok := r.usersByName[key]
	r.mu.RUnlock()
	if !ok {
		stored, err := r.Repository.FindUserByUsername(username)
		if err != nil {
			return core.User{}, err
		}
		r.cacheUser(stored)
		return cloneUser(stored), nil
	}
	return cloneUser(user), nil
}

func (r *CachedRepository) FindUserByOAuthIdentity(provider, subject string) (core.User, error) {
	key := oauthIdentityCacheKey(provider, subject)
	if key == "" {
		return core.User{}, ErrNotFound
	}
	r.mu.RLock()
	user, ok := r.usersByOAuth[key]
	r.mu.RUnlock()
	if !ok {
		stored, err := r.Repository.FindUserByOAuthIdentity(provider, subject)
		if err != nil {
			return core.User{}, err
		}
		r.cacheUser(stored)
		return cloneUser(stored), nil
	}
	return cloneUser(user), nil
}

func (r *CachedRepository) FindUserByInvitationSignature(signature string) (core.User, error) {
	signature = strings.TrimSpace(signature)
	if signature == "" {
		return core.User{}, ErrNotFound
	}
	r.mu.RLock()
	user, ok := r.usersByInvite[signature]
	r.mu.RUnlock()
	if !ok {
		stored, err := r.Repository.FindUserByInvitationSignature(signature)
		if err != nil {
			return core.User{}, err
		}
		r.cacheUser(stored)
		return cloneUser(stored), nil
	}
	return cloneUser(user), nil
}

func (r *CachedRepository) ListUsersByInviter(inviterID string) []core.User {
	inviterID = strings.TrimSpace(inviterID)
	if inviterID == "" {
		return nil
	}
	return cloneUsers(r.Repository.ListUsersByInviter(inviterID))
}

func (r *CachedRepository) CountUsersByInviter(inviterID string) int {
	inviterID = strings.TrimSpace(inviterID)
	if inviterID == "" {
		return 0
	}
	return r.Repository.CountUsersByInviter(inviterID)
}

func (r *CachedRepository) CountEnabledAdminsExcluding(excludedIDs []string) int {
	return r.Repository.CountEnabledAdminsExcluding(excludedIDs)
}

func (r *CachedRepository) GetSystemSettings() (core.SystemSettings, error) {
	r.mu.RLock()
	if r.settingsLoaded {
		settings := r.settings
		r.mu.RUnlock()
		return core.NormalizeSystemSettings(settings), nil
	}
	r.mu.RUnlock()

	settings, err := r.Repository.GetSystemSettings()
	if err != nil {
		return core.SystemSettings{}, err
	}
	r.mu.Lock()
	r.cacheFullSettingsLocked(settings)
	r.mu.Unlock()
	return core.NormalizeSystemSettings(settings), nil
}

func (r *CachedRepository) GetStartupSystemSettings() (core.SystemSettings, error) {
	r.mu.RLock()
	if r.startupLoaded {
		settings := r.startupSettings
		r.mu.RUnlock()
		return core.NormalizeSystemSettings(settings), nil
	}
	r.mu.RUnlock()

	settings, err := LoadStartupSystemSettings(r.Repository)
	if err != nil {
		return core.SystemSettings{}, err
	}
	settings = core.StartupSystemSettingsFrom(settings)
	r.mu.Lock()
	r.startupSettings = settings
	r.startupLoaded = true
	r.mu.Unlock()
	return core.NormalizeSystemSettings(settings), nil
}

func (r *CachedRepository) UpsertSystemSettings(settings core.SystemSettings) error {
	if err := r.Repository.UpsertSystemSettings(settings); err != nil {
		return err
	}
	stored, err := r.Repository.GetSystemSettings()
	if err == nil {
		settings = stored
	}
	r.mu.Lock()
	r.cacheFullSettingsLocked(settings)
	r.mu.Unlock()
	r.revision.Add(1)
	r.accountRev.Add(1)
	return nil
}

func (r *CachedRepository) UpsertUser(user core.User) error {
	if err := r.Repository.UpsertUser(user); err != nil {
		return err
	}
	r.cacheStoredUserIfLoaded(user.ID, user)
	r.userRev.Add(1)
	r.revision.Add(1)
	return nil
}

func (r *CachedRepository) UpdateUserMetadata(user core.User) error {
	if err := r.Repository.UpdateUserMetadata(user); err != nil {
		return err
	}
	r.cacheStoredUserIfLoaded(user.ID, user)
	r.userRev.Add(1)
	r.revision.Add(1)
	return nil
}

func (r *CachedRepository) TouchUserLastUsedAt(userID string, usedAt time.Time) error {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return ErrNotFound
	}
	if usedAt.IsZero() {
		usedAt = time.Now().UTC()
	} else {
		usedAt = usedAt.UTC()
	}
	if repo, ok := r.Repository.(interface {
		TouchUserLastUsedAt(string, time.Time) error
	}); ok {
		if err := repo.TouchUserLastUsedAt(userID, usedAt); err != nil {
			return err
		}
	} else {
		user, err := r.Repository.GetUser(userID)
		if err != nil {
			return err
		}
		if user.LastLoginAt != nil && !usedAt.After(*user.LastLoginAt) {
			return nil
		}
		user.LastLoginAt = &usedAt
		if err := r.Repository.UpdateUserMetadata(user); err != nil {
			return err
		}
	}
	r.touchCachedUserLastUsedAt(userID, usedAt)
	return nil
}

func (r *CachedRepository) MergeUsers(source core.User, target core.User) error {
	if err := r.Repository.MergeUsers(source, target); err != nil {
		return err
	}
	r.reload()
	return nil
}

func (r *CachedRepository) DeleteUser(id string) error {
	id = strings.TrimSpace(id)
	if err := r.Repository.DeleteUser(id); err != nil {
		return err
	}
	r.deleteCachedUser(id)
	r.deleteCachedClientsByOwner(id)
	r.userRev.Add(1)
	r.clientRev.Add(1)
	r.revision.Add(1)
	return nil
}

func (r *CachedRepository) SetUserBalance(userID string, balanceNanoUSD int64) error {
	if err := r.Repository.SetUserBalance(userID, balanceNanoUSD); err != nil {
		return err
	}
	r.refreshCachedUser(userID)
	r.userRev.Add(1)
	r.revision.Add(1)
	return nil
}

func (r *CachedRepository) AdjustUserBalance(userID string, deltaNanoUSD int64, note string) (int64, int64, error) {
	previous, next, err := r.Repository.AdjustUserBalance(userID, deltaNanoUSD, note)
	if err != nil {
		return previous, next, err
	}
	r.refreshCachedUser(userID)
	r.userRev.Add(1)
	r.revision.Add(1)
	return previous, next, nil
}

func (r *CachedRepository) cacheUser(user core.User) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.usersByID[user.ID]; ok {
		r.unindexCachedUserLocked(existing)
	}
	cloned := cloneUser(user)
	r.indexCachedUserLocked(cloned)
	replaced := false
	for i, existing := range r.users {
		if existing.ID == user.ID {
			r.users[i] = cloned
			replaced = true
			break
		}
	}
	if !replaced {
		r.users = append(r.users, cloned)
	}
	r.users = sortUsers(r.users)
}

func (r *CachedRepository) touchCachedUserLastUsedAt(userID string, usedAt time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing, ok := r.usersByID[userID]
	if !ok {
		return
	}
	if existing.LastLoginAt != nil && !usedAt.After(*existing.LastLoginAt) {
		return
	}
	r.unindexCachedUserLocked(existing)
	existing.LastLoginAt = &usedAt
	r.indexCachedUserLocked(existing)
	for i, user := range r.users {
		if user.ID == userID {
			r.users[i] = existing
			break
		}
	}
}

func (r *CachedRepository) indexCachedUserLocked(user core.User) {
	r.usersByID[user.ID] = user
	r.usersByName[usernameKey(user.Username)] = user
	for _, identity := range user.OAuthIdentities {
		key := oauthIdentityCacheKey(identity.Provider, identity.Subject)
		if key != "" {
			r.usersByOAuth[key] = user
		}
	}
	if signature := core.UserInvitationSignature(user); signature != "" {
		r.usersByInvite[signature] = user
	}
	if inviterID := strings.TrimSpace(user.InviterUserID); inviterID != "" {
		r.usersByInviter[inviterID] = sortUsers(append(r.usersByInviter[inviterID], user))
	}
	if user.Role == core.UserRoleAdmin && user.Enabled {
		r.enabledAdminIDs[strings.TrimSpace(user.ID)] = struct{}{}
	}
}

func (r *CachedRepository) unindexCachedUserLocked(user core.User) {
	delete(r.usersByName, usernameKey(user.Username))
	for _, identity := range user.OAuthIdentities {
		delete(r.usersByOAuth, oauthIdentityCacheKey(identity.Provider, identity.Subject))
	}
	delete(r.usersByInvite, core.UserInvitationSignature(user))
	if inviterID := strings.TrimSpace(user.InviterUserID); inviterID != "" {
		r.usersByInviter[inviterID] = removeCachedUserFromSlice(r.usersByInviter[inviterID], user.ID)
		if len(r.usersByInviter[inviterID]) == 0 {
			delete(r.usersByInviter, inviterID)
		}
	}
	delete(r.enabledAdminIDs, strings.TrimSpace(user.ID))
}

func (r *CachedRepository) refreshCachedUser(userID string) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return
	}
	if stored, err := r.Repository.GetUser(userID); err == nil {
		r.cacheUser(stored)
	}
}

func (r *CachedRepository) cacheStoredUserIfLoaded(userID string, user core.User) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return
	}
	r.mu.RLock()
	existing, loaded := r.usersByID[userID]
	r.mu.RUnlock()
	if !loaded {
		return
	}
	user.ID = userID
	user.BalanceNanoUSD = existing.BalanceNanoUSD
	if user.CreatedAt.IsZero() {
		user.CreatedAt = existing.CreatedAt
	}
	r.cacheUser(user)
}

func (r *CachedRepository) deleteCachedUser(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	id = strings.TrimSpace(id)
	if existing, ok := r.usersByID[id]; ok {
		r.unindexCachedUserLocked(existing)
	}
	delete(r.usersByID, id)
	for i, user := range r.users {
		if user.ID == id {
			r.users = append(r.users[:i], r.users[i+1:]...)
			break
		}
	}
}

func (r *CachedRepository) ReserveBilling(input core.BillingReservationInput) (core.BillingReservation, error) {
	reservation, err := r.Repository.ReserveBilling(input)
	if err != nil {
		return reservation, err
	}
	r.refreshCachedUser(reservation.UserID)
	r.userRev.Add(1)
	r.revision.Add(1)
	return reservation, nil
}

func (r *CachedRepository) UpdateBillingAccount(input core.BillingAccountUpdateInput) error {
	if err := r.Repository.UpdateBillingAccount(input); err != nil {
		return err
	}
	r.revision.Add(1)
	return nil
}

func (r *CachedRepository) SettleBilling(input core.BillingSettlementInput) (core.BillingSettlement, error) {
	settlement, err := r.Repository.SettleBilling(input)
	if err != nil {
		return settlement, err
	}
	r.refreshCachedUser(settlement.Request.UserID)
	r.userRev.Add(1)
	r.revision.Add(1)
	return settlement, nil
}

func (r *CachedRepository) ReleaseBilling(input core.BillingReleaseInput) error {
	if err := r.Repository.ReleaseBilling(input); err != nil {
		return err
	}
	r.clearUserAndClientCaches()
	return nil
}

func (r *CachedRepository) clearUserAndClientCaches() {
	r.mu.Lock()
	r.users = nil
	r.usersByID = map[string]core.User{}
	r.usersByName = map[string]core.User{}
	r.usersByOAuth = map[string]core.User{}
	r.usersByInvite = map[string]core.User{}
	r.usersByInviter = map[string][]core.User{}
	r.enabledAdminIDs = map[string]struct{}{}
	r.clients = nil
	r.clientsByID = map[string]core.APIClient{}
	r.clientsByKey = map[string]core.APIClient{}
	r.clientsByOwner = map[string][]core.APIClient{}
	r.mu.Unlock()
	r.userRev.Add(1)
	r.clientRev.Add(1)
	r.revision.Add(1)
}

func (r *CachedRepository) ListBillingPlanGroups() []core.BillingPlanGroup {
	return r.Repository.ListBillingPlanGroups()
}

func (r *CachedRepository) GetBillingPlanGroup(id string) (core.BillingPlanGroup, error) {
	return r.Repository.GetBillingPlanGroup(id)
}

func (r *CachedRepository) UpsertBillingPlanGroup(group core.BillingPlanGroup) error {
	if err := r.Repository.UpsertBillingPlanGroup(group); err != nil {
		return err
	}
	r.revision.Add(1)
	return nil
}

func (r *CachedRepository) DeleteBillingPlanGroup(id string) error {
	if err := r.Repository.DeleteBillingPlanGroup(id); err != nil {
		return err
	}
	r.revision.Add(1)
	return nil
}

func (r *CachedRepository) ListBillingPlans() []core.BillingPlan {
	return r.Repository.ListBillingPlans()
}

func (r *CachedRepository) GetBillingPlan(id string) (core.BillingPlan, error) {
	return r.Repository.GetBillingPlan(id)
}

func (r *CachedRepository) UpsertBillingPlan(plan core.BillingPlan) error {
	if err := r.Repository.UpsertBillingPlan(plan); err != nil {
		return err
	}
	r.revision.Add(1)
	return nil
}

func (r *CachedRepository) DeleteBillingPlan(id string) error {
	if err := r.Repository.DeleteBillingPlan(id); err != nil {
		return err
	}
	r.revision.Add(1)
	return nil
}

func (r *CachedRepository) PurchaseBillingPlan(input core.BillingPlanPurchaseInput) (core.BillingPlanPurchase, error) {
	purchase, err := r.Repository.PurchaseBillingPlan(input)
	if err != nil {
		return purchase, err
	}
	r.refreshCachedUser(purchase.Entitlement.UserID)
	r.userRev.Add(1)
	r.revision.Add(1)
	return purchase, nil
}

func (r *CachedRepository) GrantBillingPlan(input core.BillingPlanGrantInput) (core.BillingPlanPurchase, error) {
	purchase, err := r.Repository.GrantBillingPlan(input)
	if err != nil {
		return purchase, err
	}
	r.refreshCachedUser(purchase.Entitlement.UserID)
	r.userRev.Add(1)
	r.revision.Add(1)
	return purchase, nil
}

func (r *CachedRepository) ListUserPlanEntitlements(userID string) []core.UserPlanEntitlement {
	return r.Repository.ListUserPlanEntitlements(userID)
}

func (r *CachedRepository) GetActiveUserPlanEntitlement(userID string) (core.UserPlanEntitlement, error) {
	return r.Repository.GetActiveUserPlanEntitlement(userID)
}

func (r *CachedRepository) MoveUserPlanEntitlementPriority(userID, entitlementID, direction string) error {
	if err := r.Repository.MoveUserPlanEntitlementPriority(userID, entitlementID, direction); err != nil {
		return err
	}
	r.refreshCachedUser(userID)
	r.userRev.Add(1)
	r.revision.Add(1)
	return nil
}

func (r *CachedRepository) CancelUserPlanEntitlement(userID, entitlementID string) (core.UserPlanEntitlement, error) {
	entitlement, err := r.Repository.CancelUserPlanEntitlement(userID, entitlementID)
	if err != nil {
		return entitlement, err
	}
	r.refreshCachedUser(entitlement.UserID)
	r.userRev.Add(1)
	r.revision.Add(1)
	return entitlement, nil
}

func (r *CachedRepository) PlanSubscriptionStats(query PlanSubscriptionQuery) PlanSubscriptionStats {
	return r.Repository.PlanSubscriptionStats(query)
}

func (r *CachedRepository) ListPlanSubscriptionPlanSummaries(query PlanSubscriptionQuery, limit int) []PlanSubscriptionPlanSummary {
	return r.Repository.ListPlanSubscriptionPlanSummaries(query, limit)
}

func (r *CachedRepository) ListPlanSubscriptionSummariesPage(query PlanSubscriptionQuery) ([]PlanSubscriptionSummary, int) {
	return r.Repository.ListPlanSubscriptionSummariesPage(query)
}

func (r *CachedRepository) PlanQuotaUsageStats(query PlanQuotaUsageQuery) PlanQuotaUsageStats {
	return r.Repository.PlanQuotaUsageStats(query)
}

func (r *CachedRepository) ListPlanQuotaUsageByDay(query PlanQuotaUsageQuery) ([]PlanQuotaUsageDaySummary, int) {
	return r.Repository.ListPlanQuotaUsageByDay(query)
}

func (r *CachedRepository) ListPlanQuotaUsageEvents(query PlanQuotaUsageQuery) []PlanQuotaUsageEvent {
	return r.Repository.ListPlanQuotaUsageEvents(query)
}

func (r *CachedRepository) CompletePaymentOrder(outTradeNo, providerTradeNo string, paidAmountNanoUSD int64, paidAt time.Time) (core.PaymentOrder, bool, error) {
	order, credited, err := r.Repository.CompletePaymentOrder(outTradeNo, providerTradeNo, paidAmountNanoUSD, paidAt)
	if err != nil {
		return order, credited, err
	}
	if credited {
		r.refreshCachedUser(order.UserID)
		r.userRev.Add(1)
		r.revision.Add(1)
	}
	return order, credited, nil
}

func (r *CachedRepository) CompletePaymentOrderWithCredits(outTradeNo, providerTradeNo string, paidAmountNanoUSD int64, paidAt time.Time, credits []core.PaymentOrderBalanceCredit) (core.PaymentOrder, bool, error) {
	order, credited, err := r.Repository.CompletePaymentOrderWithCredits(outTradeNo, providerTradeNo, paidAmountNanoUSD, paidAt, credits)
	if err != nil {
		return order, credited, err
	}
	if credited {
		refreshed := map[string]struct{}{}
		r.refreshCachedUser(order.UserID)
		refreshed[order.UserID] = struct{}{}
		for _, credit := range credits {
			userID := strings.TrimSpace(credit.UserID)
			if userID == "" {
				continue
			}
			if _, ok := refreshed[userID]; ok {
				continue
			}
			r.refreshCachedUser(userID)
			refreshed[userID] = struct{}{}
		}
		r.userRev.Add(1)
		r.revision.Add(1)
	}
	return order, credited, nil
}

func (r *CachedRepository) CompletePaymentRefund(id, providerRefundNo, rawResponse string) (core.PaymentRefund, bool, error) {
	refund, debited, err := r.Repository.CompletePaymentRefund(id, providerRefundNo, rawResponse)
	if err != nil {
		return refund, debited, err
	}
	if debited {
		r.refreshCachedUser(refund.UserID)
		r.userRev.Add(1)
		r.revision.Add(1)
	}
	return refund, debited, nil
}

func (r *CachedRepository) ListBillingRequests(query BillingRequestQuery) []core.BillingReservation {
	return r.Repository.ListBillingRequests(query)
}

func (r *CachedRepository) ListBillingUsageSpendByClientForUser(userID string) []BillingUsageSpendSummary {
	return r.Repository.ListBillingUsageSpendByClientForUser(userID)
}

func (r *CachedRepository) UserActualSpendTotal(userID string) int64 {
	return r.Repository.UserActualSpendTotal(userID)
}

func (r *CachedRepository) ListBillingRequestCountByDay(startedAt time.Time, days int) []BillingRequestDayCountSummary {
	return r.Repository.ListBillingRequestCountByDay(startedAt, days)
}

func (r *CachedRepository) FinanceOverviewStats(startOfDay, endOfDay time.Time) FinanceOverviewStats {
	return r.Repository.FinanceOverviewStats(startOfDay, endOfDay)
}

func (r *CachedRepository) FinanceEntityCounts() FinanceEntityCounts {
	return r.Repository.FinanceEntityCounts()
}

func (r *CachedRepository) FinanceTotalSpendNanoUSD() int64 {
	return r.Repository.FinanceTotalSpendNanoUSD()
}

func (r *CachedRepository) ListFinanceReconcileIssues(now time.Time) []FinanceReconcileIssueSummary {
	return r.Repository.ListFinanceReconcileIssues(now)
}

func (r *CachedRepository) ListPaymentIncomeByDay(startedAt time.Time, days int) []PaymentIncomeDaySummary {
	return r.Repository.ListPaymentIncomeByDay(startedAt, days)
}

func (r *CachedRepository) ListFinanceTopUsersBySpend(limit int) []FinanceUserSummary {
	return r.Repository.ListFinanceTopUsersBySpend(limit)
}

func (r *CachedRepository) ListFinanceUserSummariesPage(offset, limit int) ([]FinanceUserSummary, int) {
	return r.Repository.ListFinanceUserSummariesPage(offset, limit)
}

func (r *CachedRepository) ListUsersPage(query UserListQuery) ([]UserListItem, int, int) {
	return r.Repository.ListUsersPage(query)
}

func (r *CachedRepository) ListFinanceTopClientsBySpend(limit int) []FinanceClientSpendSummary {
	return r.Repository.ListFinanceTopClientsBySpend(limit)
}

func (r *CachedRepository) ListBillingModelUsageSummaries(limit int) []BillingModelUsageSummary {
	return r.Repository.ListBillingModelUsageSummaries(limit)
}

func (r *CachedRepository) ListTokenUsageDailySummaries(startedAt time.Time, days int, limit int) []TokenUsageDailySummary {
	return r.Repository.ListTokenUsageDailySummaries(startedAt, days, limit)
}

func (r *CachedRepository) ListTokenUsageDailyUserSummaries(startedAt time.Time, days int, limit int) []TokenUsageDailySummary {
	return r.Repository.ListTokenUsageDailyUserSummaries(startedAt, days, limit)
}

func (r *CachedRepository) ListPaymentOrders(query PaymentOrderQuery) []core.PaymentOrder {
	return r.Repository.ListPaymentOrders(query)
}

func (r *CachedRepository) ListAccounts() []core.Account {
	r.mu.RLock()
	if r.accountsLoaded {
		accounts := cloneAccounts(r.accounts)
		r.mu.RUnlock()
		return accounts
	}
	r.mu.RUnlock()
	return r.hydrateAccounts(r.Repository.ListAccounts())
}

func (r *CachedRepository) GetAccount(id string) (core.Account, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return core.Account{}, ErrNotFound
	}
	r.mu.RLock()
	account, ok := r.accountsByID[id]
	accountsLoaded := r.accountsLoaded
	r.mu.RUnlock()
	if ok {
		return cloneAccount(account), nil
	}
	if accountsLoaded {
		return core.Account{}, ErrNotFound
	}
	stored, err := r.Repository.GetAccount(id)
	if err != nil {
		return core.Account{}, err
	}
	r.hydrateAccount(stored)
	return cloneAccount(stored), nil
}

func (r *CachedRepository) UpsertAccount(account core.Account) error {
	if err := r.Repository.UpsertAccount(account); err != nil {
		return err
	}
	if stored, err := r.Repository.GetAccount(account.ID); err == nil {
		account = stored
	}
	r.cacheAccount(account)
	return nil
}

func (r *CachedRepository) UpsertRuntimeAccount(account core.Account) error {
	now := time.Now().UTC()
	if account.CreatedAt.IsZero() {
		account.CreatedAt = now
	}
	account.UpdatedAt = now
	previous, hadPrevious := r.cachedAccount(account.ID)
	if hadPrevious {
		account = core.PreserveAccountControlState(account, previous)
	}

	var err error
	if repo, ok := r.Repository.(interface {
		UpsertRuntimeAccount(core.Account) error
	}); ok {
		err = repo.UpsertRuntimeAccount(account)
	} else {
		err = r.Repository.UpsertAccount(account)
	}
	if err != nil {
		if hadPrevious {
			r.cacheAccount(previous)
		} else {
			r.deleteCachedAccount(account.ID)
		}
		return err
	}
	if stored, err := r.Repository.GetAccount(account.ID); err == nil {
		account = stored
	} else if hadPrevious {
		_, _, runtimeState := splitAccountForStorage(account)
		account = applyRuntimeStateToAccount(previous, runtimeState)
	}
	r.cacheAccount(account)
	return nil
}

func (r *CachedRepository) cachedAccount(id string) (core.Account, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	account, ok := r.accountsByID[id]
	if !ok {
		return core.Account{}, false
	}
	return cloneAccount(account), true
}

func (r *CachedRepository) DeleteAccount(id string) error {
	if err := r.Repository.DeleteAccount(id); err != nil {
		return err
	}
	r.deleteCachedAccount(id)
	return nil
}

func (r *CachedRepository) UpdateAccountStatus(id string, status core.AccountStatus) error {
	if err := r.Repository.UpdateAccountStatus(id, status); err != nil {
		return err
	}
	r.cacheAccountStatus(id, status)
	return nil
}

func (r *CachedRepository) cacheAccount(account core.Account) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cloned := cloneAccount(account)
	r.accountsByID[account.ID] = cloned
	replaced := false
	for i, existing := range r.accounts {
		if existing.ID == account.ID {
			r.accounts[i] = cloned
			replaced = true
			break
		}
	}
	if !replaced {
		r.accounts = append(r.accounts, cloned)
	}
	r.accounts = sortAccounts(r.accounts)
	r.revision.Add(1)
	r.accountRev.Add(1)
}

func (r *CachedRepository) deleteCachedAccount(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.accountsByID, id)
	for i, account := range r.accounts {
		if account.ID == id {
			r.accounts = append(r.accounts[:i], r.accounts[i+1:]...)
			break
		}
	}
	r.revision.Add(1)
	r.accountRev.Add(1)
}

func (r *CachedRepository) cacheAccountStatus(id string, status core.AccountStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()

	account, ok := r.accountsByID[id]
	if !ok {
		r.revision.Add(1)
		r.accountRev.Add(1)
		return
	}
	account.Status = status
	r.accountsByID[id] = account
	for i, existing := range r.accounts {
		if existing.ID == id {
			r.accounts[i] = account
			break
		}
	}
	r.revision.Add(1)
	r.accountRev.Add(1)
}

func (r *CachedRepository) cacheAccountGroup(group core.AccountGroup) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cloned := cloneAccountGroup(group)
	replaced := false
	for i, existing := range r.groups {
		if existing.ID == group.ID {
			r.groups[i] = cloned
			replaced = true
			break
		}
	}
	if !replaced {
		r.groups = append(r.groups, cloned)
	}
	r.groups = sortAccountGroups(r.groups)
	r.revision.Add(1)
	r.accountRev.Add(1)
}

func (r *CachedRepository) deleteCachedAccountGroup(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i, group := range r.groups {
		if group.ID == id {
			r.groups = append(r.groups[:i], r.groups[i+1:]...)
			break
		}
	}
	r.revision.Add(1)
	r.accountRev.Add(1)
}

func (r *CachedRepository) ListAccountGroups() []core.AccountGroup {
	r.mu.RLock()
	if r.groupsLoaded {
		groups := cloneAccountGroups(r.groups)
		r.mu.RUnlock()
		return groups
	}
	r.mu.RUnlock()
	return r.hydrateAccountGroups(r.Repository.ListAccountGroups())
}

func (r *CachedRepository) UpsertAccountGroup(group core.AccountGroup) error {
	if err := r.Repository.UpsertAccountGroup(group); err != nil {
		return err
	}
	r.cacheAccountGroup(group)
	return nil
}

func (r *CachedRepository) DeleteAccountGroup(id string) error {
	if err := r.Repository.DeleteAccountGroup(id); err != nil {
		return err
	}
	r.deleteCachedAccountGroup(id)
	return nil
}

func (r *CachedRepository) ListModels() []core.ModelConfig {
	r.mu.RLock()
	if r.modelsLoaded {
		models := cloneModels(r.models)
		r.mu.RUnlock()
		return models
	}
	r.mu.RUnlock()
	return r.hydrateModels(r.Repository.ListModels())
}

func (r *CachedRepository) GetModel(id string) (core.ModelConfig, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return core.ModelConfig{}, ErrNotFound
	}
	r.mu.RLock()
	model, ok := r.modelsByID[id]
	modelsLoaded := r.modelsLoaded
	r.mu.RUnlock()
	if ok {
		return cloneModel(model), nil
	}
	if modelsLoaded {
		return core.ModelConfig{}, ErrNotFound
	}
	stored, err := r.Repository.GetModel(id)
	if err != nil {
		return core.ModelConfig{}, err
	}
	r.hydrateModel(stored)
	return cloneModel(stored), nil
}

func (r *CachedRepository) cacheModel(model core.ModelConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cloned := cloneModel(model)
	r.modelsByID[model.ID] = cloned
	replaced := false
	for i, existing := range r.models {
		if existing.ID == model.ID {
			r.models[i] = cloned
			replaced = true
			break
		}
	}
	if !replaced {
		r.models = append(r.models, cloned)
	}
	r.models = sortModels(r.models)
	r.revision.Add(1)
	r.modelRev.Add(1)
}

func (r *CachedRepository) deleteCachedModel(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.modelsByID, id)
	for i, model := range r.models {
		if model.ID == id {
			r.models = append(r.models[:i], r.models[i+1:]...)
			break
		}
	}
	r.revision.Add(1)
	r.modelRev.Add(1)
}

func (r *CachedRepository) UpsertModel(model core.ModelConfig) error {
	if err := r.Repository.UpsertModel(model); err != nil {
		return err
	}
	if stored, err := r.Repository.GetModel(model.ID); err == nil {
		model = stored
	}
	r.cacheModel(model)
	return nil
}

func (r *CachedRepository) DeleteModel(id string) error {
	if err := r.Repository.DeleteModel(id); err != nil {
		return err
	}
	r.deleteCachedModel(id)
	return nil
}

func (r *CachedRepository) ListClients() []core.APIClient {
	return listCachedClientSummaries(r.Repository)
}

func (r *CachedRepository) ListClientSummariesPage(offset, limit int) ([]core.APIClient, int) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 25
	}
	if pager, ok := r.Repository.(clientSummaryPager); ok {
		clients, total := pager.ListClientSummariesPage(offset, limit)
		return cloneClientSummaries(clients), total
	}
	clients := listCachedClientSummaries(r.Repository)
	return clientSummaryPage(clients, offset, limit)
}

func (r *CachedRepository) ListClientSummariesByOwnerPage(ownerUserID string, offset, limit int) ([]core.APIClient, int) {
	ownerUserID = strings.TrimSpace(ownerUserID)
	if ownerUserID == "" {
		return nil, 0
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 25
	}
	if pager, ok := r.Repository.(clientSummaryOwnerPager); ok {
		clients, total := pager.ListClientSummariesByOwnerPage(ownerUserID, offset, limit)
		return cloneClientSummaries(clients), total
	}
	clients := r.Repository.ListClientsByOwner(ownerUserID)
	return clientSummaryPage(clients, offset, limit)
}

func (r *CachedRepository) ListClientsByOwner(ownerUserID string) []core.APIClient {
	ownerUserID = strings.TrimSpace(ownerUserID)
	if ownerUserID == "" {
		return nil
	}
	return cloneClientSummaries(r.Repository.ListClientsByOwner(ownerUserID))
}

func (r *CachedRepository) GetClient(id string) (core.APIClient, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return core.APIClient{}, ErrNotFound
	}
	r.mu.RLock()
	client, ok := r.clientsByID[id]
	r.mu.RUnlock()
	if !ok {
		stored, err := r.Repository.GetClient(id)
		if err != nil {
			return core.APIClient{}, err
		}
		r.hydrateClient(stored)
		return cloneClient(stored), nil
	}
	if strings.TrimSpace(client.APIKey) != "" {
		return cloneClient(client), nil
	}
	stored, err := r.Repository.GetClient(id)
	if err != nil {
		return core.APIClient{}, err
	}
	r.hydrateClient(stored)
	return cloneClient(stored), nil
}

func (r *CachedRepository) cacheClient(client core.APIClient) {
	r.cacheClientWithRevision(client, true)
}

func (r *CachedRepository) hydrateClient(client core.APIClient) {
	r.cacheClientWithRevision(client, false)
}

func (r *CachedRepository) cacheClientWithRevision(client core.APIClient, bumpRevision bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.clientsByID[client.ID]; ok {
		r.unindexCachedClientLocked(existing)
	}
	cloned := cloneClient(client)
	r.indexCachedClientLocked(cloned)
	replaced := false
	for i, existing := range r.clients {
		if existing.ID == client.ID {
			r.clients[i] = cloned
			replaced = true
			break
		}
	}
	if !replaced {
		r.clients = append(r.clients, cloned)
	}
	r.clients = sortClients(r.clients)
	if bumpRevision {
		r.revision.Add(1)
		r.clientRev.Add(1)
	}
}

func (r *CachedRepository) indexCachedClientLocked(client core.APIClient) {
	r.clientsByID[client.ID] = client
	if client.APIKey != "" {
		r.clientsByKey[client.APIKey] = client
	}
	if ownerID := strings.TrimSpace(client.OwnerUserID); ownerID != "" {
		r.clientsByOwner[ownerID] = sortClients(append(r.clientsByOwner[ownerID], client))
	}
}

func (r *CachedRepository) unindexCachedClientLocked(client core.APIClient) {
	if client.APIKey != "" {
		delete(r.clientsByKey, client.APIKey)
	}
	if ownerID := strings.TrimSpace(client.OwnerUserID); ownerID != "" {
		r.clientsByOwner[ownerID] = removeCachedClientFromSlice(r.clientsByOwner[ownerID], client.ID)
		if len(r.clientsByOwner[ownerID]) == 0 {
			delete(r.clientsByOwner, ownerID)
		}
	}
}

func (r *CachedRepository) deleteCachedClient(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.deleteCachedClientLocked(strings.TrimSpace(id))
	r.revision.Add(1)
	r.clientRev.Add(1)
}

func (r *CachedRepository) deleteCachedClientsByOwner(ownerUserID string) {
	ownerUserID = strings.TrimSpace(ownerUserID)
	if ownerUserID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	for id, client := range r.clientsByID {
		if strings.TrimSpace(client.OwnerUserID) == ownerUserID {
			r.deleteCachedClientLocked(id)
		}
	}
}

func (r *CachedRepository) deleteCachedClientLocked(id string) {
	if existing, ok := r.clientsByID[id]; ok {
		r.unindexCachedClientLocked(existing)
	}
	delete(r.clientsByID, id)
	for i, client := range r.clients {
		if client.ID == id {
			r.clients = append(r.clients[:i], r.clients[i+1:]...)
			break
		}
	}
}

func (r *CachedRepository) FindClientByAPIKey(apiKey string) (core.APIClient, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return core.APIClient{}, ErrNotFound
	}
	r.mu.RLock()
	client, ok := r.clientsByKey[apiKey]
	r.mu.RUnlock()
	if !ok {
		stored, err := r.Repository.FindClientByAPIKey(apiKey)
		if err != nil {
			return core.APIClient{}, err
		}
		r.hydrateClient(stored)
		return cloneClient(stored), nil
	}
	return cloneClient(client), nil
}

func (r *CachedRepository) UpsertClient(client core.APIClient) error {
	if err := r.Repository.UpsertClient(client); err != nil {
		return err
	}
	if stored, err := r.Repository.GetClient(client.ID); err == nil {
		client = stored
	}
	r.cacheClient(client)
	return nil
}

func (r *CachedRepository) DeleteClient(id string) error {
	if err := r.Repository.DeleteClient(id); err != nil {
		return err
	}
	r.deleteCachedClient(id)
	return nil
}

func (r *CachedRepository) ListClientActualSpends() []core.ClientSpend {
	return r.Repository.ListClientActualSpends()
}

func (r *CachedRepository) ListSiteMessageDeliveriesPage(userID string, includeDisabled bool, offset, limit int) ([]core.SiteMessageDelivery, int) {
	if pager, ok := r.Repository.(siteMessageDeliveryPager); ok {
		return pager.ListSiteMessageDeliveriesPage(userID, includeDisabled, offset, limit)
	}
	deliveries := r.Repository.ListSiteMessageDeliveries(userID, includeDisabled)
	return siteMessageDeliveryPage(deliveries, offset, limit)
}

func (r *CachedRepository) ListVisibleSiteMessageDeliveriesPage(query SiteMessageVisibilityQuery) ([]core.SiteMessageDelivery, int) {
	if pager, ok := r.Repository.(SiteMessageVisibleDeliveryPager); ok {
		return pager.ListVisibleSiteMessageDeliveriesPage(query)
	}
	query = normalizeSiteMessageVisibilityQuery(query)
	deliveries := r.Repository.ListSiteMessageDeliveries(query.UserID, false)
	return visibleSiteMessageDeliveriesPage(deliveries, query)
}

func (r *CachedRepository) VisibleSiteMessageUnreadCount(query SiteMessageVisibilityQuery) int {
	if pager, ok := r.Repository.(SiteMessageVisibleDeliveryPager); ok {
		return pager.VisibleSiteMessageUnreadCount(query)
	}
	query = normalizeSiteMessageVisibilityQuery(query)
	deliveries := r.Repository.ListSiteMessageDeliveries(query.UserID, false)
	return visibleSiteMessageUnreadCount(deliveries, query)
}

func (r *CachedRepository) ListDocumentsPage(status core.DocumentStatus, seoOnly bool, offset, limit int) ([]core.Document, int) {
	if pager, ok := r.Repository.(documentPager); ok {
		return pager.ListDocumentsPage(status, seoOnly, offset, limit)
	}
	return documentPage(r.Repository.ListDocuments(), status, seoOnly, offset, limit)
}

func (r *CachedRepository) SearchDocumentsPage(query string, status core.DocumentStatus, seoOnly bool, offset, limit int) ([]core.Document, int) {
	if searcher, ok := r.Repository.(documentSearcher); ok {
		return searcher.SearchDocumentsPage(query, status, seoOnly, offset, limit)
	}
	return documentSearchPage(r.Repository.ListDocuments(), query, status, seoOnly, offset, limit)
}

func (r *CachedRepository) ListMonitorTargets() []core.MonitorTarget {
	return r.Repository.ListMonitorTargets()
}

func (r *CachedRepository) GetMonitorTarget(id string) (core.MonitorTarget, error) {
	return r.Repository.GetMonitorTarget(id)
}

func (r *CachedRepository) UpsertMonitorTarget(target core.MonitorTarget) error {
	return r.Repository.UpsertMonitorTarget(target)
}

func (r *CachedRepository) DeleteMonitorTarget(id string) error {
	return r.Repository.DeleteMonitorTarget(id)
}

func (r *CachedRepository) AppendMonitorResult(result core.MonitorResult) error {
	return r.Repository.AppendMonitorResult(result)
}

func (r *CachedRepository) ListMonitorResults(targetID string, limit int) []core.MonitorResult {
	return r.Repository.ListMonitorResults(targetID, limit)
}

func (r *CachedRepository) ListLatestMonitorResults() map[string]core.MonitorResult {
	return r.Repository.ListLatestMonitorResults()
}

func (r *CachedRepository) AppendAuditBatch(events []core.AuditEvent) error {
	if batcher, ok := r.Repository.(auditBatchAppender); ok {
		return batcher.AppendAuditBatch(events)
	}
	for _, event := range events {
		if err := r.Repository.AppendAudit(event); err != nil {
			return err
		}
	}
	return nil
}

func cloneAccounts(in []core.Account) []core.Account {
	out := make([]core.Account, 0, len(in))
	for _, item := range in {
		out = append(out, cloneAccount(item))
	}
	return out
}

func cloneAccountGroups(in []core.AccountGroup) []core.AccountGroup {
	out := make([]core.AccountGroup, 0, len(in))
	for _, item := range in {
		out = append(out, cloneAccountGroup(item))
	}
	return out
}

func cloneModels(in []core.ModelConfig) []core.ModelConfig {
	out := make([]core.ModelConfig, 0, len(in))
	for _, item := range in {
		out = append(out, cloneModel(item))
	}
	return out
}

func cloneUsers(in []core.User) []core.User {
	out := make([]core.User, 0, len(in))
	for _, item := range in {
		out = append(out, cloneUser(item))
	}
	return out
}

func cloneClients(in []core.APIClient) []core.APIClient {
	out := make([]core.APIClient, 0, len(in))
	for _, item := range in {
		out = append(out, cloneClient(item))
	}
	return out
}

func cloneClientSummaries(in []core.APIClient) []core.APIClient {
	out := cloneClients(in)
	for i := range out {
		out[i].APIKey = ""
		out[i].RouteAffinityKey = ""
	}
	return out
}

type clientSummaryLister interface {
	ListClientSummaries() []core.APIClient
}

func listCachedClientSummaries(repo Repository) []core.APIClient {
	if lister, ok := repo.(clientSummaryLister); ok {
		return cloneClientSummaries(lister.ListClientSummaries())
	}
	return cloneClientSummaries(repo.ListClients())
}

func clientSummaryPage(clients []core.APIClient, offset, limit int) ([]core.APIClient, int) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 25
	}
	total := len(clients)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return cloneClientSummaries(clients[offset:end]), total
}

func siteMessageDeliveryPage(deliveries []core.SiteMessageDelivery, offset, limit int) ([]core.SiteMessageDelivery, int) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 25
	}
	total := len(deliveries)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return append([]core.SiteMessageDelivery(nil), deliveries[offset:end]...), total
}

func removeCachedUserFromSlice(users []core.User, id string) []core.User {
	id = strings.TrimSpace(id)
	for i, user := range users {
		if strings.TrimSpace(user.ID) == id {
			return append(users[:i], users[i+1:]...)
		}
	}
	return users
}

func removeCachedClientFromSlice(clients []core.APIClient, id string) []core.APIClient {
	id = strings.TrimSpace(id)
	for i, client := range clients {
		if strings.TrimSpace(client.ID) == id {
			return append(clients[:i], clients[i+1:]...)
		}
	}
	return clients
}
