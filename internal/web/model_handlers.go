package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
)

func (s *Server) handleModelsPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin/models" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	locale := resolveLocale(w, r)
	data := withCSRFData(map[string]any{
		"TitleKey":  "page_title_models",
		"ActiveNav": "models",
		"Locale":    locale,
		"Page":      s.control.ModelPageForProvider(r.Context(), core.ProviderKind(r.URL.Query().Get("provider"))),
	}, r)
	s.render(w, "models.html", locale, data)
}

func (s *Server) handleUserModelsPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/models" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r, user := s.withOptionalConsoleUser(w, r)
	hasUser := strings.TrimSpace(user.ID) != ""
	if hasUser && user.IsAdmin() {
		http.Redirect(w, r, "/admin/models", http.StatusSeeOther)
		return
	}
	locale := resolveLocale(w, r)
	prices := s.control.EnabledModelPrices()
	if hasUser {
		prices = s.control.EnabledModelPricesForUser(user)
	}
	data := withCSRFData(map[string]any{
		"TitleKey":           "page_title_models",
		"ActiveNav":          "models",
		"Locale":             locale,
		"ModelPrices":        prices,
		"ModelPriceSections": controlplane.UserModelPriceSections(prices),
	}, r)
	s.render(w, "user_models.html", locale, data)
}

func (s *Server) handleModelActions(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/admin/models/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	switch {
	case len(parts) == 1 && parts[0] == "new":
		s.handleModelCreateSubmit(w, r)
	case len(parts) == 1 && parts[0] == "sync":
		s.handleModelSyncSubmit(w, r)
	case len(parts) == 2 && parts[1] == "toggle":
		before, hasBefore := s.auditModelConfig(r.Context(), parts[0])
		model, err := s.control.ToggleModel(parts[0])
		if err != nil {
			s.recordAdminAudit(r, "error", "model.toggle", "model", parts[0], "", err.Error())
			s.redirectWithNoticeError(w, r, adminModelsURLForProvider(currentModelProvider(r, "")), err)
			return
		}
		message := auditFieldsMessage(
			auditMessageField{Key: "enabled", Value: fmt.Sprintf("%t", model.Enabled)},
			auditMessageField{Key: "provider", Value: string(model.Provider)},
		)
		if hasBefore {
			message = auditChangeMessage(auditBoolChange("enabled", before.Enabled, model.Enabled))
		}
		s.recordAdminAudit(r, "ok", "model.toggle", "model", model.ID, model.DisplayName, message)
		s.publishModelsChanged(model.ID)
		http.Redirect(w, r, adminModelsURLForProvider(model.Provider), http.StatusSeeOther)
	case len(parts) == 2 && parts[1] == "price":
		s.handleModelPriceSubmit(w, r, parts[0])
	case len(parts) == 2 && parts[1] == "groups":
		s.handleModelGroupsSubmit(w, r, parts[0])
	case len(parts) == 2 && parts[1] == "delete":
		model, err := s.control.DeleteModel(parts[0])
		if err != nil {
			s.recordAdminAudit(r, "error", "model.delete", "model", parts[0], "", err.Error())
			s.redirectWithNoticeError(w, r, adminModelsURLForProvider(currentModelProvider(r, "")), err)
			return
		}
		s.recordAdminAudit(r, "ok", "model.delete", "model", model.ID, model.DisplayName, fmt.Sprintf("provider=%s", model.Provider))
		s.publishModelsChanged(model.ID)
		http.Redirect(w, r, adminModelsURLForProvider(model.Provider), http.StatusSeeOther)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleModelPriceSubmit(w http.ResponseWriter, r *http.Request, modelID string) {
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, "/admin/models", err)
		return
	}
	targetURL := adminModelsURLForProvider(currentModelProvider(r, ""))
	inputPrice, err := parseNanoUSDFormValue(r, "input_price_usd_per_1m")
	if err != nil {
		s.redirectWithNoticeError(w, r, targetURL, err)
		return
	}
	cachedInputPrice, err := parseNanoUSDFormValue(r, "cached_input_price_usd_per_1m")
	if err != nil {
		s.redirectWithNoticeError(w, r, targetURL, err)
		return
	}
	outputPrice, err := parseNanoUSDFormValue(r, "output_price_usd_per_1m")
	if err != nil {
		s.redirectWithNoticeError(w, r, targetURL, err)
		return
	}
	imageOutputPrice, err := parseNanoUSDFormValue(r, "image_output_price_usd_per_1m")
	if err != nil {
		s.redirectWithNoticeError(w, r, targetURL, err)
		return
	}
	requestPrice, err := parseNanoUSDFormValue(r, "request_price_usd")
	if err != nil {
		s.redirectWithNoticeError(w, r, targetURL, err)
		return
	}
	pricingTiers, err := parsePricingTiersForm(r)
	if err != nil {
		s.redirectWithNoticeError(w, r, targetURL, err)
		return
	}
	billingMode := r.FormValue("billing_mode")
	billingFixed := r.FormValue("billing_fixed") == "on"
	before, hasBefore := s.auditModelConfig(r.Context(), modelID)
	model, err := s.control.UpdateModelPricing(modelID, billingMode, billingFixed, inputPrice, cachedInputPrice, 0, 0, 0, outputPrice, imageOutputPrice, requestPrice, pricingTiers)
	if err != nil {
		s.recordAdminAudit(r, "error", "model.price", "model", modelID, "", err.Error())
		s.redirectWithNoticeError(w, r, targetURL, err)
		return
	}
	s.recordAdminAudit(r, "ok", "model.price", "model", model.ID, model.DisplayName, auditModelPricingChangeMessage(before, model, hasBefore))
	s.publishModelsChanged(model.ID)
	http.Redirect(w, r, adminModelsURLForProvider(model.Provider), http.StatusSeeOther)
}

