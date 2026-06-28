package controlplane

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/storage"
)

func TestUpsertBillingPlanGeneratesIDWhenCreating(t *testing.T) {
	service := New(storage.NewMemoryRepository(), nil)
	group, err := service.CreateBillingPlanGroup(BillingPlanGroupInput{Name: "Generated Plans"})
	if err != nil {
		t.Fatalf("CreateBillingPlanGroup returned error: %v", err)
	}

	first, err := service.UpsertBillingPlan(BillingPlanInput{
		Name:               "Week",
		Group:              group.ID,
		Enabled:            true,
		PriceNanoUSD:       10 * core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 100 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        7,
	})
	if err != nil {
		t.Fatalf("UpsertBillingPlan first returned error: %v", err)
	}
	second, err := service.UpsertBillingPlan(BillingPlanInput{
		Name:               "Month",
		Group:              group.ID,
		Enabled:            true,
		PriceNanoUSD:       50 * core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 100 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        30,
	})
	if err != nil {
		t.Fatalf("UpsertBillingPlan second returned error: %v", err)
	}
	if !strings.HasPrefix(first.ID, "plan_") || !strings.HasPrefix(second.ID, "plan_") {
		t.Fatalf("generated ids = %q, %q; want plan_ prefix", first.ID, second.ID)
	}
	if first.ID == second.ID {
		t.Fatalf("generated duplicate id %q", first.ID)
	}

	first.Name = "Updated Week"
	updated, err := service.UpsertBillingPlan(BillingPlanInput{
		ID:                 first.ID,
		Name:               first.Name,
		Group:              first.Group,
		Enabled:            true,
		PriceNanoUSD:       first.PriceNanoUSD,
		PeriodQuotaNanoUSD: first.PeriodQuotaNanoUSD,
		PeriodDurationSec:  first.PeriodDurationSec,
		PeriodCount:        first.PeriodCount,
	})
	if err != nil {
		t.Fatalf("UpsertBillingPlan update returned error: %v", err)
	}
	if updated.ID != first.ID || updated.Name != first.Name {
		t.Fatalf("updated plan = %#v, want id %q name %q", updated, first.ID, first.Name)
	}
}

func TestUpsertBillingPlanRejectsCallerProvidedIDWhenCreating(t *testing.T) {
	service := New(storage.NewMemoryRepository(), nil)
	group, err := service.CreateBillingPlanGroup(BillingPlanGroupInput{Name: "Custom Plans"})
	if err != nil {
		t.Fatalf("CreateBillingPlanGroup returned error: %v", err)
	}

	if _, err := service.UpsertBillingPlan(BillingPlanInput{
		ID:                 "custom_plan_id",
		Name:               "Custom",
		Group:              group.ID,
		Enabled:            true,
		PriceNanoUSD:       core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 10 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("UpsertBillingPlan error = %v, want ErrNotFound", err)
	}
}

func TestListBillingPlansHidesUnlistedPlansForUsers(t *testing.T) {
	service := New(storage.NewMemoryRepository(), nil)
	group, err := service.CreateBillingPlanGroup(BillingPlanGroupInput{Name: "Sale Plans"})
	if err != nil {
		t.Fatalf("CreateBillingPlanGroup returned error: %v", err)
	}
	listed, err := service.UpsertBillingPlan(BillingPlanInput{
		Name:               "Listed",
		Group:              group.ID,
		Enabled:            true,
		PriceNanoUSD:       core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 10 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	})
	if err != nil {
		t.Fatalf("UpsertBillingPlan listed returned error: %v", err)
	}
	unlisted, err := service.UpsertBillingPlan(BillingPlanInput{
		Name:               "Unlisted",
		Group:              group.ID,
		Enabled:            false,
		PriceNanoUSD:       core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 10 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	})
	if err != nil {
		t.Fatalf("UpsertBillingPlan unlisted returned error: %v", err)
	}

	userPlans := service.ListBillingPlans(false)
	if len(userPlans) != 1 || userPlans[0].ID != listed.ID {
		t.Fatalf("user plans = %#v, want only listed plan %q", userPlans, listed.ID)
	}
	adminPlans := service.ListBillingPlans(true)
	if len(adminPlans) != 2 {
		t.Fatalf("admin plans = %#v, want listed and unlisted plans including %q", adminPlans, unlisted.ID)
	}

	updatedGroup, err := service.UpdateBillingPlanGroup(group.ID, BillingPlanGroupInput{
		Name:         group.Name,
		SaleDisabled: true,
		SortOrder:    group.SortOrder,
	})
	if err != nil {
		t.Fatalf("UpdateBillingPlanGroup returned error: %v", err)
	}
	if !updatedGroup.SaleDisabled {
		t.Fatalf("updated group SaleDisabled = false, want true")
	}
	if userPlans := service.ListBillingPlans(false); len(userPlans) != 0 {
		t.Fatalf("user plans after group unlisted = %#v, want none", userPlans)
	}
	if adminPlans := service.ListBillingPlans(true); len(adminPlans) != 2 {
		t.Fatalf("admin plans after group unlisted = %#v, want two", adminPlans)
	}
}

func TestPurchaseBillingPlanRejectsUnlistedGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, nil)
	user := core.User{ID: "user_group_sale", Username: "user_group_sale", Enabled: true, BalanceNanoUSD: 20 * core.NanoUSDPerUSD}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	group, err := service.CreateBillingPlanGroup(BillingPlanGroupInput{Name: "Hidden Sale Plans"})
	if err != nil {
		t.Fatalf("CreateBillingPlanGroup returned error: %v", err)
	}
	plan, err := service.UpsertBillingPlan(BillingPlanInput{
		Name:               "Hidden Week",
		Group:              group.ID,
		Enabled:            true,
		PriceNanoUSD:       10 * core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 100 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        7,
	})
	if err != nil {
		t.Fatalf("UpsertBillingPlan returned error: %v", err)
	}
	if _, err := service.UpdateBillingPlanGroup(group.ID, BillingPlanGroupInput{Name: group.Name, SaleDisabled: true}); err != nil {
		t.Fatalf("UpdateBillingPlanGroup returned error: %v", err)
	}
	if _, err := service.PurchaseBillingPlan(user, plan.ID, core.BillingPlanPurchaseSeparate, ""); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("PurchaseBillingPlan hidden group error = %v, want ErrNotFound", err)
	}
}

