package web

import (
	"strings"
	"testing"
	"time"

	"net/http"
	"net/http/httptest"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

func TestUsageLogModelTextMarksFastMode(t *testing.T) {
	got := usageLogModelText(core.BillingReservation{Model: "gpt-5.5", FastMode: true})
	if got != "gpt-5.5 [fast]" {
		t.Fatalf("fast model text = %q, want %q", got, "gpt-5.5 [fast]")
	}

	got = usageLogModelText(core.BillingReservation{Model: "gpt-5.5"})
	if got != "gpt-5.5" {
		t.Fatalf("standard model text = %q, want %q", got, "gpt-5.5")
	}
}

func TestUsageFailedAccountTitleUsesOneLinePerFailure(t *testing.T) {
	got := usageFailedAccountTitle(localeZH, []string{
		"mdkj-team upstream_temporarily_unavailable: rate limited",
		" ",
		"OpenAI upstream_transport_error: timeout",
	})
	want := "mdkj-team upstream_temporarily_unavailable: rate limited\nOpenAI upstream_transport_error: timeout"
	if got != want {
		t.Fatalf("failed account title = %q, want %q", got, want)
	}
}

func TestFormatUSDDisplayKeepsSubCentPrecision(t *testing.T) {
	if got := formatUSDDisplay(5_000); got != "$0.000005" {
		t.Fatalf("formatUSDDisplay = %q, want %q", got, "$0.000005")
	}
	if got := formatUSDDisplay(-5_000); got != "-$0.000005" {
		t.Fatalf("formatUSDDisplay negative = %q, want %q", got, "-$0.000005")
	}
}

func TestFormatUSDDisplayRoundsCentValues(t *testing.T) {
	if got := formatUSDDisplay(91_955_975_000); got != "$91.96" {
		t.Fatalf("formatUSDDisplay rounded = %q, want %q", got, "$91.96")
	}
	if got := formatUSDDisplay(-91_955_975_000); got != "-$91.96" {
		t.Fatalf("formatUSDDisplay rounded negative = %q, want %q", got, "-$91.96")
	}
	if got := formatUSDDisplay(91_954_000_000); got != "$91.95" {
		t.Fatalf("formatUSDDisplay below half cent = %q, want %q", got, "$91.95")
	}
}

func TestPlanQuotaRemainingClampsDisplayValue(t *testing.T) {
	entitlement := core.UserPlanEntitlement{
		PeriodQuotaNanoUSD:  usdForWebTest(100),
		CurrentQuotaNanoUSD: -usdForWebTest(3),
	}
	if got := planQuotaRemaining(entitlement); got != 0 {
		t.Fatalf("planQuotaRemaining negative = %d, want 0", got)
	}
	if got := planQuotaUsed(entitlement); got != usdForWebTest(100) {
		t.Fatalf("planQuotaUsed negative = %d, want %d", got, usdForWebTest(100))
	}

	entitlement.CurrentQuotaNanoUSD = usdForWebTest(150)
	if got := planQuotaRemaining(entitlement); got != usdForWebTest(150) {
		t.Fatalf("planQuotaRemaining over base = %d, want %d", got, usdForWebTest(150))
	}
	if got := planCurrentQuotaLimit(entitlement); got != usdForWebTest(150) {
		t.Fatalf("planCurrentQuotaLimit over base = %d, want %d", got, usdForWebTest(150))
	}
}

func TestUsageBillingAmountTextKeepsSubCentPrecision(t *testing.T) {
	got := usageBillingAmountText(localeEN, core.BillingReservation{
		Status:        core.BillingRequestSettled,
		ActualNanoUSD: 5_000,
	})
	if got != "$0.000005" {
		t.Fatalf("usage amount = %q, want %q", got, "$0.000005")
	}

	got = usageBillingAmountText(localeEN, core.BillingReservation{
		Status:          core.BillingRequestReserved,
		ReservedNanoUSD: 5_000,
	})
	if got != "-" {
		t.Fatalf("pending usage amount = %q, want %q", got, "-")
	}

	got = usageBillingAmountText(localeEN, core.BillingReservation{
		Status:        core.BillingRequestReleased,
		ActualNanoUSD: 5_000,
	})
	if got != "not charged" {
		t.Fatalf("released usage amount = %q, want %q", got, "not charged")
	}

}

func usdForWebTest(value int64) int64 {
	return value * core.NanoUSDPerUSD
}

func TestUsageFirstTokenText(t *testing.T) {
	if got := usageFirstTokenText(core.BillingReservation{}); got != "-" {
		t.Fatalf("empty first token text = %q, want -", got)
	}
	if got := usageFirstTokenText(core.BillingReservation{FirstTokenMS: 237}); got != "237 ms" {
		t.Fatalf("first token text = %q, want 237 ms", got)
	}
	if got := usageFirstTokenText(core.BillingReservation{FirstTokenMS: 999}); got != "999 ms" {
		t.Fatalf("first token text = %q, want 999 ms", got)
	}
	if got := usageFirstTokenText(core.BillingReservation{FirstTokenMS: 1000}); got != "1 s" {
		t.Fatalf("first token text = %q, want 1 s", got)
	}
	if got := usageFirstTokenText(core.BillingReservation{FirstTokenMS: 13183}); got != "13.2 s" {
		t.Fatalf("first token text = %q, want 13.2 s", got)
	}
}

func TestModelPricingBreakdownSeparatesInputAndOutput(t *testing.T) {
	got := modelPricingBreakdown(core.ModelConfig{
		BillingMode:                  core.ModelBillingModeToken,
		InputPriceNanoUSDPer1M:       5 * core.NanoUSDPerUSD,
		CachedInputPriceNanoUSDPer1M: core.NanoUSDPerUSD / 2,
		CacheWritePriceNanoUSDPer1M:  0,
		OutputPriceNanoUSDPer1M:      30 * core.NanoUSDPerUSD,
		ImageOutputPriceNanoUSDPer1M: 0,
	})
	if got.Line1 != "in 5 / cache 0.5" || got.Line2 != "out 30 / image 0" {
		t.Fatalf("token pricing breakdown = %#v", got)
	}

	got = modelPricingBreakdown(core.ModelConfig{
		BillingMode:         core.ModelBillingModeRequest,
		RequestPriceNanoUSD: 25 * core.NanoUSDPerUSD,
	})
	if got.Line1 != "request" || got.Line2 != "$25 / request" {
		t.Fatalf("request pricing breakdown = %#v", got)
	}
}

func TestUsageAccountGroupLabelMarksPlanBillingOnly(t *testing.T) {
	got := usageAccountGroupLabel(localeEN, "Default", core.BillingReservation{BillingSource: core.ClientBillingSourcePlan})
	if got != "Default [T]" {
		t.Fatalf("plan account group label = %q, want %q", got, "Default [T]")
	}

	got = usageAccountGroupLabel(localeEN, "Default", core.BillingReservation{BillingSource: core.ClientBillingSourceCash})
	if got != "Default" {
		t.Fatalf("cash account group label = %q, want %q", got, "Default")
	}

	got = usageAccountGroupLabel(localeEN, "Default", core.BillingReservation{})
	if got != "Default" {
		t.Fatalf("default account group label = %q, want %q", got, "Default")
	}
}

func TestDefaultUsageLogDateRangeUsesLocalDay(t *testing.T) {
	oldLocal := time.Local
	time.Local = time.FixedZone("UTC+8", 8*60*60)
	defer func() { time.Local = oldLocal }()

	now := time.Date(2026, 6, 3, 7, 4, 5, 0, time.UTC)
	startedAt, endedAt := defaultUsageLogDateRange(now)

	wantStartedAt := time.Date(2026, 6, 3, 0, 0, 0, 0, time.Local).UTC()
	wantEndedAt := time.Date(2026, 6, 3, 23, 59, 59, 0, time.Local).UTC()
	if !startedAt.Equal(wantStartedAt) || !endedAt.Equal(wantEndedAt) {
		t.Fatalf("default range = %s..%s, want %s..%s", startedAt, endedAt, wantStartedAt, wantEndedAt)
	}
}

func TestDateTimeInputValuePreservesSeconds(t *testing.T) {
	oldLocal := time.Local
	time.Local = time.UTC
	defer func() { time.Local = oldLocal }()

	value := time.Date(2026, 6, 3, 23, 59, 59, 0, time.UTC)
	rendered := datetimeInputValue(value)
	if rendered != "2026-06-03T23:59:59" {
		t.Fatalf("datetimeInputValue = %q, want seconds preserved", rendered)
	}
	parsed := parseOptionalDateTime(rendered)
	if !parsed.Equal(value) {
		t.Fatalf("parseOptionalDateTime(%q) = %s, want %s", rendered, parsed, value)
	}
}

type recordingUsageLogRepository struct {
	*storage.MemoryRepository
	lastBillingQuery storage.BillingRequestQuery
}

func (r *recordingUsageLogRepository) ListBillingRequestsPage(query storage.BillingRequestQuery) ([]core.BillingReservation, int) {
	r.lastBillingQuery = query
	return r.MemoryRepository.ListBillingRequestsPage(query)
}

func TestUsageLogsPageDefaultsToAllVisibleHistory(t *testing.T) {
	repo := &recordingUsageLogRepository{MemoryRepository: storage.NewMemoryRepository()}
	control := controlplane.New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	admin, _, err := control.EnsureAdminUser("admin", "admin-secret")
	if err != nil {
		t.Fatalf("EnsureAdminUser returned error: %v", err)
	}
	if err := repo.SetUserBalance(admin.ID, core.NanoUSDPerUSD); err != nil {
		t.Fatalf("SetUserBalance returned error: %v", err)
	}
	client := core.APIClient{ID: "client_logs_page", Name: "Logs Page", APIKey: "gw_logs_page", OwnerUserID: admin.ID, Enabled: true}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	if _, err := repo.ReserveBilling(core.BillingReservationInput{
		RequestID:       "req_logs_page",
		ClientID:        client.ID,
		ClientName:      client.Name,
		UserID:          admin.ID,
		Model:           "gpt-4.1",
		ReservedNanoUSD: 100,
		Fingerprint:     "req_logs_page",
	}); err != nil {
		t.Fatalf("ReserveBilling returned error: %v", err)
	}
	if _, err := repo.SettleBilling(core.BillingSettlementInput{
		RequestID:     "req_logs_page",
		ClientID:      client.ID,
		Model:         "gpt-4.1",
		ActualNanoUSD: 100,
	}); err != nil {
		t.Fatalf("SettleBilling returned error: %v", err)
	}

	server := NewServer(control, nil, "data/state.db")
	handler := authenticatedUserHandler(t, control, admin, server.Handler())
	req := httptest.NewRequest(http.MethodGet, "/logs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Logs Page") {
		t.Fatalf("usage logs page body missing historical request: %s", rec.Body.String())
	}
	if !repo.lastBillingQuery.StartedAt.IsZero() || !repo.lastBillingQuery.EndedAt.IsZero() {
		t.Fatalf("default usage log query range = %s..%s, want no implicit time filter", repo.lastBillingQuery.StartedAt, repo.lastBillingQuery.EndedAt)
	}
}