func (s *Server) handleModelGroupsSubmit(w http.ResponseWriter, r *http.Request, modelID string) {
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, "/admin/models", err)
		return
	}
	targetURL := adminModelsURLForProvider(currentModelProvider(r, ""))
	before, hasBefore := s.auditModelConfig(r.Context(), modelID)
	model, err := s.control.UpdateModelVisibleGroups(modelID, append([]string{}, r.Form["visible_group"]...))
	if err != nil {
		s.recordAdminAudit(r, "error", "model.groups", "model", modelID, "", err.Error())
		s.redirectWithNoticeError(w, r, targetURL, err)
		return
	}
	s.recordAdminAudit(r, "ok", "model.groups", "model", model.ID, model.DisplayName, auditModelGroupsChangeMessage(before, model, hasBefore))
	s.publishModelsChanged(model.ID)
	http.Redirect(w, r, adminModelsURLForProvider(model.Provider), http.StatusSeeOther)
}

func (s *Server) handleModelCreateSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, "/admin/models", err)
		return
	}
	provider := core.ProviderKind(r.FormValue("provider"))
	targetURL := adminModelsURLForProvider(provider)
	inputPrice, err := parseNanoUSDFormValue(r, "input_price_usd_per_1m")
	if err != nil {
		s.redirectWithNoticeError(w, r, targetURL, err)
		return
	}
	cachedInputPrice, err := parseNanoUSDFormValue(r, "cached_input_price_usd_per_1m")
	if err != nil {
		s.redirectWithNoticeError(w, r, targetURL, err)
		return
	}
	outputPrice, err := parseNanoUSDFormValue(r, "output_price_usd_per_1m")
	if err != nil {
		s.redirectWithNoticeError(w, r, targetURL, err)
		return
	}
	imageOutputPrice, err := parseNanoUSDFormValue(r, "image_output_price_usd_per_1m")
	if err != nil {
		s.redirectWithNoticeError(w, r, targetURL, err)
		return
	}
	requestPrice, err := parseNanoUSDFormValue(r, "request_price_usd")
	if err != nil {
		s.redirectWithNoticeError(w, r, targetURL, err)
		return
	}
	pricingTiers, err := parsePricingTiersForm(r)
	if err != nil {
		s.redirectWithNoticeError(w, r, targetURL, err)
		return
	}
	model, err := s.control.CreateModel(controlplane.ModelInput{
		ID:                            r.FormValue("id"),
		Provider:                      provider,
		Type:                          r.FormValue("type"),
		UpstreamID:                    r.FormValue("upstream_id"),
		DisplayName:                   r.FormValue("display_name"),
		OwnedBy:                       r.FormValue("owned_by"),
		Enabled:                       true,
		VisibleGroups:                 append([]string{}, r.Form["visible_group"]...),
		BillingMode:                   r.FormValue("billing_mode"),
		BillingFixed:                  r.FormValue("billing_fixed") == "on",
		InputPriceNanoUSDPer1M:        inputPrice,
		CachedInputPriceNanoUSDPer1M:  cachedInputPrice,
		CacheWritePriceNanoUSDPer1M:   0,
		CacheWrite5mPriceNanoUSDPer1M: 0,
		CacheWrite1hPriceNanoUSDPer1M: 0,
		OutputPriceNanoUSDPer1M:       outputPrice,
		ImageOutputPriceNanoUSDPer1M:  imageOutputPrice,
		RequestPriceNanoUSD:           requestPrice,
		PricingTiers:                  pricingTiers,
	})
	if err != nil {
		s.recordAdminAudit(r, "error", "model.create", "model", strings.TrimSpace(r.FormValue("id")), "", err.Error())
		s.redirectWithNoticeError(w, r, targetURL, err)
		return
	}
	s.recordAdminAudit(r, "ok", "model.create", "model", model.ID, model.DisplayName, fmt.Sprintf("enabled=%t provider=%s", model.Enabled, model.Provider))
	s.publishModelsChanged(model.ID)
	http.Redirect(w, r, adminModelsURLForProvider(model.Provider), http.StatusSeeOther)
}

