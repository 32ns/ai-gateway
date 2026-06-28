package controlplane

import (
	"testing"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

func TestEffectiveAccountGroupMultiplier(t *testing.T) {
	now := time.Date(2026, 5, 4, 23, 30, 0, 0, time.Local)
	group := core.AccountGroup{
		BillingMultiplierBps: 10000,
		TimedMultipliers: []core.AccountGroupTimedMultiplier{
			{ID: "disabled", Enabled: false, MultiplierBps: 50000},
			{ID: "weekday", Enabled: true, MultiplierBps: 15000, Weekdays: []int{1}, Priority: 1},
			{ID: "night", Enabled: true, MultiplierBps: 20000, StartTime: "22:00", EndTime: "02:00", Priority: 2},
		},
	}
	if got := core.EffectiveAccountGroupMultiplier(group, now); got != 20000 {
		t.Fatalf("EffectiveAccountGroupMultiplier = %d, want 20000", got)
	}
}

func TestActiveAccountGroupTimedMultiplier(t *testing.T) {
	now := time.Date(2026, 5, 4, 23, 30, 0, 0, time.Local)
	group := core.AccountGroup{
		BillingMultiplierBps: 10000,
		TimedMultipliers: []core.AccountGroupTimedMultiplier{
			{ID: "weekday", Name: "Weekday", Enabled: true, MultiplierBps: 15000, Weekdays: []int{1}, Priority: 1},
			{ID: "night", Name: "Night", Enabled: true, MultiplierBps: 20000, StartTime: "22:00", EndTime: "02:00", Priority: 2},
		},
	}
	rule, ok := core.ActiveAccountGroupTimedMultiplier(group, now)
	if !ok {
		t.Fatal("expected active timed multiplier")
	}
	if rule.ID != "night" || rule.Name != "Night" || rule.MultiplierBps != 20000 {
		t.Fatalf("active rule = %#v", rule)
	}
}

func TestEffectiveAccountGroupMultiplierFallsBack(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.Local)
	group := core.AccountGroup{
		BillingMultiplierBps: 12500,
		TimedMultipliers: []core.AccountGroupTimedMultiplier{
			{ID: "later", Enabled: true, MultiplierBps: 20000, StartDate: "2026-05-05"},
		},
	}
	if got := core.EffectiveAccountGroupMultiplier(group, now); got != 12500 {
		t.Fatalf("EffectiveAccountGroupMultiplier = %d, want 12500", got)
	}
}

func TestTimedMultiplierExpiredDoesNotDisableRecurringTimeWindow(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.Local)
	rule := core.AccountGroupTimedMultiplier{Enabled: true, MultiplierBps: 20000, StartTime: "09:00", EndTime: "10:00"}
	if core.TimedMultiplierExpired(rule, now) {
		t.Fatalf("daily time-window rule should not be permanently expired")
	}
}

func TestTimedMultiplierExpiredAfterEndDate(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.Local)
	rule := core.AccountGroupTimedMultiplier{Enabled: true, MultiplierBps: 20000, EndDate: "2026-05-03"}
	if !core.TimedMultiplierExpired(rule, now) {
		t.Fatalf("rule with past end date should be expired")
	}
}

func TestTimedMultiplierExpiredAfterEndDateTime(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 1, 0, 0, time.Local)
	rule := core.AccountGroupTimedMultiplier{Enabled: true, MultiplierBps: 20000, EndDate: "2026-05-04", StartTime: "09:00", EndTime: "12:00"}
	if !core.TimedMultiplierExpired(rule, now) {
		t.Fatalf("rule with past end date time should be expired")
	}
}

func TestUpdateAccountGroupBillingStoresTimedMultipliers(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	group, err := service.UpdateAccountGroupBilling("group_plus", 10000, 0, 0, 0, []core.AccountGroupTimedMultiplier{
		{ID: "peak", Name: "Peak", Enabled: true, MultiplierBps: 20000, Weekdays: []int{5, 1}, StartTime: "22:00", EndTime: "02:00"},
	})
	if err != nil {
		t.Fatalf("UpdateAccountGroupBilling: %v", err)
	}
	if len(group.TimedMultipliers) != 1 {
		t.Fatalf("timed multipliers = %d, want 1", len(group.TimedMultipliers))
	}
	if got := group.TimedMultipliers[0].Weekdays; len(got) != 2 || got[0] != 1 || got[1] != 5 {
		t.Fatalf("weekdays = %#v, want [1 5]", got)
	}
}

