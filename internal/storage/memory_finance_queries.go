package storage

import (
	"slices"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func (r *MemoryRepository) FinanceOverviewStats(startOfDay, endOfDay time.Time) FinanceOverviewStats {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var stats FinanceOverviewStats
	stats.TotalUsers = r.financeUserEntityCountLocked()
	stats.TotalClients = r.financeClientEntityCountLocked()
	for _, user := range r.users {
		stats.TotalBalanceNanoUSD = addNanoUSDSaturating(stats.TotalBalanceNanoUSD, user.BalanceNanoUSD)
	}
	for _, client := range r.clients {
		if client.Enabled {
			stats.ActiveClients++
		}
	}
	for _, order := range r.payments {
		if order.Status != core.PaymentOrderPaid {
			continue
		}
		stats.PaidOrderNanoUSD = addNanoUSDSaturating(stats.PaidOrderNanoUSD, order.AmountNanoUSD)
		if memoryWithinWindow(financePaymentOrderTimeForStorage(order), startOfDay, endOfDay) {
			stats.TodayIncomeNanoUSD = addNanoUSDSaturating(stats.TodayIncomeNanoUSD, order.AmountNanoUSD)
		}
	}
	for _, refund := range r.refunds {
		if refund.Status != core.PaymentRefundDone {
			continue
		}
		if memoryWithinWindow(financePaymentRefundTimeForStorage(refund), startOfDay, endOfDay) {
			stats.TodayIncomeNanoUSD = addNanoUSDSaturating(stats.TodayIncomeNanoUSD, -refund.AmountNanoUSD)
		}
	}
	cutoff := time.Now().UTC().Add(-time.Hour)
	for _, user := range r.users {
		if user.BalanceNanoUSD < 0 {
			stats.ReconcileIssueCount++
		}
	}
	for _, spend := range r.clientSpend {
		if spend.SpendLimitNanoUSD > 0 && spend.SpendUsedNanoUSD > spend.SpendLimitNanoUSD {
			stats.ReconcileIssueCount++
		}
	}
	for _, request := range r.billing {
		switch request.Status {
		case core.BillingRequestUsageMissing:
			stats.UsageMissingCount++
		case core.BillingRequestReserved:
			stats.UnsettledRequestCount++
			if request.CreatedAt.Before(cutoff) {
				stats.ReconcileIssueCount++
			}
		}
	}
	requests := make(map[string]struct{}, len(r.billing))
	for _, request := range r.billing {
		requests[billingRequestKey(request.RequestID, request.ClientID)] = struct{}{}
		if memoryWithinWindow(request.CreatedAt, startOfDay, endOfDay) {
			_, _, _, _, _, total := billingRequestTokenUsageAmount(request.Status, request.PromptTokens, request.CachedPromptTokens, request.CacheCreationTokens, request.CompletionTokens, request.ImageOutputTokens, request.TotalTokens)
			stats.TodayTotalTokens = addTokenCountSaturating(stats.TodayTotalTokens, total)
			spend := billingRequestUsageSpendAmount(request.Status, request.ReservedNanoUSD, request.ActualNanoUSD)
			stats.TodaySpendNanoUSD = addNanoUSDSaturating(stats.TodaySpendNanoUSD, spend)
			if core.NormalizeClientBillingSource(request.BillingSource) == core.ClientBillingSourcePlan {
				stats.TodayPlanSpendNanoUSD = addNanoUSDSaturating(stats.TodayPlanSpendNanoUSD, spend)
			} else {
				stats.TodayBalanceSpendNanoUSD = addNanoUSDSaturating(stats.TodayBalanceSpendNanoUSD, spend)
			}
		}
	}
	for _, entry := range r.ledger {
		if entry.RequestID == "" || entry.ClientID == "" {
			continue
		}
		if _, ok := requests[billingRequestKey(entry.RequestID, entry.ClientID)]; ok {
			continue
		}
		if memoryWithinWindow(entry.CreatedAt, startOfDay, endOfDay) {
			spend := billingLedgerUsageSpendDelta(entry.Kind, entry.AmountNanoUSD)
			stats.TodaySpendNanoUSD = addNanoUSDSaturating(stats.TodaySpendNanoUSD, spend)
			stats.TodayBalanceSpendNanoUSD = addNanoUSDSaturating(stats.TodayBalanceSpendNanoUSD, spend)
		}
	}
	for _, entry := range r.planLedger {
		if entry.RequestID == "" || entry.ClientID == "" {
			continue
		}
		if _, ok := requests[billingRequestKey(entry.RequestID, entry.ClientID)]; ok {
			continue
		}
		if memoryWithinWindow(entry.CreatedAt, startOfDay, endOfDay) {
			spend := planQuotaLedgerUsageSpendDelta(entry.Kind, entry.AmountNanoUSD)
			stats.TodaySpendNanoUSD = addNanoUSDSaturating(stats.TodaySpendNanoUSD, spend)
			stats.TodayPlanSpendNanoUSD = addNanoUSDSaturating(stats.TodayPlanSpendNanoUSD, spend)
		}
	}
	return stats
}

func (r *MemoryRepository) FinanceEntityCounts() FinanceEntityCounts {
	r.mu.RLock()
	defer r.mu.RUnlock()

	counts := FinanceEntityCounts{
		TotalUsers:   r.financeUserEntityCountLocked(),
		TotalClients: r.financeClientEntityCountLocked(),
	}
	for _, client := range r.clients {
		if client.Enabled {
			counts.ActiveClients++
		}
	}
	return counts
}

func (r *MemoryRepository) FinanceTotalSpendNanoUSD() int64 {
	total := int64(0)
	for _, spend := range memoryUserActualSpendTotals(r) {
		total = addNanoUSDSaturating(total, spend)
	}
	return total
}

func (r *MemoryRepository) ListTokenUsageDailySummaries(startedAt time.Time, days int, limit int) []TokenUsageDailySummary {
	if days <= 0 || startedAt.IsZero() {
		return nil
	}
	if limit <= 0 || limit > days {
		limit = days
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	rows := memoryTokenUsageDailyRowsLocked(r, startedAt, days, false)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}

func (r *MemoryRepository) ListTokenUsageDailyUserSummaries(startedAt time.Time, days int, limit int) []TokenUsageDailySummary {
	if days <= 0 || startedAt.IsZero() {
		return nil
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	rows := memoryTokenUsageDailyRowsLocked(r, startedAt, days, true)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}

func memoryTokenUsageDailyRowsLocked(r *MemoryRepository, startedAt time.Time, days int, includeUsers bool) []TokenUsageDailySummary {
	endedAt := startedAt.AddDate(0, 0, days)
	byKey := make(map[string]*TokenUsageDailySummary)
	for _, request := range r.billing {
		if !memoryWithinWindow(request.CreatedAt, startedAt, endedAt) {
			continue
		}
		prompt, cached, cacheCreation, completion, imageOutput, total := billingRequestTokenUsageAmount(request.Status, request.PromptTokens, request.CachedPromptTokens, request.CacheCreationTokens, request.CompletionTokens, request.ImageOutputTokens, request.TotalTokens)
		if prompt == 0 && cached == 0 && cacheCreation == 0 && completion == 0 && imageOutput == 0 && total == 0 {
			continue
		}
		date := billingDayKey(request.CreatedAt)
		if date == "" {
			continue
		}
		userID := ""
		if includeUsers {
			userID = strings.TrimSpace(request.UserID)
			if userID == "" {
				continue
			}
		}
		key := date + "\x00" + userID
		summary := byKey[key]
		if summary == nil {
			username := userID
			if user, ok := r.users[userID]; ok && strings.TrimSpace(user.Username) != "" {
				username = strings.TrimSpace(user.Username)
			}
			summary = &TokenUsageDailySummary{Date: date, UserID: userID, Username: username}
			byKey[key] = summary
		}
		summary.RequestCount++
		summary.PromptTokens = addNanoUSDSaturating(summary.PromptTokens, prompt)
		summary.CachedTokens = addNanoUSDSaturating(summary.CachedTokens, cached)
		summary.CacheCreationTokens = addNanoUSDSaturating(summary.CacheCreationTokens, cacheCreation)
		summary.CompletionTokens = addNanoUSDSaturating(summary.CompletionTokens, completion)
		summary.ImageOutputTokens = addNanoUSDSaturating(summary.ImageOutputTokens, imageOutput)
		summary.TotalTokens = addNanoUSDSaturating(summary.TotalTokens, total)
	}
	out := make([]TokenUsageDailySummary, 0, len(byKey))
	for _, summary := range byKey {
		out = append(out, *summary)
	}
	slices.SortFunc(out, func(a, b TokenUsageDailySummary) int {
		if cmp := strings.Compare(b.Date, a.Date); cmp != 0 {
			return cmp
		}
		if b.TotalTokens != a.TotalTokens {
			if b.TotalTokens > a.TotalTokens {
				return 1
			}
			return -1
		}
		if cmp := strings.Compare(a.Username, b.Username); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.UserID, b.UserID)
	})
	return out
}

func (r *MemoryRepository) ListFinanceReconcileIssues(now time.Time) []FinanceReconcileIssueSummary {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	issues := make([]FinanceReconcileIssueSummary, 0)
	users := make([]core.User, 0, len(r.users))
	for _, user := range r.users {
		if user.BalanceNanoUSD < 0 {
			users = append(users, user)
		}
	}
	slices.SortFunc(users, func(a, b core.User) int {
		if a.BalanceNanoUSD != b.BalanceNanoUSD {
			if a.BalanceNanoUSD < b.BalanceNanoUSD {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	for _, user := range users {
		username := strings.TrimSpace(user.Username)
		if username == "" {
			username = user.ID
		}
		issues = append(issues, FinanceReconcileIssueSummary{
			Kind:         "negative_balance",
			Severity:     "error",
			ResourceID:   user.ID,
			Message:      username,
			DeltaNanoUSD: user.BalanceNanoUSD,
		})
	}

	clientSpends := make([]core.ClientSpend, 0, len(r.clientSpend))
	for _, spend := range r.clientSpend {
		if spend.SpendLimitNanoUSD > 0 && spend.SpendUsedNanoUSD > spend.SpendLimitNanoUSD {
			clientSpends = append(clientSpends, spend)
		}
	}
	slices.SortFunc(clientSpends, func(a, b core.ClientSpend) int {
		aOverage := a.SpendUsedNanoUSD - a.SpendLimitNanoUSD
		bOverage := b.SpendUsedNanoUSD - b.SpendLimitNanoUSD
		if aOverage != bOverage {
			if aOverage > bOverage {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ClientID, b.ClientID)
	})
	for _, spend := range clientSpends {
		clientName := spend.ClientID
		if client, ok := r.clients[spend.ClientID]; ok && strings.TrimSpace(client.Name) != "" {
			clientName = strings.TrimSpace(client.Name)
		}
		issues = append(issues, FinanceReconcileIssueSummary{
			Kind:         "client_spend_over_limit",
			Severity:     "warning",
			ResourceID:   spend.ClientID,
			Message:      clientName,
			DeltaNanoUSD: spend.SpendUsedNanoUSD - spend.SpendLimitNanoUSD,
		})
	}

	cutoff := now.Add(-time.Hour)
	for _, request := range r.billing {
		if request.Status == core.BillingRequestReserved && request.CreatedAt.Before(cutoff) {
			issues = append(issues, FinanceReconcileIssueSummary{
				Kind:       "stale_reserved_request",
				Severity:   "warning",
				ResourceID: request.RequestID,
				Message:    request.ClientID,
			})
		}
	}
	return issues
}

func (r *MemoryRepository) financeUserEntityCountLocked() int {
	userIDs := make(map[string]struct{}, len(r.users))
	add := func(userID string) {
		userID = strings.TrimSpace(userID)
		if userID != "" {
			userIDs[userID] = struct{}{}
		}
	}
	for userID := range r.users {
		add(userID)
	}
	for _, order := range r.payments {
		if order.Status == core.PaymentOrderPaid {
			add(order.UserID)
		}
	}
	for _, refund := range r.refunds {
		if refund.Status == core.PaymentRefundDone {
			add(refund.UserID)
		}
	}
	for _, entry := range r.ledger {
		add(entry.UserID)
	}
	for _, request := range r.billing {
		add(request.UserID)
	}
	return len(userIDs)
}

func (r *MemoryRepository) financeClientEntityCountLocked() int {
	clientIDs := make(map[string]struct{}, len(r.clients))
	add := func(clientID string) {
		clientID = strings.TrimSpace(clientID)
		if clientID != "" {
			clientIDs[clientID] = struct{}{}
		}
	}
	for clientID := range r.clients {
		add(clientID)
	}
	for _, entry := range r.ledger {
		add(entry.ClientID)
	}
	for _, request := range r.billing {
		add(request.ClientID)
	}
	return len(clientIDs)
}

func (r *MemoryRepository) ListPaymentIncomeByDay(startedAt time.Time, days int) []PaymentIncomeDaySummary {
	if days <= 0 || startedAt.IsZero() {
		return nil
	}
	endedAt := startedAt.AddDate(0, 0, days)
	r.mu.RLock()
	defer r.mu.RUnlock()

	byDate := make(map[string]*PaymentIncomeDaySummary, days)
	summaryFor := func(value time.Time) *PaymentIncomeDaySummary {
		date := billingDayKey(value)
		if date == "" {
			return nil
		}
		summary := byDate[date]
		if summary == nil {
			summary = &PaymentIncomeDaySummary{Date: date}
			byDate[date] = summary
		}
		return summary
	}
	for _, order := range r.payments {
		if order.Status != core.PaymentOrderPaid {
			continue
		}
		when := financePaymentOrderTimeForStorage(order)
		if !memoryWithinWindow(when, startedAt, endedAt) {
			continue
		}
		if summary := summaryFor(when); summary != nil {
			summary.IncomeNanoUSD = addNanoUSDSaturating(summary.IncomeNanoUSD, order.AmountNanoUSD)
			summary.OrderCount++
		}
	}
	for _, refund := range r.refunds {
		if refund.Status != core.PaymentRefundDone {
			continue
		}
		when := financePaymentRefundTimeForStorage(refund)
		if !memoryWithinWindow(when, startedAt, endedAt) {
			continue
		}
		if summary := summaryFor(when); summary != nil {
			summary.IncomeNanoUSD = addNanoUSDSaturating(summary.IncomeNanoUSD, -refund.AmountNanoUSD)
		}
	}
	out := make([]PaymentIncomeDaySummary, 0, len(byDate))
	for _, summary := range byDate {
		out = append(out, *summary)
	}
	slices.SortFunc(out, func(a, b PaymentIncomeDaySummary) int {
		return strings.Compare(a.Date, b.Date)
	})
	return out
}

func (r *MemoryRepository) ListFinanceTopUsersBySpend(limit int) []FinanceUserSummary {
	items, _ := r.ListFinanceUserSummariesPage(0, limit)
	return items
}

func (r *MemoryRepository) ListFinanceUserSummariesPage(offset, limit int) ([]FinanceUserSummary, int) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 25
	}
	if limit > 100 {
		limit = 100
	}

	users := r.ListUsers()
	clients := r.ListClients()
	usage := r.ListBillingUsageSpendByClient()
	requests := r.ListBillingRequests(BillingRequestQuery{})
	actualSpends := r.ListClientActualSpends()
	orders := r.ListPaymentOrders(PaymentOrderQuery{Status: core.PaymentOrderPaid})
	refunds := r.ListPaymentRefunds("")
	ledgerUsers := r.ListBillingLedgerUserSummaries()
	cashByUser, cashByClient := memoryBillingRequestCashSpendTotals(r)

	byUser := make(map[string]*FinanceUserSummary)
	summaryFor := func(userID string) *FinanceUserSummary {
		userID = strings.TrimSpace(userID)
		if userID == "" {
			return nil
		}
		summary := byUser[userID]
		if summary == nil {
			summary = &FinanceUserSummary{UserID: userID, Username: userID}
			byUser[userID] = summary
		}
		return summary
	}
	for _, user := range users {
		summary := summaryFor(user.ID)
		if summary == nil {
			continue
		}
		summary.Username = strings.TrimSpace(user.Username)
		if summary.Username == "" {
			summary.Username = strings.TrimSpace(user.ID)
		}
		summary.BalanceNanoUSD = user.BalanceNanoUSD
	}
	usageByClient := make(map[string]int64)
	for _, item := range usage {
		if summary := summaryFor(item.UserID); summary != nil {
			summary.UsageSpendNanoUSD = addNanoUSDSaturating(summary.UsageSpendNanoUSD, item.SpendNanoUSD)
		}
		usageByClient[strings.TrimSpace(item.ClientID)] = addNanoUSDSaturating(usageByClient[strings.TrimSpace(item.ClientID)], item.SpendNanoUSD)
	}
	planByUser := make(map[string]int64)
	planByClient := make(map[string]int64)
	requestKeys := make(map[string]struct{}, len(requests))
	for _, request := range requests {
		requestKeys[billingRequestKey(request.RequestID, request.ClientID)] = struct{}{}
		if core.NormalizeClientBillingSource(request.BillingSource) != core.ClientBillingSourcePlan {
			continue
		}
		spend := billingRequestHistoricalSpendAmount(request.Status, request.ActualNanoUSD)
		if spend <= 0 {
			continue
		}
		userID := strings.TrimSpace(request.UserID)
		clientID := strings.TrimSpace(request.ClientID)
		planByUser[userID] = addNanoUSDSaturating(planByUser[userID], spend)
		planByClient[clientID] = addNanoUSDSaturating(planByClient[clientID], spend)
	}
	for _, entry := range r.planLedger {
		if entry.RequestID == "" || entry.ClientID == "" {
			continue
		}
		if _, ok := requestKeys[billingRequestKey(entry.RequestID, entry.ClientID)]; ok {
			continue
		}
		spend := planQuotaLedgerUsageSpendDelta(entry.Kind, entry.AmountNanoUSD)
		if spend <= 0 {
			continue
		}
		userID := strings.TrimSpace(entry.UserID)
		clientID := strings.TrimSpace(entry.ClientID)
		planByUser[userID] = addNanoUSDSaturating(planByUser[userID], spend)
		planByClient[clientID] = addNanoUSDSaturating(planByClient[clientID], spend)
	}
	for userID, spend := range planByUser {
		if summary := summaryFor(userID); summary != nil {
			summary.PlanSpendNanoUSD = addNanoUSDSaturating(summary.PlanSpendNanoUSD, spend)
		}
	}
	actualByClient := make(map[string]int64)
	for _, spend := range actualSpends {
		actualByClient[strings.TrimSpace(spend.ClientID)] = spend.SpendUsedNanoUSD
	}
	for _, client := range clients {
		summary := summaryFor(client.OwnerUserID)
		if summary == nil {
			continue
		}
		clientID := strings.TrimSpace(client.ID)
		summary.ClientCount++
		if spend, ok := usageByClient[clientID]; ok {
			summary.ClientUsageNanoUSD = addNanoUSDSaturating(summary.ClientUsageNanoUSD, spend)
		} else {
			summary.ClientUsageNanoUSD = addNanoUSDSaturating(summary.ClientUsageNanoUSD, actualByClient[clientID])
		}
		if spend := planByClient[clientID]; spend > 0 {
			summary.ClientPlanNanoUSD = addNanoUSDSaturating(summary.ClientPlanNanoUSD, spend)
		}
		if spend := cashByClient[clientID]; spend > 0 {
			summary.ClientSpendNanoUSD = addNanoUSDSaturating(summary.ClientSpendNanoUSD, spend)
		}
	}
	for _, order := range orders {
		summary := summaryFor(order.UserID)
		if summary == nil {
			continue
		}
		summary.RechargeNanoUSD = addNanoUSDSaturating(summary.RechargeNanoUSD, order.AmountNanoUSD)
		summary.LastPaymentAt = billingLatestTimePtr(summary.LastPaymentAt, financePaymentOrderTimeForStorage(order))
	}
	for _, refund := range refunds {
		if refund.Status != core.PaymentRefundDone {
			continue
		}
		if summary := summaryFor(refund.UserID); summary != nil {
			summary.RefundNanoUSD = addNanoUSDSaturating(summary.RefundNanoUSD, refund.AmountNanoUSD)
		}
	}
	for _, ledger := range ledgerUsers {
		summary := summaryFor(ledger.UserID)
		if summary == nil {
			continue
		}
		summary.RewardNanoUSD = addNanoUSDSaturating(summary.RewardNanoUSD, ledger.RewardNanoUSD)
		summary.SpendNanoUSD = addNanoUSDSaturating(summary.SpendNanoUSD, ledger.SpendNanoUSD)
		summary.RefundNanoUSD = addNanoUSDSaturating(summary.RefundNanoUSD, ledger.RefundNanoUSD)
		if ledger.LastSpendAt != nil {
			summary.LastSpendAt = billingLatestTimePtr(summary.LastSpendAt, *ledger.LastSpendAt)
		}
	}
	for userID, spend := range cashByUser {
		if summary := summaryFor(userID); summary != nil {
			summary.SpendNanoUSD = addNanoUSDSaturating(summary.SpendNanoUSD, spend)
		}
	}

	out := make([]FinanceUserSummary, 0, len(byUser))
	for _, summary := range byUser {
		out = append(out, *summary)
	}
	slices.SortFunc(out, func(a, b FinanceUserSummary) int {
		if a.SpendNanoUSD != b.SpendNanoUSD {
			if a.SpendNanoUSD > b.SpendNanoUSD {
				return -1
			}
			return 1
		}
		if a.Username != b.Username {
			return strings.Compare(a.Username, b.Username)
		}
		return strings.Compare(a.UserID, b.UserID)
	})
	total := len(out)
	if offset >= total {
		return nil, total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return append([]FinanceUserSummary(nil), out[offset:end]...), total
}

func (r *MemoryRepository) ListFinanceTopClientsBySpend(limit int) []FinanceClientSpendSummary {
	if limit <= 0 {
		limit = 5
	}
	if limit > 100 {
		limit = 100
	}
	clients := r.ListClients()
	usage := r.ListBillingUsageSpendByClient()
	actualSpends := r.ListClientActualSpends()
	requests := r.ListBillingRequests(BillingRequestQuery{})
	_, cashByClient := memoryBillingRequestCashSpendTotals(r)

	byClient := make(map[string]*FinanceClientSpendSummary)
	clientNameByID := make(map[string]string)
	requestUserByClient := make(map[string]string)
	for _, request := range requests {
		clientID := strings.TrimSpace(request.ClientID)
		if clientID == "" {
			continue
		}
		if name := strings.TrimSpace(request.ClientName); name != "" && strings.TrimSpace(clientNameByID[clientID]) == "" {
			clientNameByID[clientID] = name
		}
		if userID := strings.TrimSpace(request.UserID); userID != "" && strings.TrimSpace(requestUserByClient[clientID]) == "" {
			requestUserByClient[clientID] = userID
		}
	}
	for _, client := range clients {
		clientID := strings.TrimSpace(client.ID)
		if clientID == "" {
			continue
		}
		name := strings.TrimSpace(client.Name)
		if name == "" {
			name = clientID
		}
		byClient[clientID] = &FinanceClientSpendSummary{
			ClientID:          clientID,
			ClientName:        name,
			OwnerUserID:       strings.TrimSpace(client.OwnerUserID),
			SpendLimitNanoUSD: client.SpendLimitNanoUSD,
		}
	}
	for _, spend := range actualSpends {
		clientID := strings.TrimSpace(spend.ClientID)
		if summary := byClient[clientID]; summary != nil {
			summary.SpendUsedNanoUSD = spend.SpendUsedNanoUSD
		}
	}
	usageByClient := make(map[string]int64)
	planByClient := make(map[string]int64)
	usageUserByClient := make(map[string]string)
	for _, item := range usage {
		clientID := strings.TrimSpace(item.ClientID)
		if clientID == "" || item.SpendNanoUSD <= 0 {
			continue
		}
		usageByClient[clientID] = addNanoUSDSaturating(usageByClient[clientID], item.SpendNanoUSD)
		if strings.TrimSpace(usageUserByClient[clientID]) == "" {
			usageUserByClient[clientID] = strings.TrimSpace(item.UserID)
		}
	}
	requestKeys := make(map[string]struct{}, len(requests))
	for _, request := range requests {
		requestKeys[billingRequestKey(request.RequestID, request.ClientID)] = struct{}{}
		if core.NormalizeClientBillingSource(request.BillingSource) != core.ClientBillingSourcePlan {
			continue
		}
		spend := billingRequestHistoricalSpendAmount(request.Status, request.ActualNanoUSD)
		if spend <= 0 {
			continue
		}
		planByClient[strings.TrimSpace(request.ClientID)] = addNanoUSDSaturating(planByClient[strings.TrimSpace(request.ClientID)], spend)
	}
	for _, entry := range r.planLedger {
		if entry.RequestID == "" || entry.ClientID == "" {
			continue
		}
		if _, ok := requestKeys[billingRequestKey(entry.RequestID, entry.ClientID)]; ok {
			continue
		}
		spend := planQuotaLedgerUsageSpendDelta(entry.Kind, entry.AmountNanoUSD)
		if spend <= 0 {
			continue
		}
		planByClient[strings.TrimSpace(entry.ClientID)] = addNanoUSDSaturating(planByClient[strings.TrimSpace(entry.ClientID)], spend)
	}
	for clientID, spend := range usageByClient {
		summary := byClient[clientID]
		if summary == nil {
			name := strings.TrimSpace(clientNameByID[clientID])
			if name == "" {
				name = clientID
			}
			ownerID := strings.TrimSpace(usageUserByClient[clientID])
			if ownerID == "" {
				ownerID = strings.TrimSpace(requestUserByClient[clientID])
			}
			summary = &FinanceClientSpendSummary{ClientID: clientID, ClientName: name, OwnerUserID: ownerID}
			byClient[clientID] = summary
		}
		summary.UsageNanoUSD = spend
		summary.PlanNanoUSD = planByClient[clientID]
		if cashSpend := cashByClient[clientID]; cashSpend > 0 {
			summary.SpendUsedNanoUSD = cashSpend
		}
	}
	for clientID, cashSpend := range cashByClient {
		if cashSpend <= 0 {
			continue
		}
		summary := byClient[clientID]
		if summary == nil {
			name := strings.TrimSpace(clientNameByID[clientID])
			if name == "" {
				name = clientID
			}
			ownerID := strings.TrimSpace(requestUserByClient[clientID])
			summary = &FinanceClientSpendSummary{ClientID: clientID, ClientName: name, OwnerUserID: ownerID}
			byClient[clientID] = summary
		}
		summary.SpendUsedNanoUSD = cashSpend
	}
	out := make([]FinanceClientSpendSummary, 0, len(byClient))
	for _, summary := range byClient {
		out = append(out, *summary)
	}
	slices.SortFunc(out, func(a, b FinanceClientSpendSummary) int {
		if a.SpendUsedNanoUSD != b.SpendUsedNanoUSD {
			if a.SpendUsedNanoUSD > b.SpendUsedNanoUSD {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ClientID, b.ClientID)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func financePaymentOrderTimeForStorage(order core.PaymentOrder) time.Time {
	if order.PaidAt != nil && !order.PaidAt.IsZero() {
		return *order.PaidAt
	}
	if !order.UpdatedAt.IsZero() {
		return order.UpdatedAt
	}
	return order.CreatedAt
}

func financePaymentRefundTimeForStorage(refund core.PaymentRefund) time.Time {
	if !refund.UpdatedAt.IsZero() {
		return refund.UpdatedAt
	}
	return refund.CreatedAt
}

func memoryWithinWindow(value, start, end time.Time) bool {
	if value.IsZero() {
		return false
	}
	local := value.Local()
	return !local.Before(start) && local.Before(end)
}
