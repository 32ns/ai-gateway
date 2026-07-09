package storage

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/storageerr"
)

var ErrNotFound = storageerr.ErrNotFound
var ErrInsufficientBalance = storageerr.ErrInsufficientBalance
var ErrPlanQuotaExhausted = storageerr.ErrPlanQuotaExhausted
var ErrClientSpendLimitExceeded = storageerr.ErrClientSpendLimitExceeded
var ErrBillingRequestConflict = storageerr.ErrBillingRequestConflict
var ErrAmountOverflow = storageerr.ErrAmountOverflow
var ErrBillingClientOwnerMismatch = storageerr.ErrBillingClientOwnerMismatch

const defaultAuditLimit = 512
const maxAuditSummaryMessageRunes = 4096
const maxMonitorResultsPerTarget = 2881
const monitorResultsRetentionWindow = 24 * time.Hour
const maxInt64 = int64(^uint64(0) >> 1)
const minInt64 = -maxInt64 - 1

type Repository interface {
	GetSystemSettings() (core.SystemSettings, error)
	UpsertSystemSettings(settings core.SystemSettings) error
	ConfigureAuditLimit(limit int) error
	ConfigureGatewayAuditRetention(maxAgeDays int) error
	ConfigureUsageLogRetention(maxAgeDays int) error
	ConfigureBillingLedgerRetention(maxAgeDays int) error
	ListUsers() []core.User
	ListUsersPage(query UserListQuery) ([]UserListItem, int, int)
	GetUser(id string) (core.User, error)
	FindUserByUsername(username string) (core.User, error)
	FindUserByEmail(email string) (core.User, error)
	FindUserByOAuthIdentity(provider, subject string) (core.User, error)
	FindUserByInvitationSignature(signature string) (core.User, error)
	ListUsersByInviter(inviterID string) []core.User
	CountUsersByInviter(inviterID string) int
	CountEnabledAdminsExcluding(excludedIDs []string) int
	UpsertUser(user core.User) error
	UpdateUserMetadata(user core.User) error
	MergeUsers(source core.User, target core.User) error
	DeleteUser(id string) error
	SetUserBalance(userID string, balanceNanoUSD int64) error
	AdjustUserBalance(userID string, deltaNanoUSD int64, note string) (int64, int64, error)
	UpsertUserSession(session core.UserSession) error
	GetUserSession(tokenHash string) (core.UserSession, error)
	DeleteUserSession(tokenHash string) error
	DeleteUserSessionsByUser(userID string) error
	DeleteExpiredUserSessions(now time.Time) error
	UpsertMCPToken(token core.MCPToken) error
	GetMCPToken(id string) (core.MCPToken, error)
	GetMCPTokenByHash(tokenHash string) (core.MCPToken, error)
	ListMCPTokens(ownerUserID string) []core.MCPToken
	DeleteMCPToken(id string) error
	CreateEmailVerificationCode(code core.EmailVerificationCode) error
	LatestEmailVerificationCode(purpose, email string) (core.EmailVerificationCode, error)
	CountEmailVerificationCodesSince(purpose, email string, since time.Time) int
	UpdateEmailVerificationCode(code core.EmailVerificationCode) error
	DeleteEmailVerificationCode(id string) error
	CreatePasswordResetToken(token core.PasswordResetToken) error
	GetPasswordResetTokenByHash(tokenHash string) (core.PasswordResetToken, error)
	LatestPasswordResetToken(email string) (core.PasswordResetToken, error)
	CountPasswordResetTokensSince(email string, since time.Time) int
	UpdatePasswordResetToken(token core.PasswordResetToken) error
	DeletePasswordResetToken(id string) error
	ListAccounts() []core.Account
	GetAccount(id string) (core.Account, error)
	UpsertAccount(account core.Account) error
	DeleteAccount(id string) error
	UpdateAccountStatus(id string, status core.AccountStatus) error
	ListAccountGroups() []core.AccountGroup
	UpsertAccountGroup(group core.AccountGroup) error
	DeleteAccountGroup(id string) error
	ListModels() []core.ModelConfig
	GetModel(id string) (core.ModelConfig, error)
	UpsertModel(model core.ModelConfig) error
	DeleteModel(id string) error
	ListClients() []core.APIClient
	ListClientsByOwner(ownerUserID string) []core.APIClient
	GetClient(id string) (core.APIClient, error)
	FindClientByAPIKey(apiKey string) (core.APIClient, error)
	UpsertClient(client core.APIClient) error
	DeleteClient(id string) error
	UpsertOpenAIResponseBinding(binding core.OpenAIResponseBinding) error
	GetOpenAIResponseBinding(responseID string) (core.OpenAIResponseBinding, error)
	GetClientSpend(clientID string) (core.ClientSpend, error)
	GetClientActualSpend(clientID string) (core.ClientSpend, error)
	ReserveBilling(input core.BillingReservationInput) (core.BillingReservation, error)
	UpdateBillingAccount(input core.BillingAccountUpdateInput) error
	SettleBilling(input core.BillingSettlementInput) (core.BillingSettlement, error)
	ReleaseBilling(input core.BillingReleaseInput) error
	ListBillingLedger(userID string, limit int) []core.BillingLedgerEntry
	ListBillingPlanGroups() []core.BillingPlanGroup
	GetBillingPlanGroup(id string) (core.BillingPlanGroup, error)
	UpsertBillingPlanGroup(group core.BillingPlanGroup) error
	DeleteBillingPlanGroup(id string) error
	ListBillingPlans() []core.BillingPlan
	GetBillingPlan(id string) (core.BillingPlan, error)
	UpsertBillingPlan(plan core.BillingPlan) error
	DeleteBillingPlan(id string) error
	PurchaseBillingPlan(input core.BillingPlanPurchaseInput) (core.BillingPlanPurchase, error)
	GrantBillingPlan(input core.BillingPlanGrantInput) (core.BillingPlanPurchase, error)
	ListUserPlanEntitlements(userID string) []core.UserPlanEntitlement
	GetActiveUserPlanEntitlement(userID string) (core.UserPlanEntitlement, error)
	MoveUserPlanEntitlementPriority(userID, entitlementID, direction string) error
	CancelUserPlanEntitlement(userID, entitlementID string) (core.UserPlanEntitlement, error)
	PlanSubscriptionStats(query PlanSubscriptionQuery) PlanSubscriptionStats
	ListPlanSubscriptionPlanSummaries(query PlanSubscriptionQuery, limit int) []PlanSubscriptionPlanSummary
	ListPlanSubscriptionSummariesPage(query PlanSubscriptionQuery) ([]PlanSubscriptionSummary, int)
	PlanQuotaUsageStats(query PlanQuotaUsageQuery) PlanQuotaUsageStats
	ListPlanQuotaUsageByDay(query PlanQuotaUsageQuery) ([]PlanQuotaUsageDaySummary, int)
	ListPlanQuotaUsageEvents(query PlanQuotaUsageQuery) []PlanQuotaUsageEvent
	BillingUsageSpendNanoUSD(startedAt, endedAt time.Time) int64
	ListBillingUsageSpendByDay(startedAt time.Time, days int) []BillingUsageSpendDaySummary
	ListBillingUsageSpendByHourForUser(userID string, startedAt, endedAt time.Time) []BillingUsageSpendHourSummary
	ListBillingUsageSpendByClient() []BillingUsageSpendSummary
	ListBillingUsageSpendByClientForUser(userID string) []BillingUsageSpendSummary
	ListBillingLedgerUserSummaries() []BillingLedgerUserSummary
	ListBillingRequestsPage(query BillingRequestQuery) ([]core.BillingReservation, int)
	ListBillingRequests(query BillingRequestQuery) []core.BillingReservation
	ListBillingRequestCountByDay(startedAt time.Time, days int) []BillingRequestDayCountSummary
	ListBillingModelUsageSummaries(limit int) []BillingModelUsageSummary
	ListTokenUsageDailySummaries(startedAt time.Time, days int, limit int) []TokenUsageDailySummary
	ListTokenUsageDailyUserSummaries(startedAt time.Time, days int, limit int) []TokenUsageDailySummary
	FinanceOverviewStats(startOfDay, endOfDay time.Time) FinanceOverviewStats
	FinanceEntityCounts() FinanceEntityCounts
	FinanceTotalSpendNanoUSD() int64
	ListFinanceReconcileIssues(now time.Time) []FinanceReconcileIssueSummary
	ListPaymentIncomeByDay(startedAt time.Time, days int) []PaymentIncomeDaySummary
	ListFinanceTopUsersBySpend(limit int) []FinanceUserSummary
	ListFinanceUserSummariesPage(offset, limit int) ([]FinanceUserSummary, int)
	ListFinanceTopClientsBySpend(limit int) []FinanceClientSpendSummary
	CreatePaymentOrder(order core.PaymentOrder) error
	GetPaymentOrder(id string) (core.PaymentOrder, error)
	GetPaymentOrderByOutTradeNo(outTradeNo string) (core.PaymentOrder, error)
	ListPaymentOrdersPage(query PaymentOrderQuery) ([]core.PaymentOrder, int)
	ListPaymentOrders(query PaymentOrderQuery) []core.PaymentOrder
	UpdatePaymentOrderStatus(id string, status core.PaymentOrderStatus, providerTradeNo string, paidAt *time.Time) (core.PaymentOrder, error)
	UpdatePaymentOrderProviderState(id string, update core.PaymentOrderProviderUpdate) (core.PaymentOrder, error)
	DeletePendingPaymentOrder(id string) (core.PaymentOrder, bool, error)
	CompletePaymentOrder(outTradeNo, providerTradeNo string, paidAmountNanoUSD int64, paidAt time.Time) (core.PaymentOrder, bool, error)
	CompletePaymentOrderWithCredits(outTradeNo, providerTradeNo string, paidAmountNanoUSD int64, paidAt time.Time, credits []core.PaymentOrderBalanceCredit) (core.PaymentOrder, bool, error)
	CreatePaymentRefund(refund core.PaymentRefund) error
	CompletePaymentRefund(id, providerRefundNo, rawResponse string) (core.PaymentRefund, bool, error)
	FailPaymentRefund(id, rawResponse string) (core.PaymentRefund, error)
	ListPaymentRefunds(orderID string) []core.PaymentRefund
	ListClientActualSpends() []core.ClientSpend
	UserActualSpendTotal(userID string) int64
	CreateSiteMessage(message core.SiteMessage) error
	UpdateSiteMessage(message core.SiteMessage) error
	GetSiteMessage(id string) (core.SiteMessage, error)
	DeleteSiteMessage(id string) error
	ListSiteMessages() []core.SiteMessage
	ListSiteMessageDeliveries(userID string, includeDisabled bool) []core.SiteMessageDelivery
	MarkSiteMessageRead(messageID, userID string, readAt time.Time) error
	ClearSiteMessageReads(messageID string) error
	SiteMessageUnreadCount(userID string) int
	CreateSupportTicket(ticket core.SupportTicket, firstMessage core.SupportMessage) error
	GetSupportTicket(id string) (core.SupportTicket, error)
	ListSupportTicketsPage(query SupportTicketQuery) ([]core.SupportTicket, int)
	ListSupportMessages(ticketID string, limit int) []core.SupportMessage
	AppendSupportMessage(ticketID string, message core.SupportMessage, ticket core.SupportTicket) (core.SupportTicket, error)
	DeleteSupportTicket(ticketID string) error
	MarkSupportTicketRead(ticketID string, user core.User, readAt time.Time) (core.SupportTicket, error)
	SupportUnreadCount(user core.User) int
	CreateDocument(document core.Document) error
	UpdateDocument(document core.Document) error
	GetDocument(id string) (core.Document, error)
	GetDocumentBySlug(slug string) (core.Document, error)
	DeleteDocument(id string) error
	ListDocuments() []core.Document
	GetDocumentRedirect(fromSlug string) (core.DocumentRedirect, error)
	UpsertDocumentRedirect(redirect core.DocumentRedirect) error
	DeleteDocumentRedirect(fromSlug string) error
	ListMonitorTargets() []core.MonitorTarget
	GetMonitorTarget(id string) (core.MonitorTarget, error)
	UpsertMonitorTarget(target core.MonitorTarget) error
	DeleteMonitorTarget(id string) error
	AppendMonitorResult(result core.MonitorResult) error
	ListMonitorResults(targetID string, limit int) []core.MonitorResult
	ListLatestMonitorResults() map[string]core.MonitorResult
	AppendAudit(event core.AuditEvent) error
	ListAudit(limit int) []core.AuditEvent
	ListAuditSummaries(limit int) []core.AuditEvent
	ListAuditSummariesPage(query AuditQuery) ([]core.AuditEvent, int)
}

type SupportTicketQuery struct {
	UserID string
	Status core.SupportTicketStatus
	Query  string
	Offset int
	Limit  int
}

type StartupSystemSettingsLoader interface {
	GetStartupSystemSettings() (core.SystemSettings, error)
}

func LoadStartupSystemSettings(repo Repository) (core.SystemSettings, error) {
	if repo == nil {
		return core.DefaultSystemSettings(), nil
	}
	if loader, ok := repo.(StartupSystemSettingsLoader); ok {
		return loader.GetStartupSystemSettings()
	}
	return repo.GetSystemSettings()
}

type MemoryRepository struct {
	mu                 sync.RWMutex
	accounts           map[string]core.Account
	users              map[string]core.User
	sessions           map[string]core.UserSession
	mcpTokens          map[string]core.MCPToken
	mcpTokenHash       map[string]string
	emailCodes         map[string]core.EmailVerificationCode
	passwordResets     map[string]core.PasswordResetToken
	passwordResetHash  map[string]string
	groups             map[string]core.AccountGroup
	models             map[string]core.ModelConfig
	clients            map[string]core.APIClient
	responses          map[string]core.OpenAIResponseBinding
	clientSpend        map[string]core.ClientSpend
	billing            map[string]core.BillingReservation
	planGroups         map[string]core.BillingPlanGroup
	plans              map[string]core.BillingPlan
	entitlements       map[string]core.UserPlanEntitlement
	allocations        map[string]core.BillingFundingAllocation
	planLedger         []core.PlanQuotaLedgerEntry
	ledger             []core.BillingLedgerEntry
	payments           map[string]core.PaymentOrder
	refunds            map[string]core.PaymentRefund
	messages           map[string]core.SiteMessage
	messageRead        map[string]core.SiteMessageRead
	supportTickets     map[string]core.SupportTicket
	supportMessages    map[string][]core.SupportMessage
	documents          map[string]core.Document
	docRedirects       map[string]core.DocumentRedirect
	monitorTargets     map[string]core.MonitorTarget
	monitorResults     []core.MonitorResult
	audit              []core.AuditEvent
	limit              int
	usageMaxAge        time.Duration
	ledgerMaxAge       time.Duration
	gatewayAuditMaxAge time.Duration
	settings           core.SystemSettings
	hasSettings        bool
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{
		accounts:          make(map[string]core.Account),
		users:             make(map[string]core.User),
		sessions:          make(map[string]core.UserSession),
		mcpTokens:         make(map[string]core.MCPToken),
		mcpTokenHash:      make(map[string]string),
		emailCodes:        make(map[string]core.EmailVerificationCode),
		passwordResets:    make(map[string]core.PasswordResetToken),
		passwordResetHash: make(map[string]string),
		groups:            make(map[string]core.AccountGroup),
		models:            make(map[string]core.ModelConfig),
		clients:           make(map[string]core.APIClient),
		responses:         make(map[string]core.OpenAIResponseBinding),
		clientSpend:       make(map[string]core.ClientSpend),
		billing:           make(map[string]core.BillingReservation),
		planGroups:        make(map[string]core.BillingPlanGroup),
		plans:             make(map[string]core.BillingPlan),
		entitlements:      make(map[string]core.UserPlanEntitlement),
		allocations:       make(map[string]core.BillingFundingAllocation),
		planLedger:        make([]core.PlanQuotaLedgerEntry, 0),
		ledger:            make([]core.BillingLedgerEntry, 0),
		payments:          make(map[string]core.PaymentOrder),
		refunds:           make(map[string]core.PaymentRefund),
		messages:          make(map[string]core.SiteMessage),
		messageRead:       make(map[string]core.SiteMessageRead),
		supportTickets:    make(map[string]core.SupportTicket),
		supportMessages:   make(map[string][]core.SupportMessage),
		documents:         make(map[string]core.Document),
		docRedirects:      make(map[string]core.DocumentRedirect),
		monitorTargets:    make(map[string]core.MonitorTarget),
		monitorResults:    make([]core.MonitorResult, 0),
		audit:             make([]core.AuditEvent, 0, defaultAuditLimit),
		limit:             defaultAuditLimit,
		usageMaxAge:       defaultUsageLogMaxAge,
		ledgerMaxAge:      defaultBillingLedgerRetentionAge,
	}
}

func (r *MemoryRepository) ConfigureAuditLimit(limit int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.limit = normalizeAuditLimit(limit)
	r.trimAuditLocked()
	return nil
}

func (r *MemoryRepository) ConfigureGatewayAuditRetention(maxAgeDays int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.gatewayAuditMaxAge = normalizeGatewayAuditRetentionAge(maxAgeDays)
	r.trimGatewayAuditLocked(time.Now().UTC())
	return nil
}

func (r *MemoryRepository) ConfigureUsageLogRetention(maxAgeDays int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.usageMaxAge = normalizeUsageLogMaxAge(maxAgeDays)
	r.trimBillingRequestsLocked(time.Now().UTC())
	return nil
}

func (r *MemoryRepository) ConfigureBillingLedgerRetention(maxAgeDays int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.ledgerMaxAge = normalizeBillingLedgerRetentionAge(maxAgeDays)
	r.trimBillingLedgerLocked(time.Now().UTC())
	return nil
}

func (r *MemoryRepository) GetSystemSettings() (core.SystemSettings, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if !r.hasSettings {
		return core.DefaultSystemSettings(), nil
	}
	return core.NormalizeSystemSettings(r.settings), nil
}

func (r *MemoryRepository) GetStartupSystemSettings() (core.SystemSettings, error) {
	return r.GetSystemSettings()
}

func (r *MemoryRepository) UpsertSystemSettings(settings core.SystemSettings) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	settings = core.NormalizeSystemSettings(settings)
	settings.UpdatedAt = time.Now().UTC()
	r.settings = settings
	r.hasSettings = true
	return nil
}

func (r *MemoryRepository) ListUsers() []core.User {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]core.User, 0, len(r.users))
	for _, user := range r.users {
		out = append(out, cloneUser(user))
	}
	return sortUsers(out)
}

func (r *MemoryRepository) GetUser(id string) (core.User, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	user, ok := r.users[id]
	if !ok {
		return core.User{}, ErrNotFound
	}
	return cloneUser(user), nil
}

func (r *MemoryRepository) FindUserByUsername(username string) (core.User, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	username = strings.TrimSpace(username)
	for _, user := range r.users {
		if strings.EqualFold(user.Username, username) {
			return cloneUser(user), nil
		}
	}
	return core.User{}, ErrNotFound
}

func (r *MemoryRepository) UpsertUser(user core.User) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing, exists := r.users[user.ID]
	if !exists && user.BalanceNanoUSD < 0 {
		return ErrInsufficientBalance
	}
	username := strings.TrimSpace(user.Username)
	for existingID, existing := range r.users {
		if existingID != user.ID && strings.EqualFold(strings.TrimSpace(existing.Username), username) {
			return fmt.Errorf("username already exists")
		}
	}
	now := time.Now().UTC()
	if user.CreatedAt.IsZero() {
		user.CreatedAt = now
	}
	if exists {
		user.BalanceNanoUSD = existing.BalanceNanoUSD
		user.CreatedAt = existing.CreatedAt
	}
	user.UpdatedAt = now
	r.users[user.ID] = cloneUser(user)
	return nil
}

func (r *MemoryRepository) UpdateUserMetadata(user core.User) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	user.ID = strings.TrimSpace(user.ID)
	if user.ID == "" {
		return ErrNotFound
	}
	existing, ok := r.users[user.ID]
	if !ok {
		return ErrNotFound
	}
	username := strings.TrimSpace(user.Username)
	for existingID, existing := range r.users {
		if existingID != user.ID && strings.EqualFold(strings.TrimSpace(existing.Username), username) {
			return fmt.Errorf("username already exists")
		}
	}
	now := time.Now().UTC()
	user.BalanceNanoUSD = existing.BalanceNanoUSD
	user.CreatedAt = existing.CreatedAt
	user.UpdatedAt = now
	r.users[user.ID] = cloneUser(user)
	return nil
}

func (r *MemoryRepository) TouchUserLastUsedAt(userID string, usedAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	userID = strings.TrimSpace(userID)
	if userID == "" {
		return ErrNotFound
	}
	user, ok := r.users[userID]
	if !ok {
		return ErrNotFound
	}
	if usedAt.IsZero() {
		usedAt = time.Now().UTC()
	} else {
		usedAt = usedAt.UTC()
	}
	if user.LastLoginAt != nil && !usedAt.After(*user.LastLoginAt) {
		return nil
	}
	user.LastLoginAt = &usedAt
	r.users[userID] = cloneUser(user)
	return nil
}

