package controlplane

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/providers"
	"github.com/32ns/ai-gateway/internal/storage"
)

type ProviderOption struct {
	Kind  core.ProviderKind
	Label string
}

type ModelPage struct {
	Models           []core.ModelConfig
	ProviderSections []ModelProviderSection
	ActiveProvider   core.ProviderKind
	ActiveSection    ModelProviderSection
	Providers        []ProviderOption
	AccountGroups    []core.AccountGroup
	Stats            ModelStats
}

type ModelProviderSection struct {
	Provider core.ProviderKind
	Label    string
	Rows     []ModelRow
	Stats    ModelStats
}

type ModelRow struct {
	Index int
	Model core.ModelConfig
}

type ModelStats struct {
	Total    int
	Enabled  int
	Disabled int
}

type UserModelPriceRow struct {
	ID                            string
	DisplayName                   string
	Provider                      core.ProviderKind
	AccountGroup                  string
	AccountGroupMultiplierBps     int64
	BillingMode                   string
	InputPriceNanoUSDPer1M        int64
	CachedInputPriceNanoUSDPer1M  int64
	CacheWritePriceNanoUSDPer1M   int64
	CacheWrite5mPriceNanoUSDPer1M int64
	CacheWrite1hPriceNanoUSDPer1M int64
	OutputPriceNanoUSDPer1M       int64
	ImageOutputPriceNanoUSDPer1M  int64
	RequestPriceNanoUSD           int64
	TierCount                     int
}

type UserModelPriceSection struct {
	Provider core.ProviderKind
	Label    string
	Rows     []UserModelPriceRow
	Count    int
}

type ModelInput struct {
	ID                            string
	Provider                      core.ProviderKind
	Type                          string
	UpstreamID                    string
	DisplayName                   string
	OwnedBy                       string
	Enabled                       bool
	VisibleGroups                 []string
	BillingMode                   string
	BillingFixed                  bool
	InputPriceNanoUSDPer1M        int64
	CachedInputPriceNanoUSDPer1M  int64
	CacheWritePriceNanoUSDPer1M   int64
	CacheWrite5mPriceNanoUSDPer1M int64
	CacheWrite1hPriceNanoUSDPer1M int64
	OutputPriceNanoUSDPer1M       int64
	ImageOutputPriceNanoUSDPer1M  int64
	RequestPriceNanoUSD           int64
	PricingTiers                  []core.ModelPricingTier
}

type ModelSyncResult struct {
	Provider core.ProviderKind
	Imported int
	Updated  int
	Skipped  int
}

func (s *Service) ModelPage(ctx context.Context) ModelPage {
	return s.ModelPageForProvider(ctx, "")
}

func (s *Service) ModelPageForProvider(_ context.Context, activeProvider core.ProviderKind) ModelPage {
	models := s.repo.ListModels()
	providers := s.ProviderOptions()
	sections := modelProviderSections(models, providers)
	activeProvider, activeSection := activeModelProviderSection(sections, activeProvider)
	return ModelPage{
		Models:           models,
		ProviderSections: sections,
		ActiveProvider:   activeProvider,
		ActiveSection:    activeSection,
		Providers:        providers,
		AccountGroups:    accountGroupsWithDefault(s.repo.ListAccountGroups()),
		Stats:            modelStats(models),
	}
}

func modelProviderSections(models []core.ModelConfig, providers []ProviderOption) []ModelProviderSection {
	sections := make([]ModelProviderSection, 0, len(providers))
	sectionIndex := map[core.ProviderKind]int{}
	for _, provider := range providers {
		if provider.Kind == "" {
			continue
		}
		label := strings.TrimSpace(provider.Label)
		if label == "" {
			label = providerLabel(provider.Kind)
		}
		sectionIndex[provider.Kind] = len(sections)
		sections = append(sections, ModelProviderSection{
			Provider: provider.Kind,
			Label:    label,
		})
	}
	for index, model := range models {
		provider := model.Provider
		section, ok := sectionIndex[provider]
		if !ok {
			section = len(sections)
			sectionIndex[provider] = section
			sections = append(sections, ModelProviderSection{
				Provider: provider,
				Label:    providerLabel(provider),
			})
		}
		sections[section].Rows = append(sections[section].Rows, ModelRow{Index: index, Model: model})
		if model.Enabled {
			sections[section].Stats.Enabled++
		} else {
			sections[section].Stats.Disabled++
		}
		sections[section].Stats.Total++
	}
	return sections
}

func activeModelProviderSection(sections []ModelProviderSection, requested core.ProviderKind) (core.ProviderKind, ModelProviderSection) {
	requested = core.ProviderKind(strings.TrimSpace(string(requested)))
	if requested != "" {
		for _, section := range sections {
			if section.Provider == requested {
				return section.Provider, section
			}
		}
	}
	for _, section := range sections {
		if len(section.Rows) > 0 {
			return section.Provider, section
		}
	}
	if len(sections) > 0 {
		return sections[0].Provider, sections[0]
	}
	return "", ModelProviderSection{}
}

