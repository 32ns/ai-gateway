package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/failover"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

type billingHold struct {
	mu                   sync.Mutex
	service              *Service
	requestID            string
	clientID             string
	userID               string
	model                core.ModelConfig
	modality             string
	unit                 string
	fixedNanoUSD         int64
	settlementModelID    string
	reservePricing       billingPricing
	accountGroupSnapshot map[string]core.AccountGroup
	billingSource        string
	fastBps              int64
	fastMode             bool
	fastModelID          string
	firstTokenMS         int64
	active               bool
	accountID            string
	accountLabel         string
	failedAccountLabels  []string
}

type billingPricing struct {
	inputNanoUSDPer1M        int64
	cachedInputNanoUSDPer1M  int64
	cacheWriteNanoUSDPer1M   int64
	cacheWrite5mNanoUSDPer1M int64
	cacheWrite1hNanoUSDPer1M int64
	outputNanoUSDPer1M       int64
	imageOutputNanoUSDPer1M  int64
}

func (s *Service) reserveGatewayBilling(requestID string, client *core.APIClient, model core.ModelConfig, routedModel string, req *core.GatewayRequest) (*billingHold, error) {
	if req == nil {
		return nil, nil
	}
	serviceTier := gatewayRequestServiceTier(req)
	fastMode := gatewayServiceTierIsFast(serviceTier)
	fastModelID := fastBillingModelID(model, routedModel)
	fastBps := fastBillingMultiplierBps(serviceTier, fastModelID)
	promptTokens := estimateGatewayPromptTokens(req)
	completionTokens := estimateGatewayCompletionTokens(req)
	fingerprint := fmt.Sprintf("chat:%s:%d:%d", strings.TrimSpace(model.ID), promptTokens, completionTokens)
	cacheDiagnostics := billingCacheDiagnosticsForGatewayRequest(req)
	if normalizeModelBillingMode(model.BillingMode) == core.ModelBillingModeRequest {
		return s.reserveRequestBilling(requestID, client, model, core.BillingModalityText, fingerprint, fastBps, fastMode, fastModelID, "", cacheDiagnostics)
	}
	return s.reserveBilling(requestID, client, model, promptTokens, completionTokens, fingerprint, fastBps, fastMode, fastModelID, cacheDiagnostics)
}

func (s *Service) reserveEmbeddingBilling(requestID string, client *core.APIClient, model core.ModelConfig, req *core.EmbeddingRequest) (*billingHold, error) {
	if req == nil {
		return nil, nil
	}
	promptTokens := estimateTextTokens(strings.Join(req.Input, "\n"))
	fingerprint := fmt.Sprintf("embedding:%s:%d:%d", strings.TrimSpace(model.ID), promptTokens, len(req.Input))
	if normalizeModelBillingMode(model.BillingMode) == core.ModelBillingModeRequest {
		return s.reserveRequestBilling(requestID, client, model, core.BillingModalityText, fingerprint, 10000, false, "")
	}
	return s.reserveBilling(requestID, client, model, promptTokens, 0, fingerprint, 10000, false, "")
}

func (s *Service) reserveRequestBilling(requestID string, client *core.APIClient, model core.ModelConfig, modality, fingerprint string, fastBps int64, fastMode bool, fastModelID string, args ...any) (*billingHold, error) {
	if client == nil || strings.TrimSpace(client.ID) == "" || strings.TrimSpace(client.OwnerUserID) == "" {
		return nil, nil
	}
	fastBps = normalizeBillingMultiplierBps(fastBps)
	billingSource := clientBillingSource(client)
	groupSnapshot := s.billingAccountGroupSnapshot(model, client)
	if err := s.ensurePlanBillingAllowed(client, billingSource); err != nil {
		return nil, err
	}
	reserveAccountGroup, reserveAccountGroupMultiplierBps := s.reservationBillingAccountGroup(client, billingSource)
	reserved := s.maxFixedReservePrice(model, client)
	if billingSource == core.ClientBillingSourcePlan {
		reserved = s.maxPlanFixedReservePrice(model, client)
	} else {
		reserved = applyBillingMultiplierBps(reserved, fastBps)
	}
	if reserved <= 0 {
		return nil, nil
	}
	settlementModel := ""
	cacheDiagnostics := core.BillingCacheDiagnostics{}
	if len(args) > 0 {
		if value, ok := args[0].(string); ok {
			settlementModel = strings.TrimSpace(value)
		}
	}
	if len(args) > 1 {
		if value, ok := args[1].(core.BillingCacheDiagnostics); ok {
			cacheDiagnostics = value
		}
	}
	request, err := s.repo.ReserveBilling(core.BillingReservationInput{
		RequestID:                 strings.TrimSpace(requestID),
		ClientID:                  strings.TrimSpace(client.ID),
		ClientName:                strings.TrimSpace(client.Name),
		UserID:                    strings.TrimSpace(client.OwnerUserID),
		AccountGroup:              reserveAccountGroup,
		AccountGroupMultiplierBps: reserveAccountGroupMultiplierBps,
		BillingSource:             billingSource,
		Provider:                  model.Provider,
		Model:                     strings.TrimSpace(model.ID),
		FastMode:                  fastMode,
		ReservedNanoUSD:           0,
		Fingerprint:               strings.TrimSpace(fingerprint),
		CacheDiagnostics:          cacheDiagnostics,
	})
	if err != nil {
		return nil, s.billingError(err)
	}
	return &billingHold{
		service:              s,
		requestID:            request.RequestID,
		clientID:             request.ClientID,
		userID:               request.UserID,
		model:                model,
		modality:             modality,
		unit:                 core.BillingUnitRequest,
		fixedNanoUSD:         reserved,
		settlementModelID:    settlementModel,
		accountGroupSnapshot: groupSnapshot,
		billingSource:        billingSource,
		fastBps:              fastBps,
		fastMode:             fastMode,
		fastModelID:          strings.TrimSpace(fastModelID),
		active:               true,
	}, nil
}

func (s *Service) reserveBilling(requestID string, client *core.APIClient, model core.ModelConfig, promptTokens, completionTokens int, fingerprint string, fastBps int64, fastMode bool, fastModelID string, cacheDiagnostics ...core.BillingCacheDiagnostics) (*billingHold, error) {
	if client == nil || strings.TrimSpace(client.ID) == "" || strings.TrimSpace(client.OwnerUserID) == "" {
		return nil, nil
	}
	fastBps = normalizeBillingMultiplierBps(fastBps)
	billingSource := clientBillingSource(client)
	groupSnapshot := s.billingAccountGroupSnapshot(model, client)
	if err := s.ensurePlanBillingAllowed(client, billingSource); err != nil {
		return nil, err
	}
	reserveAccountGroup, reserveAccountGroupMultiplierBps := s.reservationBillingAccountGroup(client, billingSource)
	baseReservePricing := s.maxReservePricing(model, client, promptTokens)
	if billingSource == core.ClientBillingSourcePlan {
		baseReservePricing = s.maxPlanReservePricing(model, client, promptTokens)
	}
	baseReservePricing = billingPricingForProvider(model.Provider, baseReservePricing)
	reservePricing := baseReservePricing
	if billingSource != core.ClientBillingSourcePlan {
		reservePricing = applyFastBillingPricing(baseReservePricing, fastBps)
	}
	if !billingPricingHasPrice(reservePricing) {
		return nil, nil
	}
	diagnostics := core.BillingCacheDiagnostics{}
	if len(cacheDiagnostics) > 0 {
		diagnostics = cacheDiagnostics[0]
	}
	request, err := s.repo.ReserveBilling(core.BillingReservationInput{
		RequestID:                     strings.TrimSpace(requestID),
		ClientID:                      strings.TrimSpace(client.ID),
		ClientName:                    strings.TrimSpace(client.Name),
		UserID:                        strings.TrimSpace(client.OwnerUserID),
		AccountGroup:                  reserveAccountGroup,
		AccountGroupMultiplierBps:     reserveAccountGroupMultiplierBps,
		BillingSource:                 billingSource,
		Provider:                      model.Provider,
		Model:                         strings.TrimSpace(model.ID),
		FastMode:                      fastMode,
		EstimatedPromptTokens:         promptTokens,
		EstimatedCompletionTokens:     completionTokens,
		InputPriceNanoUSDPer1M:        reservePricing.inputNanoUSDPer1M,
		CachedInputPriceNanoUSDPer1M:  reservePricing.cachedInputNanoUSDPer1M,
		CacheWritePriceNanoUSDPer1M:   reservePricing.cacheWriteNanoUSDPer1M,
		CacheWrite5mPriceNanoUSDPer1M: reservePricing.cacheWrite5mNanoUSDPer1M,
		CacheWrite1hPriceNanoUSDPer1M: reservePricing.cacheWrite1hNanoUSDPer1M,
		OutputPriceNanoUSDPer1M:       reservePricing.outputNanoUSDPer1M,
		ImageOutputPriceNanoUSDPer1M:  reservePricing.imageOutputNanoUSDPer1M,
		ReservedNanoUSD:               0,
		Fingerprint:                   strings.TrimSpace(fingerprint),
		CacheDiagnostics:              diagnostics,
	})
	if err != nil {
		return nil, s.billingError(err)
	}
	return &billingHold{
		service:              s,
		requestID:            request.RequestID,
		clientID:             request.ClientID,
		userID:               request.UserID,
		model:                model,
		reservePricing:       baseReservePricing,
		accountGroupSnapshot: groupSnapshot,
		billingSource:        billingSource,
		fastBps:              fastBps,
		fastMode:             fastMode,
		fastModelID:          strings.TrimSpace(fastModelID),
		active:               true,
	}, nil
}