func TestBillingPlanGroupsCreateSelectAndDelete(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, nil)

	groups := service.ListBillingPlanGroups()
	if len(groups) != 0 {
		t.Fatalf("initial groups = %#v, want none", groups)
	}
	custom, err := service.CreateBillingPlanGroup(BillingPlanGroupInput{Name: "VIP Cards", SortOrder: 15})
	if err != nil {
		t.Fatalf("CreateBillingPlanGroup returned error: %v", err)
	}
	if !strings.HasPrefix(custom.ID, "group_vip_cards") || custom.Name != "VIP Cards" {
		t.Fatalf("custom group = %#v", custom)
	}
	plan, err := service.UpsertBillingPlan(BillingPlanInput{
		Name:               "VIP Week",
		Group:              custom.ID,
		Enabled:            true,
		PriceNanoUSD:       10 * core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 100 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        7,
	})
	if err != nil {
		t.Fatalf("UpsertBillingPlan with custom group returned error: %v", err)
	}
	if plan.Group != custom.ID {
		t.Fatalf("plan group = %q, want %q", plan.Group, custom.ID)
	}
	if _, err := service.DeleteBillingPlanGroup(custom.ID); err == nil || !strings.Contains(err.Error(), "used") {
		t.Fatalf("DeleteBillingPlanGroup used error = %v, want used group rejection", err)
	}
	temporary, err := service.CreateBillingPlanGroup(BillingPlanGroupInput{ID: "group_temporary_cards", Name: "Temporary Cards"})
	if err != nil {
		t.Fatalf("CreateBillingPlanGroup temporary returned error: %v", err)
	}
	if deleted, err := service.DeleteBillingPlanGroup(temporary.ID); err != nil || deleted.ID != temporary.ID {
		t.Fatalf("DeleteBillingPlanGroup temporary returned group %#v error %v", deleted, err)
	}
	if _, err := service.UpsertBillingPlan(BillingPlanInput{
		Name:               "Missing Group",
		Enabled:            true,
		PriceNanoUSD:       core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 10 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("UpsertBillingPlan empty group error = %v, want required group rejection", err)
	}
	if _, err := service.UpsertBillingPlan(BillingPlanInput{
		Name:               "Missing Group",
		Group:              "group_missing",
		Enabled:            true,
		PriceNanoUSD:       core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 10 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); err == nil || !strings.Contains(err.Error(), "group") {
		t.Fatalf("UpsertBillingPlan missing group error = %v, want group rejection", err)
	}
}

func TestBillingPlanGroupQuotaPriceRatio(t *testing.T) {
	service := New(storage.NewMemoryRepository(), nil)
	group, err := service.CreateBillingPlanGroup(BillingPlanGroupInput{
		Name:            "Ratio Cards",
		QuotaPriceRatio: "1:0.80",
	})
	if err != nil {
		t.Fatalf("CreateBillingPlanGroup returned error: %v", err)
	}
	if group.QuotaPriceRatio != "1:0.8" {
		t.Fatalf("created ratio = %q, want %q", group.QuotaPriceRatio, "1:0.8")
	}
	updated, err := service.UpdateBillingPlanGroup(group.ID, BillingPlanGroupInput{
		Name:            group.Name,
		QuotaPriceRatio: "2:1.5",
		SortOrder:       group.SortOrder,
	})
	if err != nil {
		t.Fatalf("UpdateBillingPlanGroup returned error: %v", err)
	}
	if updated.QuotaPriceRatio != "2:1.5" {
		t.Fatalf("updated ratio = %q, want %q", updated.QuotaPriceRatio, "2:1.5")
	}
	if _, err := service.UpdateBillingPlanGroup(group.ID, BillingPlanGroupInput{
		Name:            group.Name,
		QuotaPriceRatio: "1:0",
	}); err == nil || !strings.Contains(err.Error(), "ratio") {
		t.Fatalf("invalid ratio update error = %v, want ratio error", err)
	}
}

func TestBillingPlanGroupsSortByCreatedAtWhenSortOrderMatches(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, nil)
	base := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	for index, group := range []core.BillingPlanGroup{
		{ID: "group_day", Name: "日卡"},
		{ID: "group_week", Name: "周卡"},
		{ID: "group_month", Name: "月卡"},
		{ID: "group_year", Name: "年卡"},
	} {
		group.CreatedAt = base.Add(time.Duration(index) * time.Minute)
		if err := repo.UpsertBillingPlanGroup(group); err != nil {
			t.Fatalf("UpsertBillingPlanGroup(%s) returned error: %v", group.ID, err)
		}
	}

	groups := service.ListBillingPlanGroups()
	got := make([]string, 0, len(groups))
	for _, group := range groups {
		got = append(got, group.Name)
	}
	want := []string{"日卡", "周卡", "月卡", "年卡"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("groups = %#v, want %#v", got, want)
	}
}