func modelStats(models []core.ModelConfig) ModelStats {
	stats := ModelStats{Total: len(models)}
	for _, model := range models {
		if model.Enabled {
			stats.Enabled++
		} else {
			stats.Disabled++
		}
	}
	return stats
}

func (s *Service) UserModelPrices() []UserModelPriceRow {
	return s.userModelPrices(nil)
}

func (s *Service) EnabledModelPrices() []UserModelPriceRow {
	return s.enabledModelPricesForUserID("")
}

func (s *Service) EnabledModelPricesForUser(user core.User) []UserModelPriceRow {
	if user.IsAdmin() {
		return s.UserModelPrices()
	}
	return s.enabledModelPricesForUserID(user.ID)
}

func (s *Service) enabledModelPricesForUserID(userID string) []UserModelPriceRow {
	accountGroups := accountGroupsByNormalizedName(s.repo.ListAccountGroups())
	models := s.repo.ListModels()
	now := time.Now()
	rows := make([]UserModelPriceRow, 0, len(models))
	seen := map[string]bool{}
	for _, model := range models {
		if !model.Enabled {
			continue
		}
		visibleGroups := uniqueNormalizedAccountGroups(model.VisibleGroups)
		if len(visibleGroups) == 0 {
			visibleGroups = []string{core.DefaultAccountGroupName}
		}
		for _, groupName := range visibleGroups {
			groupName = normalizeStoredAccountGroup(groupName)
			group := accountGroups[strings.ToLower(groupName)]
			if !accountGroupVisibleForModelPriceList(accountGroups, groupName, userID) {
				continue
			}
			if group.BillingMultiplierBps == 0 {
				group.BillingMultiplierBps = 10000
			}
			key := strings.ToLower(model.ID) + "\x00" + strings.ToLower(groupName)
			if seen[key] {
				continue
			}
			seen[key] = true
			rows = append(rows, userModelPriceRow(model, groupName, group, now))
		}
	}
	sortUserModelPriceRows(rows)
	return rows
}

func accountGroupVisibleForModelPriceList(accountGroups map[string]core.AccountGroup, groupName, userID string) bool {
	groupName = normalizeStoredAccountGroup(groupName)
	if strings.EqualFold(groupName, core.DefaultAccountGroupName) {
		return true
	}
	group, ok := accountGroups[strings.ToLower(groupName)]
	if !ok {
		return false
	}
	return core.AccountGroupVisibleInClientEditorForUser(group, userID)
}

func (s *Service) UserModelPricesForUser(user core.User) []UserModelPriceRow {
	if user.IsAdmin() {
		return s.UserModelPrices()
	}
	accesses := s.userModelPriceAccesses(user)
	if len(accesses) == 0 {
		return nil
	}
	return s.userModelPricesForAccesses(accesses)
}

func (s *Service) DashboardModelPricesForUser(user core.User) []UserModelPriceRow {
	if user.IsAdmin() {
		return s.UserModelPrices()
	}
	rows := s.PublicModelPrices()
	publicGroups := s.publicModelPriceGroups()
	hiddenAccesses := make([]userModelPriceAccess, 0)
	for _, access := range s.userModelPriceAccesses(user) {
		if _, ok := publicGroups[strings.ToLower(normalizeAccountGroup(access.billingGroup))]; ok {
			continue
		}
		hiddenAccesses = append(hiddenAccesses, access)
	}
	if len(hiddenAccesses) == 0 {
		return rows
	}
	rows = append(rows, s.userModelPricesForAccesses(hiddenAccesses)...)
	return dedupeAndSortUserModelPriceRows(rows)
}

func (s *Service) PublicModelPrices() []UserModelPriceRow {
	return s.userModelPrices(s.publicModelPriceGroups())
}

func (s *Service) userModelPrices(visibleGroupKeys map[string]struct{}) []UserModelPriceRow {
	accountGroups := accountGroupsByNormalizedName(s.repo.ListAccountGroups())
	models := s.repo.ListModels()
	now := time.Now()
	rows := make([]UserModelPriceRow, 0, len(models))
	seen := map[string]bool{}
	for _, model := range models {
		if !model.Enabled {
			continue
		}
		visibleGroups := model.VisibleGroups
		for _, groupName := range visibleGroups {
			groupName = normalizeStoredAccountGroup(groupName)
			if visibleGroupKeys != nil {
				if _, ok := visibleGroupKeys[strings.ToLower(groupName)]; !ok {
					continue
				}
			}
			group := accountGroups[strings.ToLower(groupName)]
			if group.BillingMultiplierBps == 0 {
				group.BillingMultiplierBps = 10000
			}
			key := strings.ToLower(model.ID) + "\x00" + strings.ToLower(groupName)
			if seen[key] {
				continue
			}
			seen[key] = true
			rows = append(rows, userModelPriceRow(model, groupName, group, now))
		}
	}
	sortUserModelPriceRows(rows)
	return rows
}