func (r *MemoryRepository) MergeUsers(source core.User, target core.User) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	source.ID = strings.TrimSpace(source.ID)
	target.ID = strings.TrimSpace(target.ID)
	if source.ID == "" || target.ID == "" || source.ID == target.ID {
		return fmt.Errorf("source and target users are required")
	}
	if target.BalanceNanoUSD < 0 || source.BalanceNanoUSD < 0 {
		return ErrInsufficientBalance
	}
	if _, ok := r.users[source.ID]; !ok {
		return ErrNotFound
	}
	previousSource := r.users[source.ID]
	previousTarget, ok := r.users[target.ID]
	if !ok {
		return ErrNotFound
	}
	now := time.Now().UTC()
	if err := r.mergeUserPlanEntitlementsLocked(source.ID, target.ID, now); err != nil {
		return err
	}
	source.UpdatedAt = now
	target.UpdatedAt = now
	r.users[source.ID] = cloneUser(source)
	r.users[target.ID] = cloneUser(target)
	for tokenHash, session := range r.sessions {
		if session.UserID == source.ID {
			delete(r.sessions, tokenHash)
		}
	}
	for id, client := range r.clients {
		if strings.TrimSpace(client.OwnerUserID) == source.ID {
			client.OwnerUserID = target.ID
			client.UpdatedAt = now
			r.clients[id] = cloneClient(client)
		}
	}
	for id, user := range r.users {
		if id == target.ID || strings.TrimSpace(user.InviterUserID) != source.ID {
			continue
		}
		user.InviterUserID = target.ID
		user.UpdatedAt = now
		r.users[id] = cloneUser(user)
	}
	for id, message := range r.messages {
		message.TargetUserIDs = replaceUserIDList(message.TargetUserIDs, source.ID, target.ID)
		r.messages[id] = cloneSiteMessage(message)
	}
	for key, read := range r.messageRead {
		if strings.TrimSpace(read.UserID) != source.ID {
			continue
		}
		delete(r.messageRead, key)
		read.UserID = target.ID
		nextKey := siteMessageReadKey(read.MessageID, target.ID)
		if existing, ok := r.messageRead[nextKey]; !ok || read.ReadAt.After(existing.ReadAt) {
			r.messageRead[nextKey] = read
		}
	}
	transferAmount := previousSource.BalanceNanoUSD
	if transferAmount > 0 && target.BalanceNanoUSD != previousTarget.BalanceNanoUSD {
		r.ledger = append([]core.BillingLedgerEntry{{
			ID:                  billingLedgerID("account_merge", source.ID, target.ID, now),
			UserID:              target.ID,
			Kind:                "account_merge",
			AmountNanoUSD:       transferAmount,
			BalanceAfterNanoUSD: target.BalanceNanoUSD,
			Note:                previousSource.Username,
			CreatedAt:           now,
		}}, r.ledger...)
	}
	return nil
}

func (r *MemoryRepository) mergeUserPlanEntitlementsLocked(sourceID, targetID string, now time.Time) error {
	_, sourceActive := r.activeUserPlanEntitlementLocked(sourceID, now)
	_, targetActive := r.activeUserPlanEntitlementLocked(targetID, now)
	if sourceActive && targetActive {
		return ErrBillingRequestConflict
	}
	for id, entitlement := range r.entitlements {
		if strings.TrimSpace(entitlement.UserID) != sourceID {
			continue
		}
		entitlement.UserID = targetID
		entitlement.UpdatedAt = now
		r.entitlements[id] = cloneUserPlanEntitlement(entitlement)
	}
	return nil
}

func (r *MemoryRepository) SetUserBalance(userID string, balanceNanoUSD int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if balanceNanoUSD < 0 {
		return ErrInsufficientBalance
	}
	user, ok := r.users[userID]
	if !ok {
		return ErrNotFound
	}
	user.BalanceNanoUSD = balanceNanoUSD
	user.UpdatedAt = time.Now().UTC()
	r.users[userID] = cloneUser(user)
	return nil
}

func (r *MemoryRepository) AdjustUserBalance(userID string, deltaNanoUSD int64, note string) (int64, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	user, ok := r.users[userID]
	if !ok {
		return 0, 0, ErrNotFound
	}
	nextBalance, err := addNanoUSD(user.BalanceNanoUSD, deltaNanoUSD)
	if err != nil {
		return user.BalanceNanoUSD, user.BalanceNanoUSD, err
	}
	if nextBalance < 0 {
		return user.BalanceNanoUSD, user.BalanceNanoUSD, ErrInsufficientBalance
	}
	now := time.Now().UTC()
	previousBalance := user.BalanceNanoUSD
	user.BalanceNanoUSD = nextBalance
	user.UpdatedAt = now
	r.users[userID] = cloneUser(user)

	kind := "manual_credit"
	if deltaNanoUSD < 0 {
		kind = "manual_debit"
	}
	r.ledger = append([]core.BillingLedgerEntry{{
		ID:                  billingLedgerID(kind, "manual", userID, now),
		UserID:              userID,
		Kind:                kind,
		AmountNanoUSD:       deltaNanoUSD,
		BalanceAfterNanoUSD: nextBalance,
		Note:                strings.TrimSpace(note),
		CreatedAt:           now,
	}}, r.ledger...)
	return previousBalance, nextBalance, nil
}

func (r *MemoryRepository) DeleteUser(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	if _, ok := r.users[id]; !ok {
		return ErrNotFound
	}
	return r.deleteUserLocked(id, now)
}

func (r *MemoryRepository) deleteUserLocked(id string, now time.Time) error {
	userEmail := ""
	if user, ok := r.users[id]; ok {
		userEmail = emailKey(user.Email)
	}
	ownedClientIDs := map[string]struct{}{}
	for clientID, client := range r.clients {
		if strings.TrimSpace(client.OwnerUserID) == id {
			ownedClientIDs[clientID] = struct{}{}
		}
	}
	if err := r.releaseReservedBillingForDeletedDataLocked(id, ownedClientIDs, now); err != nil {
		return err
	}
	r.cancelUserPlanEntitlementsLocked(id, now)
	delete(r.users, id)
	for clientID := range ownedClientIDs {
		delete(r.clients, clientID)
		delete(r.clientSpend, clientID)
	}
	for messageID, message := range r.messages {
		nextTargets := replaceUserIDList(message.TargetUserIDs, id, "")
		if slices.Equal(nextTargets, message.TargetUserIDs) {
			continue
		}
		message.TargetUserIDs = nextTargets
		message.UpdatedAt = now
		r.messages[messageID] = cloneSiteMessage(message)
	}
	for key, read := range r.messageRead {
		if read.UserID == id {
			delete(r.messageRead, key)
		}
	}
	for tokenHash, session := range r.sessions {
		if session.UserID == id {
			delete(r.sessions, tokenHash)
		}
	}
	for resetID, token := range r.passwordResets {
		if strings.TrimSpace(token.UserID) == id {
			delete(r.passwordResetHash, token.TokenHash)
			delete(r.passwordResets, resetID)
		}
	}
	for tokenID, token := range r.mcpTokens {
		if token.OwnerUserID == id {
			delete(r.mcpTokenHash, token.TokenHash)
			delete(r.mcpTokens, tokenID)
		}
	}
	for responseID, binding := range r.responses {
		if _, ok := ownedClientIDs[binding.ClientID]; ok {
			delete(r.responses, responseID)
		}
	}
	if userEmail != "" {
		for codeID, code := range r.emailCodes {
			if emailKey(code.Email) == userEmail {
				delete(r.emailCodes, codeID)
			}
		}
	}
	return nil
}

func (r *MemoryRepository) cancelUserPlanEntitlementsLocked(userID string, now time.Time) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return
	}
	entries := make([]core.PlanQuotaLedgerEntry, 0)
	for id, entitlement := range r.entitlements {
		if strings.TrimSpace(entitlement.UserID) != userID || entitlement.Status != core.UserPlanEntitlementActive {
			continue
		}
		entitlement, entry, hasEntry := cancelUserPlanEntitlement(entitlement, now, len(entries))
		r.entitlements[id] = cloneUserPlanEntitlement(entitlement)
		if hasEntry {
			entries = append(entries, entry)
		}
	}
	r.prependPlanLedgerLocked(entries)
}

func (r *MemoryRepository) UpsertUserSession(session core.UserSession) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	if session.CreatedAt.IsZero() {
		session.CreatedAt = now
	}
	session.UpdatedAt = now
	r.sessions[session.TokenHash] = cloneUserSession(session)
	return nil
}

func (r *MemoryRepository) GetUserSession(tokenHash string) (core.UserSession, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	session, ok := r.sessions[tokenHash]
	if !ok {
		return core.UserSession{}, ErrNotFound
	}
	return cloneUserSession(session), nil
}

func (r *MemoryRepository) DeleteUserSession(tokenHash string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.sessions[tokenHash]; !ok {
		return ErrNotFound
	}
	delete(r.sessions, tokenHash)
	return nil
}

func (r *MemoryRepository) DeleteUserSessionsByUser(userID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	userID = strings.TrimSpace(userID)
	if userID == "" {
		return ErrNotFound
	}
	found := false
	for tokenHash, session := range r.sessions {
		if strings.TrimSpace(session.UserID) == userID {
			delete(r.sessions, tokenHash)
			found = true
		}
	}
	if !found {
		if _, ok := r.users[userID]; !ok {
			return ErrNotFound
		}
	}
	return nil
}

func (r *MemoryRepository) DeleteExpiredUserSessions(now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	now = now.UTC()
	for tokenHash, session := range r.sessions {
		if !session.ExpiresAt.IsZero() && !session.ExpiresAt.After(now) {
			delete(r.sessions, tokenHash)
		}
	}
	return nil
}

func (r *MemoryRepository) UpsertMCPToken(token core.MCPToken) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	token.ID = strings.TrimSpace(token.ID)
	token.TokenHash = strings.TrimSpace(token.TokenHash)
	token.OwnerUserID = strings.TrimSpace(token.OwnerUserID)
	if token.ID == "" || token.TokenHash == "" || token.OwnerUserID == "" {
		return fmt.Errorf("mcp token id, hash, and owner are required")
	}
	now := time.Now().UTC()
	if token.CreatedAt.IsZero() {
		token.CreatedAt = now
	}
	token.UpdatedAt = now
	if existing, ok := r.mcpTokens[token.ID]; ok && existing.TokenHash != token.TokenHash {
		delete(r.mcpTokenHash, existing.TokenHash)
	}
	for hash, tokenID := range r.mcpTokenHash {
		if hash == token.TokenHash && tokenID != token.ID {
			return fmt.Errorf("mcp token hash already exists")
		}
	}
	r.mcpTokens[token.ID] = cloneMCPToken(token)
	r.mcpTokenHash[token.TokenHash] = token.ID
	return nil
}

func (r *MemoryRepository) GetMCPToken(id string) (core.MCPToken, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	token, ok := r.mcpTokens[strings.TrimSpace(id)]
	if !ok {
		return core.MCPToken{}, ErrNotFound
	}
	return cloneMCPToken(token), nil
}

func (r *MemoryRepository) GetMCPTokenByHash(tokenHash string) (core.MCPToken, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	id, ok := r.mcpTokenHash[strings.TrimSpace(tokenHash)]
	if !ok {
		return core.MCPToken{}, ErrNotFound
	}
	token, ok := r.mcpTokens[id]
	if !ok {
		return core.MCPToken{}, ErrNotFound
	}
	return cloneMCPToken(token), nil
}

func (r *MemoryRepository) ListMCPTokens(ownerUserID string) []core.MCPToken {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ownerUserID = strings.TrimSpace(ownerUserID)
	out := make([]core.MCPToken, 0, len(r.mcpTokens))
	for _, token := range r.mcpTokens {
		if ownerUserID != "" && token.OwnerUserID != ownerUserID {
			continue
		}
		out = append(out, cloneMCPToken(token))
	}
	slices.SortFunc(out, func(a, b core.MCPToken) int {
		if !a.CreatedAt.Equal(b.CreatedAt) {
			if a.CreatedAt.After(b.CreatedAt) {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

func (r *MemoryRepository) DeleteMCPToken(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	id = strings.TrimSpace(id)
	token, ok := r.mcpTokens[id]
	if !ok {
		return ErrNotFound
	}
	delete(r.mcpTokenHash, token.TokenHash)
	delete(r.mcpTokens, id)
	return nil
}

func (r *MemoryRepository) CreateEmailVerificationCode(code core.EmailVerificationCode) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	code.ID = strings.TrimSpace(code.ID)
	code.Purpose = strings.TrimSpace(code.Purpose)
	code.Email = strings.ToLower(strings.TrimSpace(code.Email))
	if code.ID == "" || code.Purpose == "" || code.Email == "" {
		return fmt.Errorf("email verification code is incomplete")
	}
	if code.CreatedAt.IsZero() {
		code.CreatedAt = now
	}
	code.UpdatedAt = now
	r.emailCodes[code.ID] = cloneEmailVerificationCode(code)
	return nil
}

func (r *MemoryRepository) LatestEmailVerificationCode(purpose, email string) (core.EmailVerificationCode, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	purpose = strings.TrimSpace(purpose)
	email = strings.ToLower(strings.TrimSpace(email))
	var latest core.EmailVerificationCode
	for _, code := range r.emailCodes {
		if code.Purpose != purpose || strings.ToLower(strings.TrimSpace(code.Email)) != email {
			continue
		}
		if latest.ID == "" || code.CreatedAt.After(latest.CreatedAt) {
			latest = code
		}
	}
	if latest.ID == "" {
		return core.EmailVerificationCode{}, ErrNotFound
	}
	return cloneEmailVerificationCode(latest), nil
}

func (r *MemoryRepository) CountEmailVerificationCodesSince(purpose, email string, since time.Time) int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	purpose = strings.TrimSpace(purpose)
	email = strings.ToLower(strings.TrimSpace(email))
	count := 0
	for _, code := range r.emailCodes {
		if code.Purpose == purpose && strings.ToLower(strings.TrimSpace(code.Email)) == email && !code.CreatedAt.Before(since) {
			count++
		}
	}
	return count
}

func (r *MemoryRepository) UpdateEmailVerificationCode(code core.EmailVerificationCode) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	code.ID = strings.TrimSpace(code.ID)
	if code.ID == "" {
		return fmt.Errorf("email verification code id is required")
	}
	if _, ok := r.emailCodes[code.ID]; !ok {
		return ErrNotFound
	}
	code.Email = strings.ToLower(strings.TrimSpace(code.Email))
	code.UpdatedAt = time.Now().UTC()
	r.emailCodes[code.ID] = cloneEmailVerificationCode(code)
	return nil
}

func (r *MemoryRepository) DeleteEmailVerificationCode(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("email verification code id is required")
	}
	if _, ok := r.emailCodes[id]; !ok {
		return ErrNotFound
	}
	delete(r.emailCodes, id)
	return nil
}

func (r *MemoryRepository) CreatePasswordResetToken(token core.PasswordResetToken) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	token.ID = strings.TrimSpace(token.ID)
	token.UserID = strings.TrimSpace(token.UserID)
	token.Email = strings.ToLower(strings.TrimSpace(token.Email))
	token.TokenHash = strings.TrimSpace(token.TokenHash)
	if token.ID == "" || token.UserID == "" || token.Email == "" || token.TokenHash == "" {
		return fmt.Errorf("password reset token is incomplete")
	}
	if _, ok := r.users[token.UserID]; !ok {
		return ErrNotFound
	}
	if token.ExpiresAt.IsZero() {
		return fmt.Errorf("password reset token expiry is required")
	}
	if token.CreatedAt.IsZero() {
		token.CreatedAt = now
	}
	token.UpdatedAt = now
	if existingID, ok := r.passwordResetHash[token.TokenHash]; ok && existingID != token.ID {
		return fmt.Errorf("password reset token hash already exists")
	}
	if existing, ok := r.passwordResets[token.ID]; ok && existing.TokenHash != token.TokenHash {
		delete(r.passwordResetHash, existing.TokenHash)
	}
	r.passwordResets[token.ID] = clonePasswordResetToken(token)
	r.passwordResetHash[token.TokenHash] = token.ID
	return nil
}

func (r *MemoryRepository) GetPasswordResetTokenByHash(tokenHash string) (core.PasswordResetToken, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	id, ok := r.passwordResetHash[strings.TrimSpace(tokenHash)]
	if !ok {
		return core.PasswordResetToken{}, ErrNotFound
	}
	token, ok := r.passwordResets[id]
	if !ok {
		return core.PasswordResetToken{}, ErrNotFound
	}
	return clonePasswordResetToken(token), nil
}

func (r *MemoryRepository) LatestPasswordResetToken(email string) (core.PasswordResetToken, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	email = strings.ToLower(strings.TrimSpace(email))
	var latest core.PasswordResetToken
	for _, token := range r.passwordResets {
		if strings.ToLower(strings.TrimSpace(token.Email)) != email {
			continue
		}
		if latest.ID == "" || token.CreatedAt.After(latest.CreatedAt) {
			latest = token
		}
	}
	if latest.ID == "" {
		return core.PasswordResetToken{}, ErrNotFound
	}
	return clonePasswordResetToken(latest), nil
}

func (r *MemoryRepository) CountPasswordResetTokensSince(email string, since time.Time) int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	email = strings.ToLower(strings.TrimSpace(email))
	count := 0
	for _, token := range r.passwordResets {
		if strings.ToLower(strings.TrimSpace(token.Email)) == email && !token.CreatedAt.Before(since) {
			count++
		}
	}
	return count
}

func (r *MemoryRepository) UpdatePasswordResetToken(token core.PasswordResetToken) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	token.ID = strings.TrimSpace(token.ID)
	if token.ID == "" {
		return fmt.Errorf("password reset token id is required")
	}
	existing, ok := r.passwordResets[token.ID]
	if !ok {
		return ErrNotFound
	}
	token.UserID = strings.TrimSpace(token.UserID)
	token.Email = strings.ToLower(strings.TrimSpace(token.Email))
	token.TokenHash = strings.TrimSpace(token.TokenHash)
	if token.UserID == "" || token.Email == "" || token.TokenHash == "" {
		return fmt.Errorf("password reset token is incomplete")
	}
	if token.TokenHash != existing.TokenHash {
		if existingID, ok := r.passwordResetHash[token.TokenHash]; ok && existingID != token.ID {
			return fmt.Errorf("password reset token hash already exists")
		}
		delete(r.passwordResetHash, existing.TokenHash)
		r.passwordResetHash[token.TokenHash] = token.ID
	}
	token.UpdatedAt = time.Now().UTC()
	r.passwordResets[token.ID] = clonePasswordResetToken(token)
	return nil
}

func (r *MemoryRepository) DeletePasswordResetToken(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("password reset token id is required")
	}
	token, ok := r.passwordResets[id]
	if !ok {
		return ErrNotFound
	}
	delete(r.passwordResetHash, token.TokenHash)
	delete(r.passwordResets, id)
	return nil
}

func (r *MemoryRepository) ListAccounts() []core.Account {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]core.Account, 0, len(r.accounts))
	for _, account := range r.accounts {
		out = append(out, cloneAccount(account))
	}
	return sortAccounts(out)
}

func (r *MemoryRepository) GetAccount(id string) (core.Account, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	account, ok := r.accounts[id]
	if !ok {
		return core.Account{}, ErrNotFound
	}
	return cloneAccount(account), nil
}

func (r *MemoryRepository) UpsertAccount(account core.Account) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	account.Group = core.NormalizeAccountGroupName(account.Group)
	now := time.Now().UTC()
	if account.CreatedAt.IsZero() {
		account.CreatedAt = now
	}
	account.UpdatedAt = now
	r.accounts[account.ID] = cloneAccount(account)
	return nil
}

func (r *MemoryRepository) UpsertRuntimeAccount(account core.Account) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	current, ok := r.accounts[account.ID]
	if !ok {
		return ErrNotFound
	}
	now := time.Now().UTC()
	account.UpdatedAt = now
	_, _, runtimeState := splitAccountForStorage(account)
	runtimeState.UpdatedAt = now
	r.accounts[account.ID] = cloneAccount(applyRuntimeStateToAccount(current, runtimeState))
	return nil
}

func (r *MemoryRepository) DeleteAccount(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.accounts[id]; !ok {
		return ErrNotFound
	}
	delete(r.accounts, id)
	return nil
}

func (r *MemoryRepository) UpdateAccountStatus(id string, status core.AccountStatus) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	account, ok := r.accounts[id]
	if !ok {
		return ErrNotFound
	}
	account.Status = status
	account.UpdatedAt = time.Now().UTC()
	r.accounts[id] = account
	return nil
}

func (r *MemoryRepository) ListAccountGroups() []core.AccountGroup {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]core.AccountGroup, 0, len(r.groups))
	for _, group := range r.groups {
		out = append(out, cloneAccountGroup(group))
	}
	return sortAccountGroups(out)
}

func (r *MemoryRepository) UpsertAccountGroup(group core.AccountGroup) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	if group.CreatedAt.IsZero() {
		group.CreatedAt = now
	}
	group.UpdatedAt = now
	r.groups[group.ID] = cloneAccountGroup(group)
	return nil
}

func (r *MemoryRepository) DeleteAccountGroup(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.groups[id]; !ok {
		return ErrNotFound
	}
	delete(r.groups, id)
	return nil
}

func (r *MemoryRepository) ListModels() []core.ModelConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]core.ModelConfig, 0, len(r.models))
	for _, model := range r.models {
		out = append(out, cloneModel(model))
	}
	return sortModels(out)
}

func (r *MemoryRepository) GetModel(id string) (core.ModelConfig, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	model, ok := r.models[id]
	if !ok {
		return core.ModelConfig{}, ErrNotFound
	}
	return cloneModel(model), nil
}

func (r *MemoryRepository) UpsertModel(model core.ModelConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	if model.CreatedAt.IsZero() {
		model.CreatedAt = now
	}
	model.UpdatedAt = now
	r.models[model.ID] = cloneModel(model)
	return nil
}

func (r *MemoryRepository) DeleteModel(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.models[id]; !ok {
		return ErrNotFound
	}
	delete(r.models, id)
	return nil
}

func (r *MemoryRepository) ListClients() []core.APIClient {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]core.APIClient, 0, len(r.clients))
	for _, client := range r.clients {
		out = append(out, cloneClient(client))
	}
	return sortClients(out)
}

func (r *MemoryRepository) GetClient(id string) (core.APIClient, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	client, ok := r.clients[id]
	if !ok {
		return core.APIClient{}, ErrNotFound
	}
	return cloneClient(client), nil
}

func (r *MemoryRepository) FindClientByAPIKey(apiKey string) (core.APIClient, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, client := range r.clients {
		if client.APIKey == apiKey {
			return cloneClient(client), nil
		}
	}
	return core.APIClient{}, ErrNotFound
}