func (s *Service) settleGatewayBilling(hold *billingHold, resp *core.GatewayResponse) error {
	if resp == nil {
		return nil
	}
	defer s.refreshAccountQuotaAfterUse(resp.AccountID)
	if hold == nil || !hold.active {
		return nil
	}
	if hold.fixedNanoUSD > 0 {
		hold.firstTokenMS = normalizeFirstTokenMS(resp.FirstTokenMS)
		return s.settleFixedBilling(hold, resp.AccountID, resp.Provider, resp.Model, resp.ServiceTier)
	}
	provider := resp.Provider
	if provider == "" {
		provider = hold.model.Provider
	}
	usage := billingUsageForProvider(provider, resp.Usage)
	missingUsage := usageIsEmpty(usage)
	fastMode := hold.fastMode || gatewayServiceTierIsFast(resp.ServiceTier)
	fastBps := hold.fastBps
	if fastBps == 10000 && gatewayServiceTierIsFast(resp.ServiceTier) {
		fastBps = fastBillingMultiplierBps(resp.ServiceTier, hold.fastModelID)
	}
	actual := int64(0)
	var pricing billingPricing
	if !missingUsage {
		pricing = hold.billingPricingForAccount(resp.AccountID, billingInputTokens(usage))
		if !hold.usesPlanBilling() {
			pricing = applyFastBillingPricing(pricing, fastBps)
		}
		pricing = billingPricingForProvider(provider, pricing)
		actual = billingCostNano(usage, pricing)
	}
	accountGroup, accountGroupMultiplierBps := hold.accountGroupSnapshotForAccount(resp.AccountID)
	settlement, err := s.repo.SettleBilling(core.BillingSettlementInput{
		RequestID:                     hold.requestID,
		ClientID:                      hold.clientID,
		AccountID:                     resp.AccountID,
		AccountLabel:                  hold.settlementAccountLabel(resp.AccountID),
		AccountGroup:                  accountGroup,
		AccountGroupMultiplierBps:     accountGroupMultiplierBps,
		BillingSource:                 hold.billingSource,
		Provider:                      resp.Provider,
		Model:                         resp.Model,
		FastMode:                      fastMode,
		Usage:                         usage,
		InputPriceNanoUSDPer1M:        pricing.inputNanoUSDPer1M,
		CachedInputPriceNanoUSDPer1M:  pricing.cachedInputNanoUSDPer1M,
		CacheWritePriceNanoUSDPer1M:   pricing.cacheWriteNanoUSDPer1M,
		CacheWrite5mPriceNanoUSDPer1M: pricing.cacheWrite5mNanoUSDPer1M,
		CacheWrite1hPriceNanoUSDPer1M: pricing.cacheWrite1hNanoUSDPer1M,
		OutputPriceNanoUSDPer1M:       pricing.outputNanoUSDPer1M,
		ImageOutputPriceNanoUSDPer1M:  pricing.imageOutputNanoUSDPer1M,
		ActualNanoUSD:                 actual,
		FirstTokenMS:                  normalizeFirstTokenMS(resp.FirstTokenMS),
		MissingUsage:                  missingUsage,
	})
	if err != nil {
		if settlementShouldReleaseReservation(err) {
			s.releaseBilling(hold, err.Error())
		}
		return s.billingError(err)
	}
	hold.active = false
	s.notifyBillingEvent("usage_settled", settlement.Request)
	return nil
}

func (s *Service) settleEmbeddingBilling(hold *billingHold, resp *core.EmbeddingResponse) error {
	if resp == nil {
		return nil
	}
	defer s.refreshAccountQuotaAfterUse(resp.AccountID)
	if hold == nil || !hold.active {
		return nil
	}
	if hold.fixedNanoUSD > 0 {
		return s.settleFixedBilling(hold, resp.AccountID, resp.Provider, resp.Model)
	}
	missingUsage := usageIsEmpty(resp.Usage)
	actual := int64(0)
	var pricing billingPricing
	if !missingUsage {
		pricing = hold.billingPricingForAccount(resp.AccountID, billingInputTokens(resp.Usage))
		actual = billingCostNano(resp.Usage, pricing)
	}
	accountGroup, accountGroupMultiplierBps := hold.accountGroupSnapshotForAccount(resp.AccountID)
	settlement, err := s.repo.SettleBilling(core.BillingSettlementInput{
		RequestID:                     hold.requestID,
		ClientID:                      hold.clientID,
		AccountID:                     resp.AccountID,
		AccountLabel:                  hold.settlementAccountLabel(resp.AccountID),
		AccountGroup:                  accountGroup,
		AccountGroupMultiplierBps:     accountGroupMultiplierBps,
		BillingSource:                 hold.billingSource,
		Provider:                      resp.Provider,
		Model:                         resp.Model,
		Usage:                         resp.Usage,
		InputPriceNanoUSDPer1M:        pricing.inputNanoUSDPer1M,
		CachedInputPriceNanoUSDPer1M:  pricing.cachedInputNanoUSDPer1M,
		CacheWritePriceNanoUSDPer1M:   pricing.cacheWriteNanoUSDPer1M,
		CacheWrite5mPriceNanoUSDPer1M: pricing.cacheWrite5mNanoUSDPer1M,
		CacheWrite1hPriceNanoUSDPer1M: pricing.cacheWrite1hNanoUSDPer1M,
		OutputPriceNanoUSDPer1M:       pricing.outputNanoUSDPer1M,
		ImageOutputPriceNanoUSDPer1M:  pricing.imageOutputNanoUSDPer1M,
		ActualNanoUSD:                 actual,
		MissingUsage:                  missingUsage,
	})
	if err != nil {
		if settlementShouldReleaseReservation(err) {
			s.releaseBilling(hold, err.Error())
		}
		return s.billingError(err)
	}
	hold.active = false
	s.notifyBillingEvent("usage_settled", settlement.Request)
	return nil
}

func (s *Service) settleFixedBilling(hold *billingHold, accountID string, provider core.ProviderKind, modelID string, serviceTier ...string) error {
	defer s.refreshAccountQuotaAfterUse(accountID)
	if hold == nil || !hold.active {
		return nil
	}
	if hold.fixedNanoUSD <= 0 {
		return nil
	}
	if hold.settlementModelID != "" {
		modelID = hold.settlementModelID
	}
	actualServiceTier := ""
	if len(serviceTier) > 0 {
		actualServiceTier = serviceTier[0]
	}
	fastMode := hold.fastMode || gatewayServiceTierIsFast(actualServiceTier)
	fastBps := hold.fastBps
	if fastBps == 10000 && gatewayServiceTierIsFast(actualServiceTier) {
		fastBps = fastBillingMultiplierBps(actualServiceTier, hold.fastModelID)
	}
	actual := hold.fixedBillingPriceForAccount(accountID)
	if hold.usesPlanBilling() {
		if actual <= 0 {
			actual = hold.fixedNanoUSD
		}
	} else {
		if actual <= 0 {
			actual = hold.fixedNanoUSD
			if !hold.fastMode && gatewayServiceTierIsFast(actualServiceTier) {
				actual = applyBillingMultiplierBps(actual, fastBps)
			}
		} else {
			actual = applyBillingMultiplierBps(actual, fastBps)
		}
	}
	accountGroup, accountGroupMultiplierBps := hold.accountGroupSnapshotForAccount(accountID)
	settlement, err := s.repo.SettleBilling(core.BillingSettlementInput{
		RequestID:                 hold.requestID,
		ClientID:                  hold.clientID,
		AccountID:                 accountID,
		AccountLabel:              hold.settlementAccountLabel(accountID),
		AccountGroup:              accountGroup,
		AccountGroupMultiplierBps: accountGroupMultiplierBps,
		BillingSource:             hold.billingSource,
		Provider:                  provider,
		Model:                     modelID,
		FastMode:                  fastMode,
		ActualNanoUSD:             actual,
		FirstTokenMS:              normalizeFirstTokenMS(hold.firstTokenMS),
	})
	if err != nil {
		if settlementShouldReleaseReservation(err) {
			s.releaseBilling(hold, err.Error())
		}
		return s.billingError(err)
	}
	hold.active = false
	s.notifyBillingEvent("usage_settled", settlement.Request)
	return nil
}