type userModelPriceAccess struct {
	billingGroup  string
	visibleGroups []string
}

func (s *Service) userModelPriceAccesses(user core.User) []userModelPriceAccess {
	if strings.TrimSpace(user.ID) == "" {
		return nil
	}
	out := make([]userModelPriceAccess, 0)
	seenBillingGroups := map[string]struct{}{}
	for _, client := range s.ClientsForUser(user) {
		if !client.Enabled {
			continue
		}
		billingGroup := normalizeStoredAccountGroup(client.AccountGroup)
		visibleGroups := []string{billingGroup}
		visibleGroups = uniqueNormalizedAccountGroups(visibleGroups)
		if len(visibleGroups) == 0 {
			continue
		}
		key := strings.ToLower(billingGroup)
		if _, ok := seenBillingGroups[key]; ok {
			continue
		}
		seenBillingGroups[key] = struct{}{}
		out = append(out, userModelPriceAccess{
			billingGroup:  billingGroup,
			visibleGroups: visibleGroups,
		})
	}
	return out
}

func (s *Service) userModelPricesForAccesses(accesses []userModelPriceAccess) []UserModelPriceRow {
	accountGroups := accountGroupsByNormalizedName(s.repo.ListAccountGroups())
	models := s.repo.ListModels()
	now := time.Now()
	rows := make([]UserModelPriceRow, 0, len(models))
	seen := map[string]bool{}
	for _, model := range models {
		if !model.Enabled {
			continue
		}
		for _, access := range accesses {
			if !modelVisibleToAnyGroup(model, access.visibleGroups) {
				continue
			}
			groupName := normalizeStoredAccountGroup(access.billingGroup)
			group := accountGroups[strings.ToLower(groupName)]
			if group.BillingMultiplierBps == 0 {
				group.BillingMultiplierBps = 10000
			}
			key := strings.ToLower(model.ID) + "\x00" + strings.ToLower(groupName)
			if seen[key] {
				continue
			}
			seen[key] = true
			rows = append(rows, userModelPriceRow(model, groupName, group, now))
		}
	}
	sortUserModelPriceRows(rows)
	return rows
}

func (s *Service) publicModelPriceGroups() map[string]struct{} {
	return map[string]struct{}{strings.ToLower(core.DefaultAccountGroupName): {}}
}

func sortUserModelPriceRows(rows []UserModelPriceRow) {
	slices.SortFunc(rows, func(a, b UserModelPriceRow) int {
		if strings.EqualFold(a.ID, b.ID) {
			return strings.Compare(strings.ToLower(a.AccountGroup), strings.ToLower(b.AccountGroup))
		}
		return strings.Compare(strings.ToLower(a.ID), strings.ToLower(b.ID))
	})
}

func dedupeAndSortUserModelPriceRows(rows []UserModelPriceRow) []UserModelPriceRow {
	if len(rows) == 0 {
		return nil
	}
	out := make([]UserModelPriceRow, 0, len(rows))
	seen := map[string]struct{}{}
	for _, row := range rows {
		key := strings.ToLower(strings.TrimSpace(row.ID)) + "\x00" + strings.ToLower(normalizeAccountGroup(row.AccountGroup))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, row)
	}
	sortUserModelPriceRows(out)
	return out
}

func UserModelPriceSections(rows []UserModelPriceRow) []UserModelPriceSection {
	if len(rows) == 0 {
		return nil
	}
	sectionByProvider := map[core.ProviderKind]int{}
	sections := make([]UserModelPriceSection, 0, 4)
	for _, row := range rows {
		provider := row.Provider
		sectionIndex, ok := sectionByProvider[provider]
		if !ok {
			label := strings.TrimSpace(providerLabel(provider))
			if label == "" {
				label = strings.TrimSpace(string(provider))
			}
			if label == "" {
				label = "Unknown"
			}
			sectionIndex = len(sections)
			sectionByProvider[provider] = sectionIndex
			sections = append(sections, UserModelPriceSection{
				Provider: provider,
				Label:    label,
			})
		}
		sections[sectionIndex].Rows = append(sections[sectionIndex].Rows, row)
	}
	for index := range sections {
		sortUserModelPriceRows(sections[index].Rows)
		sections[index].Count = len(sections[index].Rows)
	}
	slices.SortFunc(sections, func(a, b UserModelPriceSection) int {
		aRank := providerSortRank(a.Provider)
		bRank := providerSortRank(b.Provider)
		if aRank < bRank {
			return -1
		}
		if aRank > bRank {
			return 1
		}
		return strings.Compare(strings.ToLower(a.Label), strings.ToLower(b.Label))
	})
	return sections
}

func (s *Service) ProviderOptions() []ProviderOption {
	return []ProviderOption{
		{Kind: core.ProviderOpenAI, Label: providerLabel(core.ProviderOpenAI)},
		{Kind: core.ProviderClaude, Label: providerLabel(core.ProviderClaude)},
	}
}