func (r *MemoryRepository) UpsertClient(client core.APIClient) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	client.AccountGroup = core.NormalizeAccountGroupName(client.AccountGroup)
	client.BillingSource = core.NormalizeClientBillingSource(client.BillingSource)
	if client.SpendLimitNanoUSD < 0 {
		return ErrClientSpendLimitExceeded
	}
	now := time.Now().UTC()
	if client.CreatedAt.IsZero() {
		client.CreatedAt = now
	}
	client.UpdatedAt = now
	r.clients[client.ID] = cloneClient(client)
	spend := r.clientSpend[client.ID]
	spend.ClientID = client.ID
	spend.SpendLimitNanoUSD = client.SpendLimitNanoUSD
	spend.UpdatedAt = now
	r.clientSpend[client.ID] = spend
	return nil
}

func (r *MemoryRepository) DeleteClient(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.clients[id]; !ok {
		return ErrNotFound
	}
	if err := r.releaseReservedBillingForDeletedDataLocked("", map[string]struct{}{id: {}}, time.Now().UTC()); err != nil {
		return err
	}
	delete(r.clients, id)
	delete(r.clientSpend, id)
	for responseID, binding := range r.responses {
		if binding.ClientID == id {
			delete(r.responses, responseID)
		}
	}
	return nil
}

func (r *MemoryRepository) releaseReservedBillingForDeletedDataLocked(userID string, clientIDs map[string]struct{}, now time.Time) error {
	for key, request := range r.billing {
		if request.Status != core.BillingRequestReserved {
			continue
		}
		matchesUser := userID != "" && strings.TrimSpace(request.UserID) == userID
		_, matchesClient := clientIDs[strings.TrimSpace(request.ClientID)]
		if !matchesUser && !matchesClient {
			continue
		}

		request.Status = core.BillingRequestReleased
		request.SettledAt = &now
		r.billing[key] = cloneBillingReservation(request)
	}
	return nil
}

func (r *MemoryRepository) UpsertOpenAIResponseBinding(binding core.OpenAIResponseBinding) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	binding.ResponseID = strings.TrimSpace(binding.ResponseID)
	binding.AccountID = strings.TrimSpace(binding.AccountID)
	binding.ClientID = strings.TrimSpace(binding.ClientID)
	binding.PromptCacheKey = strings.TrimSpace(binding.PromptCacheKey)
	if binding.ResponseID == "" || binding.AccountID == "" {
		return fmt.Errorf("response id and account id are required")
	}
	now := time.Now().UTC()
	if existing, ok := r.responses[binding.ResponseID]; ok && !existing.CreatedAt.IsZero() {
		binding.CreatedAt = existing.CreatedAt
	}
	if binding.CreatedAt.IsZero() {
		binding.CreatedAt = now
	}
	binding.UpdatedAt = now
	r.responses[binding.ResponseID] = binding
	return nil
}

func (r *MemoryRepository) GetOpenAIResponseBinding(responseID string) (core.OpenAIResponseBinding, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	binding, ok := r.responses[strings.TrimSpace(responseID)]
	if !ok {
		return core.OpenAIResponseBinding{}, ErrNotFound
	}
	return binding, nil
}

func (r *MemoryRepository) GetClientSpend(clientID string) (core.ClientSpend, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	spend, ok := r.clientSpend[clientID]
	if !ok {
		client, ok := r.clients[clientID]
		if !ok {
			return core.ClientSpend{}, ErrNotFound
		}
		return core.ClientSpend{
			ClientID:          client.ID,
			SpendLimitNanoUSD: client.SpendLimitNanoUSD,
		}, nil
	}
	return spend, nil
}

func (r *MemoryRepository) GetClientActualSpend(clientID string) (core.ClientSpend, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	clientID = strings.TrimSpace(clientID)
	client, ok := r.clients[clientID]
	if !ok {
		return core.ClientSpend{}, ErrNotFound
	}
	spend := r.clientSpend[clientID]
	if spend.ClientID == "" {
		spend = core.ClientSpend{
			ClientID:          client.ID,
			SpendLimitNanoUSD: client.SpendLimitNanoUSD,
		}
	}
	spend.ClientID = client.ID
	spend.SpendLimitNanoUSD = client.SpendLimitNanoUSD
	return spend, nil
}

func (r *MemoryRepository) ListBillingPlanGroups() []core.BillingPlanGroup {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]core.BillingPlanGroup, 0, len(r.planGroups))
	for _, group := range r.planGroups {
		out = append(out, cloneBillingPlanGroup(group))
	}
	return sortBillingPlanGroups(out)
}

func (r *MemoryRepository) GetBillingPlanGroup(id string) (core.BillingPlanGroup, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	group, ok := r.planGroups[strings.TrimSpace(id)]
	if !ok {
		return core.BillingPlanGroup{}, ErrNotFound
	}
	return cloneBillingPlanGroup(group), nil
}

func (r *MemoryRepository) UpsertBillingPlanGroup(group core.BillingPlanGroup) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	group = normalizeBillingPlanGroup(group)
	if group.ID == "" {
		return fmt.Errorf("billing plan group id is required")
	}
	if group.Name == "" {
		return fmt.Errorf("billing plan group name is required")
	}
	now := time.Now().UTC()
	if group.CreatedAt.IsZero() {
		if existing, ok := r.planGroups[group.ID]; ok && !existing.CreatedAt.IsZero() {
			group.CreatedAt = existing.CreatedAt
		} else {
			group.CreatedAt = now
		}
	}
	group.UpdatedAt = now
	r.planGroups[group.ID] = cloneBillingPlanGroup(group)
	return nil
}

func (r *MemoryRepository) DeleteBillingPlanGroup(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	id = strings.TrimSpace(id)
	if _, ok := r.planGroups[id]; !ok {
		return ErrNotFound
	}
	delete(r.planGroups, id)
	return nil
}

func (r *MemoryRepository) ListBillingPlans() []core.BillingPlan {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]core.BillingPlan, 0, len(r.plans))
	for _, plan := range r.plans {
		out = append(out, cloneBillingPlan(plan))
	}
	return sortBillingPlans(out)
}

func (r *MemoryRepository) GetBillingPlan(id string) (core.BillingPlan, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	plan, ok := r.plans[strings.TrimSpace(id)]
	if !ok {
		return core.BillingPlan{}, ErrNotFound
	}
	return cloneBillingPlan(plan), nil
}

func (r *MemoryRepository) UpsertBillingPlan(plan core.BillingPlan) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	plan = normalizeBillingPlan(plan)
	if plan.ID == "" {
		return fmt.Errorf("billing plan id is required")
	}
	if plan.Name == "" {
		return fmt.Errorf("billing plan name is required")
	}
	now := time.Now().UTC()
	if plan.CreatedAt.IsZero() {
		if existing, ok := r.plans[plan.ID]; ok && !existing.CreatedAt.IsZero() {
			plan.CreatedAt = existing.CreatedAt
		} else {
			plan.CreatedAt = now
		}
	}
	plan.UpdatedAt = now
	r.plans[plan.ID] = cloneBillingPlan(plan)
	return nil
}

func (r *MemoryRepository) DeleteBillingPlan(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	id = strings.TrimSpace(id)
	if _, ok := r.plans[id]; !ok {
		return ErrNotFound
	}
	delete(r.plans, id)
	return nil
}

func (r *MemoryRepository) PurchaseBillingPlan(input core.BillingPlanPurchaseInput) (core.BillingPlanPurchase, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	userID := strings.TrimSpace(input.UserID)
	planID := strings.TrimSpace(input.PlanID)
	mode := normalizeBillingPlanPurchaseMode(input.Mode)
	if userID == "" || planID == "" {
		return core.BillingPlanPurchase{}, fmt.Errorf("user and plan are required")
	}
	user, ok := r.users[userID]
	if !ok {
		return core.BillingPlanPurchase{}, ErrNotFound
	}
	plan, ok := r.plans[planID]
	if !ok {
		return core.BillingPlanPurchase{}, ErrNotFound
	}
	plan = normalizeBillingPlan(plan)
	if !plan.Enabled {
		return core.BillingPlanPurchase{}, ErrNotFound
	}
	if groupID := core.NormalizeBillingPlanGroup(plan.Group); groupID != "" {
		group, ok := r.planGroups[groupID]
		if !ok || !billingPlanGroupSaleEnabled(group) {
			return core.BillingPlanPurchase{}, ErrNotFound
		}
	}
	now := time.Now().UTC()
	activeEntitlements := r.activeUserPlanEntitlementsLocked(userID, now)
	activeEntitlement := core.UserPlanEntitlement{}
	hasMatchingActiveEntitlement := false
	if len(activeEntitlements) == 0 {
		mode = core.BillingPlanPurchaseSeparate
	}
	if mode == core.BillingPlanPurchaseMergeQuota {
		activeEntitlement, hasMatchingActiveEntitlement = selectBillingPlanCombineEntitlement(plan, activeEntitlements, input.TargetEntitlementID)
		if !hasMatchingActiveEntitlement {
			return core.BillingPlanPurchase{}, fmt.Errorf("active plan must match selected plan to merge or extend")
		}
	} else if mode == core.BillingPlanPurchaseExtendPeriod {
		activeEntitlement, hasMatchingActiveEntitlement = selectBillingPlanExtendEntitlement(plan, activeEntitlements, input.TargetEntitlementID)
		if !hasMatchingActiveEntitlement {
			return core.BillingPlanPurchase{}, fmt.Errorf("active plan must match selected plan quota and period to extend")
		}
	}
	if user.BalanceNanoUSD < plan.PriceNanoUSD {
		return core.BillingPlanPurchase{}, ErrInsufficientBalance
	}
	balanceBefore := user.BalanceNanoUSD
	var entitlement core.UserPlanEntitlement
	if mode == core.BillingPlanPurchaseMergeQuota {
		entitlement = mergeBillingPlanQuota(activeEntitlement, plan, now)
		r.entitlements[entitlement.ID] = cloneUserPlanEntitlement(entitlement)
	} else if mode == core.BillingPlanPurchaseExtendPeriod {
		entitlement = extendBillingPlanPeriod(activeEntitlement, plan, now)
		r.entitlements[entitlement.ID] = cloneUserPlanEntitlement(entitlement)
	} else {
		for attempt := 0; attempt < 1000; attempt++ {
			entitlement = newUserPlanEntitlement(userID, plan, now.Add(time.Duration(attempt)*time.Nanosecond))
			if _, exists := r.entitlements[entitlement.ID]; !exists {
				break
			}
			entitlement = core.UserPlanEntitlement{}
		}
		if entitlement.ID == "" {
			return core.BillingPlanPurchase{}, fmt.Errorf("failed to generate unique plan entitlement id")
		}
		entitlement.Priority = r.nextUserPlanEntitlementPriorityLocked(userID, now)
		r.entitlements[entitlement.ID] = cloneUserPlanEntitlement(entitlement)
	}
	user.BalanceNanoUSD -= plan.PriceNanoUSD
	user.UpdatedAt = now
	r.users[userID] = cloneUser(user)
	ledgerIDKind := "plan_purchase"
	if mode == core.BillingPlanPurchaseMergeQuota {
		ledgerIDKind = "plan_purchase_merge"
	} else if mode == core.BillingPlanPurchaseExtendPeriod {
		ledgerIDKind = "plan_purchase_extend"
	}
	if plan.PriceNanoUSD != 0 {
		ledgerID := uniqueMemoryBillingLedgerID(r.ledger, ledgerIDKind, entitlement.ID, userID, now)
		r.ledger = append([]core.BillingLedgerEntry{{
			ID:                  ledgerID,
			UserID:              userID,
			Kind:                "plan_purchase",
			AmountNanoUSD:       -plan.PriceNanoUSD,
			BalanceAfterNanoUSD: user.BalanceNanoUSD,
			Note:                plan.Name,
			CreatedAt:           now,
		}}, r.ledger...)
	}
	quotaLedgerAmount := entitlement.CurrentQuotaNanoUSD
	quotaLedgerKind := "purchase"
	if mode == core.BillingPlanPurchaseMergeQuota {
		quotaLedgerAmount = plan.PeriodQuotaNanoUSD
		quotaLedgerKind = "merge_purchase"
	} else if mode == core.BillingPlanPurchaseExtendPeriod {
		quotaLedgerAmount = 0
		quotaLedgerKind = "extend_purchase"
	}
	if mode == core.BillingPlanPurchaseExtendPeriod || quotaLedgerAmount != 0 {
		quotaLedgerID := uniqueMemoryPlanQuotaLedgerID(r.planLedger, quotaLedgerKind, entitlement.ID, "", "", now)
		r.planLedger = append([]core.PlanQuotaLedgerEntry{{
			ID:                quotaLedgerID,
			EntitlementID:     entitlement.ID,
			UserID:            userID,
			Kind:              quotaLedgerKind,
			AmountNanoUSD:     quotaLedgerAmount,
			QuotaAfterNanoUSD: entitlement.CurrentQuotaNanoUSD,
			Note:              plan.Name,
			CreatedAt:         now,
		}}, r.planLedger...)
	}
	return core.BillingPlanPurchase{
		Plan:                 cloneBillingPlan(plan),
		Entitlement:          cloneUserPlanEntitlement(entitlement),
		BalanceBeforeNanoUSD: balanceBefore,
		BalanceAfterNanoUSD:  user.BalanceNanoUSD,
	}, nil
}

func uniqueMemoryBillingLedgerID(entries []core.BillingLedgerEntry, kind, requestID, clientID string, ts time.Time) string {
	existing := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		existing[entry.ID] = struct{}{}
	}
	for attempt := 0; attempt < 1000; attempt++ {
		id := billingLedgerID(kind, requestID, ledgerIDPartWithAttempt(clientID, attempt), ts)
		if _, ok := existing[id]; !ok {
			return id
		}
	}
	return billingLedgerID(kind, requestID, ledgerIDPartWithAttempt(clientID, time.Now().Nanosecond()), time.Now().UTC())
}

func uniqueMemoryPlanQuotaLedgerID(entries []core.PlanQuotaLedgerEntry, kind, entitlementID, requestID, clientID string, ts time.Time) string {
	existing := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		existing[entry.ID] = struct{}{}
	}
	for attempt := 0; attempt < 1000; attempt++ {
		id := planQuotaLedgerID(kind, entitlementID, requestID, clientID, ts, attempt)
		if _, ok := existing[id]; !ok {
			return id
		}
	}
	return planQuotaLedgerID(kind, entitlementID, requestID, clientID, time.Now().UTC(), time.Now().Nanosecond())
}

func ledgerIDPartWithAttempt(value string, attempt int) string {
	value = strings.TrimSpace(value)
	if attempt == 0 {
		return value
	}
	return fmt.Sprintf("%s_%d", value, attempt)
}

func (r *MemoryRepository) GrantBillingPlan(input core.BillingPlanGrantInput) (core.BillingPlanPurchase, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	userID := strings.TrimSpace(input.UserID)
	planID := strings.TrimSpace(input.PlanID)
	if userID == "" || planID == "" {
		return core.BillingPlanPurchase{}, fmt.Errorf("user and plan are required")
	}
	user, ok := r.users[userID]
	if !ok {
		return core.BillingPlanPurchase{}, ErrNotFound
	}
	plan, ok := r.plans[planID]
	if !ok {
		return core.BillingPlanPurchase{}, ErrNotFound
	}
	plan = normalizeBillingPlan(plan)
	if !plan.Enabled {
		return core.BillingPlanPurchase{}, ErrNotFound
	}
	now := time.Now().UTC()
	entitlement := core.UserPlanEntitlement{}
	for attempt := 0; attempt < 1000; attempt++ {
		entitlement = newUserPlanEntitlement(userID, plan, now.Add(time.Duration(attempt)*time.Nanosecond))
		entitlement.PriceNanoUSD = 0
		entitlement = normalizeUserPlanEntitlement(entitlement)
		if _, exists := r.entitlements[entitlement.ID]; !exists {
			break
		}
		entitlement = core.UserPlanEntitlement{}
	}
	if entitlement.ID == "" {
		return core.BillingPlanPurchase{}, fmt.Errorf("failed to generate unique plan entitlement id")
	}
	entitlement.Priority = r.nextUserPlanEntitlementPriorityLocked(userID, now)
	r.entitlements[entitlement.ID] = cloneUserPlanEntitlement(entitlement)
	note := strings.TrimSpace(input.Note)
	if note == "" {
		note = "admin grant"
	}
	if entitlement.CurrentQuotaNanoUSD != 0 {
		r.planLedger = append([]core.PlanQuotaLedgerEntry{{
			ID:                planQuotaLedgerID("grant", entitlement.ID, "", "", now, 0),
			EntitlementID:     entitlement.ID,
			UserID:            userID,
			Kind:              "grant",
			AmountNanoUSD:     entitlement.CurrentQuotaNanoUSD,
			QuotaAfterNanoUSD: entitlement.CurrentQuotaNanoUSD,
			Note:              strings.TrimSpace(plan.Name + " " + note),
			CreatedAt:         now,
		}}, r.planLedger...)
	}
	return core.BillingPlanPurchase{
		Plan:                 cloneBillingPlan(plan),
		Entitlement:          cloneUserPlanEntitlement(entitlement),
		BalanceBeforeNanoUSD: user.BalanceNanoUSD,
		BalanceAfterNanoUSD:  user.BalanceNanoUSD,
	}, nil
}

func (r *MemoryRepository) ListUserPlanEntitlements(userID string) []core.UserPlanEntitlement {
	r.mu.Lock()
	defer r.mu.Unlock()

	userID = strings.TrimSpace(userID)
	now := time.Now().UTC()
	out := make([]core.UserPlanEntitlement, 0)
	for _, entitlement := range r.entitlements {
		if userID != "" && entitlement.UserID != userID {
			continue
		}
		advanced, entries, changed := advanceUserPlanEntitlement(entitlement, now)
		if changed {
			r.entitlements[advanced.ID] = cloneUserPlanEntitlement(advanced)
			r.prependPlanLedgerLocked(entries)
		}
		out = append(out, cloneUserPlanEntitlement(advanced))
	}
	return sortUserPlanEntitlements(out)
}

func (r *MemoryRepository) GetActiveUserPlanEntitlement(userID string) (core.UserPlanEntitlement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	entitlement, ok := r.activeUserPlanEntitlementLocked(strings.TrimSpace(userID), time.Now().UTC())
	if !ok {
		return core.UserPlanEntitlement{}, ErrNotFound
	}
	return cloneUserPlanEntitlement(entitlement), nil
}

func (r *MemoryRepository) MoveUserPlanEntitlementPriority(userID, entitlementID, direction string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	userID = strings.TrimSpace(userID)
	entitlementID = strings.TrimSpace(entitlementID)
	if userID == "" || entitlementID == "" {
		return ErrNotFound
	}
	entitlements := r.activeUserPlanEntitlementsLocked(userID, time.Now().UTC())
	index := -1
	for i, entitlement := range entitlements {
		if entitlement.ID == entitlementID {
			index = i
			break
		}
	}
	if index < 0 {
		return ErrNotFound
	}
	target := index
	switch strings.ToLower(strings.TrimSpace(direction)) {
	case "up":
		target = index - 1
	case "down":
		target = index + 1
	default:
		return fmt.Errorf("unsupported priority direction")
	}
	if target < 0 || target >= len(entitlements) {
		return nil
	}
	entitlements[index], entitlements[target] = entitlements[target], entitlements[index]
	for i, entitlement := range entitlements {
		entitlement.Priority = i + 1
		r.entitlements[entitlement.ID] = cloneUserPlanEntitlement(entitlement)
	}
	return nil
}

func (r *MemoryRepository) CancelUserPlanEntitlement(userID, entitlementID string) (core.UserPlanEntitlement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	userID = strings.TrimSpace(userID)
	entitlementID = strings.TrimSpace(entitlementID)
	if userID == "" || entitlementID == "" {
		return core.UserPlanEntitlement{}, ErrNotFound
	}
	entitlement, ok := r.entitlements[entitlementID]
	if !ok || strings.TrimSpace(entitlement.UserID) != userID {
		return core.UserPlanEntitlement{}, ErrNotFound
	}
	now := time.Now().UTC()
	entitlement, entries, changed := advanceUserPlanEntitlement(entitlement, now)
	if changed {
		r.entitlements[entitlement.ID] = cloneUserPlanEntitlement(entitlement)
		r.prependPlanLedgerLocked(entries)
	}
	if entitlement.Status != core.UserPlanEntitlementActive {
		return core.UserPlanEntitlement{}, ErrNotFound
	}
	cancelled, entry, hasEntry := cancelUserPlanEntitlement(entitlement, now, 0)
	r.entitlements[cancelled.ID] = cloneUserPlanEntitlement(cancelled)
	if hasEntry {
		r.prependPlanLedgerLocked([]core.PlanQuotaLedgerEntry{entry})
	}
	return cloneUserPlanEntitlement(cancelled), nil
}

func (r *MemoryRepository) PlanSubscriptionStats(query PlanSubscriptionQuery) PlanSubscriptionStats {
	items := r.planSubscriptionSummaries(query)
	var stats PlanSubscriptionStats
	for _, item := range items {
		entitlement := item.Entitlement
		stats.TotalCount++
		switch entitlement.Status {
		case core.UserPlanEntitlementActive:
			stats.ActiveCount++
			stats.ActiveRemainingNanoUSD = addNanoUSDSaturating(stats.ActiveRemainingNanoUSD, planSubscriptionRemainingNanoUSD(entitlement))
			stats.ActiveUsedNanoUSD = addNanoUSDSaturating(stats.ActiveUsedNanoUSD, planSubscriptionUsedNanoUSD(entitlement))
		case core.UserPlanEntitlementExpired:
			stats.ExpiredCount++
		case core.UserPlanEntitlementCancelled:
			stats.CancelledCount++
		}
		stats.RevenueNanoUSD = addNanoUSDSaturating(stats.RevenueNanoUSD, entitlement.PriceNanoUSD)
	}
	return stats
}