func (h *billingHold) billingPricingForAccount(accountID string, inputTokens int) billingPricing {
	if h == nil {
		return billingPricing{}
	}
	if group, ok := h.accountGroupSnapshot[strings.TrimSpace(accountID)]; ok {
		if h.usesPlanBilling() {
			return groupPlanBillingPricing(h.model, group, inputTokens)
		}
		return groupBillingPricing(h.model, group, inputTokens)
	}
	if billingPricingHasPrice(h.reservePricing) {
		return h.reservePricing
	}
	return modelBillingPricing(h.model, inputTokens)
}

func (h *billingHold) fixedBillingPriceForAccount(accountID string) int64 {
	if h == nil {
		return 0
	}
	if group, ok := h.accountGroupSnapshot[strings.TrimSpace(accountID)]; ok {
		if h.usesPlanBilling() {
			return fixedPlanBillingPrice(h.model, group)
		}
		return fixedBillingPrice(h.model, group)
	}
	return 0
}

func (h *billingHold) accountGroupSnapshotForAccount(accountID string) (string, int64) {
	if h == nil {
		return "", 0
	}
	group, ok := h.accountGroupSnapshot[strings.TrimSpace(accountID)]
	if !ok {
		return "", 0
	}
	if h.usesPlanBilling() {
		return strings.TrimSpace(group.Name), normalizeBillingMultiplierBps(core.EffectiveAccountGroupPlanMultiplier(group, time.Now()))
	}
	return strings.TrimSpace(group.Name), normalizeBillingMultiplierBps(group.BillingMultiplierBps)
}

func (h *billingHold) usesPlanBilling() bool {
	return h != nil && core.NormalizeClientBillingSource(h.billingSource) == core.ClientBillingSourcePlan
}

func (h *billingHold) settlementAccountLabel(accountID string) string {
	if h == nil {
		return ""
	}
	accountID = strings.TrimSpace(accountID)
	h.mu.Lock()
	defer h.mu.Unlock()
	if accountID != "" && accountID == h.accountID {
		return h.accountLabel
	}
	if h.accountLabel != "" {
		return h.accountLabel
	}
	return accountID
}

func (h *billingHold) AttemptStarted(attempt core.AttemptRecord) {
	if h == nil || strings.TrimSpace(attempt.AccountID) == "" {
		return
	}
	h.mu.Lock()
	h.accountID = strings.TrimSpace(attempt.AccountID)
	h.accountLabel = strings.TrimSpace(attempt.AccountLabel)
	if h.accountLabel == "" {
		h.accountLabel = h.accountID
	}
	accountID := h.accountID
	accountLabel := h.accountLabel
	failed := append([]string(nil), h.failedAccountLabels...)
	h.mu.Unlock()
	h.updateBillingAccount(accountID, accountLabel, failed)
}

func (h *billingHold) AttemptFinished(attempt core.AttemptRecord) {
	if h == nil || strings.TrimSpace(attempt.AccountID) == "" {
		return
	}
	h.mu.Lock()
	if attempt.Status != "ok" && attempt.Status != "running" {
		failedAccount := failedAttemptAccountText(attempt)
		h.failedAccountLabels = compactAttemptAccountLabels(append(h.failedAccountLabels, failedAccount))
		if h.accountID == "" && h.accountLabel == "" {
			label := strings.TrimSpace(attempt.AccountLabel)
			if label == "" {
				label = strings.TrimSpace(attempt.AccountID)
			}
			h.accountLabel = label
		}
	}
	accountID := h.accountID
	accountLabel := h.accountLabel
	failed := append([]string(nil), h.failedAccountLabels...)
	h.mu.Unlock()
	h.updateBillingAccount(accountID, accountLabel, failed)
}

func (h *billingHold) updateBillingAccount(accountID, accountLabel string, failedAccountLabels []string) {
	if h == nil || h.service == nil {
		return
	}
	s := h.service
	if err := s.repo.UpdateBillingAccount(core.BillingAccountUpdateInput{
		RequestID:           h.requestID,
		ClientID:            h.clientID,
		AccountID:           accountID,
		AccountLabel:        accountLabel,
		FailedAccountLabels: failedAccountLabels,
	}); err == nil {
		s.notifyBillingEvent("usage_account_updated", core.BillingReservation{
			RequestID: h.requestID,
			ClientID:  h.clientID,
			UserID:    h.userID,
		})
	}
}

func failedAttemptAccountText(attempt core.AttemptRecord) string {
	label := strings.TrimSpace(attempt.AccountLabel)
	if label == "" {
		label = strings.TrimSpace(attempt.AccountID)
	}
	reason := strings.TrimSpace(attempt.ErrorMessage)
	if reason == "" {
		reason = strings.TrimSpace(attempt.ErrorCode)
	}
	if reason == "" {
		reason = strings.TrimSpace(attempt.Status)
	}
	reason = strings.Join(strings.Fields(reason), " ")
	if label == "" {
		return reason
	}
	if reason == "" {
		return label
	}
	return label + " " + reason
}

func compactAttemptAccountLabels(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func billingAttemptContext(ctx context.Context, hold *billingHold) context.Context {
	if hold == nil {
		return ctx
	}
	return failover.WithAttemptObserver(ctx, hold)
}

func (s *Service) refreshAccountQuotaAfterUse(accountID string) {
	accountID = strings.TrimSpace(accountID)
	if s == nil || s.repo == nil || s.quotaRegistry == nil || accountID == "" {
		return
	}
	account, err := s.repo.GetAccount(accountID)
	if err != nil {
		return
	}
	if providers.IsOpenAIChatGPTFreeAccount(account) {
		return
	}
	now := time.Now()
	s.quotaMu.Lock()
	if s.quotaRefresh == nil {
		s.quotaRefresh = make(map[string]time.Time)
	}
	if last := s.quotaRefresh[accountID]; !last.IsZero() && now.Sub(last) < accountQuotaRefreshMinInterval {
		s.quotaMu.Unlock()
		return
	}
	s.quotaRefresh[accountID] = now
	s.quotaMu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := s.refreshAccountQuota(ctx, accountID); err != nil {
			log.Printf("auto quota refresh for account %s failed: %v", accountID, err)
		}
	}()
}