func accountGroupsByNormalizedName(groups []core.AccountGroup) map[string]core.AccountGroup {
	out := make(map[string]core.AccountGroup, len(groups)+1)
	for _, group := range accountGroupsWithDefault(groups) {
		name := normalizeAccountGroup(group.Name)
		out[strings.ToLower(name)] = group
	}
	return out
}

func userModelPriceRow(model core.ModelConfig, groupName string, group core.AccountGroup, now time.Time) UserModelPriceRow {
	if model.BillingFixed {
		group = core.AccountGroup{BillingMultiplierBps: core.AccountGroupDefaultMultiplierBps}
	}
	multiplierBps := core.EffectiveAccountGroupMultiplier(group, now)
	row := UserModelPriceRow{
		ID:                        model.ID,
		DisplayName:               model.DisplayName,
		Provider:                  model.Provider,
		AccountGroup:              groupName,
		AccountGroupMultiplierBps: multiplierBps,
		BillingMode:               normalizeModelBillingModeForPriceList(model.BillingMode),
		TierCount:                 len(model.PricingTiers),
	}
	switch row.BillingMode {
	case core.ModelBillingModeRequest:
		row.RequestPriceNanoUSD = applyBillingMultiplier(model.RequestPriceNanoUSD, multiplierBps)
	case core.ModelBillingModeTieredExpr:
		if tier, ok := highestModelPricingTier(model.PricingTiers); ok {
			row.InputPriceNanoUSDPer1M = effectiveTokenPrice(tier.InputPriceNanoUSD, group.InputPriceNanoUSDPer1M, multiplierBps)
			row.CachedInputPriceNanoUSDPer1M = effectiveTokenPrice(tier.CachedInputPriceNanoUSD, group.CachedInputPriceNanoUSDPer1M, multiplierBps)
			row.CacheWritePriceNanoUSDPer1M = effectiveTokenPrice(tier.CacheWritePriceNanoUSD, group.CacheWritePriceNanoUSDPer1M, multiplierBps)
			row.CacheWrite5mPriceNanoUSDPer1M = effectiveTokenPrice(tier.CacheWrite5mPriceNanoUSD, group.CacheWrite5mPriceNanoUSDPer1M, multiplierBps)
			row.CacheWrite1hPriceNanoUSDPer1M = effectiveTokenPrice(tier.CacheWrite1hPriceNanoUSD, group.CacheWrite1hPriceNanoUSDPer1M, multiplierBps)
			row.OutputPriceNanoUSDPer1M = effectiveTokenPrice(tier.OutputPriceNanoUSD, group.OutputPriceNanoUSDPer1M, multiplierBps)
			row.ImageOutputPriceNanoUSDPer1M = effectiveTokenPrice(tier.ImageOutputPriceNanoUSD, group.ImageOutputPriceNanoUSDPer1M, multiplierBps)
		}
	default:
		row.InputPriceNanoUSDPer1M = effectiveTokenPrice(model.InputPriceNanoUSDPer1M, group.InputPriceNanoUSDPer1M, multiplierBps)
		row.CachedInputPriceNanoUSDPer1M = effectiveTokenPrice(model.CachedInputPriceNanoUSDPer1M, group.CachedInputPriceNanoUSDPer1M, multiplierBps)
		row.CacheWritePriceNanoUSDPer1M = effectiveTokenPrice(model.CacheWritePriceNanoUSDPer1M, group.CacheWritePriceNanoUSDPer1M, multiplierBps)
		row.CacheWrite5mPriceNanoUSDPer1M = effectiveTokenPrice(model.CacheWrite5mPriceNanoUSDPer1M, group.CacheWrite5mPriceNanoUSDPer1M, multiplierBps)
		row.CacheWrite1hPriceNanoUSDPer1M = effectiveTokenPrice(model.CacheWrite1hPriceNanoUSDPer1M, group.CacheWrite1hPriceNanoUSDPer1M, multiplierBps)
		row.OutputPriceNanoUSDPer1M = effectiveTokenPrice(model.OutputPriceNanoUSDPer1M, group.OutputPriceNanoUSDPer1M, multiplierBps)
		row.ImageOutputPriceNanoUSDPer1M = effectiveTokenPrice(model.ImageOutputPriceNanoUSDPer1M, group.ImageOutputPriceNanoUSDPer1M, multiplierBps)
	}
	return row
}

func normalizeModelBillingModeForPriceList(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case core.ModelBillingModeRequest:
		return core.ModelBillingModeRequest
	case core.ModelBillingModeTieredExpr:
		return core.ModelBillingModeTieredExpr
	default:
		return core.ModelBillingModeToken
	}
}