func TestUpdateAccountGroupBillingDisablesExpiredTimedMultiplier(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	group, err := service.UpdateAccountGroupBilling("group_plus", 10000, 0, 0, 0, []core.AccountGroupTimedMultiplier{
		{ID: "expired", Name: "Expired", Enabled: true, MultiplierBps: 20000, EndDate: "2000-01-01"},
	})
	if err != nil {
		t.Fatalf("UpdateAccountGroupBilling: %v", err)
	}
	if len(group.TimedMultipliers) != 1 {
		t.Fatalf("timed multipliers = %d, want 1", len(group.TimedMultipliers))
	}
	if group.TimedMultipliers[0].Enabled {
		t.Fatalf("expired timed multiplier should be disabled")
	}
}

func TestUserModelPricesUsesTimedMultiplier(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:                   "group_plus",
		Name:                 "Plus",
		BillingMultiplierBps: 10000,
		TimedMultipliers: []core.AccountGroupTimedMultiplier{
			{ID: "always", Enabled: true, MultiplierBps: 20000},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                     "gpt-test",
		Provider:               core.ProviderOpenAI,
		Enabled:                true,
		VisibleGroups:          []string{"Plus"},
		InputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}
	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	rows := service.UserModelPrices()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].InputPriceNanoUSDPer1M != 2*core.NanoUSDPerUSD {
		t.Fatalf("input price = %d, want %d", rows[0].InputPriceNanoUSDPer1M, 2*core.NanoUSDPerUSD)
	}
	if rows[0].AccountGroupMultiplierBps != 20000 {
		t.Fatalf("account group multiplier = %d, want 20000", rows[0].AccountGroupMultiplierBps)
	}
}

func TestEnabledModelPricesIncludesEnabledModelsForVisibleGroups(t *testing.T) {
	repo := storage.NewMemoryRepository()
	hide := false
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_hidden", Name: "Hidden", ShowInClientEditor: &hide}); err != nil {
		t.Fatal(err)
	}
	for _, model := range []core.ModelConfig{
		{ID: "default-model", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{core.DefaultAccountGroupName}},
		{ID: "hidden-model", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{"Hidden"}},
		{ID: "ungrouped-model", Provider: core.ProviderOpenAI, Enabled: true},
		{ID: "disabled-model", Provider: core.ProviderOpenAI, Enabled: false, VisibleGroups: []string{core.DefaultAccountGroupName}},
	} {
		if err := repo.UpsertModel(model); err != nil {
			t.Fatal(err)
		}
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	groupsByModel := map[string][]string{}
	for _, row := range service.EnabledModelPrices() {
		groupsByModel[row.ID] = append(groupsByModel[row.ID], row.AccountGroup)
	}
	if got := groupsByModel["default-model"]; len(got) != 1 || got[0] != core.DefaultAccountGroupName {
		t.Fatalf("default-model groups = %#v, want Default", got)
	}
	if got := groupsByModel["hidden-model"]; len(got) != 0 {
		t.Fatalf("hidden-model groups = %#v, want none for hidden client-editor group", got)
	}
	if got := groupsByModel["ungrouped-model"]; len(got) != 1 || got[0] != core.DefaultAccountGroupName {
		t.Fatalf("ungrouped-model groups = %#v, want Default fallback", got)
	}
	if got := groupsByModel["disabled-model"]; len(got) != 0 {
		t.Fatalf("disabled-model groups = %#v, want none", got)
	}
}

func TestEnabledModelPricesIncludesPrivateGroupsForVisibleUser(t *testing.T) {
	repo := storage.NewMemoryRepository()
	hide := false
	user := core.User{ID: "user_private_models", Username: "private_models", Enabled: true}
	other := core.User{ID: "user_other_models", Username: "other_models", Enabled: true}
	for _, item := range []core.User{user, other} {
		if err := repo.UpsertUser(item); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_private", Name: "Private", ShowInClientEditor: &hide, VisibleUserIDs: []string{user.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{ID: "private-model", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{"Private"}}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	if rows := service.EnabledModelPrices(); len(rows) != 0 {
		t.Fatalf("public enabled prices = %#v, want no private rows", rows)
	}
	rows := service.EnabledModelPricesForUser(user)
	if len(rows) != 1 || rows[0].ID != "private-model" || rows[0].AccountGroup != "Private" {
		t.Fatalf("visible user prices = %#v, want private model row", rows)
	}
	if rows := service.EnabledModelPricesForUser(other); len(rows) != 0 {
		t.Fatalf("other user prices = %#v, want no private rows", rows)
	}
}

