package storage

import (
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

const defaultUsageLogMaxAge = time.Duration(core.DefaultUsageLogMaxAgeDays) * 24 * time.Hour
const defaultBillingLedgerRetentionAge = time.Duration(core.DefaultBillingLedgerRetentionDays) * 24 * time.Hour

func normalizeUsageLogMaxAge(days int) time.Duration {
	if days < 0 {
		days = core.DefaultUsageLogMaxAgeDays
	}
	if days > core.MaximumUsageLogMaxAgeDays {
		days = core.MaximumUsageLogMaxAgeDays
	}
	return time.Duration(days) * 24 * time.Hour
}

func normalizeBillingLedgerRetentionAge(days int) time.Duration {
	if days <= 0 {
		days = core.DefaultBillingLedgerRetentionDays
	}
	if days < core.MinimumBillingLedgerRetentionDays {
		days = core.MinimumBillingLedgerRetentionDays
	}
	if days > core.MaximumBillingLedgerRetentionDays {
		days = core.MaximumBillingLedgerRetentionDays
	}
	return time.Duration(days) * 24 * time.Hour
}

type BillingRequestQuery struct {
	UserID    string
	ClientID  string
	Model     string
	Status    core.BillingRequestStatus
	StartedAt time.Time
	EndedAt   time.Time
	Offset    int
	Limit     int
}

type BillingUsageSpendSummary struct {
	UserID       string
	ClientID     string
	SpendNanoUSD int64
}

type BillingUsageSpendDaySummary struct {
	Date         string
	SpendNanoUSD int64
}

type BillingUsageSpendHourSummary struct {
	Hour         int
	SpendNanoUSD int64
}

type BillingRequestDayCountSummary struct {
	Date  string
	Count int
}

type PaymentIncomeDaySummary struct {
	Date          string
	IncomeNanoUSD int64
	OrderCount    int
}

type BillingModelUsageSummary struct {
	Model            string
	RequestCount     int
	PromptTokens     int64
	CompletionTokens int64
	SpendNanoUSD     int64
}

type TokenUsageDailySummary struct {
	Date                string
	UserID              string
	Username            string
	RequestCount        int
	PromptTokens        int64
	CachedTokens        int64
	CacheCreationTokens int64
	CompletionTokens    int64
	ImageOutputTokens   int64
	TotalTokens         int64
}

type BillingLedgerUserSummary struct {
	UserID          string
	RechargeNanoUSD int64
	RewardNanoUSD   int64
	SpendNanoUSD    int64
	RefundNanoUSD   int64
	LastPaymentAt   *time.Time
	LastSpendAt     *time.Time
}

type FinanceOverviewStats struct {
	TotalBalanceNanoUSD      int64
	TodayIncomeNanoUSD       int64
	TodaySpendNanoUSD        int64
	TodayBalanceSpendNanoUSD int64
	TodayPlanSpendNanoUSD    int64
	PaidOrderNanoUSD         int64
	UsageMissingCount        int
	UnsettledRequestCount    int
	ReconcileIssueCount      int
	TotalUsers               int
	TotalClients             int
	ActiveClients            int
}

type FinanceEntityCounts struct {
	TotalUsers    int
	TotalClients  int
	ActiveClients int
}

type FinanceReconcileIssueSummary struct {
	Kind         string
	Severity     string
	ResourceID   string
	Message      string
	DeltaNanoUSD int64
}

type FinanceUserSummary struct {
	UserID             string
	Username           string
	BalanceNanoUSD     int64
	RechargeNanoUSD    int64
	RewardNanoUSD      int64
	SpendNanoUSD       int64
	UsageSpendNanoUSD  int64
	PlanSpendNanoUSD   int64
	RefundNanoUSD      int64
	LastPaymentAt      *time.Time
	LastSpendAt        *time.Time
	ClientCount        int
	ClientSpendNanoUSD int64
	ClientUsageNanoUSD int64
	ClientPlanNanoUSD  int64
}

type FinanceClientSpendSummary struct {
	ClientID          string
	ClientName        string
	OwnerUserID       string
	SpendLimitNanoUSD int64
	SpendUsedNanoUSD  int64
	UsageNanoUSD      int64
	PlanNanoUSD       int64
}

type PlanSubscriptionQuery struct {
	UserID string
	PlanID string
	Status core.UserPlanEntitlementStatus
	Offset int
	Limit  int
}

type PlanSubscriptionSummary struct {
	Entitlement        core.UserPlanEntitlement
	Username           string
	PlanGroup          string
	UserBalanceNanoUSD int64
}

type PlanSubscriptionStats struct {
	TotalCount             int
	ActiveCount            int
	ExpiredCount           int
	CancelledCount         int
	RevenueNanoUSD         int64
	ActiveRemainingNanoUSD int64
	ActiveUsedNanoUSD      int64
}

type PlanSubscriptionPlanSummary struct {
	PlanID                 string
	PlanName               string
	PlanGroup              string
	PurchaseCount          int
	ActiveCount            int
	RevenueNanoUSD         int64
	ActiveRemainingNanoUSD int64
}

type PlanQuotaUsageQuery struct {
	UserID        string
	PlanID        string
	EntitlementID string
	StartedAt     time.Time
	EndedAt       time.Time
	Offset        int
	Limit         int
}

type PlanQuotaUsageDaySummary struct {
	Date                string
	UserID              string
	Username            string
	PlanID              string
	PlanName            string
	EntitlementID       string
	GrantedNanoUSD      int64
	UsedNanoUSD         int64
	ReturnedNanoUSD     int64
	ExpiredNanoUSD      int64
	NetNanoUSD          int64
	LastLedgerCreatedAt time.Time
}

type PlanQuotaUsageEvent struct {
	UserID          string
	Username        string
	PlanID          string
	PlanName        string
	EntitlementID   string
	Kind            string
	GrantedNanoUSD  int64
	UsedNanoUSD     int64
	ReturnedNanoUSD int64
	ExpiredNanoUSD  int64
	NetNanoUSD      int64
	CreatedAt       time.Time
}

type PlanQuotaUsageStats struct {
	GrantedNanoUSD  int64
	UsedNanoUSD     int64
	ReturnedNanoUSD int64
	ExpiredNanoUSD  int64
	NetNanoUSD      int64
}

type UserListQuery struct {
	Query     string
	Role      core.UserRole
	Status    string
	Inviter   string
	Sort      string
	Direction string
	Offset    int
	Limit     int
}

type UserListItem struct {
	User         core.User
	InviteCount  int64
	SpendNanoUSD int64
}

func normalizeBillingRequestQuery(query BillingRequestQuery) BillingRequestQuery {
	query.UserID = strings.TrimSpace(query.UserID)
	query.ClientID = strings.TrimSpace(query.ClientID)
	query.Model = strings.TrimSpace(query.Model)
	query.Status = core.BillingRequestStatus(strings.TrimSpace(string(query.Status)))
	if query.Offset < 0 {
		query.Offset = 0
	}
	if query.Limit < 0 {
		query.Limit = 0
	}
	if query.Limit > 100 {
		query.Limit = 100
	}
	return query
}

func normalizePlanSubscriptionQuery(query PlanSubscriptionQuery) PlanSubscriptionQuery {
	query.UserID = strings.TrimSpace(query.UserID)
	query.PlanID = strings.TrimSpace(query.PlanID)
	query.Status = core.UserPlanEntitlementStatus(strings.TrimSpace(string(query.Status)))
	switch query.Status {
	case "", core.UserPlanEntitlementActive, core.UserPlanEntitlementExpired, core.UserPlanEntitlementCancelled:
	default:
		query.Status = ""
	}
	if query.Offset < 0 {
		query.Offset = 0
	}
	if query.Limit < 0 {
		query.Limit = 0
	}
	if query.Limit > 100 {
		query.Limit = 100
	}
	return query
}

func normalizePlanQuotaUsageQuery(query PlanQuotaUsageQuery) PlanQuotaUsageQuery {
	query.UserID = strings.TrimSpace(query.UserID)
	query.PlanID = strings.TrimSpace(query.PlanID)
	query.EntitlementID = strings.TrimSpace(query.EntitlementID)
	if !query.StartedAt.IsZero() && !query.EndedAt.IsZero() && !query.StartedAt.Before(query.EndedAt) {
		query.EndedAt = query.StartedAt.AddDate(0, 0, 1)
	}
	if query.Offset < 0 {
		query.Offset = 0
	}
	if query.Limit < 0 {
		query.Limit = 0
	}
	if query.Limit > 100 {
		query.Limit = 100
	}
	return query
}

func billingRequestMatchesQuery(request core.BillingReservation, query BillingRequestQuery) bool {
	if query.UserID != "" && request.UserID != query.UserID {
		return false
	}
	if query.ClientID != "" && !billingRequestDisplayTextEquals(request.ClientName, request.ClientID, query.ClientID) {
		return false
	}
	if query.Model != "" && !billingRequestTextContains(request.Model, query.Model) {
		return false
	}
	if query.Status != "" && request.Status != query.Status {
		return false
	}
	if !query.StartedAt.IsZero() && request.CreatedAt.Before(query.StartedAt) {
		return false
	}
	if !query.EndedAt.IsZero() && request.CreatedAt.After(query.EndedAt) {
		return false
	}
	return true
}

func billingRequestDisplayTextEquals(display, fallback, needle string) bool {
	display = strings.TrimSpace(display)
	fallback = strings.TrimSpace(fallback)
	needle = strings.TrimSpace(needle)
	return strings.EqualFold(display, needle) || strings.EqualFold(fallback, needle)
}

func billingRequestTextContains(value, needle string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	needle = strings.ToLower(strings.TrimSpace(needle))
	return needle == "" || strings.Contains(value, needle)
}

func billingSQLLikePattern(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	b.Grow(len(value) + 2)
	b.WriteByte('%')
	for _, r := range value {
		switch r {
		case '\\', '%', '_':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('%')
	return b.String()
}

func billingLedgerUsageSpendDelta(kind string, amountNanoUSD int64) int64 {
	switch strings.TrimSpace(kind) {
	case "settle":
		return -amountNanoUSD
	default:
		return 0
	}
}

func billingRequestUsageSpendAmount(status core.BillingRequestStatus, reservedNanoUSD, actualNanoUSD int64) int64 {
	switch status {
	case core.BillingRequestReserved, core.BillingRequestReleased, core.BillingRequestUsageMissing:
		return 0
	case core.BillingRequestSettled:
		if actualNanoUSD > 0 {
			return actualNanoUSD
		}
		return 0
	default:
		if actualNanoUSD > 0 {
			return actualNanoUSD
		}
		return 0
	}
}

func planQuotaLedgerUsageSpendDelta(kind string, amountNanoUSD int64) int64 {
	switch strings.TrimSpace(kind) {
	case "settle":
		return -amountNanoUSD
	default:
		return 0
	}
}

func planQuotaUsageRequestLedgerKind(kind string) bool {
	switch strings.TrimSpace(kind) {
	case "settle":
		return true
	default:
		return false
	}
}

func addPlanQuotaUsageUsedAmount(summary *PlanQuotaUsageDaySummary, amountNanoUSD int64) {
	if summary == nil || amountNanoUSD <= 0 {
		return
	}
	summary.UsedNanoUSD = addNanoUSDSaturating(summary.UsedNanoUSD, amountNanoUSD)
	summary.NetNanoUSD = addNanoUSDSaturating(summary.NetNanoUSD, -amountNanoUSD)
}

func addPlanQuotaUsageLedgerAmount(summary *PlanQuotaUsageDaySummary, kind string, amountNanoUSD int64) {
	if summary == nil || amountNanoUSD == 0 {
		return
	}
	switch strings.TrimSpace(kind) {
	case "purchase", "merge_purchase", "grant", "reset":
		if amountNanoUSD > 0 {
			summary.GrantedNanoUSD = addNanoUSDSaturating(summary.GrantedNanoUSD, amountNanoUSD)
		}
	case "settle":
		usageNet := addNanoUSDSaturating(summary.ReturnedNanoUSD-summary.UsedNanoUSD, amountNanoUSD)
		if usageNet < 0 {
			summary.UsedNanoUSD = absBillingNanoUSD(usageNet)
			summary.ReturnedNanoUSD = 0
		} else {
			summary.UsedNanoUSD = 0
			summary.ReturnedNanoUSD = usageNet
		}
	case "expire", "cancel":
		if amountNanoUSD < 0 {
			summary.ExpiredNanoUSD = addNanoUSDSaturating(summary.ExpiredNanoUSD, absBillingNanoUSD(amountNanoUSD))
		}
	}
	summary.NetNanoUSD = addNanoUSDSaturating(summary.NetNanoUSD, amountNanoUSD)
}

func addPlanQuotaUsageEventLedgerAmount(event *PlanQuotaUsageEvent, kind string, amountNanoUSD int64) {
	if event == nil || amountNanoUSD == 0 {
		return
	}
	switch strings.TrimSpace(kind) {
	case "purchase", "merge_purchase", "grant", "reset":
		if amountNanoUSD > 0 {
			event.GrantedNanoUSD = addNanoUSDSaturating(event.GrantedNanoUSD, amountNanoUSD)
		}
	case "settle":
		if amountNanoUSD < 0 {
			event.UsedNanoUSD = addNanoUSDSaturating(event.UsedNanoUSD, absBillingNanoUSD(amountNanoUSD))
		} else {
			event.ReturnedNanoUSD = addNanoUSDSaturating(event.ReturnedNanoUSD, amountNanoUSD)
		}
	case "expire", "cancel":
		if amountNanoUSD < 0 {
			event.ExpiredNanoUSD = addNanoUSDSaturating(event.ExpiredNanoUSD, absBillingNanoUSD(amountNanoUSD))
		}
	}
	event.NetNanoUSD = addNanoUSDSaturating(event.NetNanoUSD, amountNanoUSD)
}

func addPlanQuotaUsageEventToDaySummary(summary *PlanQuotaUsageDaySummary, event PlanQuotaUsageEvent) {
	if summary == nil {
		return
	}
	summary.GrantedNanoUSD = addNanoUSDSaturating(summary.GrantedNanoUSD, event.GrantedNanoUSD)
	usageDelta := addNanoUSDSaturating(event.ReturnedNanoUSD, -event.UsedNanoUSD)
	usageNet := addNanoUSDSaturating(summary.ReturnedNanoUSD-summary.UsedNanoUSD, usageDelta)
	if usageNet < 0 {
		summary.UsedNanoUSD = absBillingNanoUSD(usageNet)
		summary.ReturnedNanoUSD = 0
	} else {
		summary.UsedNanoUSD = 0
		summary.ReturnedNanoUSD = usageNet
	}
	summary.ExpiredNanoUSD = addNanoUSDSaturating(summary.ExpiredNanoUSD, event.ExpiredNanoUSD)
	summary.NetNanoUSD = addNanoUSDSaturating(summary.NetNanoUSD, event.NetNanoUSD)
}

func planQuotaUsageRowsFromEvents(events []PlanQuotaUsageEvent, query PlanQuotaUsageQuery) []PlanQuotaUsageDaySummary {
	query = normalizePlanQuotaUsageQuery(query)
	byKey := make(map[string]*PlanQuotaUsageDaySummary)
	for _, event := range events {
		date := billingDayKey(event.CreatedAt)
		userID := strings.TrimSpace(event.UserID)
		if date == "" || userID == "" {
			continue
		}
		username := strings.TrimSpace(event.Username)
		if username == "" {
			username = userID
		}
		planID := strings.TrimSpace(event.PlanID)
		planName := strings.TrimSpace(event.PlanName)
		if planName == "" {
			planName = planID
		}
		entitlementID := strings.TrimSpace(event.EntitlementID)
		keyEntitlementID := ""
		if query.EntitlementID != "" {
			keyEntitlementID = entitlementID
		}
		key := date + "\x00" + userID + "\x00" + planID + "\x00" + keyEntitlementID
		summary := byKey[key]
		if summary == nil {
			summary = &PlanQuotaUsageDaySummary{
				Date:          date,
				UserID:        userID,
				Username:      username,
				PlanID:        planID,
				PlanName:      planName,
				EntitlementID: keyEntitlementID,
			}
			byKey[key] = summary
		}
		if event.CreatedAt.After(summary.LastLedgerCreatedAt) {
			summary.LastLedgerCreatedAt = event.CreatedAt
		}
		addPlanQuotaUsageEventToDaySummary(summary, event)
	}
	out := make([]PlanQuotaUsageDaySummary, 0, len(byKey))
	for _, summary := range byKey {
		out = append(out, *summary)
	}
	return sortPlanQuotaUsageDaySummaries(out)
}

func planQuotaUsageEventHasAmount(event PlanQuotaUsageEvent) bool {
	return event.GrantedNanoUSD != 0 ||
		event.UsedNanoUSD != 0 ||
		event.ReturnedNanoUSD != 0 ||
		event.ExpiredNanoUSD != 0 ||
		event.NetNanoUSD != 0
}

func planQuotaUsageStatsFromRows(rows []PlanQuotaUsageDaySummary) PlanQuotaUsageStats {
	var stats PlanQuotaUsageStats
	for _, row := range rows {
		stats.GrantedNanoUSD = addNanoUSDSaturating(stats.GrantedNanoUSD, row.GrantedNanoUSD)
		stats.UsedNanoUSD = addNanoUSDSaturating(stats.UsedNanoUSD, row.UsedNanoUSD)
		stats.ReturnedNanoUSD = addNanoUSDSaturating(stats.ReturnedNanoUSD, row.ReturnedNanoUSD)
		stats.ExpiredNanoUSD = addNanoUSDSaturating(stats.ExpiredNanoUSD, row.ExpiredNanoUSD)
		stats.NetNanoUSD = addNanoUSDSaturating(stats.NetNanoUSD, row.NetNanoUSD)
	}
	return stats
}

func billingRequestHistoricalSpendAmount(status core.BillingRequestStatus, actualNanoUSD int64) int64 {
	switch status {
	case core.BillingRequestSettled:
		if actualNanoUSD > 0 {
			return actualNanoUSD
		}
	case core.BillingRequestReserved, core.BillingRequestReleased, core.BillingRequestUsageMissing:
		return 0
	default:
		if actualNanoUSD > 0 {
			return actualNanoUSD
		}
	}
	return 0
}

func billingModelUsageSpendAmount(status core.BillingRequestStatus, reservedNanoUSD, actualNanoUSD int64) int64 {
	switch status {
	case core.BillingRequestReserved, core.BillingRequestReleased, core.BillingRequestUsageMissing:
		return 0
	case core.BillingRequestSettled:
		if actualNanoUSD > 0 {
			return actualNanoUSD
		}
		return 0
	default:
		if actualNanoUSD > 0 {
			return actualNanoUSD
		}
		return 0
	}
}

func billingRequestTokenUsageAmount(status core.BillingRequestStatus, promptTokens, cachedTokens, cacheCreationTokens, completionTokens, imageOutputTokens, totalTokens int) (int64, int64, int64, int64, int64, int64) {
	switch status {
	case core.BillingRequestSettled:
	default:
		return 0, 0, 0, 0, 0, 0
	}
	prompt := int64(promptTokens)
	cached := int64(cachedTokens)
	cacheCreation := int64(cacheCreationTokens)
	completion := int64(completionTokens)
	imageOutput := int64(imageOutputTokens)
	total := int64(totalTokens)
	if total <= 0 {
		total = addNanoUSDSaturating(prompt, completion)
		total = addNanoUSDSaturating(total, cacheCreation)
	}
	return prompt, cached, cacheCreation, completion, imageOutput, total
}

func billingDayKey(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Local().Format("2006-01-02")
}

func billingLatestTimePtr(current *time.Time, candidate time.Time) *time.Time {
	if candidate.IsZero() {
		return current
	}
	if current == nil || candidate.After(*current) {
		value := candidate
		return &value
	}
	return current
}

func absBillingNanoUSD(value int64) int64 {
	if value < 0 {
		if value == -1<<63 {
			return 1<<63 - 1
		}
		return -value
	}
	return value
}

func addNanoUSDSaturating(a, b int64) int64 {
	if b > 0 && a > (1<<63-1)-b {
		return 1<<63 - 1
	}
	if b < 0 && a < (-1<<63)-b {
		return -1 << 63
	}
	return a + b
}