func highestModelPricingTier(tiers []core.ModelPricingTier) (core.ModelPricingTier, bool) {
	if len(tiers) == 0 {
		return core.ModelPricingTier{}, false
	}
	best := tiers[0]
	for _, tier := range tiers[1:] {
		if tier.InputPriceNanoUSD > best.InputPriceNanoUSD {
			best.InputPriceNanoUSD = tier.InputPriceNanoUSD
		}
		if tier.CachedInputPriceNanoUSD > best.CachedInputPriceNanoUSD {
			best.CachedInputPriceNanoUSD = tier.CachedInputPriceNanoUSD
		}
		if tier.CacheWritePriceNanoUSD > best.CacheWritePriceNanoUSD {
			best.CacheWritePriceNanoUSD = tier.CacheWritePriceNanoUSD
		}
		if tier.CacheWrite5mPriceNanoUSD > best.CacheWrite5mPriceNanoUSD {
			best.CacheWrite5mPriceNanoUSD = tier.CacheWrite5mPriceNanoUSD
		}
		if tier.CacheWrite1hPriceNanoUSD > best.CacheWrite1hPriceNanoUSD {
			best.CacheWrite1hPriceNanoUSD = tier.CacheWrite1hPriceNanoUSD
		}
		if tier.OutputPriceNanoUSD > best.OutputPriceNanoUSD {
			best.OutputPriceNanoUSD = tier.OutputPriceNanoUSD
		}
		if tier.ImageOutputPriceNanoUSD > best.ImageOutputPriceNanoUSD {
			best.ImageOutputPriceNanoUSD = tier.ImageOutputPriceNanoUSD
		}
	}
	return best, true
}

func effectiveTokenPrice(modelPrice, groupOverride, multiplierBps int64) int64 {
	if groupOverride > 0 {
		modelPrice = groupOverride
	}
	return applyBillingMultiplier(modelPrice, multiplierBps)
}

func applyBillingMultiplier(value, multiplierBps int64) int64 {
	if multiplierBps == 0 {
		multiplierBps = 10000
	}
	if value <= 0 || multiplierBps <= 0 {
		return 0
	}
	const maxInt64 = int64(^uint64(0) >> 1)
	if value > maxInt64/multiplierBps {
		return maxInt64
	}
	product := value * multiplierBps
	if product > maxInt64-(10000-1) {
		return maxInt64
	}
	return (product + 10000 - 1) / 10000
}

func (s *Service) CreateModel(input ModelInput) (core.ModelConfig, error) {
	model, err := s.normalizeModelInput(input)
	if err != nil {
		return core.ModelConfig{}, err
	}
	model.Source = core.ModelSourceManual
	if existing, err := s.repo.GetModel(model.ID); err == nil {
		model.CreatedAt = existing.CreatedAt
		if existing.Source != "" {
			model.Source = existing.Source
		}
		model.LastSyncedAt = cloneTimePtr(existing.LastSyncedAt)
	}
	if err := s.repo.UpsertModel(model); err != nil {
		return core.ModelConfig{}, err
	}
	return s.repo.GetModel(model.ID)
}

func (s *Service) ToggleModel(id string) (core.ModelConfig, error) {
	model, err := s.repo.GetModel(strings.TrimSpace(id))
	if err != nil {
		return core.ModelConfig{}, err
	}
	model.Enabled = !model.Enabled
	if err := s.repo.UpsertModel(model); err != nil {
		return core.ModelConfig{}, err
	}
	return s.repo.GetModel(model.ID)
}

func (s *Service) UpdateModelPricing(id string, billingMode string, billingFixed bool, inputPriceNanoUSDPer1M, cachedInputPriceNanoUSDPer1M, cacheWritePriceNanoUSDPer1M, cacheWrite5mPriceNanoUSDPer1M, cacheWrite1hPriceNanoUSDPer1M, outputPriceNanoUSDPer1M, imageOutputPriceNanoUSDPer1M, requestPriceNanoUSD int64, pricingTiers []core.ModelPricingTier) (core.ModelConfig, error) {
	model, err := s.repo.GetModel(strings.TrimSpace(id))
	if err != nil {
		return core.ModelConfig{}, err
	}
	if inputPriceNanoUSDPer1M < 0 || cachedInputPriceNanoUSDPer1M < 0 || cacheWritePriceNanoUSDPer1M < 0 || cacheWrite5mPriceNanoUSDPer1M < 0 || cacheWrite1hPriceNanoUSDPer1M < 0 || outputPriceNanoUSDPer1M < 0 || imageOutputPriceNanoUSDPer1M < 0 || requestPriceNanoUSD < 0 {
		return core.ModelConfig{}, fmt.Errorf("model prices must be zero or greater")
	}
	mode := normalizeBillingMode(billingMode)
	tiers, err := normalizeModelPricingTiers(pricingTiers)
	if err != nil {
		return core.ModelConfig{}, err
	}
	if mode == core.ModelBillingModeTieredExpr && len(tiers) == 0 {
		return core.ModelConfig{}, fmt.Errorf("tiered billing requires at least one tier")
	}
	model.InputPriceNanoUSDPer1M = inputPriceNanoUSDPer1M
	model.CachedInputPriceNanoUSDPer1M = cachedInputPriceNanoUSDPer1M
	model.CacheWritePriceNanoUSDPer1M = cacheWritePriceNanoUSDPer1M
	model.CacheWrite5mPriceNanoUSDPer1M = cacheWrite5mPriceNanoUSDPer1M
	model.CacheWrite1hPriceNanoUSDPer1M = cacheWrite1hPriceNanoUSDPer1M
	model.OutputPriceNanoUSDPer1M = outputPriceNanoUSDPer1M
	model.ImageOutputPriceNanoUSDPer1M = imageOutputPriceNanoUSDPer1M
	model.RequestPriceNanoUSD = requestPriceNanoUSD
	model.BillingMode = mode
	model.BillingFixed = billingFixed
	model.PricingTiers = tiers
	if err := s.repo.UpsertModel(model); err != nil {
		return core.ModelConfig{}, err
	}
	return s.repo.GetModel(model.ID)
}

