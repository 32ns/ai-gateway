package postgresrepo

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

const defaultAuditLimit = 512
const maxAuditSummaryMessageRunes = 4096
const maxInt64 = int64(^uint64(0) >> 1)
const minInt64 = -maxInt64 - 1

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

func clonePaymentOrder(order core.PaymentOrder) core.PaymentOrder {
	copyOrder := order
	copyOrder.PaidAt = cloneTimePtr(order.PaidAt)
	return copyOrder
}

func auditSummaryEvent(event core.AuditEvent) core.AuditEvent {
	summary := cloneAuditEvent(event)
	summary.RequestBody = ""
	summary.Message = truncateAuditSummaryMessage(summary.Message)
	return summary
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

func normalizeAuditLimit(limit int) int {
	if limit <= 0 {
		return defaultAuditLimit
	}
	return limit
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

func ledgerIDPartWithAttempt(value string, attempt int) string {
	value = strings.TrimSpace(value)
	if attempt == 0 {
		return value
	}
	return fmt.Sprintf("%s_%d", value, attempt)
}

func storageIDPart(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "empty"
	}
	replacer := strings.NewReplacer(" ", "_", "\t", "_", "\r", "_", "\n", "_", "/", "_", "\\", "_", ":", "_", "\x00", "_")
	return replacer.Replace(value)
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

func addNanoUSD(a, b int64) (int64, error) {
	if b > 0 && a > maxInt64-b {
		return 0, ErrAmountOverflow
	}
	if b < 0 && a < minInt64-b {
		return 0, ErrAmountOverflow
	}
	return a + b, nil
}