func (r *MemoryRepository) ListPlanSubscriptionPlanSummaries(query PlanSubscriptionQuery, limit int) []PlanSubscriptionPlanSummary {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	items := r.planSubscriptionSummaries(query)
	byPlan := make(map[string]*PlanSubscriptionPlanSummary)
	for _, item := range items {
		entitlement := item.Entitlement
		planID := strings.TrimSpace(entitlement.PlanID)
		if planID == "" {
			continue
		}
		summary := byPlan[planID]
		if summary == nil {
			name := strings.TrimSpace(entitlement.PlanName)
			if name == "" {
				name = planID
			}
			summary = &PlanSubscriptionPlanSummary{
				PlanID:    planID,
				PlanName:  name,
				PlanGroup: strings.TrimSpace(item.PlanGroup),
			}
			byPlan[planID] = summary
		}
		summary.PurchaseCount++
		summary.RevenueNanoUSD = addNanoUSDSaturating(summary.RevenueNanoUSD, entitlement.PriceNanoUSD)
		if entitlement.Status == core.UserPlanEntitlementActive {
			summary.ActiveCount++
			summary.ActiveRemainingNanoUSD = addNanoUSDSaturating(summary.ActiveRemainingNanoUSD, planSubscriptionRemainingNanoUSD(entitlement))
		}
	}
	out := make([]PlanSubscriptionPlanSummary, 0, len(byPlan))
	for _, summary := range byPlan {
		out = append(out, *summary)
	}
	slices.SortFunc(out, func(a, b PlanSubscriptionPlanSummary) int {
		if a.RevenueNanoUSD != b.RevenueNanoUSD {
			if a.RevenueNanoUSD > b.RevenueNanoUSD {
				return -1
			}
			return 1
		}
		if a.PurchaseCount != b.PurchaseCount {
			return b.PurchaseCount - a.PurchaseCount
		}
		if a.PlanName != b.PlanName {
			return strings.Compare(a.PlanName, b.PlanName)
		}
		return strings.Compare(a.PlanID, b.PlanID)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (r *MemoryRepository) ListPlanSubscriptionSummariesPage(query PlanSubscriptionQuery) ([]PlanSubscriptionSummary, int) {
	query = normalizePlanSubscriptionQuery(query)
	items := r.planSubscriptionSummaries(query)
	total := len(items)
	if query.Offset >= total {
		return nil, total
	}
	end := total
	if query.Limit > 0 {
		end = query.Offset + query.Limit
		if end > total {
			end = total
		}
	}
	out := append([]PlanSubscriptionSummary(nil), items[query.Offset:end]...)
	return out, total
}

func (r *MemoryRepository) PlanQuotaUsageStats(query PlanQuotaUsageQuery) PlanQuotaUsageStats {
	return planQuotaUsageStatsFromRows(r.planQuotaUsageRows(query))
}

func (r *MemoryRepository) ListPlanQuotaUsageByDay(query PlanQuotaUsageQuery) ([]PlanQuotaUsageDaySummary, int) {
	query = normalizePlanQuotaUsageQuery(query)
	items := r.planQuotaUsageRows(query)
	total := len(items)
	if query.Offset >= total {
		return nil, total
	}
	end := total
	if query.Limit > 0 {
		end = query.Offset + query.Limit
		if end > total {
			end = total
		}
	}
	out := append([]PlanQuotaUsageDaySummary(nil), items[query.Offset:end]...)
	return out, total
}

func (r *MemoryRepository) ListPlanQuotaUsageEvents(query PlanQuotaUsageQuery) []PlanQuotaUsageEvent {
	query = normalizePlanQuotaUsageQuery(query)
	_ = r.ListUserPlanEntitlements("")

	r.mu.RLock()
	defer r.mu.RUnlock()

	usersByID := make(map[string]core.User, len(r.users))
	for _, user := range r.users {
		usersByID[strings.TrimSpace(user.ID)] = user
	}
	entitlementsByID := make(map[string]core.UserPlanEntitlement, len(r.entitlements))
	for _, entitlement := range r.entitlements {
		entitlementsByID[strings.TrimSpace(entitlement.ID)] = entitlement
	}
	plansByID := make(map[string]core.BillingPlan, len(r.plans))
	for _, plan := range r.plans {
		plansByID[strings.TrimSpace(plan.ID)] = plan
	}
	requestsByKey := make(map[string]core.BillingReservation, len(r.billing))
	for _, request := range r.billing {
		requestsByKey[billingRequestKey(request.RequestID, request.ClientID)] = request
	}
	planAllocationKeys := make(map[string]struct{}, len(r.allocations))
	for _, allocation := range r.allocations {
		if strings.TrimSpace(allocation.Source) != core.BillingFundingSourcePlan || strings.TrimSpace(allocation.EntitlementID) == "" {
			continue
		}
		planAllocationKeys[billingRequestKey(allocation.RequestID, allocation.ClientID)+"\x00"+strings.TrimSpace(allocation.EntitlementID)] = struct{}{}
	}

	metadataFor := func(userID, entitlementID, fallbackPlanName string) (string, string, string) {
		user := usersByID[userID]
		username := strings.TrimSpace(user.Username)
		if username == "" {
			username = userID
		}
		entitlement := entitlementsByID[entitlementID]
		planID := strings.TrimSpace(entitlement.PlanID)
		planName := strings.TrimSpace(entitlement.PlanName)
		if plan := plansByID[planID]; planName == "" {
			planName = strings.TrimSpace(plan.Name)
		}
		if planName == "" {
			planName = strings.TrimSpace(fallbackPlanName)
		}
		if planName == "" {
			planName = planID
		}
		return username, planID, planName
	}
	matchesQuery := func(userID, username, planID, entitlementID string, createdAt time.Time) bool {
		userID = strings.TrimSpace(userID)
		username = strings.TrimSpace(username)
		planID = strings.TrimSpace(planID)
		entitlementID = strings.TrimSpace(entitlementID)
		if userID == "" {
			return false
		}
		if username == "" {
			username = userID
		}
		if query.UserID != "" && !strings.EqualFold(userID, query.UserID) && !strings.EqualFold(username, query.UserID) {
			return false
		}
		if query.EntitlementID != "" && entitlementID != query.EntitlementID {
			return false
		}
		if query.PlanID != "" && planID != query.PlanID {
			return false
		}
		if !query.StartedAt.IsZero() && createdAt.Before(query.StartedAt) {
			return false
		}
		if !query.EndedAt.IsZero() && !createdAt.Before(query.EndedAt) {
			return false
		}
		return true
	}

	out := make([]PlanQuotaUsageEvent, 0)
	for _, entry := range r.planLedger {
		entitlementID := strings.TrimSpace(entry.EntitlementID)
		requestKey := billingRequestKey(entry.RequestID, entry.ClientID)
		if planQuotaUsageRequestLedgerKind(entry.Kind) && requestKey != billingRequestKey("", "") {
			if _, hasRequest := requestsByKey[requestKey]; hasRequest {
				if _, hasAllocation := planAllocationKeys[requestKey+"\x00"+entitlementID]; hasAllocation {
					continue
				}
			}
		}
		userID := strings.TrimSpace(entry.UserID)
		username, planID, planName := metadataFor(userID, entitlementID, entry.Note)
		if !matchesQuery(userID, username, planID, entitlementID, entry.CreatedAt) {
			continue
		}
		event := PlanQuotaUsageEvent{
			UserID:        userID,
			Username:      username,
			PlanID:        planID,
			PlanName:      planName,
			EntitlementID: entitlementID,
			Kind:          strings.TrimSpace(entry.Kind),
			CreatedAt:     entry.CreatedAt,
		}
		addPlanQuotaUsageEventLedgerAmount(&event, entry.Kind, entry.AmountNanoUSD)
		if planQuotaUsageEventHasAmount(event) {
			out = append(out, event)
		}
	}
	for _, allocation := range r.allocations {
		if strings.TrimSpace(allocation.Source) != core.BillingFundingSourcePlan {
			continue
		}
		entitlementID := strings.TrimSpace(allocation.EntitlementID)
		if entitlementID == "" {
			continue
		}
		request, ok := requestsByKey[billingRequestKey(allocation.RequestID, allocation.ClientID)]
		if !ok {
			continue
		}
		createdAt := request.CreatedAt
		amountNanoUSD := billingRequestUsageSpendAmount(request.Status, allocation.ReservedNanoUSD, allocation.ActualNanoUSD)
		if amountNanoUSD <= 0 {
			continue
		}
		userID := strings.TrimSpace(allocation.UserID)
		if userID == "" {
			userID = strings.TrimSpace(request.UserID)
		}
		username, planID, planName := metadataFor(userID, entitlementID, "")
		if !matchesQuery(userID, username, planID, entitlementID, createdAt) {
			continue
		}
		out = append(out, PlanQuotaUsageEvent{
			UserID:        userID,
			Username:      username,
			PlanID:        planID,
			PlanName:      planName,
			EntitlementID: entitlementID,
			Kind:          "usage",
			UsedNanoUSD:   amountNanoUSD,
			NetNanoUSD:    -amountNanoUSD,
			CreatedAt:     createdAt,
		})
	}

	return sortPlanQuotaUsageEvents(out)
}

func (r *MemoryRepository) planQuotaUsageRows(query PlanQuotaUsageQuery) []PlanQuotaUsageDaySummary {
	return planQuotaUsageRowsFromEvents(r.ListPlanQuotaUsageEvents(query), query)
}

func (r *MemoryRepository) planSubscriptionSummaries(query PlanSubscriptionQuery) []PlanSubscriptionSummary {
	query = normalizePlanSubscriptionQuery(query)
	entitlements := r.ListUserPlanEntitlements("")
	users := r.ListUsers()
	plans := r.ListBillingPlans()

	usersByID := make(map[string]core.User, len(users))
	for _, user := range users {
		usersByID[strings.TrimSpace(user.ID)] = user
	}
	plansByID := make(map[string]core.BillingPlan, len(plans))
	for _, plan := range plans {
		plansByID[strings.TrimSpace(plan.ID)] = plan
	}

	out := make([]PlanSubscriptionSummary, 0, len(entitlements))
	for _, entitlement := range entitlements {
		user := usersByID[strings.TrimSpace(entitlement.UserID)]
		username := strings.TrimSpace(user.Username)
		if username == "" {
			username = strings.TrimSpace(entitlement.UserID)
		}
		plan := plansByID[strings.TrimSpace(entitlement.PlanID)]
		item := PlanSubscriptionSummary{
			Entitlement:        cloneUserPlanEntitlement(entitlement),
			Username:           username,
			PlanGroup:          strings.TrimSpace(plan.Group),
			UserBalanceNanoUSD: user.BalanceNanoUSD,
		}
		if !plan.CreatedAt.IsZero() || plan.ID != "" {
			item.PlanGroup = strings.TrimSpace(plan.Group)
		}
		if !planSubscriptionMatchesQuery(item, query) {
			continue
		}
		out = append(out, item)
	}
	return sortPlanSubscriptionSummaries(out)
}

func (r *MemoryRepository) activeUserPlanEntitlementLocked(userID string, now time.Time) (core.UserPlanEntitlement, bool) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return core.UserPlanEntitlement{}, false
	}
	var active core.UserPlanEntitlement
	found := false
	for _, entitlement := range r.entitlements {
		if entitlement.UserID != userID {
			continue
		}
		if entitlement.Status != core.UserPlanEntitlementActive && entitlement.Status != core.UserPlanEntitlementExpired {
			continue
		}
		advanced, entries, changed := advanceUserPlanEntitlement(entitlement, now)
		if changed {
			r.entitlements[advanced.ID] = cloneUserPlanEntitlement(advanced)
			r.prependPlanLedgerLocked(entries)
		}
		if advanced.Status != core.UserPlanEntitlementActive {
			continue
		}
		if !found || planEntitlementPriorityLess(active, advanced) {
			active = advanced
			found = true
		}
	}
	return active, found
}

func (r *MemoryRepository) activeUserPlanEntitlementsLocked(userID string, now time.Time) []core.UserPlanEntitlement {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil
	}
	out := make([]core.UserPlanEntitlement, 0)
	for _, entitlement := range r.entitlements {
		if entitlement.UserID != userID {
			continue
		}
		if entitlement.Status != core.UserPlanEntitlementActive && entitlement.Status != core.UserPlanEntitlementExpired {
			continue
		}
		advanced, entries, changed := advanceUserPlanEntitlement(entitlement, now)
		if changed {
			r.entitlements[advanced.ID] = cloneUserPlanEntitlement(advanced)
			r.prependPlanLedgerLocked(entries)
		}
		if advanced.Status == core.UserPlanEntitlementActive {
			out = append(out, cloneUserPlanEntitlement(advanced))
		}
	}
	return sortUserPlanEntitlements(out)
}

func (r *MemoryRepository) nextUserPlanEntitlementPriorityLocked(userID string, now time.Time) int {
	entitlements := r.activeUserPlanEntitlementsLocked(userID, now)
	maxPriority := 0
	for _, entitlement := range entitlements {
		if entitlement.Priority > maxPriority {
			maxPriority = entitlement.Priority
		}
	}
	if maxPriority > 0 {
		return maxPriority + 1
	}
	return len(entitlements) + 1
}

func (r *MemoryRepository) prependPlanLedgerLocked(entries []core.PlanQuotaLedgerEntry) {
	for i := len(entries) - 1; i >= 0; i-- {
		r.planLedger = append([]core.PlanQuotaLedgerEntry{entries[i]}, r.planLedger...)
	}
}

func (r *MemoryRepository) requestFundingAllocationsLocked(requestID, clientID string) []core.BillingFundingAllocation {
	out := make([]core.BillingFundingAllocation, 0, 2)
	for _, allocation := range r.allocations {
		if allocation.RequestID == requestID && allocation.ClientID == clientID {
			out = append(out, allocation)
		}
	}
	slices.SortFunc(out, func(a, b core.BillingFundingAllocation) int {
		if a.Source != b.Source {
			if a.Source == core.BillingFundingSourcePlan {
				return -1
			}
			return 1
		}
		return strings.Compare(a.EntitlementID, b.EntitlementID)
	})
	return out
}

func (r *MemoryRepository) upsertFundingAllocationActualLocked(requestID, clientID, userID, source, entitlementID string, delta int64, now time.Time) {
	if delta == 0 {
		return
	}
	key := billingAllocationKey(requestID, clientID, source, entitlementID)
	allocation := r.allocations[key]
	if allocation.ID == "" {
		allocation = core.BillingFundingAllocation{
			ID:            billingAllocationID(requestID, clientID, source, entitlementID),
			RequestID:     requestID,
			ClientID:      clientID,
			UserID:        userID,
			Source:        source,
			EntitlementID: entitlementID,
			CreatedAt:     now,
		}
	}
	allocation.ActualNanoUSD += delta
	if allocation.ActualNanoUSD < 0 {
		allocation.ActualNanoUSD = 0
	}
	allocation.UpdatedAt = now
	r.allocations[key] = allocation
}

func (r *MemoryRepository) ReserveBilling(input core.BillingReservationInput) (core.BillingReservation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	input.RequestID = strings.TrimSpace(input.RequestID)
	input.ClientID = strings.TrimSpace(input.ClientID)
	input.UserID = strings.TrimSpace(input.UserID)
	input.AccountID = strings.TrimSpace(input.AccountID)
	input.AccountLabel = strings.TrimSpace(input.AccountLabel)
	input.Model = strings.TrimSpace(input.Model)
	input.Fingerprint = strings.TrimSpace(input.Fingerprint)
	input.BillingSource = core.NormalizeClientBillingSource(input.BillingSource)
	if input.RequestID == "" || input.ClientID == "" || input.UserID == "" {
		return core.BillingReservation{}, fmt.Errorf("billing request, client, and user are required")
	}

	now := time.Now().UTC()
	r.trimBillingRequestsLocked(now)

	key := billingRequestKey(input.RequestID, input.ClientID)
	if existing, ok := r.billing[key]; ok {
		if strings.TrimSpace(existing.Fingerprint) != input.Fingerprint {
			return core.BillingReservation{}, ErrBillingRequestConflict
		}
		return existing, nil
	}
	user, ok := r.users[input.UserID]
	if !ok {
		return core.BillingReservation{}, ErrNotFound
	}
	client, ok := r.clients[input.ClientID]
	if !ok {
		return core.BillingReservation{}, ErrNotFound
	}
	if ownerID := strings.TrimSpace(client.OwnerUserID); ownerID != "" && ownerID != input.UserID {
		return core.BillingReservation{}, ErrBillingClientOwnerMismatch
	}
	// Reservations only anchor a pending billing request; usage is charged on settlement.
	input.ReservedNanoUSD = 0
	var planEntitlement core.UserPlanEntitlement
	if input.BillingSource == core.ClientBillingSourcePlan {
		var ok bool
		planEntitlement, ok = selectPlanEntitlementForReservation(r.activeUserPlanEntitlementsLocked(input.UserID, now), 1)
		if !ok {
			return core.BillingReservation{}, ErrPlanQuotaExhausted
		}
	} else if user.BalanceNanoUSD <= 0 {
		return core.BillingReservation{}, ErrInsufficientBalance
	}
	spend := r.clientSpend[input.ClientID]
	if spend.ClientID == "" {
		spend = core.ClientSpend{ClientID: input.ClientID, SpendLimitNanoUSD: client.SpendLimitNanoUSD}
	}
	spend.SpendLimitNanoUSD = client.SpendLimitNanoUSD
	if spend.SpendLimitNanoUSD > 0 && spend.SpendUsedNanoUSD >= spend.SpendLimitNanoUSD {
		return core.BillingReservation{}, ErrClientSpendLimitExceeded
	}

	reservation := core.BillingReservation{
		ID:                            billingRequestID(input.RequestID, input.ClientID, now),
		RequestID:                     input.RequestID,
		ClientID:                      input.ClientID,
		ClientName:                    strings.TrimSpace(input.ClientName),
		UserID:                        input.UserID,
		AccountID:                     input.AccountID,
		AccountLabel:                  input.AccountLabel,
		FailedAccountLabels:           compactBillingAccountLabels(input.FailedAccountLabels),
		AccountGroup:                  strings.TrimSpace(input.AccountGroup),
		AccountGroupMultiplierBps:     input.AccountGroupMultiplierBps,
		BillingSource:                 input.BillingSource,
		Provider:                      input.Provider,
		Model:                         input.Model,
		FastMode:                      input.FastMode,
		Status:                        core.BillingRequestReserved,
		EstimatedPromptTokens:         input.EstimatedPromptTokens,
		EstimatedCompletionTokens:     input.EstimatedCompletionTokens,
		InputPriceNanoUSDPer1M:        input.InputPriceNanoUSDPer1M,
		CachedInputPriceNanoUSDPer1M:  input.CachedInputPriceNanoUSDPer1M,
		CacheWritePriceNanoUSDPer1M:   input.CacheWritePriceNanoUSDPer1M,
		CacheWrite5mPriceNanoUSDPer1M: input.CacheWrite5mPriceNanoUSDPer1M,
		CacheWrite1hPriceNanoUSDPer1M: input.CacheWrite1hPriceNanoUSDPer1M,
		OutputPriceNanoUSDPer1M:       input.OutputPriceNanoUSDPer1M,
		ImageOutputPriceNanoUSDPer1M:  input.ImageOutputPriceNanoUSDPer1M,
		ReservedNanoUSD:               input.ReservedNanoUSD,
		Fingerprint:                   input.Fingerprint,
		CacheDiagnostics:              input.CacheDiagnostics,
		CreatedAt:                     now,
	}
	r.billing[key] = reservation
	if strings.TrimSpace(planEntitlement.ID) != "" {
		allocation := core.BillingFundingAllocation{
			ID:            billingAllocationID(input.RequestID, input.ClientID, core.BillingFundingSourcePlan, planEntitlement.ID),
			RequestID:     input.RequestID,
			ClientID:      input.ClientID,
			UserID:        input.UserID,
			Source:        core.BillingFundingSourcePlan,
			EntitlementID: planEntitlement.ID,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		r.allocations[billingAllocationKey(allocation.RequestID, allocation.ClientID, allocation.Source, allocation.EntitlementID)] = allocation
	}
	return reservation, nil
}

func (r *MemoryRepository) UpdateBillingAccount(input core.BillingAccountUpdateInput) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	requestID := strings.TrimSpace(input.RequestID)
	clientID := strings.TrimSpace(input.ClientID)
	if requestID == "" || clientID == "" {
		return fmt.Errorf("billing request and client are required")
	}
	key := billingRequestKey(requestID, clientID)
	request, ok := r.billing[key]
	if !ok {
		return ErrNotFound
	}
	if request.Status != core.BillingRequestReserved {
		return nil
	}
	request.AccountID = strings.TrimSpace(input.AccountID)
	request.AccountLabel = strings.TrimSpace(input.AccountLabel)
	request.FailedAccountLabels = compactBillingAccountLabels(input.FailedAccountLabels)
	r.billing[key] = request
	return nil
}