func TestGetClientQuotaDoesNotOffsetPlanQuotaWithNegativeBalance(t *testing.T) {
	repo := storage.NewMemoryRepository()
	wrapped := &negativeBalancePlanRepository{MemoryRepository: repo, userID: "user_negative_plan", balance: -5 * core.NanoUSDPerUSD}
	service := New(wrapped, nil)
	if err := repo.UpsertUser(core.User{ID: "user_negative_plan", Username: "negative", Enabled: true, BalanceNanoUSD: 20 * core.NanoUSDPerUSD}); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	cashClient := core.APIClient{ID: "client_negative_plan_cash", Name: "Negative Plan Cash", APIKey: "gw_negative_plan_cash", OwnerUserID: "user_negative_plan", Enabled: true, BillingSource: core.ClientBillingSourceCash}
	if err := repo.UpsertClient(cashClient); err != nil {
		t.Fatalf("UpsertClient cash returned error: %v", err)
	}
	planClient := core.APIClient{ID: "client_negative_plan", Name: "Negative Plan", APIKey: "gw_negative_plan", OwnerUserID: "user_negative_plan", Enabled: true, BillingSource: core.ClientBillingSourcePlan}
	if err := repo.UpsertClient(planClient); err != nil {
		t.Fatalf("UpsertClient plan returned error: %v", err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "negative_plan",
		Name:               "Negative Plan",
		Enabled:            true,
		PriceNanoUSD:       10 * core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 100 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        1,
	}); err != nil {
		t.Fatalf("UpsertBillingPlan returned error: %v", err)
	}
	if _, err := repo.PurchaseBillingPlan(core.BillingPlanPurchaseInput{UserID: "user_negative_plan", PlanID: "negative_plan"}); err != nil {
		t.Fatalf("PurchaseBillingPlan returned error: %v", err)
	}

	cashQuota, err := service.GetClientQuota(cashClient)
	if err != nil {
		t.Fatalf("GetClientQuota cash returned error: %v", err)
	}
	if cashQuota.BalanceNanoUSD != -5*core.NanoUSDPerUSD {
		t.Fatalf("cash balance = %d, want %d", cashQuota.BalanceNanoUSD, -5*core.NanoUSDPerUSD)
	}
	if cashQuota.PlanQuotaNanoUSD != 100*core.NanoUSDPerUSD {
		t.Fatalf("cash plan quota = %d, want %d", cashQuota.PlanQuotaNanoUSD, 100*core.NanoUSDPerUSD)
	}
	if cashQuota.RemainingNanoUSD != 0 {
		t.Fatalf("cash remaining = %d, want 0 for balance billing", cashQuota.RemainingNanoUSD)
	}

	quota, err := service.GetClientQuota(planClient)
	if err != nil {
		t.Fatalf("GetClientQuota plan returned error: %v", err)
	}
	if quota.BalanceNanoUSD != -5*core.NanoUSDPerUSD {
		t.Fatalf("balance = %d, want %d", quota.BalanceNanoUSD, -5*core.NanoUSDPerUSD)
	}
	if quota.PlanQuotaNanoUSD != 100*core.NanoUSDPerUSD {
		t.Fatalf("plan quota = %d, want %d", quota.PlanQuotaNanoUSD, 100*core.NanoUSDPerUSD)
	}
	if quota.RemainingNanoUSD != 100*core.NanoUSDPerUSD {
		t.Fatalf("remaining = %d, want full plan quota %d", quota.RemainingNanoUSD, 100*core.NanoUSDPerUSD)
	}
}