func normalizeFirstTokenMS(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func elapsedFirstTokenMS(started time.Time) int64 {
	if started.IsZero() {
		return 0
	}
	elapsed := time.Since(started).Milliseconds()
	if elapsed <= 0 {
		return 1
	}
	return elapsed
}

func (s *Service) refreshAccountQuota(ctx context.Context, accountID string) error {
	account, err := s.repo.GetAccount(accountID)
	if err != nil {
		return err
	}
	account = core.NormalizeAccountRuntimeState(account, time.Now().UTC())
	account.EffectiveProxyURL = s.effectiveProxyURLForAccount(account)
	adapter, ok := s.quotaRegistry.Get(account.Provider)
	if !ok {
		return fmt.Errorf("provider %q is not registered", account.Provider)
	}
	fetcher, ok := adapter.(providers.QuotaFetchingAdapter)
	if !ok || !accountSupportsQuotaRefresh(account) {
		return nil
	}
	if refresher, ok := adapter.(providers.RefreshingAdapter); ok && providers.CredentialNeedsRefresh(account) {
		credential, err := refresher.Refresh(ctx, account)
		if err != nil {
			return s.persistQuotaCredentialRefreshError(account, err)
		}
		account.Credential = credential
		if current, err := s.repo.GetAccount(account.ID); err == nil {
			account = core.PreserveAccountControlState(account, current)
		}
		if err := s.repo.UpsertAccount(account); err != nil {
			return err
		}
	}
	snapshot, err := fetcher.FetchQuota(ctx, account)
	if err != nil {
		return s.persistQuotaFetchError(account, err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	metadata := cloneStringMap(account.Credential.Metadata)
	metadata[core.AccountQuotaMetadataKey] = string(encoded)
	delete(metadata, core.AccountQuotaErrorMetadataKey)
	delete(metadata, core.AccountQuotaErrorAtMetadataKey)
	delete(metadata, core.AccountQuotaErrorCodeMetadataKey)
	delete(metadata, core.AccountQuotaErrorStatusMetadataKey)
	account.Credential.Metadata = metadata
	if account.Status == core.AccountStatusExpired || account.Status == core.AccountStatusProviderBanned {
		account.Status = core.AccountStatusActive
		account.CooldownUntil = nil
		account.ConsecutiveFails = 0
	}
	return s.upsertAccountQuotaRuntimeState(account)
}

func (s *Service) persistQuotaFetchError(account core.Account, err error) error {
	account = preserveQuotaFetchErrorRuntimeState(account, s.repo)
	account.Credential.Metadata = quotaRefreshErrorMetadata(account, err)
	return s.upsertAccountQuotaRuntimeState(account)
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
	return s.upsertAccountQuotaRuntimeState(account)
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

func (s *Service) upsertAccountQuotaRuntimeState(account core.Account) error {
	if current, err := s.repo.GetAccount(account.ID); err == nil {
		account = core.PreserveAccountControlState(account, current)
	}
	if repo, ok := s.repo.(interface {
		UpsertRuntimeAccount(core.Account) error
	}); ok {
		return repo.UpsertRuntimeAccount(account)
	}
	return s.repo.UpsertAccount(account)
}

func (s *Service) effectiveProxyURLForAccount(account core.Account) string {
	if proxyURL := strings.TrimSpace(account.ProxyURL); proxyURL != "" {
		return proxyURL
	}
	systemProxyURL := ""
	if settings, err := s.repo.GetSystemSettings(); err == nil {
		systemProxyURL = strings.TrimSpace(settings.Network.SystemProxyURL)
	}
	groupName := strings.TrimSpace(account.Group)
	if groupName == "" {
		return systemProxyURL
	}
	for _, group := range s.repo.ListAccountGroups() {
		if strings.EqualFold(group.Name, groupName) {
			if proxyURL := strings.TrimSpace(group.ProxyURL); proxyURL != "" {
				return proxyURL
			}
			return systemProxyURL
		}
	}
	return systemProxyURL
}

func accountSupportsQuotaRefresh(account core.Account) bool {
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

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func cloneTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copyValue := value.UTC()
	return &copyValue
}

func (s *Service) releaseBilling(hold *billingHold, reason string) {
	if hold == nil || !hold.active {
		return
	}
	if err := s.repo.ReleaseBilling(core.BillingReleaseInput{
		RequestID: hold.requestID,
		ClientID:  hold.clientID,
		Reason:    reason,
	}); err == nil {
		hold.active = false
		s.notifyBillingEvent("usage_released", core.BillingReservation{
			RequestID: hold.requestID,
			ClientID:  hold.clientID,
			UserID:    hold.userID,
		})
	}
}

func (s *Service) notifyBillingEvent(reason string, request core.BillingReservation) {
	if s == nil || s.billingEvents == nil {
		return
	}
	s.billingEvents(BillingEvent{
		Reason:    strings.TrimSpace(reason),
		UserID:    strings.TrimSpace(request.UserID),
		RequestID: strings.TrimSpace(request.RequestID),
		ClientID:  strings.TrimSpace(request.ClientID),
	})
}

func settlementShouldReleaseReservation(err error) bool {
	return errors.Is(err, storage.ErrAmountOverflow)
}

func (s *Service) billingError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, storage.ErrPlanQuotaExhausted):
		return &BillingError{StatusCode: http.StatusPaymentRequired, Code: ErrorCodePlanQuotaExhausted, Message: "plan quota exhausted; purchase a plan or switch this API key to balance billing", Err: err}
	case errors.Is(err, storage.ErrInsufficientBalance):
		publicURL := s.billingPublicBaseURL()
		return &BillingError{StatusCode: http.StatusPaymentRequired, Code: ErrorCodeQuotaError, Message: insufficientBalanceMessage(publicURL), PublicURL: publicURL, Err: err}
	case errors.Is(err, storage.ErrClientSpendLimitExceeded):
		return &BillingError{StatusCode: http.StatusTooManyRequests, Code: ErrorCodeQuotaError, Message: "api client spend limit exceeded", Err: err}
	case errors.Is(err, storage.ErrAmountOverflow):
		return &BillingError{StatusCode: http.StatusBadRequest, Code: ErrorCodeBillingAmountOverflow, Message: "billing amount is too large", Err: err}
	case errors.Is(err, storage.ErrBillingClientOwnerMismatch):
		return &BillingError{StatusCode: http.StatusForbidden, Code: ErrorCodeBillingOwnerMismatch, Message: "api client owner does not match billing user", Err: err}
	case errors.Is(err, storage.ErrBillingRequestConflict):
		return &BillingError{StatusCode: http.StatusConflict, Code: ErrorCodeBillingConflict, Message: "billing request conflict", Err: err}
	default:
		return err
	}
}

func (s *Service) billingPublicBaseURL() string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(s.currentSystemSettings().Runtime.PublicBaseURL)
}

func insufficientBalanceMessage(publicURL string) string {
	publicURL = strings.TrimSpace(publicURL)
	if publicURL == "" {
		return "Insufficient account balance. Please recharge before continuing."
	}
	return fmt.Sprintf("Insufficient account balance. Please visit %s to recharge before continuing.", publicURL)
}

func estimateGatewayPromptTokens(req *core.GatewayRequest) int {
	if req == nil {
		return 0
	}
	if len(req.RawMessages) > 0 {
		return estimateTextTokens(string(req.RawMessages))
	}
	if len(req.RawBody) > 0 {
		return estimateTextTokens(string(req.RawBody))
	}
	total := 0
	for _, message := range req.Messages {
		total += estimateTextTokens(message.Role)
		total += estimateTextTokens(message.Content)
	}
	return total
}

func estimateGatewayCompletionTokens(req *core.GatewayRequest) int {
	if req == nil {
		return 0
	}
	if req.MaxCompletionTokens != nil && *req.MaxCompletionTokens > 0 {
		return *req.MaxCompletionTokens
	}
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		return *req.MaxTokens
	}
	return 1024
}

func estimateTextTokens(text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}
	return (len(text) + 3) / 4
}

func usageIsEmpty(usage core.Usage) bool {
	return usage.PromptTokens <= 0 && usage.CompletionTokens <= 0 && usage.TotalTokens <= 0
}

func (s *Service) billingAccountGroupSnapshot(model core.ModelConfig, client *core.APIClient) map[string]core.AccountGroup {
	groups := accountGroupsByName(s.repo.ListAccountGroups())
	clientGroup := clientAccountGroup(client)
	groupRestricted := client != nil
	now := time.Now().UTC()
	clientBillingGroup := core.AccountGroup{}
	if client != nil {
		clientBillingGroup = s.clientBillingAccountGroupSnapshot(groups, clientGroup, now)
	}
	out := make(map[string]core.AccountGroup)
	for _, account := range s.repo.ListAccounts() {
		account = core.NormalizeAccountRuntimeState(account, now)
		if account.Provider != model.Provider {
			continue
		}
		if !accountAvailableForBillingReserve(account, now) {
			continue
		}
		accountGroup := gatewayAccountGroupName(account.Group)
		groupKey := strings.ToLower(accountGroup)
		if groupRestricted && !strings.EqualFold(accountGroup, clientGroup) {
			continue
		}
		group := clientBillingGroup
		if client == nil {
			var ok bool
			group, ok = groups[groupKey]
			if !ok {
				group = core.AccountGroup{Name: accountGroup, BillingMultiplierBps: core.AccountGroupDefaultMultiplierBps}
			} else if strings.TrimSpace(group.Name) == "" {
				group.Name = accountGroup
			}
		}
		out[strings.TrimSpace(account.ID)] = group
	}
	return out
}

func (s *Service) reservationBillingAccountGroup(client *core.APIClient, billingSource string) (string, int64) {
	if client == nil || s == nil || s.repo == nil {
		return "", 0
	}
	group := s.clientBillingAccountGroupSnapshot(accountGroupsByName(s.repo.ListAccountGroups()), clientAccountGroup(client), time.Now().UTC())
	if core.NormalizeClientBillingSource(billingSource) == core.ClientBillingSourcePlan {
		return strings.TrimSpace(group.Name), normalizeBillingMultiplierBps(core.EffectiveAccountGroupPlanMultiplier(group, time.Now()))
	}
	return strings.TrimSpace(group.Name), normalizeBillingMultiplierBps(group.BillingMultiplierBps)
}