func (r *MemoryRepository) SettleBilling(input core.BillingSettlementInput) (core.BillingSettlement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := billingRequestKey(input.RequestID, input.ClientID)
	request, ok := r.billing[key]
	if !ok {
		return core.BillingSettlement{}, ErrNotFound
	}
	if request.Status == core.BillingRequestSettled || request.Status == core.BillingRequestUsageMissing {
		return core.BillingSettlement{Request: request}, nil
	}
	if request.Status == core.BillingRequestReleased {
		return core.BillingSettlement{Request: request}, nil
	}
	user, ok := r.users[request.UserID]
	if !ok {
		return core.BillingSettlement{}, ErrNotFound
	}
	if input.ActualNanoUSD < 0 {
		input.ActualNanoUSD = 0
	}
	billingSource := core.NormalizeClientBillingSource(request.BillingSource)
	if strings.TrimSpace(request.BillingSource) == "" {
		billingSource = core.NormalizeClientBillingSource(input.BillingSource)
	}
	chargeNanoUSD := input.ActualNanoUSD
	now := time.Now().UTC()
	spend := r.clientSpend[request.ClientID]
	spend.ClientID = request.ClientID
	if client, ok := r.clients[request.ClientID]; ok {
		spend.SpendLimitNanoUSD = client.SpendLimitNanoUSD
	}
	nextSpend := spend.SpendUsedNanoUSD
	if chargeNanoUSD != 0 {
		var err error
		nextSpend, err = addNanoUSD(spend.SpendUsedNanoUSD, chargeNanoUSD)
		if err != nil {
			return core.BillingSettlement{}, ErrAmountOverflow
		}
		if nextSpend < 0 {
			nextSpend = 0
		}
	}
	cashDelta := int64(0)
	planDelta := int64(0)
	var planEntitlement core.UserPlanEntitlement
	if chargeNanoUSD > 0 {
		if billingSource == core.ClientBillingSourcePlan {
			for _, allocation := range r.requestFundingAllocationsLocked(request.RequestID, request.ClientID) {
				if allocation.Source != core.BillingFundingSourcePlan || strings.TrimSpace(allocation.EntitlementID) == "" {
					continue
				}
				if entitlement, ok := r.entitlements[allocation.EntitlementID]; ok {
					planEntitlement = entitlement
					break
				}
			}
			if planEntitlement.ID == "" {
				if entitlement, hasEntitlement := r.activeUserPlanEntitlementLocked(request.UserID, now); hasEntitlement {
					planEntitlement = entitlement
				}
			}
			if planEntitlement.ID == "" {
				return core.BillingSettlement{}, ErrPlanQuotaExhausted
			}
			planDelta = chargeNanoUSD
		} else {
			cashDelta = chargeNanoUSD
		}
	}
	if planDelta > 0 {
		planEntitlement.CurrentQuotaNanoUSD -= planDelta
		planEntitlement.UpdatedAt = now
		r.entitlements[planEntitlement.ID] = cloneUserPlanEntitlement(planEntitlement)
		r.upsertFundingAllocationActualLocked(request.RequestID, request.ClientID, request.UserID, core.BillingFundingSourcePlan, planEntitlement.ID, planDelta, now)
		r.prependPlanLedgerLocked([]core.PlanQuotaLedgerEntry{{
			ID:                planQuotaLedgerID("settle", planEntitlement.ID, request.RequestID, request.ClientID, now, 0),
			EntitlementID:     planEntitlement.ID,
			UserID:            request.UserID,
			ClientID:          request.ClientID,
			RequestID:         request.RequestID,
			Kind:              "settle",
			AmountNanoUSD:     -planDelta,
			QuotaAfterNanoUSD: planEntitlement.CurrentQuotaNanoUSD,
			Note:              request.Model,
			CreatedAt:         now,
		}})
	}
	if cashDelta > 0 {
		r.upsertFundingAllocationActualLocked(request.RequestID, request.ClientID, request.UserID, core.BillingFundingSourceCash, "", cashDelta, now)
	}
	if cashDelta != 0 {
		nextBalance, err := addNanoUSD(user.BalanceNanoUSD, -cashDelta)
		if err != nil {
			return core.BillingSettlement{}, err
		}
		user.BalanceNanoUSD = nextBalance
	}
	user.UpdatedAt = now
	r.users[user.ID] = cloneUser(user)

	spend.SpendUsedNanoUSD = nextSpend
	spend.UpdatedAt = now
	r.clientSpend[request.ClientID] = spend

	request.AccountID = strings.TrimSpace(input.AccountID)
	if label := strings.TrimSpace(input.AccountLabel); label != "" || strings.TrimSpace(request.AccountLabel) == "" {
		request.AccountLabel = label
	}
	request.AccountGroup = strings.TrimSpace(input.AccountGroup)
	request.AccountGroupMultiplierBps = input.AccountGroupMultiplierBps
	request.BillingSource = billingSource
	request.Provider = input.Provider
	if strings.TrimSpace(input.Model) != "" {
		request.Model = strings.TrimSpace(input.Model)
	}
	request.FastMode = request.FastMode || input.FastMode
	request.PromptTokens = input.Usage.PromptTokens
	request.CachedPromptTokens = input.Usage.CachedPromptTokens
	request.CacheCreationTokens = input.Usage.CacheCreationTokens
	request.CacheCreation5mTokens = input.Usage.CacheCreation5mTokens
	request.CacheCreation1hTokens = input.Usage.CacheCreation1hTokens
	request.CompletionTokens = input.Usage.CompletionTokens
	request.ImageOutputTokens = input.Usage.ImageOutputTokens
	request.TotalTokens = input.Usage.TotalTokens
	request.InputPriceNanoUSDPer1M = input.InputPriceNanoUSDPer1M
	request.CachedInputPriceNanoUSDPer1M = input.CachedInputPriceNanoUSDPer1M
	request.CacheWritePriceNanoUSDPer1M = input.CacheWritePriceNanoUSDPer1M
	request.CacheWrite5mPriceNanoUSDPer1M = input.CacheWrite5mPriceNanoUSDPer1M
	request.CacheWrite1hPriceNanoUSDPer1M = input.CacheWrite1hPriceNanoUSDPer1M
	request.OutputPriceNanoUSDPer1M = input.OutputPriceNanoUSDPer1M
	request.ImageOutputPriceNanoUSDPer1M = input.ImageOutputPriceNanoUSDPer1M
	request.ActualNanoUSD = input.ActualNanoUSD
	request.FirstTokenMS = input.FirstTokenMS
	if input.MissingUsage {
		request.Status = core.BillingRequestUsageMissing
	} else {
		request.Status = core.BillingRequestSettled
	}
	request.SettledAt = &now
	r.billing[key] = request
	if cashDelta != 0 {
		r.ledger = append([]core.BillingLedgerEntry{{
			ID:                  billingLedgerID("settle", request.RequestID, request.ClientID, now),
			UserID:              request.UserID,
			ClientID:            request.ClientID,
			RequestID:           request.RequestID,
			Kind:                "settle",
			AmountNanoUSD:       -cashDelta,
			BalanceAfterNanoUSD: user.BalanceNanoUSD,
			Note:                request.Model,
			CreatedAt:           now,
		}}, r.ledger...)
	}
	return core.BillingSettlement{Request: request, DeltaNanoUSD: chargeNanoUSD, BalanceAfterNanoUSD: user.BalanceNanoUSD}, nil
}

func (r *MemoryRepository) ReleaseBilling(input core.BillingReleaseInput) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := billingRequestKey(input.RequestID, input.ClientID)
	request, ok := r.billing[key]
	if !ok {
		return ErrNotFound
	}
	if request.Status != core.BillingRequestReserved {
		return nil
	}
	now := time.Now().UTC()
	request.Status = core.BillingRequestReleased
	request.SettledAt = &now
	r.billing[key] = request
	return nil
}

func (r *MemoryRepository) ListBillingLedger(userID string, limit int) []core.BillingLedgerEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.trimBillingLedgerLocked(time.Now().UTC())

	if limit <= 0 {
		limit = len(r.ledger)
	}
	out := make([]core.BillingLedgerEntry, 0, limit)
	for _, entry := range r.ledger {
		if userID != "" && entry.UserID != userID {
			continue
		}
		out = append(out, entry)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func (r *MemoryRepository) BillingUsageSpendNanoUSD(startedAt, endedAt time.Time) int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()

	total := int64(0)
	requests := make(map[string]struct{}, len(r.billing))
	for _, request := range r.billing {
		requests[billingRequestKey(request.RequestID, request.ClientID)] = struct{}{}
		if !startedAt.IsZero() && request.CreatedAt.Before(startedAt) {
			continue
		}
		if !endedAt.IsZero() && !request.CreatedAt.Before(endedAt) {
			continue
		}
		amount := billingRequestUsageSpendAmount(request.Status, request.ReservedNanoUSD, request.ActualNanoUSD)
		if amount <= 0 {
			continue
		}
		next, err := addNanoUSD(total, amount)
		if err != nil {
			if amount > 0 {
				return maxInt64
			}
			return minInt64
		}
		total = next
	}
	for _, entry := range r.ledger {
		if entry.RequestID == "" || entry.ClientID == "" {
			continue
		}
		if _, ok := requests[billingRequestKey(entry.RequestID, entry.ClientID)]; ok {
			continue
		}
		if !startedAt.IsZero() && entry.CreatedAt.Before(startedAt) {
			continue
		}
		if !endedAt.IsZero() && !entry.CreatedAt.Before(endedAt) {
			continue
		}
		total = addNanoUSDSaturating(total, billingLedgerUsageSpendDelta(entry.Kind, entry.AmountNanoUSD))
	}
	for _, entry := range r.planLedger {
		if entry.RequestID == "" || entry.ClientID == "" {
			continue
		}
		if _, ok := requests[billingRequestKey(entry.RequestID, entry.ClientID)]; ok {
			continue
		}
		if !startedAt.IsZero() && entry.CreatedAt.Before(startedAt) {
			continue
		}
		if !endedAt.IsZero() && !entry.CreatedAt.Before(endedAt) {
			continue
		}
		total = addNanoUSDSaturating(total, planQuotaLedgerUsageSpendDelta(entry.Kind, entry.AmountNanoUSD))
	}
	return total
}