func TestGrantBillingPlanTargetsPaidUsers(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, nil)
	now := time.Now().UTC()

	if err := repo.UpsertBillingPlanGroup(core.BillingPlanGroup{ID: "group_paid_grant", Name: "Paid Grant"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertBillingPlan(core.BillingPlan{
		ID:                 "paid-grant-week",
		Name:               "Paid Grant Week",
		Group:              "group_paid_grant",
		Enabled:            true,
		PriceNanoUSD:       10 * core.NanoUSDPerUSD,
		PeriodQuotaNanoUSD: 100 * core.NanoUSDPerUSD,
		PeriodDurationSec:  24 * 60 * 60,
		PeriodCount:        7,
	}); err != nil {
		t.Fatal(err)
	}
	for _, user := range []core.User{
		{ID: "paid_enabled", Username: "paid-enabled", Enabled: true},
		{ID: "paid_disabled", Username: "paid-disabled", Enabled: false},
		{ID: "unpaid_enabled", Username: "unpaid-enabled", Enabled: true},
	} {
		if err := repo.UpsertUser(user); err != nil {
			t.Fatal(err)
		}
	}
	for _, order := range []core.PaymentOrder{
		{
			ID:            "pay_enabled_1",
			OutTradeNo:    "out_enabled_1",
			UserID:        "paid_enabled",
			Provider:      core.PaymentProviderAlipay,
			Channel:       core.PaymentChannelPage,
			AmountNanoUSD: core.NanoUSDPerUSD,
			Currency:      "USD",
			Status:        core.PaymentOrderPending,
			CreatedAt:     now,
		},
		{
			ID:            "pay_enabled_2",
			OutTradeNo:    "out_enabled_2",
			UserID:        "paid_enabled",
			Provider:      core.PaymentProviderAlipay,
			Channel:       core.PaymentChannelPage,
			AmountNanoUSD: 2 * core.NanoUSDPerUSD,
			Currency:      "USD",
			Status:        core.PaymentOrderPending,
			CreatedAt:     now,
		},
		{
			ID:            "pay_disabled",
			OutTradeNo:    "out_disabled",
			UserID:        "paid_disabled",
			Provider:      core.PaymentProviderAlipay,
			Channel:       core.PaymentChannelPage,
			AmountNanoUSD: core.NanoUSDPerUSD,
			Currency:      "USD",
			Status:        core.PaymentOrderPending,
			CreatedAt:     now,
		},
		{
			ID:            "pay_pending",
			OutTradeNo:    "out_pending",
			UserID:        "unpaid_enabled",
			Provider:      core.PaymentProviderAlipay,
			Channel:       core.PaymentChannelPage,
			AmountNanoUSD: core.NanoUSDPerUSD,
			Currency:      "USD",
			Status:        core.PaymentOrderPending,
			CreatedAt:     now,
		},
	} {
		if err := repo.CreatePaymentOrder(order); err != nil {
			t.Fatalf("CreatePaymentOrder(%s) returned error: %v", order.ID, err)
		}
	}
	for _, outTradeNo := range []string{"out_enabled_1", "out_disabled"} {
		if _, _, err := repo.CompletePaymentOrder(outTradeNo, "trade_"+outTradeNo, core.NanoUSDPerUSD, now); err != nil {
			t.Fatalf("CompletePaymentOrder(%s) returned error: %v", outTradeNo, err)
		}
	}
	if _, _, err := repo.CompletePaymentOrder("out_enabled_2", "trade_out_enabled_2", 2*core.NanoUSDPerUSD, now); err != nil {
		t.Fatalf("CompletePaymentOrder(out_enabled_2) returned error: %v", err)
	}

	result, err := service.GrantBillingPlan(BillingPlanGrantInput{
		PlanID:          "paid-grant-week",
		TargetPaidUsers: true,
	})
	if err != nil {
		t.Fatalf("GrantBillingPlan returned error: %v", err)
	}
	if result.TotalTargets != 1 || len(result.Granted) != 1 || result.Granted[0].Entitlement.UserID != "paid_enabled" {
		t.Fatalf("grant result = %#v, want only paid enabled user", result)
	}
	if got := repo.ListUserPlanEntitlements("paid_enabled"); len(got) != 1 {
		t.Fatalf("paid enabled entitlements len = %d, want 1: %#v", len(got), got)
	}
	if got := repo.ListUserPlanEntitlements("paid_disabled"); len(got) != 0 {
		t.Fatalf("paid disabled entitlements len = %d, want 0: %#v", len(got), got)
	}
	if got := repo.ListUserPlanEntitlements("unpaid_enabled"); len(got) != 0 {
		t.Fatalf("unpaid enabled entitlements len = %d, want 0: %#v", len(got), got)
	}
	deliveries := service.ListSiteMessages(core.User{ID: "paid_enabled", Username: "paid-enabled", Enabled: true})
	if len(deliveries) != 1 {
		t.Fatalf("paid enabled message deliveries len = %d, want 1: %#v", len(deliveries), deliveries)
	}
	if deliveries[0].Read || deliveries[0].Message.Title != "套餐赠送到账" || !strings.Contains(deliveries[0].Message.Body, "Paid Grant Week") {
		t.Fatalf("paid enabled message delivery = %#v, want unread plan grant notice", deliveries[0])
	}
	if got := service.ListSiteMessages(core.User{ID: "unpaid_enabled", Username: "unpaid-enabled", Enabled: true}); len(got) != 0 {
		t.Fatalf("unpaid enabled message deliveries len = %d, want 0: %#v", len(got), got)
	}
}

func TestPlanQuotaUsagePeriodSummariesNetReservedAndReturnedUsage(t *testing.T) {
	usdCents := func(cents int64) int64 {
		return cents * core.NanoUSDPerUSD / 100
	}
	start := time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC)
	entitlement := core.UserPlanEntitlement{
		ID:                     "ent_1",
		UserID:                 "user_1",
		PlanID:                 "plan_1",
		PlanName:               "Daily",
		Status:                 core.UserPlanEntitlementActive,
		PeriodQuotaNanoUSD:     usdCents(30000),
		BasePeriodQuotaNanoUSD: usdCents(30000),
		PeriodDurationSec:      int64((24 * time.Hour) / time.Second),
		TotalPeriods:           3,
		RemainingPeriods:       1,
		CurrentQuotaNanoUSD:    usdCents(30000),
		CurrentPeriodStartedAt: start,
		CurrentPeriodEndsAt:    start.Add(24 * time.Hour),
		PurchasedAt:            start.Add(-48 * time.Hour),
	}
	events := []storage.PlanQuotaUsageEvent{
		{Kind: "reset", GrantedNanoUSD: usdCents(30000), NetNanoUSD: usdCents(30000), CreatedAt: start.Add(-24 * time.Hour)},
		{Kind: "settle", UsedNanoUSD: usdCents(152341), NetNanoUSD: -usdCents(152341), CreatedAt: start.Add(-23 * time.Hour)},
		{Kind: "settle", ReturnedNanoUSD: usdCents(122307), NetNanoUSD: usdCents(122307), CreatedAt: start.Add(-22 * time.Hour)},
		{Kind: "reset", GrantedNanoUSD: usdCents(30000), NetNanoUSD: usdCents(30000), CreatedAt: start.Add(-48 * time.Hour)},
		{Kind: "settle", UsedNanoUSD: usdCents(67925), NetNanoUSD: -usdCents(67925), CreatedAt: start.Add(-47 * time.Hour)},
		{Kind: "settle", ReturnedNanoUSD: usdCents(57219), NetNanoUSD: usdCents(57219), CreatedAt: start.Add(-46 * time.Hour)},
	}

	rows := planQuotaUsagePeriodSummaries(entitlement, "alice", events)
	if len(rows) != 3 {
		t.Fatalf("rows len = %d, want 3: %#v", len(rows), rows)
	}

	assertRow := func(index int, granted, used, returned, expired, net int64) {
		t.Helper()
		row := rows[index]
		if row.GrantedNanoUSD != granted ||
			row.UsedNanoUSD != used ||
			row.ReturnedNanoUSD != returned ||
			row.ExpiredNanoUSD != expired ||
			row.NetNanoUSD != net {
			t.Fatalf("row %d = granted %d used %d returned %d expired %d net %d, want granted %d used %d returned %d expired %d net %d",
				index,
				row.GrantedNanoUSD, row.UsedNanoUSD, row.ReturnedNanoUSD, row.ExpiredNanoUSD, row.NetNanoUSD,
				granted, used, returned, expired, net)
		}
	}
	assertRow(0, usdCents(30000), 0, 0, 0, usdCents(30000))
	assertRow(1, usdCents(30000), usdCents(30034), usdCents(34), 0, 0)
	assertRow(2, usdCents(30000), usdCents(10706), 0, usdCents(19294), 0)

	stats := planQuotaUsageStatsFromSummaries(rows)
	if stats.GrantedNanoUSD != usdCents(90000) ||
		stats.UsedNanoUSD != usdCents(40740) ||
		stats.ReturnedNanoUSD != usdCents(34) ||
		stats.ExpiredNanoUSD != usdCents(19294) ||
		stats.NetNanoUSD != usdCents(30000) {
		t.Fatalf("stats = %#v, want granted 900 used 407.40 returned 0.34 expired 192.94 net 300", stats)
	}
}