func (s *Service) ensurePlanBillingAllowed(client *core.APIClient, billingSource string) error {
	if client == nil || s == nil || s.repo == nil || core.NormalizeClientBillingSource(billingSource) != core.ClientBillingSourcePlan {
		return nil
	}
	group := s.clientBillingAccountGroupSnapshot(accountGroupsByName(s.repo.ListAccountGroups()), clientAccountGroup(client), time.Now().UTC())
	if core.AccountGroupPlanBillingEnabled(group) {
		return nil
	}
	return &BillingError{
		StatusCode: http.StatusForbidden,
		Code:       ErrorCodePlanBillingDisabled,
		Message:    "plan billing is disabled for this account group; switch this API key to balance billing",
		Err:        fmt.Errorf("account group %q does not allow plan billing", strings.TrimSpace(group.Name)),
	}
}

func (s *Service) clientBillingAccountGroupSnapshot(groups map[string]core.AccountGroup, clientGroup string, now time.Time) core.AccountGroup {
	clientGroup = core.NormalizeAccountGroupName(clientGroup)
	groupKey := strings.ToLower(clientGroup)
	group, ok := groups[groupKey]
	if !ok {
		group = core.AccountGroup{Name: clientGroup, BillingMultiplierBps: core.AccountGroupDefaultMultiplierBps}
	}
	return billingGroupSnapshot(group, now)
}

func billingGroupSnapshot(group core.AccountGroup, now time.Time) core.AccountGroup {
	group = core.NormalizeAccountGroupBilling(group)
	group.BillingMultiplierBps = core.EffectiveAccountGroupMultiplier(group, now)
	group.TimedMultipliers = nil
	return group
}

func (s *Service) maxReservePricing(model core.ModelConfig, client *core.APIClient, inputTokens int) billingPricing {
	groups := accountGroupsByName(s.repo.ListAccountGroups())
	clientGroup := clientAccountGroup(client)
	if client != nil {
		return s.maxReservePricingForClientGroup(model, groups, clientGroup, inputTokens)
	}
	maxPricing := modelMaxBillingPricing(model, inputTokens)
	seenGroup := make(map[string]bool, len(groups)+1)
	now := time.Now().UTC()
	for _, account := range s.repo.ListAccounts() {
		account = core.NormalizeAccountRuntimeState(account, now)
		if account.Provider != model.Provider {
			continue
		}
		if !accountAvailableForBillingReserve(account, now) {
			continue
		}
		key := strings.ToLower(gatewayAccountGroupName(account.Group))
		if seenGroup[key] {
			continue
		}
		seenGroup[key] = true
		group, ok := groups[key]
		if !ok {
			continue
		}
		group = billingGroupSnapshot(group, now)
		pricing := groupMaxBillingPricing(model, group, inputTokens)
		maxPricing = maxBillingPricing(maxPricing, pricing)
	}
	return maxPricing
}

func (s *Service) maxReservePricingForClientGroup(model core.ModelConfig, groups map[string]core.AccountGroup, clientGroup string, inputTokens int) billingPricing {
	clientGroup = core.NormalizeAccountGroupName(clientGroup)
	groupKey := strings.ToLower(clientGroup)
	maxPricing := billingPricing{}
	seenGroup := make(map[string]bool, 1)
	found := false
	now := time.Now().UTC()
	for _, account := range s.repo.ListAccounts() {
		account = core.NormalizeAccountRuntimeState(account, now)
		if account.Provider != model.Provider {
			continue
		}
		if !accountAvailableForBillingReserve(account, now) {
			continue
		}
		key := strings.ToLower(gatewayAccountGroupName(account.Group))
		if key != groupKey {
			continue
		}
		if seenGroup[key] {
			continue
		}
		seenGroup[key] = true
		group, ok := groups[key]
		if !ok {
			group = core.AccountGroup{Name: clientGroup, BillingMultiplierBps: core.AccountGroupDefaultMultiplierBps}
		}
		group = billingGroupSnapshot(group, now)
		pricing := groupMaxBillingPricing(model, group, inputTokens)
		maxPricing = maxBillingPricing(maxPricing, pricing)
		found = true
	}
	if found {
		return maxPricing
	}
	if group, ok := groups[groupKey]; ok {
		group = billingGroupSnapshot(group, time.Now().UTC())
		return groupMaxBillingPricing(model, group, inputTokens)
	}
	return modelMaxBillingPricing(model, inputTokens)
}

func (s *Service) maxPlanReservePricing(model core.ModelConfig, client *core.APIClient, inputTokens int) billingPricing {
	groups := accountGroupsByName(s.repo.ListAccountGroups())
	clientGroup := clientAccountGroup(client)
	if client != nil {
		return s.maxPlanReservePricingForClientGroup(model, groups, clientGroup, inputTokens)
	}
	return modelMaxBillingPricing(model, inputTokens)
}

func (s *Service) maxPlanReservePricingForClientGroup(model core.ModelConfig, groups map[string]core.AccountGroup, clientGroup string, inputTokens int) billingPricing {
	clientGroup = core.NormalizeAccountGroupName(clientGroup)
	groupKey := strings.ToLower(clientGroup)
	maxPricing := billingPricing{}
	seenGroup := make(map[string]bool, 1)
	found := false
	now := time.Now().UTC()
	for _, account := range s.repo.ListAccounts() {
		account = core.NormalizeAccountRuntimeState(account, now)
		if account.Provider != model.Provider {
			continue
		}
		if !accountAvailableForBillingReserve(account, now) {
			continue
		}
		key := strings.ToLower(gatewayAccountGroupName(account.Group))
		if key != groupKey {
			continue
		}
		if seenGroup[key] {
			continue
		}
		seenGroup[key] = true
		group, ok := groups[key]
		if !ok {
			group = core.AccountGroup{Name: clientGroup, BillingMultiplierBps: core.AccountGroupDefaultMultiplierBps}
		}
		group = billingGroupSnapshot(group, now)
		pricing := groupMaxPlanBillingPricing(model, group, inputTokens)
		maxPricing = maxBillingPricing(maxPricing, pricing)
		found = true
	}
	if found {
		return maxPricing
	}
	if group, ok := groups[groupKey]; ok {
		group = billingGroupSnapshot(group, time.Now().UTC())
		return groupMaxPlanBillingPricing(model, group, inputTokens)
	}
	return groupMaxPlanBillingPricing(model, core.AccountGroup{Name: clientGroup, BillingMultiplierBps: core.AccountGroupDefaultMultiplierBps}, inputTokens)
}

func (s *Service) maxFixedReservePrice(model core.ModelConfig, client *core.APIClient) int64 {
	groups := accountGroupsByName(s.repo.ListAccountGroups())
	clientGroup := clientAccountGroup(client)
	if client != nil {
		return s.maxFixedReservePriceForClientGroup(model, groups, clientGroup)
	}
	maxPrice := fixedBillingPrice(model, core.AccountGroup{BillingMultiplierBps: 10000})
	seenGroup := make(map[string]bool, len(groups)+1)
	now := time.Now().UTC()
	for _, account := range s.repo.ListAccounts() {
		account = core.NormalizeAccountRuntimeState(account, now)
		if account.Provider != model.Provider {
			continue
		}
		if !accountAvailableForBillingReserve(account, now) {
			continue
		}
		groupName := strings.ToLower(gatewayAccountGroupName(account.Group))
		if seenGroup[groupName] {
			continue
		}
		seenGroup[groupName] = true
		group := groups[groupName]
		group = billingGroupSnapshot(group, now)
		maxPrice = maxInt64(maxPrice, fixedBillingPrice(model, group))
	}
	return maxPrice
}

func (s *Service) maxFixedReservePriceForClientGroup(model core.ModelConfig, groups map[string]core.AccountGroup, clientGroup string) int64 {
	clientGroup = core.NormalizeAccountGroupName(clientGroup)
	groupKey := strings.ToLower(clientGroup)
	maxPrice := int64(0)
	seenGroup := make(map[string]bool, 1)
	found := false
	now := time.Now().UTC()
	for _, account := range s.repo.ListAccounts() {
		account = core.NormalizeAccountRuntimeState(account, now)
		if account.Provider != model.Provider {
			continue
		}
		if !accountAvailableForBillingReserve(account, now) {
			continue
		}
		groupName := strings.ToLower(gatewayAccountGroupName(account.Group))
		if groupName != groupKey {
			continue
		}
		if seenGroup[groupName] {
			continue
		}
		seenGroup[groupName] = true
		group, ok := groups[groupName]
		if !ok {
			group = core.AccountGroup{Name: clientGroup, BillingMultiplierBps: core.AccountGroupDefaultMultiplierBps}
		}
		group = billingGroupSnapshot(group, now)
		maxPrice = maxInt64(maxPrice, fixedBillingPrice(model, group))
		found = true
	}
	if found {
		return maxPrice
	}
	if group, ok := groups[groupKey]; ok {
		group = billingGroupSnapshot(group, time.Now().UTC())
		return fixedBillingPrice(model, group)
	}
	return fixedBillingPrice(model, core.AccountGroup{Name: clientGroup, BillingMultiplierBps: core.AccountGroupDefaultMultiplierBps})
}