func (r *MemoryRepository) ListBillingUsageSpendByDay(startedAt time.Time, days int) []BillingUsageSpendDaySummary {
	if days <= 0 || startedAt.IsZero() {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	endedAt := startedAt.AddDate(0, 0, days)
	byDate := make(map[string]int64, days)
	requests := make(map[string]struct{}, len(r.billing))
	for _, request := range r.billing {
		requests[billingRequestKey(request.RequestID, request.ClientID)] = struct{}{}
		if request.CreatedAt.Before(startedAt) || !request.CreatedAt.Before(endedAt) {
			continue
		}
		amount := billingRequestUsageSpendAmount(request.Status, request.ReservedNanoUSD, request.ActualNanoUSD)
		if amount <= 0 {
			continue
		}
		date := billingDayKey(request.CreatedAt)
		if date == "" {
			continue
		}
		next, err := addNanoUSD(byDate[date], amount)
		if err != nil {
			if amount > 0 {
				next = maxInt64
			} else {
				next = minInt64
			}
		}
		byDate[date] = next
	}
	for _, entry := range r.ledger {
		if entry.RequestID == "" || entry.ClientID == "" {
			continue
		}
		if _, ok := requests[billingRequestKey(entry.RequestID, entry.ClientID)]; ok {
			continue
		}
		if entry.CreatedAt.Before(startedAt) || !entry.CreatedAt.Before(endedAt) {
			continue
		}
		amount := billingLedgerUsageSpendDelta(entry.Kind, entry.AmountNanoUSD)
		if amount == 0 {
			continue
		}
		date := billingDayKey(entry.CreatedAt)
		if date == "" {
			continue
		}
		byDate[date] = addNanoUSDSaturating(byDate[date], amount)
	}
	for _, entry := range r.planLedger {
		if entry.RequestID == "" || entry.ClientID == "" {
			continue
		}
		if _, ok := requests[billingRequestKey(entry.RequestID, entry.ClientID)]; ok {
			continue
		}
		if entry.CreatedAt.Before(startedAt) || !entry.CreatedAt.Before(endedAt) {
			continue
		}
		amount := planQuotaLedgerUsageSpendDelta(entry.Kind, entry.AmountNanoUSD)
		if amount == 0 {
			continue
		}
		date := billingDayKey(entry.CreatedAt)
		if date == "" {
			continue
		}
		byDate[date] = addNanoUSDSaturating(byDate[date], amount)
	}
	out := make([]BillingUsageSpendDaySummary, 0, len(byDate))
	for date, spend := range byDate {
		out = append(out, BillingUsageSpendDaySummary{Date: date, SpendNanoUSD: spend})
	}
	slices.SortFunc(out, func(a, b BillingUsageSpendDaySummary) int {
		return strings.Compare(a.Date, b.Date)
	})
	return out
}

func (r *MemoryRepository) ListBillingRequestCountByDay(startedAt time.Time, days int) []BillingRequestDayCountSummary {
	if days <= 0 || startedAt.IsZero() {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	endedAt := startedAt.AddDate(0, 0, days)
	byDate := make(map[string]int, days)
	for _, request := range r.billing {
		if request.CreatedAt.Before(startedAt) || !request.CreatedAt.Before(endedAt) {
			continue
		}
		date := billingDayKey(request.CreatedAt)
		if date == "" {
			continue
		}
		byDate[date]++
	}
	out := make([]BillingRequestDayCountSummary, 0, len(byDate))
	for date, count := range byDate {
		out = append(out, BillingRequestDayCountSummary{Date: date, Count: count})
	}
	slices.SortFunc(out, func(a, b BillingRequestDayCountSummary) int {
		return strings.Compare(a.Date, b.Date)
	})
	return out
}

func (r *MemoryRepository) ListBillingUsageSpendByHourForUser(userID string, startedAt, endedAt time.Time) []BillingUsageSpendHourSummary {
	userID = strings.TrimSpace(userID)
	if userID == "" || startedAt.IsZero() || endedAt.IsZero() || !startedAt.Before(endedAt) {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	byHour := make(map[int]int64)
	for _, request := range r.billing {
		if strings.TrimSpace(request.UserID) != userID {
			continue
		}
		if request.CreatedAt.Before(startedAt) || !request.CreatedAt.Before(endedAt) {
			continue
		}
		amount := billingRequestUsageSpendAmount(request.Status, request.ReservedNanoUSD, request.ActualNanoUSD)
		if amount <= 0 {
			continue
		}
		hour := request.CreatedAt.Local().Hour()
		if hour < 0 || hour > 23 {
			continue
		}
		byHour[hour] = addNanoUSDSaturating(byHour[hour], amount)
	}
	out := make([]BillingUsageSpendHourSummary, 0, len(byHour))
	for hour, spend := range byHour {
		out = append(out, BillingUsageSpendHourSummary{Hour: hour, SpendNanoUSD: spend})
	}
	slices.SortFunc(out, func(a, b BillingUsageSpendHourSummary) int {
		return a.Hour - b.Hour
	})
	return out
}

func (r *MemoryRepository) ListBillingUsageSpendByClient() []BillingUsageSpendSummary {
	return r.listBillingUsageSpendByClient("")
}

func (r *MemoryRepository) ListBillingUsageSpendByClientForUser(userID string) []BillingUsageSpendSummary {
	return r.listBillingUsageSpendByClient(strings.TrimSpace(userID))
}

func (r *MemoryRepository) listBillingUsageSpendByClient(userID string) []BillingUsageSpendSummary {
	r.mu.RLock()
	defer r.mu.RUnlock()

	byKey := make(map[string]BillingUsageSpendSummary)
	addSpend := func(userID, clientID string, amount int64) {
		userID = strings.TrimSpace(userID)
		clientID = strings.TrimSpace(clientID)
		if userID == "" || clientID == "" || amount == 0 {
			return
		}
		key := userID + "\x00" + clientID
		summary := byKey[key]
		summary.UserID = userID
		summary.ClientID = clientID
		next, err := addNanoUSD(summary.SpendNanoUSD, amount)
		if err != nil {
			if amount > 0 {
				next = maxInt64
			} else {
				next = minInt64
			}
		}
		summary.SpendNanoUSD = next
		byKey[key] = summary
	}

	requests := make(map[string]struct{}, len(r.billing))
	for _, request := range r.billing {
		requests[billingRequestKey(request.RequestID, request.ClientID)] = struct{}{}
		if userID != "" && strings.TrimSpace(request.UserID) != userID {
			continue
		}
		addSpend(request.UserID, request.ClientID, billingRequestHistoricalSpendAmount(request.Status, request.ActualNanoUSD))
	}
	for _, entry := range r.ledger {
		if entry.RequestID == "" || entry.ClientID == "" {
			continue
		}
		if _, ok := requests[billingRequestKey(entry.RequestID, entry.ClientID)]; ok {
			continue
		}
		if userID != "" && strings.TrimSpace(entry.UserID) != userID {
			continue
		}
		addSpend(entry.UserID, entry.ClientID, billingLedgerUsageSpendDelta(entry.Kind, entry.AmountNanoUSD))
	}
	for _, entry := range r.planLedger {
		if entry.RequestID == "" || entry.ClientID == "" {
			continue
		}
		if _, ok := requests[billingRequestKey(entry.RequestID, entry.ClientID)]; ok {
			continue
		}
		if userID != "" && strings.TrimSpace(entry.UserID) != userID {
			continue
		}
		addSpend(entry.UserID, entry.ClientID, planQuotaLedgerUsageSpendDelta(entry.Kind, entry.AmountNanoUSD))
	}

	out := make([]BillingUsageSpendSummary, 0, len(byKey))
	for _, summary := range byKey {
		if summary.SpendNanoUSD <= 0 {
			continue
		}
		out = append(out, summary)
	}
	slices.SortFunc(out, func(a, b BillingUsageSpendSummary) int {
		if a.UserID < b.UserID {
			return -1
		}
		if a.UserID > b.UserID {
			return 1
		}
		if a.ClientID < b.ClientID {
			return -1
		}
		if a.ClientID > b.ClientID {
			return 1
		}
		return 0
	})
	return out
}

func (r *MemoryRepository) ListClientActualSpends() []core.ClientSpend {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]core.ClientSpend, 0, len(r.clients))
	for _, client := range r.clients {
		clientID := strings.TrimSpace(client.ID)
		if clientID == "" {
			continue
		}
		spend := r.clientSpend[clientID]
		if spend.ClientID == "" {
			spend = core.ClientSpend{
				ClientID:          clientID,
				SpendLimitNanoUSD: client.SpendLimitNanoUSD,
			}
		}
		spend.ClientID = clientID
		spend.SpendLimitNanoUSD = client.SpendLimitNanoUSD
		out = append(out, spend)
	}
	slices.SortFunc(out, func(a, b core.ClientSpend) int {
		if a.ClientID < b.ClientID {
			return -1
		}
		if a.ClientID > b.ClientID {
			return 1
		}
		return 0
	})
	return out
}

func (r *MemoryRepository) ListBillingLedgerUserSummaries() []BillingLedgerUserSummary {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.trimBillingLedgerLocked(time.Now().UTC())

	byUser := make(map[string]*BillingLedgerUserSummary)
	summaryForUser := func(userID string) *BillingLedgerUserSummary {
		userID = strings.TrimSpace(userID)
		if userID == "" {
			return nil
		}
		summary := byUser[userID]
		if summary == nil {
			summary = &BillingLedgerUserSummary{UserID: userID}
			byUser[userID] = summary
		}
		return summary
	}

	for _, entry := range r.ledger {
		summary := summaryForUser(entry.UserID)
		if summary == nil {
			continue
		}
		switch strings.TrimSpace(entry.Kind) {
		case "manual_credit", "account_merge":
			summary.RewardNanoUSD = addNanoUSDSaturating(summary.RewardNanoUSD, entry.AmountNanoUSD)
		case "manual_debit", "plan_purchase":
			if entry.AmountNanoUSD < 0 {
				summary.SpendNanoUSD = addNanoUSDSaturating(summary.SpendNanoUSD, absBillingNanoUSD(entry.AmountNanoUSD))
				summary.LastSpendAt = billingLatestTimePtr(summary.LastSpendAt, entry.CreatedAt)
			}
		}
		if billingLedgerUsageSpendDelta(entry.Kind, entry.AmountNanoUSD) > 0 {
			summary.LastSpendAt = billingLatestTimePtr(summary.LastSpendAt, entry.CreatedAt)
		}
	}

	out := make([]BillingLedgerUserSummary, 0, len(byUser))
	for _, summary := range byUser {
		out = append(out, *summary)
	}
	slices.SortFunc(out, func(a, b BillingLedgerUserSummary) int {
		return strings.Compare(a.UserID, b.UserID)
	})
	return out
}

func (r *MemoryRepository) ListBillingRequestsPage(query BillingRequestQuery) ([]core.BillingReservation, int) {
	query = normalizeBillingRequestQuery(query)
	r.mu.Lock()
	defer r.mu.Unlock()

	r.trimBillingRequestsLocked(time.Now().UTC())

	items := make([]core.BillingReservation, 0, len(r.billing))
	queryNoUser := query
	queryNoUser.UserID = ""
	for _, request := range r.billing {
		if query.UserID != "" && !r.userReferenceMatchesLocked(request.UserID, query.UserID) {
			continue
		}
		if billingRequestMatchesQuery(request, queryNoUser) {
			items = append(items, request)
		}
	}
	slices.SortFunc(items, func(a, b core.BillingReservation) int {
		if !a.CreatedAt.Equal(b.CreatedAt) {
			if a.CreatedAt.After(b.CreatedAt) {
				return -1
			}
			return 1
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})

	total := len(items)
	if query.Offset >= total {
		return nil, total
	}
	end := query.Offset + query.Limit
	if end > total {
		end = total
	}
	out := make([]core.BillingReservation, 0, end-query.Offset)
	for _, request := range items[query.Offset:end] {
		out = append(out, cloneBillingReservation(request))
	}
	return out, total
}

func (r *MemoryRepository) ListBillingRequests(query BillingRequestQuery) []core.BillingReservation {
	query = normalizeBillingRequestQuery(query)
	r.mu.Lock()
	defer r.mu.Unlock()

	r.trimBillingRequestsLocked(time.Now().UTC())

	items := make([]core.BillingReservation, 0, len(r.billing))
	queryNoUser := query
	queryNoUser.UserID = ""
	for _, request := range r.billing {
		if query.UserID != "" && !r.userReferenceMatchesLocked(request.UserID, query.UserID) {
			continue
		}
		if billingRequestMatchesQuery(request, queryNoUser) {
			items = append(items, cloneBillingReservation(request))
		}
	}
	slices.SortFunc(items, func(a, b core.BillingReservation) int {
		if !a.CreatedAt.Equal(b.CreatedAt) {
			if a.CreatedAt.After(b.CreatedAt) {
				return -1
			}
			return 1
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return items
}

func (r *MemoryRepository) ListBillingModelUsageSummaries(limit int) []BillingModelUsageSummary {
	r.mu.RLock()
	defer r.mu.RUnlock()

	byModel := make(map[string]*BillingModelUsageSummary)
	for _, request := range r.billing {
		model := strings.TrimSpace(request.Model)
		if model == "" {
			model = "unknown"
		}
		summary := byModel[model]
		if summary == nil {
			summary = &BillingModelUsageSummary{Model: model}
			byModel[model] = summary
		}
		summary.RequestCount++
		summary.PromptTokens = addNanoUSDSaturating(summary.PromptTokens, int64(request.PromptTokens))
		summary.CompletionTokens = addNanoUSDSaturating(summary.CompletionTokens, int64(request.CompletionTokens))
		summary.SpendNanoUSD = addNanoUSDSaturating(summary.SpendNanoUSD, billingModelUsageSpendAmount(request.Status, request.ReservedNanoUSD, request.ActualNanoUSD))
	}
	out := make([]BillingModelUsageSummary, 0, len(byModel))
	for _, summary := range byModel {
		out = append(out, *summary)
	}
	slices.SortFunc(out, func(a, b BillingModelUsageSummary) int {
		if a.SpendNanoUSD != b.SpendNanoUSD {
			if a.SpendNanoUSD > b.SpendNanoUSD {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Model, b.Model)
	})
	if limit > 0 && len(out) > limit {
		return out[:limit]
	}
	return out
}

func (r *MemoryRepository) CreatePaymentOrder(order core.PaymentOrder) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	order.ID = strings.TrimSpace(order.ID)
	order.OutTradeNo = strings.TrimSpace(order.OutTradeNo)
	order.UserID = strings.TrimSpace(order.UserID)
	if order.ID == "" || order.OutTradeNo == "" || order.UserID == "" {
		return fmt.Errorf("payment id, trade number, and user are required")
	}
	if order.AmountNanoUSD <= 0 {
		return ErrInsufficientBalance
	}
	if order.AmountNanoUSD%(core.NanoUSDPerUSD/100) != 0 {
		return fmt.Errorf("payment amount must be accurate to cents")
	}
	if _, ok := r.users[order.UserID]; !ok {
		return ErrNotFound
	}
	for _, existing := range r.payments {
		if existing.OutTradeNo == order.OutTradeNo {
			return ErrBillingRequestConflict
		}
	}
	now := time.Now().UTC()
	if order.CreatedAt.IsZero() {
		order.CreatedAt = now
	}
	order.UpdatedAt = now
	if order.Status == "" {
		order.Status = core.PaymentOrderPending
	}
	if strings.TrimSpace(order.Currency) == "" {
		order.Currency = "USD"
	}
	if strings.TrimSpace(order.ProviderCurrency) == "" {
		order.ProviderCurrency = "CNY"
	}
	r.payments[order.ID] = clonePaymentOrder(order)
	return nil
}

func (r *MemoryRepository) GetPaymentOrder(id string) (core.PaymentOrder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	order, ok := r.payments[strings.TrimSpace(id)]
	if !ok {
		return core.PaymentOrder{}, ErrNotFound
	}
	order = expirePaymentOrderIfNeeded(order, time.Now().UTC())
	r.payments[order.ID] = clonePaymentOrder(order)
	return clonePaymentOrder(order), nil
}

func (r *MemoryRepository) GetPaymentOrderByOutTradeNo(outTradeNo string) (core.PaymentOrder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	outTradeNo = strings.TrimSpace(outTradeNo)
	for id, order := range r.payments {
		if order.OutTradeNo == outTradeNo {
			order = expirePaymentOrderIfNeeded(order, time.Now().UTC())
			r.payments[id] = clonePaymentOrder(order)
			return clonePaymentOrder(order), nil
		}
	}
	return core.PaymentOrder{}, ErrNotFound
}

func (r *MemoryRepository) ListPaymentOrdersPage(query PaymentOrderQuery) ([]core.PaymentOrder, int) {
	query = normalizePaymentOrderQuery(query)
	r.mu.Lock()
	defer r.mu.Unlock()

	items := make([]core.PaymentOrder, 0, len(r.payments))
	queryNoUser := query
	queryNoUser.UserID = ""
	now := time.Now().UTC()
	for id, order := range r.payments {
		order = expirePaymentOrderIfNeeded(order, now)
		r.payments[id] = clonePaymentOrder(order)
		if query.UserID != "" && !r.userReferenceMatchesLocked(order.UserID, query.UserID) {
			continue
		}
		if paymentOrderMatchesQuery(order, queryNoUser) {
			items = append(items, clonePaymentOrder(order))
		}
	}
	slices.SortFunc(items, func(a, b core.PaymentOrder) int {
		if !a.CreatedAt.Equal(b.CreatedAt) {
			if a.CreatedAt.After(b.CreatedAt) {
				return -1
			}
			return 1
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})

	total := len(items)
	if query.Offset >= total {
		return nil, total
	}
	end := query.Offset + query.Limit
	if end > total {
		end = total
	}
	return items[query.Offset:end], total
}

func (r *MemoryRepository) ListPaymentOrders(query PaymentOrderQuery) []core.PaymentOrder {
	query = normalizePaymentOrderQuery(query)
	r.mu.Lock()
	defer r.mu.Unlock()

	items := make([]core.PaymentOrder, 0, len(r.payments))
	queryNoUser := query
	queryNoUser.UserID = ""
	now := time.Now().UTC()
	for id, order := range r.payments {
		order = expirePaymentOrderIfNeeded(order, now)
		r.payments[id] = clonePaymentOrder(order)
		if query.UserID != "" && !r.userReferenceMatchesLocked(order.UserID, query.UserID) {
			continue
		}
		if paymentOrderMatchesQuery(order, queryNoUser) {
			items = append(items, clonePaymentOrder(order))
		}
	}
	slices.SortFunc(items, func(a, b core.PaymentOrder) int {
		if !a.CreatedAt.Equal(b.CreatedAt) {
			if a.CreatedAt.After(b.CreatedAt) {
				return -1
			}
			return 1
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return items
}

func (r *MemoryRepository) userReferenceMatchesLocked(userID, ref string) bool {
	userID = strings.TrimSpace(userID)
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return true
	}
	if strings.EqualFold(userID, ref) {
		return true
	}
	user, ok := r.users[userID]
	return ok && strings.EqualFold(strings.TrimSpace(user.Username), ref)
}

func (r *MemoryRepository) UpdatePaymentOrderStatus(id string, status core.PaymentOrderStatus, providerTradeNo string, paidAt *time.Time) (core.PaymentOrder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	id = strings.TrimSpace(id)
	order, ok := r.payments[id]
	if !ok {
		return core.PaymentOrder{}, ErrNotFound
	}
	if order.Status == core.PaymentOrderPaid {
		return clonePaymentOrder(order), nil
	}
	order.Status = status
	order.ProviderTradeNo = strings.TrimSpace(providerTradeNo)
	if paidAt != nil && !paidAt.IsZero() {
		value := paidAt.UTC()
		order.PaidAt = &value
	}
	order.UpdatedAt = time.Now().UTC()
	r.payments[id] = clonePaymentOrder(order)
	return clonePaymentOrder(order), nil
}

func (r *MemoryRepository) UpdatePaymentOrderProviderState(id string, update core.PaymentOrderProviderUpdate) (core.PaymentOrder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	id = strings.TrimSpace(id)
	order, ok := r.payments[id]
	if !ok {
		return core.PaymentOrder{}, ErrNotFound
	}
	if order.Status == core.PaymentOrderPaid {
		return clonePaymentOrder(order), nil
	}
	if update.Status != "" {
		order.Status = update.Status
	}
	if strings.TrimSpace(update.ProviderStatus) != "" {
		order.ProviderStatus = strings.TrimSpace(update.ProviderStatus)
	}
	if strings.TrimSpace(update.ProviderTradeNo) != "" {
		order.ProviderTradeNo = strings.TrimSpace(update.ProviderTradeNo)
	}
	if strings.TrimSpace(update.CodeURL) != "" {
		order.CodeURL = strings.TrimSpace(update.CodeURL)
	}
	if strings.TrimSpace(update.PayURL) != "" {
		order.PayURL = strings.TrimSpace(update.PayURL)
	}
	if strings.TrimSpace(update.PrepayID) != "" {
		order.PrepayID = strings.TrimSpace(update.PrepayID)
	}
	if strings.TrimSpace(update.RawResponse) != "" {
		order.RawResponse = update.RawResponse
	}
	if update.PaidAt != nil && !update.PaidAt.IsZero() {
		value := update.PaidAt.UTC()
		order.PaidAt = &value
	}
	order.UpdatedAt = time.Now().UTC()
	r.payments[id] = clonePaymentOrder(order)
	return clonePaymentOrder(order), nil
}

func (r *MemoryRepository) DeletePendingPaymentOrder(id string) (core.PaymentOrder, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	id = strings.TrimSpace(id)
	order, ok := r.payments[id]
	if !ok {
		return core.PaymentOrder{}, false, ErrNotFound
	}
	if order.Status != core.PaymentOrderPending {
		r.payments[id] = clonePaymentOrder(order)
		return clonePaymentOrder(order), false, nil
	}
	delete(r.payments, id)
	return clonePaymentOrder(order), true, nil
}

func (r *MemoryRepository) CompletePaymentOrder(outTradeNo, providerTradeNo string, paidAmountNanoUSD int64, paidAt time.Time) (core.PaymentOrder, bool, error) {
	return r.CompletePaymentOrderWithCredits(outTradeNo, providerTradeNo, paidAmountNanoUSD, paidAt, nil)
}

func (r *MemoryRepository) CompletePaymentOrderWithCredits(outTradeNo, providerTradeNo string, paidAmountNanoUSD int64, paidAt time.Time, credits []core.PaymentOrderBalanceCredit) (core.PaymentOrder, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	outTradeNo = strings.TrimSpace(outTradeNo)
	orderID := ""
	var order core.PaymentOrder
	for id, existing := range r.payments {
		if existing.OutTradeNo == outTradeNo {
			orderID = id
			order = existing
			break
		}
	}
	if orderID == "" {
		return core.PaymentOrder{}, false, ErrNotFound
	}
	if order.Status == core.PaymentOrderPaid {
		if !paidPaymentReplayMatches(order, providerTradeNo, paidAmountNanoUSD) {
			return core.PaymentOrder{}, false, ErrBillingRequestConflict
		}
		return clonePaymentOrder(order), false, nil
	}
	if paidAmountNanoUSD != order.AmountNanoUSD {
		return core.PaymentOrder{}, false, ErrBillingRequestConflict
	}
	user, ok := r.users[order.UserID]
	if !ok {
		return core.PaymentOrder{}, false, ErrNotFound
	}
	nextBalance, err := addNanoUSD(user.BalanceNanoUSD, order.AmountNanoUSD)
	if err != nil {
		return core.PaymentOrder{}, false, err
	}
	now := time.Now().UTC()
	if paidAt.IsZero() {
		paidAt = now
	}
	normalizedCredits, err := normalizePaymentOrderBalanceCredits(order.OutTradeNo, credits)
	if err != nil {
		return core.PaymentOrder{}, false, err
	}
	balances := map[string]int64{user.ID: nextBalance}
	affectedUsers := map[string]core.User{user.ID: user}
	ledgerIDs := make(map[string]struct{}, len(normalizedCredits)+1)
	for _, entry := range r.ledger {
		if strings.TrimSpace(entry.ID) != "" {
			ledgerIDs[entry.ID] = struct{}{}
		}
	}
	paymentLedgerID := billingLedgerID("payment", order.OutTradeNo, string(order.Provider), now)
	if _, exists := ledgerIDs[paymentLedgerID]; exists {
		return core.PaymentOrder{}, false, ErrBillingRequestConflict
	}
	ledgerIDs[paymentLedgerID] = struct{}{}
	creditLedgers := make([]core.BillingLedgerEntry, 0, len(normalizedCredits))
	for _, credit := range normalizedCredits {
		if _, exists := ledgerIDs[credit.LedgerID]; exists {
			return core.PaymentOrder{}, false, ErrBillingRequestConflict
		}
		creditUser, ok := r.users[credit.UserID]
		if !ok {
			return core.PaymentOrder{}, false, ErrNotFound
		}
		previousBalance, ok := balances[credit.UserID]
		if !ok {
			previousBalance = creditUser.BalanceNanoUSD
		}
		creditBalance, err := addNanoUSD(previousBalance, credit.AmountNanoUSD)
		if err != nil {
			return core.PaymentOrder{}, false, err
		}
		balances[credit.UserID] = creditBalance
		affectedUsers[credit.UserID] = creditUser
		ledgerIDs[credit.LedgerID] = struct{}{}
		creditLedgers = append(creditLedgers, core.BillingLedgerEntry{
			ID:                  credit.LedgerID,
			UserID:              credit.UserID,
			Kind:                credit.Kind,
			AmountNanoUSD:       credit.AmountNanoUSD,
			BalanceAfterNanoUSD: creditBalance,
			Note:                credit.Note,
			CreatedAt:           now,
		})
	}

	order.Status = core.PaymentOrderPaid
	order.ProviderTradeNo = strings.TrimSpace(providerTradeNo)
	order.PaidAt = &paidAt
	order.UpdatedAt = now
	r.payments[orderID] = clonePaymentOrder(order)
	for userID, affected := range affectedUsers {
		affected.BalanceNanoUSD = balances[userID]
		affected.UpdatedAt = now
		r.users[userID] = cloneUser(affected)
	}
	ledgerEntries := []core.BillingLedgerEntry{{
		ID:                  paymentLedgerID,
		UserID:              order.UserID,
		Kind:                "payment",
		AmountNanoUSD:       order.AmountNanoUSD,
		BalanceAfterNanoUSD: nextBalance,
		Note:                fmt.Sprintf("%s %s", order.Provider, order.OutTradeNo),
		CreatedAt:           now,
	}}
	ledgerEntries = append(ledgerEntries, creditLedgers...)
	r.ledger = append(append([]core.BillingLedgerEntry{}, ledgerEntries...), r.ledger...)
	return clonePaymentOrder(order), true, nil
}

func (r *MemoryRepository) CreatePaymentRefund(refund core.PaymentRefund) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	refund.ID = strings.TrimSpace(refund.ID)
	refund.OrderID = strings.TrimSpace(refund.OrderID)
	refund.OutTradeNo = strings.TrimSpace(refund.OutTradeNo)
	refund.UserID = strings.TrimSpace(refund.UserID)
	if refund.ID == "" || refund.OrderID == "" || refund.OutTradeNo == "" || refund.UserID == "" || refund.AmountNanoUSD <= 0 {
		return fmt.Errorf("payment refund is incomplete")
	}
	if _, exists := r.refunds[refund.ID]; exists {
		return ErrBillingRequestConflict
	}
	order, ok := r.payments[refund.OrderID]
	if !ok {
		return ErrNotFound
	}
	if order.Status != core.PaymentOrderPaid || order.OutTradeNo != refund.OutTradeNo || order.UserID != refund.UserID || order.Provider != refund.Provider {
		return ErrBillingRequestConflict
	}
	user, ok := r.users[refund.UserID]
	if !ok {
		return ErrNotFound
	}
	availableBalance := user.BalanceNanoUSD - pendingPaymentRefundAmountLocked(r.refunds, refund.UserID)
	if availableBalance < 0 {
		availableBalance = 0
	}
	if refund.AmountNanoUSD > availableBalance {
		return ErrInsufficientBalance
	}
	if refund.AmountNanoUSD > refundablePaymentAmountLocked(r.refunds, order) {
		return ErrInsufficientBalance
	}
	now := time.Now().UTC()
	if refund.CreatedAt.IsZero() {
		refund.CreatedAt = now
	}
	refund.UpdatedAt = now
	if refund.Status == "" {
		refund.Status = core.PaymentRefundPending
	}
	if refund.Status != core.PaymentRefundPending {
		return ErrBillingRequestConflict
	}
	r.refunds[refund.ID] = clonePaymentRefund(refund)
	return nil
}

func (r *MemoryRepository) CompletePaymentRefund(id, providerRefundNo, rawResponse string) (core.PaymentRefund, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	id = strings.TrimSpace(id)
	refund, ok := r.refunds[id]
	if !ok {
		return core.PaymentRefund{}, false, ErrNotFound
	}
	if refund.Status == core.PaymentRefundDone {
		return clonePaymentRefund(refund), false, nil
	}
	if refund.Status != core.PaymentRefundPending {
		return core.PaymentRefund{}, false, ErrBillingRequestConflict
	}
	user, ok := r.users[refund.UserID]
	if !ok {
		return core.PaymentRefund{}, false, ErrNotFound
	}
	if user.BalanceNanoUSD < refund.AmountNanoUSD {
		return core.PaymentRefund{}, false, ErrInsufficientBalance
	}
	now := time.Now().UTC()
	user.BalanceNanoUSD -= refund.AmountNanoUSD
	user.UpdatedAt = now
	r.users[user.ID] = cloneUser(user)

	refund.Status = core.PaymentRefundDone
	refund.ProviderRefundNo = strings.TrimSpace(providerRefundNo)
	refund.RawResponse = rawResponse
	if refund.ManualPayoutRef == "" {
		refund.ManualPayoutRef = refund.ProviderRefundNo
	}
	if refund.ManualPayoutNote == "" {
		refund.ManualPayoutNote = strings.TrimSpace(rawResponse)
	}
	if refund.ManualPayoutAt == nil {
		refund.ManualPayoutAt = &now
	}
	refund.UpdatedAt = now
	r.refunds[id] = clonePaymentRefund(refund)
	r.ledger = append([]core.BillingLedgerEntry{{
		ID:                  billingLedgerID("payment_refund", refund.ID, refund.OrderID, now),
		UserID:              refund.UserID,
		Kind:                "payment_refund",
		AmountNanoUSD:       -refund.AmountNanoUSD,
		BalanceAfterNanoUSD: user.BalanceNanoUSD,
		Note:                refund.OutTradeNo,
		CreatedAt:           now,
	}}, r.ledger...)
	return clonePaymentRefund(refund), true, nil
}

func (r *MemoryRepository) FailPaymentRefund(id, rawResponse string) (core.PaymentRefund, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	refund, ok := r.refunds[strings.TrimSpace(id)]
	if !ok {
		return core.PaymentRefund{}, ErrNotFound
	}
	if refund.Status == core.PaymentRefundDone {
		return core.PaymentRefund{}, ErrBillingRequestConflict
	}
	if refund.Status != core.PaymentRefundFailed {
		refund.Status = core.PaymentRefundFailed
		refund.RawResponse = rawResponse
		refund.UpdatedAt = time.Now().UTC()
		r.refunds[refund.ID] = clonePaymentRefund(refund)
	}
	return clonePaymentRefund(refund), nil
}

func (r *MemoryRepository) ListPaymentRefunds(orderID string) []core.PaymentRefund {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]core.PaymentRefund, 0)
	orderID = strings.TrimSpace(orderID)
	for _, refund := range r.refunds {
		if orderID != "" && refund.OrderID != orderID {
			continue
		}
		out = append(out, clonePaymentRefund(refund))
	}
	slices.SortFunc(out, func(a, b core.PaymentRefund) int {
		if cmp := b.CreatedAt.Compare(a.CreatedAt); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

func (r *MemoryRepository) CreateSiteMessage(message core.SiteMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	message.ID = strings.TrimSpace(message.ID)
	message.Title = strings.TrimSpace(message.Title)
	message.Body = strings.TrimSpace(message.Body)
	message.CreatedBy = strings.TrimSpace(message.CreatedBy)
	if message.ID == "" || message.Title == "" || message.Body == "" {
		return fmt.Errorf("message id, title, and body are required")
	}
	if _, ok := r.messages[message.ID]; ok {
		return ErrBillingRequestConflict
	}
	now := time.Now().UTC()
	if message.CreatedAt.IsZero() {
		message.CreatedAt = now
	}
	if message.UpdatedAt.IsZero() {
		message.UpdatedAt = message.CreatedAt
	}
	r.messages[message.ID] = cloneSiteMessage(message)
	return nil
}

func (r *MemoryRepository) UpdateSiteMessage(message core.SiteMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	message.ID = strings.TrimSpace(message.ID)
	existing, ok := r.messages[message.ID]
	if !ok {
		return ErrNotFound
	}
	message.Title = strings.TrimSpace(message.Title)
	message.Body = strings.TrimSpace(message.Body)
	if message.Title == "" || message.Body == "" {
		return fmt.Errorf("message title and body are required")
	}
	message.CreatedBy = existing.CreatedBy
	message.CreatedAt = existing.CreatedAt
	message.UpdatedAt = time.Now().UTC()
	r.messages[message.ID] = cloneSiteMessage(message)
	return nil
}

func (r *MemoryRepository) GetSiteMessage(id string) (core.SiteMessage, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	message, ok := r.messages[strings.TrimSpace(id)]
	if !ok {
		return core.SiteMessage{}, ErrNotFound
	}
	return cloneSiteMessage(message), nil
}

func (r *MemoryRepository) DeleteSiteMessage(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	id = strings.TrimSpace(id)
	if _, ok := r.messages[id]; !ok {
		return ErrNotFound
	}
	delete(r.messages, id)
	for key, read := range r.messageRead {
		if read.MessageID == id {
			delete(r.messageRead, key)
		}
	}
	return nil
}

func (r *MemoryRepository) ListSiteMessages() []core.SiteMessage {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]core.SiteMessage, 0, len(r.messages))
	for _, message := range r.messages {
		out = append(out, cloneSiteMessage(message))
	}
	sortSiteMessages(out)
	return out
}

func (r *MemoryRepository) ListSiteMessageDeliveries(userID string, includeDisabled bool) []core.SiteMessageDelivery {
	r.mu.RLock()
	defer r.mu.RUnlock()

	userID = strings.TrimSpace(userID)
	messages := make([]core.SiteMessage, 0, len(r.messages))
	for _, message := range r.messages {
		if !includeDisabled && !message.Enabled {
			continue
		}
		messages = append(messages, cloneSiteMessage(message))
	}
	sortSiteMessages(messages)
	out := make([]core.SiteMessageDelivery, 0, len(messages))
	for _, message := range messages {
		delivery := core.SiteMessageDelivery{Message: message}
		if read, ok := r.messageRead[siteMessageReadKey(message.ID, userID)]; ok {
			delivery.Read = true
			value := read.ReadAt
			delivery.ReadAt = &value
		}
		out = append(out, delivery)
	}
	return out
}

func (r *MemoryRepository) ListSiteMessageDeliveriesPage(userID string, includeDisabled bool, offset, limit int) ([]core.SiteMessageDelivery, int) {
	deliveries := r.ListSiteMessageDeliveries(userID, includeDisabled)
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

func (r *MemoryRepository) ListVisibleSiteMessageDeliveriesPage(query SiteMessageVisibilityQuery) ([]core.SiteMessageDelivery, int) {
	query = normalizeSiteMessageVisibilityQuery(query)
	if query.UserID == "" {
		return nil, 0
	}
	deliveries := r.ListSiteMessageDeliveries(query.UserID, false)
	return visibleSiteMessageDeliveriesPage(deliveries, query)
}

func (r *MemoryRepository) MarkSiteMessageRead(messageID, userID string, readAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	messageID = strings.TrimSpace(messageID)
	userID = strings.TrimSpace(userID)
	if _, ok := r.messages[messageID]; !ok {
		return ErrNotFound
	}
	if _, ok := r.users[userID]; !ok {
		return ErrNotFound
	}
	if readAt.IsZero() {
		readAt = time.Now().UTC()
	}
	r.messageRead[siteMessageReadKey(messageID, userID)] = core.SiteMessageRead{MessageID: messageID, UserID: userID, ReadAt: readAt.UTC()}
	return nil
}

func (r *MemoryRepository) ClearSiteMessageReads(messageID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	messageID = strings.TrimSpace(messageID)
	if _, ok := r.messages[messageID]; !ok {
		return ErrNotFound
	}
	for key, read := range r.messageRead {
		if read.MessageID == messageID {
			delete(r.messageRead, key)
		}
	}
	return nil
}

func (r *MemoryRepository) SiteMessageUnreadCount(userID string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	userID = strings.TrimSpace(userID)
	count := 0
	for _, message := range r.messages {
		if !message.Enabled || message.PublicPopup {
			continue
		}
		if _, ok := r.messageRead[siteMessageReadKey(message.ID, userID)]; !ok {
			count++
		}
	}
	return count
}

func (r *MemoryRepository) VisibleSiteMessageUnreadCount(query SiteMessageVisibilityQuery) int {
	query = normalizeSiteMessageVisibilityQuery(query)
	if query.UserID == "" {
		return 0
	}
	deliveries := r.ListSiteMessageDeliveries(query.UserID, false)
	return visibleSiteMessageUnreadCount(deliveries, query)
}

func (r *MemoryRepository) CreateSupportTicket(ticket core.SupportTicket, firstMessage core.SupportMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	ticket = normalizeSupportTicketForStorage(ticket)
	firstMessage = normalizeSupportMessageForStorage(firstMessage)
	if ticket.ID == "" || ticket.UserID == "" || ticket.Title == "" || firstMessage.ID == "" || firstMessage.TicketID != ticket.ID || firstMessage.Body == "" {
		return fmt.Errorf("support ticket id, user, title, and first message are required")
	}
	if _, ok := r.supportTickets[ticket.ID]; ok {
		return ErrBillingRequestConflict
	}
	if _, ok := r.users[ticket.UserID]; !ok {
		return ErrNotFound
	}
	r.supportTickets[ticket.ID] = cloneSupportTicket(ticket)
	r.supportMessages[ticket.ID] = []core.SupportMessage{cloneSupportMessage(firstMessage)}
	return nil
}

func (r *MemoryRepository) GetSupportTicket(id string) (core.SupportTicket, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ticket, ok := r.supportTickets[strings.TrimSpace(id)]
	if !ok {
		return core.SupportTicket{}, ErrNotFound
	}
	return cloneSupportTicket(ticket), nil
}

func (r *MemoryRepository) ListSupportTicketsPage(query SupportTicketQuery) ([]core.SupportTicket, int) {
	query = normalizeSupportTicketQuery(query)
	r.mu.RLock()
	defer r.mu.RUnlock()

	filtered := make([]core.SupportTicket, 0, len(r.supportTickets))
	for _, ticket := range r.supportTickets {
		if !supportTicketMatchesQuery(ticket, query) {
			continue
		}
		filtered = append(filtered, cloneSupportTicket(ticket))
	}
	sortSupportTickets(filtered)
	return supportTicketPage(filtered, query.Offset, query.Limit)
}

func (r *MemoryRepository) ListSupportMessages(ticketID string, limit int) []core.SupportMessage {
	r.mu.RLock()
	defer r.mu.RUnlock()

	messages := r.supportMessages[strings.TrimSpace(ticketID)]
	if limit <= 0 || limit > len(messages) {
		limit = len(messages)
	}
	start := len(messages) - limit
	if start < 0 {
		start = 0
	}
	out := make([]core.SupportMessage, 0, limit)
	for _, message := range messages[start:] {
		out = append(out, cloneSupportMessage(message))
	}
	return out
}

func (r *MemoryRepository) AppendSupportMessage(ticketID string, message core.SupportMessage, ticket core.SupportTicket) (core.SupportTicket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	ticketID = strings.TrimSpace(ticketID)
	existing, ok := r.supportTickets[ticketID]
	if !ok {
		return core.SupportTicket{}, ErrNotFound
	}
	message = normalizeSupportMessageForStorage(message)
	ticket = normalizeSupportTicketForStorage(ticket)
	if message.ID == "" || message.TicketID != ticketID || message.Body == "" || ticket.ID != ticketID {
		return core.SupportTicket{}, fmt.Errorf("support message and ticket update are required")
	}
	if existing.UserID != ticket.UserID {
		return core.SupportTicket{}, ErrNotFound
	}
	for _, existingMessage := range r.supportMessages[ticketID] {
		if existingMessage.ID == message.ID {
			return core.SupportTicket{}, ErrBillingRequestConflict
		}
	}
	r.supportMessages[ticketID] = append(r.supportMessages[ticketID], cloneSupportMessage(message))
	r.supportTickets[ticketID] = cloneSupportTicket(ticket)
	return cloneSupportTicket(ticket), nil
}

func (r *MemoryRepository) DeleteSupportTicket(ticketID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	ticketID = strings.TrimSpace(ticketID)
	if ticketID == "" {
		return ErrNotFound
	}
	if _, ok := r.supportTickets[ticketID]; !ok {
		return ErrNotFound
	}
	delete(r.supportTickets, ticketID)
	delete(r.supportMessages, ticketID)
	return nil
}

func (r *MemoryRepository) MarkSupportTicketRead(ticketID string, user core.User, readAt time.Time) (core.SupportTicket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	ticket, ok := r.supportTickets[strings.TrimSpace(ticketID)]
	if !ok {
		return core.SupportTicket{}, ErrNotFound
	}
	if readAt.IsZero() {
		readAt = time.Now().UTC()
	} else {
		readAt = readAt.UTC()
	}
	if user.IsAdmin() {
		value := readAt.UTC()
		ticket.LastReadByAdminAt = &value
	} else {
		if ticket.UserID != strings.TrimSpace(user.ID) {
			return core.SupportTicket{}, ErrNotFound
		}
		value := readAt.UTC()
		ticket.LastReadByUserAt = &value
	}
	r.supportTickets[ticket.ID] = cloneSupportTicket(ticket)
	return cloneSupportTicket(ticket), nil
}

func (r *MemoryRepository) SupportUnreadCount(user core.User) int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	count := 0
	for _, ticket := range r.supportTickets {
		if supportTicketUnreadForUser(ticket, user) {
			count++
		}
	}
	return count
}

func (r *MemoryRepository) CreateDocument(document core.Document) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	document.ID = strings.TrimSpace(document.ID)
	document.Slug = normalizeDocumentSlugForStorage(document.Slug)
	document.Title = strings.TrimSpace(document.Title)
	document.Body = strings.TrimSpace(document.Body)
	if document.ID == "" || document.Slug == "" || document.Title == "" || document.Body == "" {
		return fmt.Errorf("document id, slug, title, and body are required")
	}
	if _, ok := r.documents[document.ID]; ok {
		return ErrBillingRequestConflict
	}
	slugKey := documentSlugKey(document.Slug)
	for _, existing := range r.documents {
		if documentSlugKey(existing.Slug) == slugKey {
			return ErrBillingRequestConflict
		}
	}
	now := time.Now().UTC()
	if document.CreatedAt.IsZero() {
		document.CreatedAt = now
	}
	if document.UpdatedAt.IsZero() {
		document.UpdatedAt = document.CreatedAt
	}
	r.documents[document.ID] = cloneDocument(document)
	return nil
}

func (r *MemoryRepository) UpdateDocument(document core.Document) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	document.ID = strings.TrimSpace(document.ID)
	existing, ok := r.documents[document.ID]
	if !ok {
		return ErrNotFound
	}
	document.Slug = normalizeDocumentSlugForStorage(document.Slug)
	document.Title = strings.TrimSpace(document.Title)
	document.Body = strings.TrimSpace(document.Body)
	if document.Slug == "" || document.Title == "" || document.Body == "" {
		return fmt.Errorf("document slug, title, and body are required")
	}
	slugKey := documentSlugKey(document.Slug)
	for _, candidate := range r.documents {
		if candidate.ID != document.ID && documentSlugKey(candidate.Slug) == slugKey {
			return ErrBillingRequestConflict
		}
	}
	document.CreatedBy = existing.CreatedBy
	document.CreatedAt = existing.CreatedAt
	document.UpdatedAt = time.Now().UTC()
	r.documents[document.ID] = cloneDocument(document)
	return nil
}

func (r *MemoryRepository) GetDocument(id string) (core.Document, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	document, ok := r.documents[strings.TrimSpace(id)]
	if !ok {
		return core.Document{}, ErrNotFound
	}
	return cloneDocument(document), nil
}

func (r *MemoryRepository) GetDocumentBySlug(slug string) (core.Document, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	slugKey := documentSlugKey(slug)
	for _, document := range r.documents {
		if documentSlugKey(document.Slug) == slugKey {
			return cloneDocument(document), nil
		}
	}
	return core.Document{}, ErrNotFound
}

func (r *MemoryRepository) DeleteDocument(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	id = strings.TrimSpace(id)
	if _, ok := r.documents[id]; !ok {
		return ErrNotFound
	}
	delete(r.documents, id)
	return nil
}

func (r *MemoryRepository) ListDocuments() []core.Document {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]core.Document, 0, len(r.documents))
	for _, document := range r.documents {
		out = append(out, cloneDocument(document))
	}
	return sortDocuments(out)
}

func (r *MemoryRepository) ListDocumentsPage(status core.DocumentStatus, seoOnly bool, offset, limit int) ([]core.Document, int) {
	return documentPage(r.ListDocuments(), status, seoOnly, offset, limit)
}

func (r *MemoryRepository) SearchDocumentsPage(query string, status core.DocumentStatus, seoOnly bool, offset, limit int) ([]core.Document, int) {
	return documentSearchPage(r.ListDocuments(), query, status, seoOnly, offset, limit)
}

func (r *MemoryRepository) GetDocumentRedirect(fromSlug string) (core.DocumentRedirect, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	redirect, ok := r.docRedirects[documentSlugKey(fromSlug)]
	if !ok {
		return core.DocumentRedirect{}, ErrNotFound
	}
	return redirect, nil
}

func (r *MemoryRepository) UpsertDocumentRedirect(redirect core.DocumentRedirect) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	redirect.FromSlug = normalizeDocumentSlugForStorage(redirect.FromSlug)
	redirect.ToSlug = normalizeDocumentSlugForStorage(redirect.ToSlug)
	if redirect.FromSlug == "" || redirect.ToSlug == "" || documentSlugKey(redirect.FromSlug) == documentSlugKey(redirect.ToSlug) {
		return fmt.Errorf("document redirect source and target slugs are required")
	}
	if redirect.StatusCode == 0 {
		redirect.StatusCode = 301
	}
	if redirect.CreatedAt.IsZero() {
		redirect.CreatedAt = time.Now().UTC()
	}
	r.docRedirects[documentSlugKey(redirect.FromSlug)] = redirect
	return nil
}

func (r *MemoryRepository) DeleteDocumentRedirect(fromSlug string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := documentSlugKey(fromSlug)
	if _, ok := r.docRedirects[key]; !ok {
		return ErrNotFound
	}
	delete(r.docRedirects, key)
	return nil
}

func (r *MemoryRepository) ListMonitorTargets() []core.MonitorTarget {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]core.MonitorTarget, 0, len(r.monitorTargets))
	for _, target := range r.monitorTargets {
		out = append(out, cloneMonitorTarget(target))
	}
	return sortMonitorTargets(out)
}