func (s *Service) UpdateModelVisibleGroups(id string, visibleGroups []string) (core.ModelConfig, error) {
	model, err := s.repo.GetModel(strings.TrimSpace(id))
	if err != nil {
		return core.ModelConfig{}, err
	}
	groups, err := s.normalizeModelVisibleGroups(visibleGroups)
	if err != nil {
		return core.ModelConfig{}, err
	}
	model.VisibleGroups = groups
	if err := s.repo.UpsertModel(model); err != nil {
		return core.ModelConfig{}, err
	}
	return s.repo.GetModel(model.ID)
}

func (s *Service) DeleteModel(id string) (core.ModelConfig, error) {
	id = strings.TrimSpace(id)
	model, err := s.repo.GetModel(id)
	if err != nil {
		return core.ModelConfig{}, err
	}
	if err := s.repo.DeleteModel(id); err != nil {
		return core.ModelConfig{}, err
	}
	return model, nil
}

func (s *Service) SyncProviderModels(ctx context.Context, provider core.ProviderKind) (ModelSyncResult, error) {
	provider = core.ProviderKind(strings.TrimSpace(string(provider)))
	result := ModelSyncResult{Provider: provider}
	if s.providers == nil {
		return result, fmt.Errorf("provider registry is not configured")
	}
	adapter, ok := s.providers.Get(provider)
	if !ok {
		return result, fmt.Errorf("provider %q is not registered", provider)
	}
	listing, ok := adapter.(providers.ModelListingAdapter)
	if !ok {
		return result, fmt.Errorf("provider %q does not support model sync", provider)
	}

	upstreamModels, err := s.fetchProviderModelsWithAvailableAccount(ctx, provider, adapter, listing)
	if err != nil {
		return result, err
	}

	now := time.Now().UTC()
	for _, upstream := range upstreamModels {
		modelID := strings.TrimSpace(upstream.ID)
		if modelID == "" {
			result.Skipped++
			continue
		}
		model := core.ModelConfig{
			ID:            modelID,
			Provider:      provider,
			Type:          core.InferModelType(modelID),
			UpstreamID:    modelID,
			DisplayName:   strings.TrimSpace(upstream.DisplayName),
			OwnedBy:       strings.TrimSpace(upstream.OwnedBy),
			Source:        core.ModelSourceUpstream,
			Enabled:       false,
			VisibleGroups: s.defaultVisibleGroupsForProvider(provider),
			LastSyncedAt:  &now,
		}
		if existing, err := s.repo.GetModel(modelID); err == nil {
			if existing.Provider != "" && existing.Provider != provider {
				result.Skipped++
				continue
			}
			model.Enabled = existing.Enabled
			model.CreatedAt = existing.CreatedAt
			if strings.TrimSpace(existing.UpstreamID) != "" {
				model.UpstreamID = existing.UpstreamID
			}
			if strings.TrimSpace(model.DisplayName) == "" {
				model.DisplayName = existing.DisplayName
			}
			if strings.TrimSpace(model.OwnedBy) == "" {
				model.OwnedBy = existing.OwnedBy
			}
			model.Type = core.NormalizeModelType(existing.Type, modelID)
			model.InputPriceNanoUSDPer1M = existing.InputPriceNanoUSDPer1M
			model.CachedInputPriceNanoUSDPer1M = existing.CachedInputPriceNanoUSDPer1M
			model.CacheWritePriceNanoUSDPer1M = existing.CacheWritePriceNanoUSDPer1M
			model.CacheWrite5mPriceNanoUSDPer1M = existing.CacheWrite5mPriceNanoUSDPer1M
			model.CacheWrite1hPriceNanoUSDPer1M = existing.CacheWrite1hPriceNanoUSDPer1M
			model.OutputPriceNanoUSDPer1M = existing.OutputPriceNanoUSDPer1M
			model.ImageOutputPriceNanoUSDPer1M = existing.ImageOutputPriceNanoUSDPer1M
			model.RequestPriceNanoUSD = existing.RequestPriceNanoUSD
			model.BillingMode = existing.BillingMode
			model.BillingFixed = existing.BillingFixed
			if existing.VisibleGroups != nil {
				model.VisibleGroups = append([]string{}, existing.VisibleGroups...)
			}
			model.PricingTiers = append([]core.ModelPricingTier(nil), existing.PricingTiers...)
			result.Updated++
		} else if err == storage.ErrNotFound {
			result.Imported++
		} else {
			return result, err
		}
		if err := s.repo.UpsertModel(model); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *Service) fetchProviderModelsWithAvailableAccount(ctx context.Context, provider core.ProviderKind, adapter providers.Adapter, listing providers.ModelListingAdapter) ([]providers.UpstreamModel, error) {
	accounts := s.modelSyncAccounts(provider)
	if len(accounts) == 0 {
		return nil, fmt.Errorf("provider %q has no available account for model sync", provider)
	}

	var lastErr error
	for _, account := range accounts {
		if refresher, ok := adapter.(providers.RefreshingAdapter); ok && providers.CredentialNeedsRefresh(account) {
			credential, err := refresher.Refresh(ctx, account)
			if err != nil {
				lastErr = err
				continue
			}
			account.Credential = credential
			if err := s.repo.UpsertAccount(account); err != nil {
				return nil, err
			}
		}
		models, err := listing.FetchModels(ctx, account)
		if err == nil {
			return models, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, modelSyncError(provider, len(accounts), lastErr)
	}
	return nil, fmt.Errorf("provider %q model sync failed", provider)
}

func (s *Service) defaultVisibleGroupsForProvider(provider core.ProviderKind) []string {
	out := make([]string, 0)
	seen := map[string]struct{}{}
	for _, group := range accountGroupsWithDefault(s.repo.ListAccountGroups()) {
		if !core.AccountGroupTypeAllowsProvider(group, provider) {
			continue
		}
		if core.NormalizeAccountGroupType(group.Type) == core.AccountGroupTypeMixed {
			continue
		}
		name := normalizeAccountGroup(group.Name)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, name)
	}
	return out
}

func (s *Service) normalizeModelInput(input ModelInput) (core.ModelConfig, error) {
	id := strings.TrimSpace(input.ID)
	if id == "" {
		return core.ModelConfig{}, fmt.Errorf("model id is required")
	}
	if strings.ContainsAny(id, " \t\r\n") {
		return core.ModelConfig{}, fmt.Errorf("model id cannot contain whitespace")
	}
	provider := core.ProviderKind(strings.TrimSpace(string(input.Provider)))
	if s.providers == nil {
		return core.ModelConfig{}, fmt.Errorf("provider registry is not configured")
	}
	if _, ok := s.providers.Get(provider); !ok {
		return core.ModelConfig{}, fmt.Errorf("provider %q is not registered", provider)
	}
	upstreamID := strings.TrimSpace(input.UpstreamID)
	if upstreamID == "" {
		upstreamID = id
	}
	if input.InputPriceNanoUSDPer1M < 0 || input.CachedInputPriceNanoUSDPer1M < 0 || input.CacheWritePriceNanoUSDPer1M < 0 || input.CacheWrite5mPriceNanoUSDPer1M < 0 || input.CacheWrite1hPriceNanoUSDPer1M < 0 || input.OutputPriceNanoUSDPer1M < 0 || input.ImageOutputPriceNanoUSDPer1M < 0 || input.RequestPriceNanoUSD < 0 {
		return core.ModelConfig{}, fmt.Errorf("model prices must be zero or greater")
	}
	mode := normalizeBillingMode(input.BillingMode)
	tiers, err := normalizeModelPricingTiers(input.PricingTiers)
	if err != nil {
		return core.ModelConfig{}, err
	}
	visibleGroups, err := s.normalizeModelVisibleGroups(input.VisibleGroups)
	if err != nil {
		return core.ModelConfig{}, err
	}
	if mode == core.ModelBillingModeTieredExpr && len(tiers) == 0 {
		return core.ModelConfig{}, fmt.Errorf("tiered billing requires at least one tier")
	}
	return core.ModelConfig{
		ID:                            id,
		Provider:                      provider,
		Type:                          core.NormalizeModelType(input.Type, id),
		UpstreamID:                    upstreamID,
		DisplayName:                   strings.TrimSpace(input.DisplayName),
		OwnedBy:                       strings.TrimSpace(input.OwnedBy),
		Enabled:                       input.Enabled,
		VisibleGroups:                 visibleGroups,
		BillingMode:                   mode,
		BillingFixed:                  input.BillingFixed,
		InputPriceNanoUSDPer1M:        input.InputPriceNanoUSDPer1M,
		CachedInputPriceNanoUSDPer1M:  input.CachedInputPriceNanoUSDPer1M,
		CacheWritePriceNanoUSDPer1M:   input.CacheWritePriceNanoUSDPer1M,
		CacheWrite5mPriceNanoUSDPer1M: input.CacheWrite5mPriceNanoUSDPer1M,
		CacheWrite1hPriceNanoUSDPer1M: input.CacheWrite1hPriceNanoUSDPer1M,
		OutputPriceNanoUSDPer1M:       input.OutputPriceNanoUSDPer1M,
		ImageOutputPriceNanoUSDPer1M:  input.ImageOutputPriceNanoUSDPer1M,
		RequestPriceNanoUSD:           input.RequestPriceNanoUSD,
		PricingTiers:                  tiers,
	}, nil
}

func normalizeBillingMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case core.ModelBillingModeRequest:
		return core.ModelBillingModeRequest
	case core.ModelBillingModeTieredExpr:
		return core.ModelBillingModeTieredExpr
	default:
		return core.ModelBillingModeToken
	}
}