func TestUserModelPricesBillingFixedIgnoresAccountGroupBilling(t *testing.T) {
	repo := storage.NewMemoryRepository()
	user := core.User{ID: "user_fixed_prices", Username: "fixed_prices", Enabled: true}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_fixed_prices", Name: "Plus Key", APIKey: "gw_fixed_prices", OwnerUserID: user.ID, Enabled: true, AccountGroup: "Plus"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:                           "group_plus",
		Name:                         "Plus",
		BillingMultiplierBps:         20000,
		InputPriceNanoUSDPer1M:       8 * core.NanoUSDPerUSD,
		CachedInputPriceNanoUSDPer1M: 4 * core.NanoUSDPerUSD,
		OutputPriceNanoUSDPer1M:      9 * core.NanoUSDPerUSD,
		TimedMultipliers: []core.AccountGroupTimedMultiplier{
			{ID: "always", Enabled: true, MultiplierBps: 30000},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                           "fixed-model",
		Provider:                     core.ProviderOpenAI,
		Enabled:                      true,
		VisibleGroups:                []string{"Plus"},
		BillingFixed:                 true,
		InputPriceNanoUSDPer1M:       core.NanoUSDPerUSD,
		CachedInputPriceNanoUSDPer1M: core.NanoUSDPerUSD / 4,
		OutputPriceNanoUSDPer1M:      2 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	rows := service.UserModelPricesForUser(user)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: %#v", len(rows), rows)
	}
	row := rows[0]
	if row.InputPriceNanoUSDPer1M != core.NanoUSDPerUSD {
		t.Fatalf("input price = %d, want %d", row.InputPriceNanoUSDPer1M, core.NanoUSDPerUSD)
	}
	if row.CachedInputPriceNanoUSDPer1M != core.NanoUSDPerUSD/4 {
		t.Fatalf("cached input price = %d, want %d", row.CachedInputPriceNanoUSDPer1M, core.NanoUSDPerUSD/4)
	}
	if row.OutputPriceNanoUSDPer1M != 2*core.NanoUSDPerUSD {
		t.Fatalf("output price = %d, want %d", row.OutputPriceNanoUSDPer1M, 2*core.NanoUSDPerUSD)
	}
	if row.AccountGroupMultiplierBps != core.AccountGroupDefaultMultiplierBps {
		t.Fatalf("fixed billing multiplier = %d, want %d", row.AccountGroupMultiplierBps, core.AccountGroupDefaultMultiplierBps)
	}
}

func TestUserModelPricesForDefaultClientUseClientBillingGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	user := core.User{ID: "user_default_prices", Username: "default_prices", Enabled: true}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_default_prices", Name: "Default Key", APIKey: "gw_default_prices", OwnerUserID: user.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:                   "default",
		BillingMultiplierBps: 25000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:                      "group_plus",
		Name:                    "Plus",
		BillingMultiplierBps:    40000,
		OutputPriceNanoUSDPer1M: 9 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                      "plus-model",
		Provider:                core.ProviderOpenAI,
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName},
		OutputPriceNanoUSDPer1M: 2 * core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	rows := service.UserModelPricesForUser(user)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: %#v", len(rows), rows)
	}
	row := rows[0]
	if row.AccountGroup != core.DefaultAccountGroupName {
		t.Fatalf("account group = %q, want default billing group", row.AccountGroup)
	}
	if row.OutputPriceNanoUSDPer1M != 5*core.NanoUSDPerUSD {
		t.Fatalf("output price = %d, want %d", row.OutputPriceNanoUSDPer1M, 5*core.NanoUSDPerUSD)
	}
	if row.AccountGroupMultiplierBps != 25000 {
		t.Fatalf("account group multiplier = %d, want 25000", row.AccountGroupMultiplierBps)
	}
}

func TestUserModelPricesForMultipleClientGroupsKeepSeparatePrices(t *testing.T) {
	repo := storage.NewMemoryRepository()
	user := core.User{ID: "user_multi_prices", Username: "multi_prices", Enabled: true}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_default_prices", Name: "Default Key", APIKey: "gw_default_prices", OwnerUserID: user.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_plus_prices", Name: "Plus Key", APIKey: "gw_plus_prices", OwnerUserID: user.ID, Enabled: true, AccountGroup: "Plus"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "default", BillingMultiplierBps: 20000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus", BillingMultiplierBps: 30000}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{
		ID:                      "plus-model",
		Provider:                core.ProviderOpenAI,
		Enabled:                 true,
		VisibleGroups:           []string{core.DefaultAccountGroupName, "Plus"},
		OutputPriceNanoUSDPer1M: core.NanoUSDPerUSD,
	}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	rows := service.UserModelPricesForUser(user)
	pricesByGroup := map[string]int64{}
	for _, row := range rows {
		if row.ID == "plus-model" {
			pricesByGroup[row.AccountGroup] = row.OutputPriceNanoUSDPer1M
		}
	}
	if pricesByGroup[core.DefaultAccountGroupName] != 2*core.NanoUSDPerUSD || pricesByGroup["Plus"] != 3*core.NanoUSDPerUSD {
		t.Fatalf("prices by billing group = %#v, want default 2x and Plus 3x", pricesByGroup)
	}
}