func (r *MemoryRepository) GetMonitorTarget(id string) (core.MonitorTarget, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	target, ok := r.monitorTargets[strings.TrimSpace(id)]
	if !ok {
		return core.MonitorTarget{}, ErrNotFound
	}
	return cloneMonitorTarget(target), nil
}

func (r *MemoryRepository) UpsertMonitorTarget(target core.MonitorTarget) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	target.ID = strings.TrimSpace(target.ID)
	if target.ID == "" {
		return fmt.Errorf("monitor target id is required")
	}
	target.AccountGroup = core.NormalizeAccountGroupName(target.AccountGroup)
	target.Model = strings.TrimSpace(target.Model)
	if target.Model == "" {
		return fmt.Errorf("monitor model is required")
	}
	if strings.TrimSpace(target.Name) == "" {
		target.Name = target.AccountGroup + " / " + target.Model
	}
	now := time.Now().UTC()
	if existing, ok := r.monitorTargets[target.ID]; ok && !existing.CreatedAt.IsZero() {
		target.CreatedAt = existing.CreatedAt
	}
	if target.CreatedAt.IsZero() {
		target.CreatedAt = now
	}
	target.UpdatedAt = now
	r.monitorTargets[target.ID] = cloneMonitorTarget(target)
	return nil
}

func (r *MemoryRepository) DeleteMonitorTarget(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	id = strings.TrimSpace(id)
	if _, ok := r.monitorTargets[id]; !ok {
		return ErrNotFound
	}
	delete(r.monitorTargets, id)
	filtered := r.monitorResults[:0]
	for _, result := range r.monitorResults {
		if result.TargetID != id {
			filtered = append(filtered, result)
		}
	}
	r.monitorResults = filtered
	return nil
}

func (r *MemoryRepository) AppendMonitorResult(result core.MonitorResult) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	result.ID = strings.TrimSpace(result.ID)
	if result.ID == "" {
		return fmt.Errorf("monitor result id is required")
	}
	result.TargetID = strings.TrimSpace(result.TargetID)
	if result.TargetID == "" {
		return fmt.Errorf("monitor target id is required")
	}
	if result.CheckedAt.IsZero() {
		result.CheckedAt = time.Now().UTC()
	} else {
		result.CheckedAt = result.CheckedAt.UTC()
	}
	if result.Status == "" {
		result.Status = core.MonitorStatusUnknown
	}
	r.monitorResults = append(r.monitorResults, cloneMonitorResult(result))
	r.trimMonitorResultsLocked(result.TargetID)
	return nil
}

func (r *MemoryRepository) ListMonitorResults(targetID string, limit int) []core.MonitorResult {
	r.mu.RLock()
	defer r.mu.RUnlock()

	targetID = strings.TrimSpace(targetID)
	if limit <= 0 {
		limit = 100
	}
	out := make([]core.MonitorResult, 0, len(r.monitorResults))
	for _, result := range r.monitorResults {
		if targetID != "" && result.TargetID != targetID {
			continue
		}
		out = append(out, cloneMonitorResult(result))
	}
	out = sortMonitorResults(out)
	if len(out) > limit {
		return append([]core.MonitorResult(nil), out[:limit]...)
	}
	return out
}

func (r *MemoryRepository) ListLatestMonitorResults() map[string]core.MonitorResult {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make(map[string]core.MonitorResult, len(r.monitorTargets))
	for _, result := range r.monitorResults {
		if result.TargetID == "" {
			continue
		}
		if existing, ok := out[result.TargetID]; ok && !monitorResultNewer(result, existing) {
			continue
		}
		out[result.TargetID] = cloneMonitorResult(result)
	}
	return out
}

func replaceUserIDList(values []string, oldID, newID string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		next := strings.TrimSpace(value)
		if next == oldID {
			next = newID
		}
		if next == "" {
			continue
		}
		if _, ok := seen[next]; ok {
			continue
		}
		seen[next] = struct{}{}
		out = append(out, next)
	}
	return out
}

func (r *MemoryRepository) AppendAudit(event core.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.audit = append([]core.AuditEvent{cloneAuditEvent(event)}, r.audit...)
	r.trimAuditLocked()
	r.trimGatewayAuditLocked(time.Now().UTC())
	return nil
}

func (r *MemoryRepository) AppendAuditBatch(events []core.AuditEvent) error {
	if len(events) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, event := range events {
		r.audit = append([]core.AuditEvent{cloneAuditEvent(event)}, r.audit...)
	}
	r.trimAuditLocked()
	r.trimGatewayAuditLocked(time.Now().UTC())
	return nil
}

func (r *MemoryRepository) ListAudit(limit int) []core.AuditEvent {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if limit <= 0 || limit > len(r.audit) {
		limit = len(r.audit)
	}
	out := make([]core.AuditEvent, limit)
	for i := 0; i < limit; i++ {
		out[i] = cloneAuditEvent(r.audit[i])
	}
	return out
}

func (r *MemoryRepository) ListAuditSummaries(limit int) []core.AuditEvent {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if limit <= 0 || limit > len(r.audit) {
		limit = len(r.audit)
	}
	out := make([]core.AuditEvent, limit)
	for i := 0; i < limit; i++ {
		out[i] = auditSummaryEvent(r.audit[i])
	}
	return out
}

func (r *MemoryRepository) ListAuditSummariesPage(query AuditQuery) ([]core.AuditEvent, int) {
	query = normalizeAuditQuery(query)
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]core.AuditEvent, 0, query.Limit)
	total := 0
	for _, event := range r.audit {
		if !auditMatchesQuery(event, query) {
			continue
		}
		if total >= query.Offset && len(out) < query.Limit {
			out = append(out, auditSummaryEvent(event))
		}
		total++
	}
	return out, total
}

func cloneAccount(account core.Account) core.Account {
	account.Group = core.NormalizeAccountGroupName(account.Group)
	copyAccount := account
	copyAccount.EffectiveProxyURL = ""
	copyAccount.Tags = append([]string(nil), account.Tags...)
	copyAccount.LastUsedAt = cloneTimePtr(account.LastUsedAt)
	copyAccount.CooldownUntil = cloneTimePtr(account.CooldownUntil)
	copyAccount.Credential.ExpiresAt = cloneTimePtr(account.Credential.ExpiresAt)
	copyAccount.Credential.Metadata = cloneMap(account.Credential.Metadata)
	return copyAccount
}

func cloneAccountGroup(group core.AccountGroup) core.AccountGroup {
	group = core.NormalizeAccountGroupBilling(group)
	copyGroup := group
	copyGroup.VisibleUserIDs = slices.Clone(group.VisibleUserIDs)
	copyGroup.TimedMultipliers = slices.Clone(group.TimedMultipliers)
	for i := range copyGroup.TimedMultipliers {
		copyGroup.TimedMultipliers[i].Weekdays = slices.Clone(copyGroup.TimedMultipliers[i].Weekdays)
	}
	return copyGroup
}

func cloneUser(user core.User) core.User {
	copyUser := user
	copyUser.ConcurrentRequestLimitOverride = cloneIntPtr(user.ConcurrentRequestLimitOverride)
	copyUser.IPConcurrentRequestLimitOverride = cloneIntPtr(user.IPConcurrentRequestLimitOverride)
	copyUser.RequestRateLimitPerMinuteOverride = cloneIntPtr(user.RequestRateLimitPerMinuteOverride)
	copyUser.LastLoginAt = cloneTimePtr(user.LastLoginAt)
	copyUser.OAuthIdentities = slices.Clone(user.OAuthIdentities)
	return copyUser
}

func cloneUserSession(session core.UserSession) core.UserSession {
	return session
}

func cloneMCPToken(token core.MCPToken) core.MCPToken {
	copyToken := token
	copyToken.Scopes = slices.Clone(token.Scopes)
	copyToken.ExpiresAt = cloneTimePtr(token.ExpiresAt)
	copyToken.LastUsedAt = cloneTimePtr(token.LastUsedAt)
	copyToken.RevokedAt = cloneTimePtr(token.RevokedAt)
	return copyToken
}

func cloneEmailVerificationCode(code core.EmailVerificationCode) core.EmailVerificationCode {
	copyCode := code
	copyCode.UsedAt = cloneTimePtr(code.UsedAt)
	return copyCode
}

func clonePasswordResetToken(token core.PasswordResetToken) core.PasswordResetToken {
	copyToken := token
	copyToken.UsedAt = cloneTimePtr(token.UsedAt)
	return copyToken
}