func TestGetClientQuotaOwnerSpendLimitDoesNotCreateBalance(t *testing.T) {
	repo := storage.NewMemoryRepository()
	service := New(repo, nil)
	if err := repo.UpsertUser(core.User{ID: "user_zero_quota", Username: "zero", Enabled: true, BalanceNanoUSD: 0}); err != nil {
		t.Fatalf("UpsertUser returned error: %v", err)
	}
	client := core.APIClient{ID: "client_zero_quota", Name: "Zero Quota", APIKey: "gw_zero_quota", OwnerUserID: "user_zero_quota", Enabled: true, SpendLimitNanoUSD: 10 * core.NanoUSDPerUSD}
	if err := repo.UpsertClient(client); err != nil {
		t.Fatalf("UpsertClient returned error: %v", err)
	}
	quota, err := service.GetClientQuota(client)
	if err != nil {
		t.Fatalf("GetClientQuota returned error: %v", err)
	}
	if quota.RemainingNanoUSD != 0 {
		t.Fatalf("remaining = %d, want 0 when owner has no balance or plan", quota.RemainingNanoUSD)
	}

	ownerless := core.APIClient{ID: "client_ownerless_quota", Name: "Ownerless Quota", APIKey: "gw_ownerless_quota", Enabled: true, SpendLimitNanoUSD: 10 * core.NanoUSDPerUSD}
	if err := repo.UpsertClient(ownerless); err != nil {
		t.Fatalf("UpsertClient ownerless returned error: %v", err)
	}
	quota, err = service.GetClientQuota(ownerless)
	if err != nil {
		t.Fatalf("GetClientQuota ownerless returned error: %v", err)
	}
	if quota.RemainingNanoUSD != 10*core.NanoUSDPerUSD {
		t.Fatalf("ownerless remaining = %d, want spend limit", quota.RemainingNanoUSD)
	}
}

type negativeBalancePlanRepository struct {
	*storage.MemoryRepository
	userID  string
	balance int64
}

func (r *negativeBalancePlanRepository) GetUser(id string) (core.User, error) {
	user, err := r.MemoryRepository.GetUser(id)
	if err != nil {
		return core.User{}, err
	}
	if id == r.userID {
		user.BalanceNanoUSD = r.balance
	}
	return user, nil
}