func (s *Service) maxPlanFixedReservePrice(model core.ModelConfig, client *core.APIClient) int64 {
	groups := accountGroupsByName(s.repo.ListAccountGroups())
	clientGroup := clientAccountGroup(client)
	if client != nil {
		return s.maxPlanFixedReservePriceForClientGroup(model, groups, clientGroup)
	}
	return fixedPlanBillingPrice(model, core.AccountGroup{BillingMultiplierBps: core.AccountGroupDefaultMultiplierBps})
}

func (s *Service) maxPlanFixedReservePriceForClientGroup(model core.ModelConfig, groups map[string]core.AccountGroup, clientGroup string) int64 {
	clientGroup = core.NormalizeAccountGroupName(clientGroup)
	groupKey := strings.ToLower(clientGroup)
	maxPrice := int64(0)
	seenGroup := make(map[string]bool, 1)
	found := false
	now := time.Now().UTC()
	for _, account := range s.repo.ListAccounts() {
		account = core.NormalizeAccountRuntimeState(account, now)
		if account.Provider != model.Provider {
			continue
		}
		if !accountAvailableForBillingReserve(account, now) {
			continue
		}
		groupName := strings.ToLower(gatewayAccountGroupName(account.Group))
		if groupName != groupKey {
			continue
		}
		if seenGroup[groupName] {
			continue
		}
		seenGroup[groupName] = true
		group, ok := groups[groupName]
		if !ok {
			group = core.AccountGroup{Name: clientGroup, BillingMultiplierBps: core.AccountGroupDefaultMultiplierBps}
		}
		group = billingGroupSnapshot(group, now)
		maxPrice = maxInt64(maxPrice, fixedPlanBillingPrice(model, group))
		found = true
	}
	if found {
		return maxPrice
	}
	if group, ok := groups[groupKey]; ok {
		group = billingGroupSnapshot(group, time.Now().UTC())
		return fixedPlanBillingPrice(model, group)
	}
	return fixedPlanBillingPrice(model, core.AccountGroup{Name: clientGroup, BillingMultiplierBps: core.AccountGroupDefaultMultiplierBps})
}

func modelBillingPricing(model core.ModelConfig, inputTokens int) billingPricing {
	if normalizeModelBillingMode(model.BillingMode) == core.ModelBillingModeTieredExpr {
		if tier, ok := selectModelPricingTier(model, inputTokens); ok {
			return normalizeBillingPricing(billingPricing{
				inputNanoUSDPer1M:        tier.InputPriceNanoUSD,
				cachedInputNanoUSDPer1M:  tier.CachedInputPriceNanoUSD,
				cacheWriteNanoUSDPer1M:   tier.CacheWritePriceNanoUSD,
				cacheWrite5mNanoUSDPer1M: tier.CacheWrite5mPriceNanoUSD,
				cacheWrite1hNanoUSDPer1M: tier.CacheWrite1hPriceNanoUSD,
				outputNanoUSDPer1M:       tier.OutputPriceNanoUSD,
				imageOutputNanoUSDPer1M:  tier.ImageOutputPriceNanoUSD,
			})
		}
	}
	return normalizeBillingPricing(billingPricing{
		inputNanoUSDPer1M:        model.InputPriceNanoUSDPer1M,
		cachedInputNanoUSDPer1M:  model.CachedInputPriceNanoUSDPer1M,
		cacheWriteNanoUSDPer1M:   model.CacheWritePriceNanoUSDPer1M,
		cacheWrite5mNanoUSDPer1M: model.CacheWrite5mPriceNanoUSDPer1M,
		cacheWrite1hNanoUSDPer1M: model.CacheWrite1hPriceNanoUSDPer1M,
		outputNanoUSDPer1M:       model.OutputPriceNanoUSDPer1M,
		imageOutputNanoUSDPer1M:  model.ImageOutputPriceNanoUSDPer1M,
	})
}

func groupBillingPricing(model core.ModelConfig, group core.AccountGroup, inputTokens int) billingPricing {
	pricing := modelBillingPricing(model, inputTokens)
	if model.BillingFixed {
		return pricing
	}
	return applyGroupBillingPricing(pricing, group)
}

func groupPlanBillingPricing(model core.ModelConfig, group core.AccountGroup, inputTokens int) billingPricing {
	pricing := modelBillingPricing(model, inputTokens)
	if model.BillingFixed {
		return pricing
	}
	return applyGroupPlanBillingPricing(pricing, group)
}

func modelMaxBillingPricing(model core.ModelConfig, inputTokens int) billingPricing {
	pricing := modelBillingPricing(model, inputTokens)
	if normalizeModelBillingMode(model.BillingMode) != core.ModelBillingModeTieredExpr {
		return pricing
	}
	for _, tier := range model.PricingTiers {
		pricing = maxBillingPricing(pricing, normalizeBillingPricing(billingPricing{
			inputNanoUSDPer1M:        tier.InputPriceNanoUSD,
			cachedInputNanoUSDPer1M:  tier.CachedInputPriceNanoUSD,
			cacheWriteNanoUSDPer1M:   tier.CacheWritePriceNanoUSD,
			cacheWrite5mNanoUSDPer1M: tier.CacheWrite5mPriceNanoUSD,
			cacheWrite1hNanoUSDPer1M: tier.CacheWrite1hPriceNanoUSD,
			outputNanoUSDPer1M:       tier.OutputPriceNanoUSD,
			imageOutputNanoUSDPer1M:  tier.ImageOutputPriceNanoUSD,
		}))
	}
	return pricing
}

func groupMaxBillingPricing(model core.ModelConfig, group core.AccountGroup, inputTokens int) billingPricing {
	pricing := modelMaxBillingPricing(model, inputTokens)
	if model.BillingFixed {
		return pricing
	}
	return applyGroupBillingPricing(pricing, group)
}

func groupMaxPlanBillingPricing(model core.ModelConfig, group core.AccountGroup, inputTokens int) billingPricing {
	pricing := modelMaxBillingPricing(model, inputTokens)
	if model.BillingFixed {
		return pricing
	}
	return applyGroupPlanBillingPricing(pricing, group)
}

func applyGroupBillingPricing(pricing billingPricing, group core.AccountGroup) billingPricing {
	if group.InputPriceNanoUSDPer1M > 0 {
		pricing.inputNanoUSDPer1M = group.InputPriceNanoUSDPer1M
	}
	if group.CachedInputPriceNanoUSDPer1M > 0 {
		pricing.cachedInputNanoUSDPer1M = group.CachedInputPriceNanoUSDPer1M
	}
	if group.CacheWritePriceNanoUSDPer1M > 0 {
		pricing.cacheWriteNanoUSDPer1M = group.CacheWritePriceNanoUSDPer1M
	}
	if group.CacheWrite5mPriceNanoUSDPer1M > 0 {
		pricing.cacheWrite5mNanoUSDPer1M = group.CacheWrite5mPriceNanoUSDPer1M
	}
	if group.CacheWrite1hPriceNanoUSDPer1M > 0 {
		pricing.cacheWrite1hNanoUSDPer1M = group.CacheWrite1hPriceNanoUSDPer1M
	}
	if group.OutputPriceNanoUSDPer1M > 0 {
		pricing.outputNanoUSDPer1M = group.OutputPriceNanoUSDPer1M
	}
	if group.ImageOutputPriceNanoUSDPer1M > 0 {
		pricing.imageOutputNanoUSDPer1M = group.ImageOutputPriceNanoUSDPer1M
	}
	pricing = normalizeBillingPricing(pricing)
	multiplierBps := core.EffectiveAccountGroupMultiplier(group, time.Now())
	pricing.inputNanoUSDPer1M = mulDivRoundUp(pricing.inputNanoUSDPer1M, multiplierBps, 10000)
	pricing.cachedInputNanoUSDPer1M = mulDivRoundUp(pricing.cachedInputNanoUSDPer1M, multiplierBps, 10000)
	pricing.cacheWriteNanoUSDPer1M = mulDivRoundUp(pricing.cacheWriteNanoUSDPer1M, multiplierBps, 10000)
	pricing.cacheWrite5mNanoUSDPer1M = mulDivRoundUp(pricing.cacheWrite5mNanoUSDPer1M, multiplierBps, 10000)
	pricing.cacheWrite1hNanoUSDPer1M = mulDivRoundUp(pricing.cacheWrite1hNanoUSDPer1M, multiplierBps, 10000)
	pricing.outputNanoUSDPer1M = mulDivRoundUp(pricing.outputNanoUSDPer1M, multiplierBps, 10000)
	pricing.imageOutputNanoUSDPer1M = mulDivRoundUp(pricing.imageOutputNanoUSDPer1M, multiplierBps, 10000)
	return pricing
}