func (s *Server) handleModelSyncSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.redirectWithNoticeError(w, r, "/admin/models", err)
		return
	}
	provider := core.ProviderKind(r.FormValue("provider"))
	targetURL := adminModelsURLForProvider(provider)
	result, err := s.control.SyncProviderModels(r.Context(), provider)
	if err != nil {
		s.recordAdminAudit(r, "error", "model.sync", "model", string(provider), "", err.Error())
		s.redirectWithNoticeError(w, r, targetURL, err)
		return
	}
	message := fmt.Sprintf("imported=%d updated=%d skipped=%d", result.Imported, result.Updated, result.Skipped)
	s.recordAdminAudit(r, "ok", "model.sync", "model", string(provider), string(provider), message)
	s.publishModelsChanged(string(provider))
	http.Redirect(w, r, targetURL, http.StatusSeeOther)
}

func adminModelsURLForProvider(provider core.ProviderKind) string {
	provider = core.ProviderKind(strings.TrimSpace(string(provider)))
	if provider == "" {
		return "/admin/models"
	}
	return "/admin/models?provider=" + url.QueryEscape(string(provider))
}

func currentModelProvider(r *http.Request, fallback core.ProviderKind) core.ProviderKind {
	if r == nil {
		return fallback
	}
	if err := r.ParseForm(); err == nil {
		if provider := strings.TrimSpace(r.FormValue("current_provider")); provider != "" {
			return core.ProviderKind(provider)
		}
	}
	if r.URL != nil {
		if provider := strings.TrimSpace(r.URL.Query().Get("provider")); provider != "" {
			return core.ProviderKind(provider)
		}
	}
	return fallback
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	models := s.protocolModelObjects(r.Context())

	response := struct {
		Object string        `json:"object"`
		Data   []modelObject `json:"data"`
	}{Object: "list", Data: models}

	writeJSON(w, http.StatusOK, response)
}