func (s *Service) normalizeModelVisibleGroups(values []string) ([]string, error) {
	if values == nil {
		return nil, nil
	}
	available := map[string]string{}
	for _, group := range accountGroupsWithDefault(s.repo.ListAccountGroups()) {
		name := normalizeAccountGroup(group.Name)
		available[strings.ToLower(name)] = name
	}
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		group := normalizeAccountGroup(value)
		if group == "" {
			return nil, fmt.Errorf("account group is required")
		}
		key := strings.ToLower(group)
		canonical, ok := available[key]
		if !ok {
			return nil, fmt.Errorf("account group %q does not exist", group)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, canonical)
	}
	slices.SortFunc(out, func(a, b string) int {
		if a == b {
			return 0
		}
		if strings.EqualFold(a, core.DefaultAccountGroupName) {
			return -1
		}
		if strings.EqualFold(b, core.DefaultAccountGroupName) {
			return 1
		}
		if strings.ToLower(a) < strings.ToLower(b) {
			return -1
		}
		return 1
	})
	return out, nil
}

func normalizeModelPricingTiers(tiers []core.ModelPricingTier) ([]core.ModelPricingTier, error) {
	out := make([]core.ModelPricingTier, 0, len(tiers))
	for _, tier := range tiers {
		tier.Name = strings.TrimSpace(tier.Name)
		if tier.Name == "" {
			tier.Name = fmt.Sprintf("tier_%d", len(out)+1)
		}
		if strings.Contains(tier.Name, `"`) {
			return nil, fmt.Errorf("pricing tier name cannot contain quotes")
		}
		if tier.MaxInputTokens < 0 {
			return nil, fmt.Errorf("pricing tier max input tokens must be zero or greater")
		}
		if tier.InputPriceNanoUSD < 0 || tier.CachedInputPriceNanoUSD < 0 || tier.CacheWritePriceNanoUSD < 0 || tier.CacheWrite5mPriceNanoUSD < 0 || tier.CacheWrite1hPriceNanoUSD < 0 || tier.OutputPriceNanoUSD < 0 || tier.ImageOutputPriceNanoUSD < 0 {
			return nil, fmt.Errorf("pricing tier prices must be zero or greater")
		}
		if tier.MaxInputTokens == 0 && tier.InputPriceNanoUSD == 0 && tier.CachedInputPriceNanoUSD == 0 && tier.CacheWritePriceNanoUSD == 0 && tier.CacheWrite5mPriceNanoUSD == 0 && tier.CacheWrite1hPriceNanoUSD == 0 && tier.OutputPriceNanoUSD == 0 && tier.ImageOutputPriceNanoUSD == 0 {
			continue
		}
		out = append(out, tier)
	}
	slices.SortStableFunc(out, func(a, b core.ModelPricingTier) int {
		if a.MaxInputTokens == b.MaxInputTokens {
			return 0
		}
		if a.MaxInputTokens == 0 {
			return 1
		}
		if b.MaxInputTokens == 0 {
			return -1
		}
		if a.MaxInputTokens < b.MaxInputTokens {
			return -1
		}
		return 1
	})
	return out, nil
}

func (s *Service) modelSyncAccounts(provider core.ProviderKind) []core.Account {
	now := time.Now().UTC()
	accounts := make([]core.Account, 0)
	for _, account := range s.repo.ListAccounts() {
		account = core.NormalizeAccountRuntimeState(account, now)
		if account.Provider != provider {
			continue
		}
		if !isAccountAvailable(account, now) {
			continue
		}
		account.EffectiveProxyURL = s.effectiveProxyURLForAccount(account)
		accounts = append(accounts, account)
	}
	return accounts
}

func modelSyncError(provider core.ProviderKind, accountCount int, err error) error {
	message := err.Error()
	if provider == core.ProviderOpenAI && strings.Contains(message, "api.model.read") {
		return fmt.Errorf("tried %d available OpenAI account(s), but none can read upstream models: missing api.model.read scope. Use an OpenAI API key with model read permission, or add models manually", accountCount)
	}
	return fmt.Errorf("tried %d available %s account(s), but model sync failed: %w", accountCount, provider, err)
}