func applyGroupPlanBillingPricing(pricing billingPricing, group core.AccountGroup) billingPricing {
	if group.InputPriceNanoUSDPer1M > 0 {
		pricing.inputNanoUSDPer1M = group.InputPriceNanoUSDPer1M
	}
	if group.CachedInputPriceNanoUSDPer1M > 0 {
		pricing.cachedInputNanoUSDPer1M = group.CachedInputPriceNanoUSDPer1M
	}
	if group.CacheWritePriceNanoUSDPer1M > 0 {
		pricing.cacheWriteNanoUSDPer1M = group.CacheWritePriceNanoUSDPer1M
	}
	if group.CacheWrite5mPriceNanoUSDPer1M > 0 {
		pricing.cacheWrite5mNanoUSDPer1M = group.CacheWrite5mPriceNanoUSDPer1M
	}
	if group.CacheWrite1hPriceNanoUSDPer1M > 0 {
		pricing.cacheWrite1hNanoUSDPer1M = group.CacheWrite1hPriceNanoUSDPer1M
	}
	if group.OutputPriceNanoUSDPer1M > 0 {
		pricing.outputNanoUSDPer1M = group.OutputPriceNanoUSDPer1M
	}
	if group.ImageOutputPriceNanoUSDPer1M > 0 {
		pricing.imageOutputNanoUSDPer1M = group.ImageOutputPriceNanoUSDPer1M
	}
	pricing = normalizeBillingPricing(pricing)
	multiplierBps := core.EffectiveAccountGroupPlanMultiplier(group, time.Now())
	pricing.inputNanoUSDPer1M = mulDivRoundUp(pricing.inputNanoUSDPer1M, multiplierBps, 10000)
	pricing.cachedInputNanoUSDPer1M = mulDivRoundUp(pricing.cachedInputNanoUSDPer1M, multiplierBps, 10000)
	pricing.cacheWriteNanoUSDPer1M = mulDivRoundUp(pricing.cacheWriteNanoUSDPer1M, multiplierBps, 10000)
	pricing.cacheWrite5mNanoUSDPer1M = mulDivRoundUp(pricing.cacheWrite5mNanoUSDPer1M, multiplierBps, 10000)
	pricing.cacheWrite1hNanoUSDPer1M = mulDivRoundUp(pricing.cacheWrite1hNanoUSDPer1M, multiplierBps, 10000)
	pricing.outputNanoUSDPer1M = mulDivRoundUp(pricing.outputNanoUSDPer1M, multiplierBps, 10000)
	pricing.imageOutputNanoUSDPer1M = mulDivRoundUp(pricing.imageOutputNanoUSDPer1M, multiplierBps, 10000)
	return pricing
}

func fixedBillingPrice(model core.ModelConfig, group core.AccountGroup) int64 {
	price := model.RequestPriceNanoUSD
	if model.BillingFixed {
		return price
	}
	multiplierBps := core.EffectiveAccountGroupMultiplier(group, time.Now())
	return mulDivRoundUp(price, multiplierBps, 10000)
}

func fixedPlanBillingPrice(model core.ModelConfig, group core.AccountGroup) int64 {
	price := model.RequestPriceNanoUSD
	if model.BillingFixed {
		return price
	}
	multiplierBps := core.EffectiveAccountGroupPlanMultiplier(group, time.Now())
	return mulDivRoundUp(price, multiplierBps, 10000)
}

func selectModelPricingTier(model core.ModelConfig, inputTokens int) (core.ModelPricingTier, bool) {
	var fallback core.ModelPricingTier
	var hasFallback bool
	var best core.ModelPricingTier
	var hasBest bool
	for _, tier := range model.PricingTiers {
		if tier.MaxInputTokens <= 0 {
			fallback = tier
			hasFallback = true
			continue
		}
		if inputTokens <= tier.MaxInputTokens && (!hasBest || tier.MaxInputTokens < best.MaxInputTokens) {
			best = tier
			hasBest = true
		}
	}
	if hasBest {
		return best, true
	}
	return fallback, hasFallback
}

func normalizeModelBillingMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case core.ModelBillingModeRequest:
		return core.ModelBillingModeRequest
	case core.ModelBillingModeTieredExpr:
		return core.ModelBillingModeTieredExpr
	default:
		return core.ModelBillingModeToken
	}
}

func billingInputTokens(usage core.Usage) int {
	if usage.PromptTokens > 0 {
		return usage.PromptTokens
	}
	if usage.TotalTokens > usage.CompletionTokens {
		return usage.TotalTokens - usage.CompletionTokens
	}
	return usage.TotalTokens
}

func billingUsageForProvider(provider core.ProviderKind, usage core.Usage) core.Usage {
	if provider == core.ProviderOpenAI {
		usage.CacheCreationTokens = 0
		usage.CacheCreation5mTokens = 0
		usage.CacheCreation1hTokens = 0
	}
	return usage
}

func billingPricingForProvider(provider core.ProviderKind, pricing billingPricing) billingPricing {
	if provider == core.ProviderOpenAI {
		pricing.cacheWriteNanoUSDPer1M = 0
		pricing.cacheWrite5mNanoUSDPer1M = 0
		pricing.cacheWrite1hNanoUSDPer1M = 0
	}
	return pricing
}

func gatewayRequestServiceTier(req *core.GatewayRequest) string {
	if req == nil {
		return ""
	}
	if serviceTier := strings.TrimSpace(req.ServiceTier); serviceTier != "" {
		return serviceTier
	}
	if len(req.RawBody) == 0 {
		return ""
	}
	var payload struct {
		ServiceTier string `json:"service_tier"`
	}
	if err := json.Unmarshal(req.RawBody, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.ServiceTier)
}

func gatewayServiceTierIsFast(serviceTier string) bool {
	switch strings.ToLower(strings.TrimSpace(serviceTier)) {
	case "fast", "priority":
		return true
	default:
		return false
	}
}

func fastBillingMultiplierBps(serviceTier, modelID string) int64 {
	if !gatewayServiceTierIsFast(serviceTier) {
		return 10000
	}
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	switch {
	case strings.HasPrefix(modelID, "gpt-5.5"):
		return 25000
	case strings.HasPrefix(modelID, "gpt-5.4"):
		return 20000
	default:
		return 10000
	}
}

func fastBillingModelID(model core.ModelConfig, routedModel string) string {
	if routedModel = strings.TrimSpace(routedModel); routedModel != "" {
		return routedModel
	}
	if upstreamID := strings.TrimSpace(model.UpstreamID); upstreamID != "" {
		return upstreamID
	}
	return strings.TrimSpace(model.ID)
}

func applyFastBillingPricing(pricing billingPricing, fastBps int64) billingPricing {
	fastBps = normalizeBillingMultiplierBps(fastBps)
	pricing.inputNanoUSDPer1M = applyBillingMultiplierBps(pricing.inputNanoUSDPer1M, fastBps)
	pricing.cachedInputNanoUSDPer1M = applyBillingMultiplierBps(pricing.cachedInputNanoUSDPer1M, fastBps)
	pricing.cacheWriteNanoUSDPer1M = applyBillingMultiplierBps(pricing.cacheWriteNanoUSDPer1M, fastBps)
	pricing.cacheWrite5mNanoUSDPer1M = applyBillingMultiplierBps(pricing.cacheWrite5mNanoUSDPer1M, fastBps)
	pricing.cacheWrite1hNanoUSDPer1M = applyBillingMultiplierBps(pricing.cacheWrite1hNanoUSDPer1M, fastBps)
	pricing.outputNanoUSDPer1M = applyBillingMultiplierBps(pricing.outputNanoUSDPer1M, fastBps)
	pricing.imageOutputNanoUSDPer1M = applyBillingMultiplierBps(pricing.imageOutputNanoUSDPer1M, fastBps)
	return pricing
}