func emailKey(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func cloneModel(model core.ModelConfig) core.ModelConfig {
	copyModel := model
	copyModel.LastSyncedAt = cloneTimePtr(model.LastSyncedAt)
	if model.VisibleGroups != nil {
		copyModel.VisibleGroups = append([]string{}, model.VisibleGroups...)
	}
	copyModel.PricingTiers = append([]core.ModelPricingTier(nil), model.PricingTiers...)
	return copyModel
}

func cloneSiteMessage(message core.SiteMessage) core.SiteMessage {
	if message.TargetUserIDs != nil {
		message.TargetUserIDs = append([]string{}, message.TargetUserIDs...)
	}
	if message.TargetAccountGroups != nil {
		message.TargetAccountGroups = append([]string{}, message.TargetAccountGroups...)
	}
	return message
}

func cloneSupportTicket(ticket core.SupportTicket) core.SupportTicket {
	ticket.LastReadByUserAt = cloneTimePtr(ticket.LastReadByUserAt)
	ticket.LastReadByAdminAt = cloneTimePtr(ticket.LastReadByAdminAt)
	ticket.ClosedAt = cloneTimePtr(ticket.ClosedAt)
	return ticket
}

func cloneSupportMessage(message core.SupportMessage) core.SupportMessage {
	return message
}

func cloneDocument(document core.Document) core.Document {
	copyDocument := document
	copyDocument.PublishedAt = cloneTimePtr(document.PublishedAt)
	return copyDocument
}

func cloneMonitorTarget(target core.MonitorTarget) core.MonitorTarget {
	return target
}

func cloneMonitorResult(result core.MonitorResult) core.MonitorResult {
	result.Attempts = append([]core.AttemptRecord(nil), result.Attempts...)
	return result
}

func sortSiteMessages(messages []core.SiteMessage) {
	slices.SortFunc(messages, func(a, b core.SiteMessage) int {
		if !a.CreatedAt.Equal(b.CreatedAt) {
			if a.CreatedAt.After(b.CreatedAt) {
				return -1
			}
			return 1
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
}

func normalizeSupportTicketForStorage(ticket core.SupportTicket) core.SupportTicket {
	ticket.ID = strings.TrimSpace(ticket.ID)
	ticket.UserID = strings.TrimSpace(ticket.UserID)
	ticket.Username = strings.TrimSpace(ticket.Username)
	ticket.Title = strings.TrimSpace(ticket.Title)
	ticket.LastMessage = strings.TrimSpace(ticket.LastMessage)
	ticket.LastActorID = strings.TrimSpace(ticket.LastActorID)
	ticket.Status = normalizeSupportTicketStatus(ticket.Status)
	now := time.Now().UTC()
	if ticket.CreatedAt.IsZero() {
		ticket.CreatedAt = now
	}
	if ticket.UpdatedAt.IsZero() {
		ticket.UpdatedAt = ticket.CreatedAt
	}
	if ticket.LastReadByUserAt != nil {
		value := ticket.LastReadByUserAt.UTC()
		ticket.LastReadByUserAt = &value
	}
	if ticket.LastReadByAdminAt != nil {
		value := ticket.LastReadByAdminAt.UTC()
		ticket.LastReadByAdminAt = &value
	}
	if ticket.ClosedAt != nil {
		value := ticket.ClosedAt.UTC()
		ticket.ClosedAt = &value
	}
	return ticket
}

func normalizeSupportMessageForStorage(message core.SupportMessage) core.SupportMessage {
	message.ID = strings.TrimSpace(message.ID)
	message.TicketID = strings.TrimSpace(message.TicketID)
	message.ActorID = strings.TrimSpace(message.ActorID)
	message.Body = strings.TrimSpace(message.Body)
	if message.ActorRole != core.UserRoleAdmin {
		message.ActorRole = core.UserRoleUser
	}
	if message.CreatedAt.IsZero() {
		message.CreatedAt = time.Now().UTC()
	} else {
		message.CreatedAt = message.CreatedAt.UTC()
	}
	return message
}

func normalizeSupportTicketStatus(status core.SupportTicketStatus) core.SupportTicketStatus {
	switch status {
	case core.SupportTicketPendingAdmin, core.SupportTicketPendingUser, core.SupportTicketResolved, core.SupportTicketClosed:
		return status
	default:
		return core.SupportTicketOpen
	}
}

func normalizeSupportTicketQuery(query SupportTicketQuery) SupportTicketQuery {
	query.UserID = strings.TrimSpace(query.UserID)
	query.Query = strings.ToLower(strings.TrimSpace(query.Query))
	query.Status = core.SupportTicketStatus(strings.TrimSpace(string(query.Status)))
	if query.Offset < 0 {
		query.Offset = 0
	}
	if query.Limit <= 0 {
		query.Limit = 50
	}
	if query.Limit > 200 {
		query.Limit = 200
	}
	return query
}

func supportTicketMatchesQuery(ticket core.SupportTicket, query SupportTicketQuery) bool {
	if query.UserID != "" && !strings.EqualFold(strings.TrimSpace(ticket.UserID), query.UserID) {
		return false
	}
	if query.Status != "" && ticket.Status != query.Status {
		return false
	}
	if query.Query != "" {
		haystack := strings.ToLower(strings.Join([]string{ticket.ID, ticket.UserID, ticket.Username, ticket.Title, ticket.LastMessage}, "\n"))
		if !strings.Contains(haystack, query.Query) {
			return false
		}
	}
	return true
}

func sortSupportTickets(tickets []core.SupportTicket) {
	slices.SortFunc(tickets, func(a, b core.SupportTicket) int {
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			if a.UpdatedAt.After(b.UpdatedAt) {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
}

func supportTicketPage(tickets []core.SupportTicket, offset, limit int) ([]core.SupportTicket, int) {
	total := len(tickets)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return append([]core.SupportTicket(nil), tickets[offset:end]...), total
}

func supportTicketUnreadForUser(ticket core.SupportTicket, user core.User) bool {
	if strings.TrimSpace(ticket.LastActorID) == "" {
		return false
	}
	if user.IsAdmin() {
		if ticket.Status == core.SupportTicketClosed || strings.TrimSpace(ticket.LastActorID) != strings.TrimSpace(ticket.UserID) {
			return false
		}
		return ticket.LastReadByAdminAt == nil || ticket.UpdatedAt.After(*ticket.LastReadByAdminAt)
	}
	if strings.TrimSpace(ticket.UserID) != strings.TrimSpace(user.ID) || ticket.LastActorID == user.ID {
		return false
	}
	return ticket.LastReadByUserAt == nil || ticket.UpdatedAt.After(*ticket.LastReadByUserAt)
}

func maxTime(a, b time.Time) time.Time {
	if a.IsZero() || b.After(a) {
		return b
	}
	return a
}

func sortDocuments(documents []core.Document) []core.Document {
	slices.SortFunc(documents, func(a, b core.Document) int {
		if a.Status != b.Status {
			return documentStatusRank(a.Status) - documentStatusRank(b.Status)
		}
		if a.Pinned != b.Pinned {
			if a.Pinned {
				return -1
			}
			return 1
		}
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			if a.UpdatedAt.After(b.UpdatedAt) {
				return -1
			}
			return 1
		}
		left := strings.ToLower(a.Slug)
		right := strings.ToLower(b.Slug)
		if left < right {
			return -1
		}
		if left > right {
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	return documents
}

func sortMonitorTargets(targets []core.MonitorTarget) []core.MonitorTarget {
	slices.SortFunc(targets, func(a, b core.MonitorTarget) int {
		if a.Enabled != b.Enabled {
			if a.Enabled {
				return -1
			}
			return 1
		}
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			if a.UpdatedAt.After(b.UpdatedAt) {
				return -1
			}
			return 1
		}
		an := strings.ToLower(strings.TrimSpace(a.Name))
		bn := strings.ToLower(strings.TrimSpace(b.Name))
		if an != bn {
			return strings.Compare(an, bn)
		}
		return strings.Compare(a.ID, b.ID)
	})
	return targets
}

func sortMonitorResults(results []core.MonitorResult) []core.MonitorResult {
	slices.SortFunc(results, func(a, b core.MonitorResult) int {
		if !a.CheckedAt.Equal(b.CheckedAt) {
			if a.CheckedAt.After(b.CheckedAt) {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	return results
}

func monitorResultNewer(a, b core.MonitorResult) bool {
	if !a.CheckedAt.Equal(b.CheckedAt) {
		return a.CheckedAt.After(b.CheckedAt)
	}
	return strings.Compare(a.ID, b.ID) < 0
}

func documentStatusRank(status core.DocumentStatus) int {
	switch status {
	case core.DocumentStatusPublished:
		return 0
	case core.DocumentStatusDraft:
		return 1
	case core.DocumentStatusArchived:
		return 2
	default:
		return 3
	}
}

func documentPage(documents []core.Document, status core.DocumentStatus, seoOnly bool, offset, limit int) ([]core.Document, int) {
	return documentFilterPage(documents, status, seoOnly, offset, limit, nil)
}

func documentSearchPage(documents []core.Document, query string, status core.DocumentStatus, seoOnly bool, offset, limit int) ([]core.Document, int) {
	terms := documentSearchTerms(query)
	if len(terms) == 0 {
		return documentPage(documents, status, seoOnly, offset, limit)
	}
	return documentFilterPage(documents, status, seoOnly, offset, limit, func(document core.Document) bool {
		return documentSearchMatches(document, terms)
	})
}

func documentFilterPage(documents []core.Document, status core.DocumentStatus, seoOnly bool, offset, limit int, match func(core.Document) bool) ([]core.Document, int) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 25
	}
	filtered := make([]core.Document, 0, len(documents))
	for _, document := range documents {
		if !documentPageMatches(document, status, seoOnly) {
			continue
		}
		if match != nil && !match(document) {
			continue
		}
		filtered = append(filtered, cloneDocument(document))
	}
	filtered = sortDocuments(filtered)
	total := len(filtered)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return append([]core.Document(nil), filtered[offset:end]...), total
}

func documentSearchMatches(document core.Document, terms []string) bool {
	if len(terms) == 0 {
		return true
	}
	haystack := documentSearchText(document)
	for _, term := range terms {
		if !strings.Contains(haystack, term) {
			return false
		}
	}
	return true
}

func documentSearchText(document core.Document) string {
	return strings.ToLower(strings.Join([]string{
		document.Title,
		document.Slug,
		document.MetaTitle,
		document.MetaDescription,
		document.Body,
	}, "\n"))
}

func documentSearchTerms(query string) []string {
	return strings.Fields(strings.ToLower(query))
}

func documentSearchLikePattern(term string) string {
	var builder strings.Builder
	builder.Grow(len(term) + 2)
	builder.WriteByte('%')
	for _, r := range term {
		switch r {
		case '\\', '%', '_':
			builder.WriteByte('\\')
		}
		builder.WriteRune(r)
	}
	builder.WriteByte('%')
	return builder.String()
}

func documentPageMatches(document core.Document, status core.DocumentStatus, seoOnly bool) bool {
	if status != "" && document.Status != status {
		return false
	}
	if seoOnly && (document.Status != core.DocumentStatusPublished || document.NoIndex) {
		return false
	}
	return true
}

func documentSearchWhere(status core.DocumentStatus, seoOnly bool, terms []string, noIndexValue any) (string, []any) {
	clauses := make([]string, 0, 2+len(terms))
	args := make([]any, 0, 2+len(terms))
	if seoOnly {
		if status != "" && status != core.DocumentStatusPublished {
			return "WHERE 1 = 0", nil
		}
		status = core.DocumentStatusPublished
	}
	if status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, string(status))
	}
	if seoOnly {
		clauses = append(clauses, "noindex = ?")
		args = append(args, noIndexValue)
	}
	for _, term := range terms {
		clauses = append(clauses, `search_text LIKE ? ESCAPE '\'`)
		args = append(args, documentSearchLikePattern(term))
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}

func documentPageWhere(status core.DocumentStatus, seoOnly bool) (string, []any) {
	clauses := make([]string, 0, 2)
	args := make([]any, 0, 2)
	if seoOnly {
		if status != "" && status != core.DocumentStatusPublished {
			return "WHERE 1 = 0", nil
		}
		status = core.DocumentStatusPublished
	}
	if status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, string(status))
	}
	if seoOnly {
		clauses = append(clauses, "noindex = ?")
		args = append(args, 0)
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}

func siteMessageReadKey(messageID, userID string) string {
	return strings.TrimSpace(messageID) + "\x00" + strings.TrimSpace(userID)
}

func documentSlugKey(slug string) string {
	return strings.ToLower(normalizeDocumentSlugForStorage(slug))
}

func normalizeDocumentSlugForStorage(slug string) string {
	return strings.Trim(strings.TrimSpace(slug), "/")
}

func cloneClient(client core.APIClient) core.APIClient {
	client.AccountGroup = core.NormalizeAccountGroupName(client.AccountGroup)
	client.BillingSource = core.NormalizeClientBillingSource(client.BillingSource)
	copyClient := client
	copyClient.RouteAffinityKey = ""
	copyClient.LastUsedAt = cloneTimePtr(client.LastUsedAt)
	copyClient.RoutePolicy = clonePolicy(client.RoutePolicy)
	return copyClient
}

func cloneAuditEvent(event core.AuditEvent) core.AuditEvent {
	copyEvent := event
	copyEvent.Attempts = append([]core.AttemptRecord(nil), event.Attempts...)
	return copyEvent
}

func cloneBillingReservation(request core.BillingReservation) core.BillingReservation {
	copyRequest := request
	copyRequest.SettledAt = cloneTimePtr(request.SettledAt)
	return copyRequest
}

func clonePaymentOrder(order core.PaymentOrder) core.PaymentOrder {
	copyOrder := order
	copyOrder.PaidAt = cloneTimePtr(order.PaidAt)
	return copyOrder
}

func clonePaymentRefund(refund core.PaymentRefund) core.PaymentRefund {
	copyRefund := refund
	copyRefund.ManualPayoutAt = cloneTimePtr(refund.ManualPayoutAt)
	return copyRefund
}

func refundablePaymentAmountLocked(refunds map[string]core.PaymentRefund, order core.PaymentOrder) int64 {
	refunded := int64(0)
	for _, refund := range refunds {
		if refund.OrderID != order.ID {
			continue
		}
		if refund.Status != core.PaymentRefundPending && refund.Status != core.PaymentRefundDone {
			continue
		}
		next, err := addNanoUSD(refunded, refund.AmountNanoUSD)
		if err != nil {
			return 0
		}
		refunded = next
	}
	remaining := order.AmountNanoUSD - refunded
	if remaining < 0 {
		return 0
	}
	return remaining
}

func pendingPaymentRefundAmountLocked(refunds map[string]core.PaymentRefund, userID string) int64 {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return 0
	}
	pending := int64(0)
	for _, refund := range refunds {
		if refund.UserID != userID || refund.Status != core.PaymentRefundPending {
			continue
		}
		next, err := addNanoUSD(pending, refund.AmountNanoUSD)
		if err != nil {
			return maxInt64
		}
		pending = next
	}
	return pending
}

func auditSummaryEvent(event core.AuditEvent) core.AuditEvent {
	summary := cloneAuditEvent(event)
	summary.RequestBody = ""
	summary.Message = truncateAuditSummaryMessage(summary.Message)
	return summary
}

func auditQueueSummaryEvent(event core.AuditEvent) core.AuditEvent {
	event.RequestBody = ""
	event.Message = truncateAuditSummaryMessage(event.Message)
	return event
}

func truncateAuditSummaryMessage(message string) string {
	if message == "" {
		return ""
	}
	count := 0
	for index := range message {
		if count == maxAuditSummaryMessageRunes {
			return message[:index] + "...[truncated]"
		}
		count++
	}
	return message
}

func clonePolicy(policy core.RoutePolicy) core.RoutePolicy {
	copyPolicy := policy
	copyPolicy.FallbackProviders = append([]core.ProviderKind(nil), policy.FallbackProviders...)
	copyPolicy.Rules = append([]core.RouteRule(nil), policy.Rules...)
	return copyPolicy
}

func cloneMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneTimePtr(in *time.Time) *time.Time {
	if in == nil {
		return nil
	}
	value := in.UTC()
	return &value
}

func cloneIntPtr(in *int) *int {
	if in == nil {
		return nil
	}
	value := *in
	return &value
}

func (r *MemoryRepository) trimAuditLocked() {
	if r.limit > 0 && len(r.audit) > r.limit {
		r.audit = r.audit[:r.limit]
	}
}

func (r *MemoryRepository) trimGatewayAuditLocked(now time.Time) {
	if r.gatewayAuditMaxAge <= 0 {
		return
	}
	cutoff := now.UTC().Add(-r.gatewayAuditMaxAge)
	retained := r.audit[:0]
	for _, event := range r.audit {
		if event.EffectiveKind() == core.AuditKindGateway && gatewayAuditEventIsError(event) && !event.CreatedAt.IsZero() && event.CreatedAt.UTC().Before(cutoff) {
			continue
		}
		retained = append(retained, event)
	}
	r.audit = retained
}

func (r *MemoryRepository) trimMonitorResultsLocked(targetID string) {
	targetID = strings.TrimSpace(targetID)
	if targetID == "" || maxMonitorResultsPerTarget <= 0 {
		return
	}
	targetResults := make([]core.MonitorResult, 0)
	for _, result := range r.monitorResults {
		if result.TargetID == targetID {
			targetResults = append(targetResults, result)
		}
	}
	if len(targetResults) == 0 {
		return
	}
	sortMonitorResults(targetResults)
	cutoff := targetResults[0].CheckedAt.Add(-monitorResultsRetentionWindow)
	retained := make(map[string]struct{}, maxMonitorResultsPerTarget)
	for _, result := range targetResults {
		if len(retained) >= maxMonitorResultsPerTarget {
			break
		}
		if result.CheckedAt.Before(cutoff) {
			continue
		}
		retained[result.ID] = struct{}{}
	}
	write := 0
	for _, result := range r.monitorResults {
		if result.TargetID == targetID {
			if _, ok := retained[result.ID]; !ok {
				continue
			}
		}
		r.monitorResults[write] = result
		write++
	}
	clear(r.monitorResults[write:])
	r.monitorResults = r.monitorResults[:write]
}

func (r *MemoryRepository) trimBillingRequestsLocked(now time.Time) {
	maxAge := r.usageMaxAge
	if maxAge == 0 {
		for key, request := range r.billing {
			if billingRequestRetentionEligible(request) {
				delete(r.billing, key)
				r.deleteBillingFundingAllocationsLocked(request.RequestID, request.ClientID)
			}
		}
		return
	}
	if maxAge < 0 {
		maxAge = defaultUsageLogMaxAge
	}
	cutoff := now.UTC().Add(-maxAge)
	for key, request := range r.billing {
		if !billingRequestRetentionEligible(request) || request.CreatedAt.IsZero() || !request.CreatedAt.Before(cutoff) {
			continue
		}
		delete(r.billing, key)
		r.deleteBillingFundingAllocationsLocked(request.RequestID, request.ClientID)
	}
}

func (r *MemoryRepository) deleteBillingFundingAllocationsLocked(requestID, clientID string) {
	for key, allocation := range r.allocations {
		if allocation.RequestID == requestID && allocation.ClientID == clientID {
			delete(r.allocations, key)
		}
	}
}

func (r *MemoryRepository) trimBillingLedgerLocked(now time.Time) {
	maxAge := r.ledgerMaxAge
	if maxAge <= 0 {
		maxAge = defaultBillingLedgerRetentionAge
	}
	cutoff := now.UTC().Add(-maxAge)
	write := 0
	for _, entry := range r.ledger {
		if !entry.CreatedAt.IsZero() && entry.CreatedAt.Before(cutoff) {
			continue
		}
		r.ledger[write] = entry
		write++
	}
	clear(r.ledger[write:])
	r.ledger = r.ledger[:write]
}

func billingRequestRetentionEligible(request core.BillingReservation) bool {
	return request.Status != core.BillingRequestReserved
}

func normalizeAuditLimit(limit int) int {
	if limit <= 0 {
		return defaultAuditLimit
	}
	return limit
}

func billingRequestKey(requestID, clientID string) string {
	return strings.TrimSpace(requestID) + "\x00" + strings.TrimSpace(clientID)
}

func billingRequestID(requestID, clientID string, ts time.Time) string {
	return fmt.Sprintf("bill_%s_%s_%d", storageIDPart(clientID), storageIDPart(requestID), ts.UnixNano())
}

func billingLedgerID(kind, requestID, clientID string, ts time.Time) string {
	return fmt.Sprintf("ledger_%s_%s_%s_%d", storageIDPart(kind), storageIDPart(clientID), storageIDPart(requestID), ts.UnixNano())
}

func paymentOrderCreditLedgerID(orderOutTradeNo, userID, kind string) string {
	return fmt.Sprintf("ledger_%s_%s_%s", storageIDPart(kind), storageIDPart(userID), storageIDPart(orderOutTradeNo))
}

func normalizePaymentOrderBalanceCredits(orderOutTradeNo string, credits []core.PaymentOrderBalanceCredit) ([]core.PaymentOrderBalanceCredit, error) {
	orderOutTradeNo = strings.TrimSpace(orderOutTradeNo)
	out := make([]core.PaymentOrderBalanceCredit, 0, len(credits))
	seen := make(map[string]struct{}, len(credits))
	for _, credit := range credits {
		credit.UserID = strings.TrimSpace(credit.UserID)
		credit.Kind = strings.TrimSpace(credit.Kind)
		credit.LedgerID = strings.TrimSpace(credit.LedgerID)
		credit.Note = strings.TrimSpace(credit.Note)
		if credit.UserID == "" || credit.AmountNanoUSD <= 0 {
			return nil, fmt.Errorf("payment balance credit is incomplete")
		}
		if credit.Kind == "" {
			credit.Kind = "manual_credit"
		}
		if credit.LedgerID == "" {
			credit.LedgerID = paymentOrderCreditLedgerID(orderOutTradeNo, credit.UserID, credit.Kind)
		}
		if _, exists := seen[credit.LedgerID]; exists {
			return nil, ErrBillingRequestConflict
		}
		seen[credit.LedgerID] = struct{}{}
		out = append(out, credit)
	}
	return out, nil
}

func storageIDPart(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "empty"
	}
	replacer := strings.NewReplacer(" ", "_", "\t", "_", "\r", "_", "\n", "_", "/", "_", "\\", "_", ":", "_", "\x00", "_")
	return replacer.Replace(value)
}

func addNanoUSD(a, b int64) (int64, error) {
	if b > 0 && a > maxInt64-b {
		return 0, ErrAmountOverflow
	}
	if b < 0 && a < minInt64-b {
		return 0, ErrAmountOverflow
	}
	return a + b, nil
}