func TestUserModelPricesForUserUsesEnabledClientVisibleGroups(t *testing.T) {
	repo := storage.NewMemoryRepository()
	hide := false
	user := core.User{ID: "user_prices", Username: "prices", Enabled: true}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_hidden", Name: "Hidden", ShowInClientEditor: &hide}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_default", Name: "Default Key", APIKey: "gw_default", OwnerUserID: user.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_plus", Name: "Plus Key", APIKey: "gw_plus", OwnerUserID: user.ID, Enabled: true, AccountGroup: "Plus"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_hidden_disabled", Name: "Hidden Disabled", APIKey: "gw_hidden_disabled", OwnerUserID: user.ID, Enabled: false, AccountGroup: "Hidden"}); err != nil {
		t.Fatal(err)
	}
	for _, model := range []core.ModelConfig{
		{ID: "default-model", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{core.DefaultAccountGroupName}},
		{ID: "plus-model", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{"Plus"}},
		{ID: "hidden-model", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{"Hidden"}},
		{ID: "unavailable-model", Provider: core.ProviderOpenAI, Enabled: true},
	} {
		if err := repo.UpsertModel(model); err != nil {
			t.Fatal(err)
		}
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	rows := service.UserModelPricesForUser(user)
	ids := map[string]bool{}
	for _, row := range rows {
		ids[row.ID] = true
	}
	for _, want := range []string{"default-model", "plus-model"} {
		if !ids[want] {
			t.Fatalf("prices missing %q: %#v", want, rows)
		}
	}
	for _, unwanted := range []string{"hidden-model", "unavailable-model"} {
		if ids[unwanted] {
			t.Fatalf("prices leaked %q: %#v", unwanted, rows)
		}
	}

	if err := repo.UpsertClient(core.APIClient{ID: "client_hidden_enabled", Name: "Hidden Enabled", APIKey: "gw_hidden_enabled", OwnerUserID: user.ID, Enabled: true, AccountGroup: "Hidden"}); err != nil {
		t.Fatal(err)
	}
	rows = service.UserModelPricesForUser(user)
	ids = map[string]bool{}
	for _, row := range rows {
		ids[row.ID] = true
	}
	if !ids["hidden-model"] {
		t.Fatalf("explicit hidden-group key should reveal hidden model pricing: %#v", rows)
	}
}

func TestPublicModelPricesUsesDefaultVisibleGroup(t *testing.T) {
	repo := storage.NewMemoryRepository()
	if err := repo.UpsertAccountGroup(core.AccountGroup{ID: "group_plus", Name: "Plus"}); err != nil {
		t.Fatal(err)
	}
	for _, model := range []core.ModelConfig{
		{ID: "default-model", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{core.DefaultAccountGroupName}},
		{ID: "plus-model", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{"Plus"}},
	} {
		if err := repo.UpsertModel(model); err != nil {
			t.Fatal(err)
		}
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	groupsByModel := map[string][]string{}
	for _, row := range service.PublicModelPrices() {
		groupsByModel[row.ID] = append(groupsByModel[row.ID], row.AccountGroup)
	}
	if got := groupsByModel["default-model"]; len(got) != 1 || got[0] != core.DefaultAccountGroupName {
		t.Fatalf("default-model groups = %#v, want Default", got)
	}
	if got := groupsByModel["plus-model"]; len(got) != 0 {
		t.Fatalf("plus-model groups = %#v, want hidden from public Default prices", got)
	}
}

func TestDefaultClientDoesNotInheritHiddenGroupModelPricing(t *testing.T) {
	repo := storage.NewMemoryRepository()
	hide := false
	user := core.User{ID: "user_default_hidden", Username: "default_hidden", Enabled: true}
	if err := repo.UpsertUser(user); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertClient(core.APIClient{ID: "client_default_hidden", Name: "Default Key", APIKey: "gw_default_hidden", OwnerUserID: user.ID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertAccountGroup(core.AccountGroup{
		ID:                 "group_hidden",
		Name:               "Hidden",
		ShowInClientEditor: &hide,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertModel(core.ModelConfig{ID: "hidden-model", Provider: core.ProviderOpenAI, Enabled: true, VisibleGroups: []string{"Hidden"}}); err != nil {
		t.Fatal(err)
	}

	service := New(repo, providers.NewRegistry(&providers.OpenAIAdapter{}))
	for _, group := range service.NewClientEditor().AvailableAccountGroups {
		if group.Name == "Hidden" {
			t.Fatalf("hidden editor group leaked into new client editor: %#v", service.NewClientEditor().AvailableAccountGroups)
		}
	}
	rows := service.UserModelPricesForUser(user)
	if len(rows) != 0 {
		t.Fatalf("default client pricing rows = %#v, want no hidden-group rows", rows)
	}
}