func normalizeBillingPricing(pricing billingPricing) billingPricing {
	if pricing.cacheWriteNanoUSDPer1M <= 0 {
		pricing.cacheWriteNanoUSDPer1M = pricing.inputNanoUSDPer1M
	}
	if pricing.cacheWrite5mNanoUSDPer1M <= 0 {
		pricing.cacheWrite5mNanoUSDPer1M = pricing.cacheWriteNanoUSDPer1M
	}
	if pricing.cacheWrite1hNanoUSDPer1M <= 0 {
		pricing.cacheWrite1hNanoUSDPer1M = pricing.cacheWriteNanoUSDPer1M
	}
	if pricing.imageOutputNanoUSDPer1M <= 0 {
		pricing.imageOutputNanoUSDPer1M = pricing.outputNanoUSDPer1M
	}
	return pricing
}

func maxBillingPricing(a, b billingPricing) billingPricing {
	a.inputNanoUSDPer1M = maxInt64(a.inputNanoUSDPer1M, b.inputNanoUSDPer1M)
	a.cachedInputNanoUSDPer1M = maxInt64(a.cachedInputNanoUSDPer1M, b.cachedInputNanoUSDPer1M)
	a.cacheWriteNanoUSDPer1M = maxInt64(a.cacheWriteNanoUSDPer1M, b.cacheWriteNanoUSDPer1M)
	a.cacheWrite5mNanoUSDPer1M = maxInt64(a.cacheWrite5mNanoUSDPer1M, b.cacheWrite5mNanoUSDPer1M)
	a.cacheWrite1hNanoUSDPer1M = maxInt64(a.cacheWrite1hNanoUSDPer1M, b.cacheWrite1hNanoUSDPer1M)
	a.outputNanoUSDPer1M = maxInt64(a.outputNanoUSDPer1M, b.outputNanoUSDPer1M)
	a.imageOutputNanoUSDPer1M = maxInt64(a.imageOutputNanoUSDPer1M, b.imageOutputNanoUSDPer1M)
	return a
}

func applyBillingMultiplierBps(value, multiplierBps int64) int64 {
	return mulDivRoundUp(value, normalizeBillingMultiplierBps(multiplierBps), 10000)
}

func normalizeBillingMultiplierBps(multiplierBps int64) int64 {
	if multiplierBps <= 0 {
		return 10000
	}
	return multiplierBps
}

func accountGroupsByName(groups []core.AccountGroup) map[string]core.AccountGroup {
	out := make(map[string]core.AccountGroup, len(groups)+1)
	planBillingEnabled := true
	out[strings.ToLower(core.DefaultAccountGroupName)] = core.AccountGroup{
		ID:                       core.DefaultAccountGroupID,
		Name:                     core.DefaultAccountGroupName,
		BillingMultiplierBps:     core.AccountGroupDefaultMultiplierBps,
		PlanBillingMultiplierBps: core.AccountGroupDefaultMultiplierBps,
		PlanBillingEnabled:       &planBillingEnabled,
	}
	for _, group := range groups {
		group = core.NormalizeAccountGroupBilling(group)
		name := strings.TrimSpace(group.Name)
		if strings.EqualFold(strings.TrimSpace(group.ID), core.DefaultAccountGroupID) || strings.EqualFold(name, core.DefaultAccountGroupName) {
			group.ID = core.DefaultAccountGroupID
			group.Name = core.DefaultAccountGroupName
			name = core.DefaultAccountGroupName
		}
		if name == "" {
			continue
		}
		out[strings.ToLower(name)] = group
	}
	return out
}

func accountAvailableForBillingReserve(account core.Account, now time.Time) bool {
	return core.AccountAvailableForRouting(account, now)
}

func clientAccountGroup(client *core.APIClient) string {
	if client == nil {
		return ""
	}
	return core.NormalizeAccountGroupName(client.AccountGroup)
}

func clientBillingSource(client *core.APIClient) string {
	if client == nil {
		return core.ClientBillingSourceCash
	}
	return core.NormalizeClientBillingSource(client.BillingSource)
}

func maxInt64(a, b int64) int64 {
	if a >= b {
		return a
	}
	return b
}

func billingCostNano(usage core.Usage, pricing billingPricing) int64 {
	pricing = normalizeBillingPricing(pricing)
	promptTokens := int64(usage.PromptTokens)
	cachedPromptTokens := int64(usage.CachedPromptTokens)
	cacheCreationTokens := int64(usage.CacheCreationTokens)
	cacheCreation5mTokens := int64(usage.CacheCreation5mTokens)
	cacheCreation1hTokens := int64(usage.CacheCreation1hTokens)
	completionTokens := int64(usage.CompletionTokens)
	imageOutputTokens := int64(usage.ImageOutputTokens)
	if promptTokens < 0 {
		promptTokens = 0
	}
	if cachedPromptTokens < 0 {
		cachedPromptTokens = 0
	}
	if cachedPromptTokens > promptTokens {
		cachedPromptTokens = promptTokens
	}
	if cacheCreationTokens < 0 {
		cacheCreationTokens = 0
	}
	if cacheCreation5mTokens < 0 {
		cacheCreation5mTokens = 0
	}
	if cacheCreation1hTokens < 0 {
		cacheCreation1hTokens = 0
	}
	if completionTokens < 0 {
		completionTokens = 0
	}
	if imageOutputTokens < 0 {
		imageOutputTokens = 0
	}
	if imageOutputTokens > completionTokens {
		imageOutputTokens = completionTokens
	}
	if promptTokens == 0 && completionTokens == 0 && usage.TotalTokens > 0 {
		promptTokens = int64(usage.TotalTokens)
	}
	if cacheCreationTokens == 0 && (cacheCreation5mTokens > 0 || cacheCreation1hTokens > 0) {
		cacheCreationTokens = addBillingCostSaturating(cacheCreation5mTokens, cacheCreation1hTokens)
	}
	cacheCreationRemainderTokens := cacheCreationTokens - cacheCreation5mTokens - cacheCreation1hTokens
	if cacheCreationRemainderTokens < 0 {
		cacheCreationRemainderTokens = 0
	}
	textCompletionTokens := completionTokens - imageOutputTokens
	uncachedPromptTokens := promptTokens - cachedPromptTokens
	total := addBillingCostSaturating(
		mulDivRoundUp(uncachedPromptTokens, pricing.inputNanoUSDPer1M, 1_000_000),
		mulDivRoundUp(cachedPromptTokens, pricing.cachedInputNanoUSDPer1M, 1_000_000),
	)
	total = addBillingCostSaturating(total, mulDivRoundUp(cacheCreationRemainderTokens, pricing.cacheWriteNanoUSDPer1M, 1_000_000))
	total = addBillingCostSaturating(total, mulDivRoundUp(cacheCreation5mTokens, pricing.cacheWrite5mNanoUSDPer1M, 1_000_000))
	total = addBillingCostSaturating(total, mulDivRoundUp(cacheCreation1hTokens, pricing.cacheWrite1hNanoUSDPer1M, 1_000_000))
	total = addBillingCostSaturating(total, mulDivRoundUp(textCompletionTokens, pricing.outputNanoUSDPer1M, 1_000_000))
	return addBillingCostSaturating(total, mulDivRoundUp(imageOutputTokens, pricing.imageOutputNanoUSDPer1M, 1_000_000))
}

func billingPricingHasPrice(pricing billingPricing) bool {
	pricing = normalizeBillingPricing(pricing)
	return pricing.inputNanoUSDPer1M > 0 ||
		pricing.cachedInputNanoUSDPer1M > 0 ||
		pricing.cacheWriteNanoUSDPer1M > 0 ||
		pricing.cacheWrite5mNanoUSDPer1M > 0 ||
		pricing.cacheWrite1hNanoUSDPer1M > 0 ||
		pricing.outputNanoUSDPer1M > 0 ||
		pricing.imageOutputNanoUSDPer1M > 0
}

func mulDivRoundUp(a, b, divisor int64) int64 {
	if a <= 0 || b <= 0 || divisor <= 0 {
		return 0
	}
	if a > math.MaxInt64/b {
		return math.MaxInt64
	}
	product := a * b
	if product > math.MaxInt64-(divisor-1) {
		return math.MaxInt64
	}
	return (product + divisor - 1) / divisor
}

func addBillingCostSaturating(a, b int64) int64 {
	if a <= 0 {
		return maxInt64(0, b)
	}
	if b <= 0 {
		return a
	}
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
