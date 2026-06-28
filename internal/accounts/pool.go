package accounts

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/storage"
)

type Pool struct {
	repo           storage.Repository
	mu             sync.RWMutex
	snap           accountSnapshot
	candidateMu    sync.RWMutex
	candidateCache map[candidateCacheKey][]core.Account
}

const maxProviderCandidates = 16
const maxImageProviderCandidates = 64
const initialCandidateCacheEntries = 64
const maxCandidateCacheEntries = 4096
const refreshStateTTL = 2 * time.Minute

type candidateCacheKey struct {
	revision  uint64
	provider  core.ProviderKind
	stickyKey string
	kind      candidateKind
}

type candidateKind string

const (
	candidateKindDefault candidateKind = ""
	candidateKindImage   candidateKind = "image"
)

func NewPool(repo storage.Repository) *Pool {
	return &Pool{repo: repo}
}

func (p *Pool) Candidates(provider core.ProviderKind, client *core.APIClient) []core.Account {
	return p.candidates(provider, client, candidateKindDefault)
}

func (p *Pool) ImageCandidates(provider core.ProviderKind, client *core.APIClient) []core.Account {
	return p.candidates(provider, client, candidateKindImage)
}

func (p *Pool) candidates(provider core.ProviderKind, client *core.APIClient, kind candidateKind) []core.Account {
	snapshot := p.snapshot()
	accounts := snapshot.byProvider[provider]
	needsRuntimeFilter := snapshot.cooldownByProvider[provider] || kind == candidateKindImage
	accountGroup := clientAccountGroup(client)
	groupRestricted := client != nil
	stickyClientID := stickyClientKey(client)
	hasStickyRank := stickyClientID != ""
	limit := minInt(len(accounts), candidateLimit(kind))
	if limit == 0 {
		return nil
	}
	var cacheKey candidateCacheKey
	useCandidateCache := !groupRestricted && !needsRuntimeFilter && hasStickyRank && snapshot.cacheable && len(accounts) > maxProviderCandidates
	if useCandidateCache {
		cacheKey = candidateCacheKey{revision: snapshot.revision, provider: provider, stickyKey: stickyClientID, kind: kind}
		if candidates, ok := p.loadCachedCandidates(cacheKey); ok {
			return candidates
		}
	}
	now := time.Time{}
	if needsRuntimeFilter {
		now = time.Now().UTC()
	}
	var primaryRanked [maxImageProviderCandidates]core.Account
	var primaryStickyRanks [maxImageProviderCandidates]uint32
	var backupRanked [maxImageProviderCandidates]core.Account
	var backupStickyRanks [maxImageProviderCandidates]uint32
	count := 0
	primaryEligibleCount := 0
	backupCount := 0
	for _, account := range accounts {
		if needsRuntimeFilter {
			account = core.NormalizeAccountRuntimeState(account, now)
			if !candidateAccountAvailable(account, now, kind) {
				continue
			}
		}
		if groupRestricted && !strings.EqualFold(accountGroupName(account.Group), accountGroup) {
			continue
		}
		var stickyRank uint32
		if hasStickyRank {
			stickyRank = stickyAccountRank(stickyClientID, provider, account.ID)
		}
		if account.Backup {
			backupCount = insertRankedAccountCandidate(&backupRanked, &backupStickyRanks, backupCount, account, stickyRank, hasStickyRank, limit, kind, now)
			continue
		}
		primaryEligibleCount++
		count = insertRankedAccountCandidate(&primaryRanked, &primaryStickyRanks, count, account, stickyRank, hasStickyRank, limit, kind, now)
	}
	if count == 0 && backupCount == 0 {
		return nil
	}
	includeBackups := primaryEligibleCount == count || count == 0
	out := make([]core.Account, 0, count+backupCount)
	out = append(out, primaryRanked[:count]...)
	if includeBackups {
		out = append(out, backupRanked[:backupCount]...)
	}
	if useCandidateCache {
		p.storeCachedCandidates(cacheKey, out)
	}
	return out
}

func candidateAccountAvailable(account core.Account, now time.Time, kind candidateKind) bool {
	if kind == candidateKindImage {
		return core.AccountAvailableForImageRouting(account, now)
	}
	return core.AccountAvailableForRouting(account, now)
}

func candidateLimit(kind candidateKind) int {
	if kind == candidateKindImage {
		return maxImageProviderCandidates
	}
	return maxProviderCandidates
}

func insertRankedAccountCandidate(ranked *[maxImageProviderCandidates]core.Account, stickyRanks *[maxImageProviderCandidates]uint32, count int, account core.Account, stickyRank uint32, hasStickyRank bool, limit int, kind candidateKind, now time.Time) int {
	insertAt := count
	for i := 0; i < count; i++ {
		existing := ranked[i]
		var existingStickyRank uint32
		if hasStickyRank {
			existingStickyRank = stickyRanks[i]
		}
		if accountCandidateLess(account, stickyRank, existing, existingStickyRank, hasStickyRank, kind, now) {
			insertAt = i
			break
		}
	}
	if insertAt == count {
		if count >= limit {
			return count
		}
		ranked[count] = account
		if hasStickyRank {
			stickyRanks[count] = stickyRank
		}
		return count + 1
	}
	if count < limit {
		count++
	}
	copy(ranked[insertAt+1:count], ranked[insertAt:count-1])
	ranked[insertAt] = account
	if hasStickyRank {
		copy(stickyRanks[insertAt+1:count], stickyRanks[insertAt:count-1])
		stickyRanks[insertAt] = stickyRank
	}
	return count
}

func accountCandidateLess(a core.Account, aStickyRank uint32, b core.Account, bStickyRank uint32, hasStickyRank bool, kind candidateKind, now time.Time) bool {
	if kind == candidateKindImage {
		aClass, aRemaining := imageQuotaRank(a, now)
		bClass, bRemaining := imageQuotaRank(b, now)
		if aClass != bClass {
			return aClass > bClass
		}
		if aClass == imageQuotaRankKnown && aRemaining != bRemaining {
			return aRemaining > bRemaining
		}
	}
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	if a.ConsecutiveFails != b.ConsecutiveFails {
		return a.ConsecutiveFails < b.ConsecutiveFails
	}
	if hasStickyRank && aStickyRank != bStickyRank {
		return aStickyRank < bStickyRank
	}
	if a.LastUsedAt == nil && b.LastUsedAt != nil {
		return true
	}
	if a.LastUsedAt != nil && b.LastUsedAt == nil {
		return false
	}
	if a.LastUsedAt != nil && b.LastUsedAt != nil {
		if diff := a.LastUsedAt.Compare(*b.LastUsedAt); diff != 0 {
			return diff < 0
		}
	}
	if a.Weight != b.Weight {
		return a.Weight > b.Weight
	}
	return a.ID < b.ID
}

const (
	imageQuotaRankExhausted = iota
	imageQuotaRankUnknown
	imageQuotaRankKnown
)

func imageQuotaRank(account core.Account, now time.Time) (int, int64) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	quota := core.ReadAccountQuota(account)
	if quota == nil || quota.Image == nil || quota.Image.Unknown {
		return imageQuotaRankUnknown, 0
	}
	if quota.Image.Remaining > 0 {
		return imageQuotaRankKnown, quota.Image.Remaining
	}
	if quota.Image.ResetsAt != nil && !quota.Image.ResetsAt.IsZero() && !quota.Image.ResetsAt.UTC().After(now.UTC()) {
		return imageQuotaRankUnknown, 0
	}
	return imageQuotaRankExhausted, 0
}

type revisionedRepository interface {
	ConfigRevision() uint64
}

type accountRevisionedRepository interface {
	AccountRevision() uint64
}

type runtimeAccountUpdater interface {
	UpsertRuntimeAccount(core.Account) error
}

type accountSnapshot struct {
	revision           uint64
	cacheable          bool
	byProvider         map[core.ProviderKind][]core.Account
	cooldownByProvider map[core.ProviderKind]bool
}

func (p *Pool) snapshot() accountSnapshot {
	revision, cacheable := p.repoRevision()
	if cacheable {
		p.mu.RLock()
		if p.snap.cacheable && p.snap.revision == revision {
			snapshot := p.snap
			p.mu.RUnlock()
			return snapshot
		}
		p.mu.RUnlock()
	}

	snapshot := p.buildSnapshot(revision, cacheable)
	if !cacheable {
		return snapshot
	}

	p.mu.Lock()
	cacheChanged := false
	if !p.snap.cacheable || p.snap.revision != revision {
		p.snap = snapshot
		cacheChanged = true
	}
	snapshot = p.snap
	p.mu.Unlock()
	if cacheChanged {
		p.clearCandidateCache()
	}
	return snapshot
}

func (p *Pool) loadCachedCandidates(key candidateCacheKey) ([]core.Account, bool) {
	p.candidateMu.RLock()
	candidates, ok := p.candidateCache[key]
	p.candidateMu.RUnlock()
	return candidates, ok
}

func (p *Pool) storeCachedCandidates(key candidateCacheKey, candidates []core.Account) {
	p.candidateMu.Lock()
	defer p.candidateMu.Unlock()
	if p.candidateCache == nil || len(p.candidateCache) >= maxCandidateCacheEntries {
		p.candidateCache = make(map[candidateCacheKey][]core.Account, initialCandidateCacheEntries)
	}
	p.candidateCache[key] = candidates
}

func (p *Pool) clearCandidateCache() {
	p.candidateMu.Lock()
	if len(p.candidateCache) > 0 {
		p.candidateCache = nil
	}
	p.candidateMu.Unlock()
}

func (p *Pool) repoRevision() (uint64, bool) {
	if repo, ok := p.repo.(accountRevisionedRepository); ok {
		revision := repo.AccountRevision()
		return revision, revision != 0
	}
	repo, ok := p.repo.(revisionedRepository)
	if !ok {
		return 0, false
	}
	revision := repo.ConfigRevision()
	return revision, revision != 0
}

func (p *Pool) buildSnapshot(revision uint64, cacheable bool) accountSnapshot {
	now := time.Now().UTC()
	accounts := core.NormalizeAccountsRuntimeState(p.repo.ListAccounts(), now)
	needsGroupProxy := false
	needsSystemProxy := false
	for _, account := range accounts {
		if core.AccountControlDisabled(account) {
			continue
		}
		if accountUnavailableForCandidates(account.Status) {
			continue
		}
		if strings.TrimSpace(account.ProxyURL) != "" {
			continue
		}
		if accountGroupName(account.Group) != "" {
			needsGroupProxy = true
		} else {
			needsSystemProxy = true
		}
	}
	var groupByName map[string]accountGroupRuntime
	if needsGroupProxy || hasGroupedAccounts(accounts) {
		groupByName = accountGroupRuntimeMap(p.repo.ListAccountGroups(), now)
	}
	if !needsSystemProxy && needsGroupProxy {
		for _, account := range accounts {
			if core.AccountControlDisabled(account) || accountUnavailableForCandidates(account.Status) || strings.TrimSpace(account.ProxyURL) != "" {
				continue
			}
			group := strings.ToLower(accountGroupName(account.Group))
			if group == "" {
				continue
			}
			runtime, ok := groupByName[group]
			if !ok || strings.TrimSpace(runtime.proxyURL) == "" {
				needsSystemProxy = true
				break
			}
		}
	}
	systemProxyURL := ""
	if needsSystemProxy {
		systemProxyURL = p.systemProxyURL()
	}
	byProvider := make(map[core.ProviderKind][]core.Account, 4)
	cooldownByProvider := make(map[core.ProviderKind]bool, 4)
	for _, account := range accounts {
		if core.AccountControlDisabled(account) {
			continue
		}
		if accountDurablyUnavailableForCandidates(account.Status) {
			continue
		}
		if group := strings.ToLower(accountGroupName(account.Group)); group != "" {
			if runtime, ok := groupByName[group]; ok {
				if !runtime.routable {
					continue
				}
			}
		}
		account.EffectiveProxyURL = effectiveProxyURL(account, groupByName, systemProxyURL)
		byProvider[account.Provider] = append(byProvider[account.Provider], account)
		if !core.AccountAvailableForRouting(account, now) {
			cooldownByProvider[account.Provider] = true
		}
	}
	return accountSnapshot{
		revision:           revision,
		cacheable:          cacheable,
		byProvider:         byProvider,
		cooldownByProvider: cooldownByProvider,
	}
}

func accountUnavailableForCandidates(status core.AccountStatus) bool {
	return accountDurablyUnavailableForCandidates(status)
}

func accountDurablyUnavailableForCandidates(status core.AccountStatus) bool {
	switch status {
	case core.AccountStatusBlocked,
		core.AccountStatusProviderBanned,
		core.AccountStatusExpired:
		return true
	default:
		return false
	}
}

func stickyClientKey(client *core.APIClient) string {
	if client == nil {
		return ""
	}
	if key := strings.TrimSpace(client.RouteAffinityKey); key != "" {
		return key
	}
	if id := strings.TrimSpace(client.ID); id != "" {
		return id
	}
	return strings.TrimSpace(client.APIKey)
}

func stickyAccountRank(clientID string, provider core.ProviderKind, accountID string) uint32 {
	hash := uint32(2166136261)
	hash = fnv32aString(hash, clientID)
	hash = fnv32aByte(hash, 0)
	hash = fnv32aString(hash, string(provider))
	hash = fnv32aByte(hash, 0)
	hash = fnv32aString(hash, accountID)
	return hash
}

func fnv32aString(hash uint32, value string) uint32 {
	for i := 0; i < len(value); i++ {
		hash = fnv32aByte(hash, value[i])
	}
	return hash
}

func fnv32aByte(hash uint32, value byte) uint32 {
	hash ^= uint32(value)
	hash *= 16777619
	return hash
}

func (p *Pool) GetAccount(accountID string) (core.Account, error) {
	account, err := p.repo.GetAccount(accountID)
	if err != nil {
		return core.Account{}, err
	}
	now := time.Now().UTC()
	account = core.NormalizeAccountRuntimeState(account, now)
	account.EffectiveProxyURL = p.effectiveProxyURLForAccount(account, now)
	return account, nil
}

func (p *Pool) systemProxyURL() string {
	settings, err := p.repo.GetSystemSettings()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(settings.Network.SystemProxyURL)
}

func (p *Pool) effectiveProxyURLForAccount(account core.Account, now time.Time) string {
	if strings.TrimSpace(account.ProxyURL) != "" {
		return strings.TrimSpace(account.ProxyURL)
	}
	groupByName := map[string]accountGroupRuntime(nil)
	if accountGroupName(account.Group) != "" {
		groupByName = accountGroupRuntimeMap(p.repo.ListAccountGroups(), now)
		if proxyURL := effectiveProxyURL(account, groupByName, ""); proxyURL != "" {
			return proxyURL
		}
	}
	return effectiveProxyURL(account, groupByName, p.systemProxyURL())
}

type accountGroupRuntime struct {
	proxyURL string
	routable bool
}

func hasGroupedAccounts(accounts []core.Account) bool {
	for _, account := range accounts {
		if accountGroupName(account.Group) != "" {
			return true
		}
	}
	return false
}

func accountGroupRuntimeMap(groups []core.AccountGroup, now time.Time) map[string]accountGroupRuntime {
	out := make(map[string]accountGroupRuntime, len(groups))
	for _, group := range groups {
		name := strings.ToLower(strings.TrimSpace(group.Name))
		if name == "" {
			continue
		}
		out[name] = accountGroupRuntime{
			proxyURL: strings.TrimSpace(group.ProxyURL),
			routable: true,
		}
	}
	return out
}

func effectiveProxyURL(account core.Account, groupByName map[string]accountGroupRuntime, systemProxyURL string) string {
	if proxyURL := strings.TrimSpace(account.ProxyURL); proxyURL != "" {
		return proxyURL
	}
	group := strings.ToLower(accountGroupName(account.Group))
	if group == "" {
		return strings.TrimSpace(systemProxyURL)
	}
	if runtime, ok := groupByName[group]; ok {
		if proxyURL := strings.TrimSpace(runtime.proxyURL); proxyURL != "" {
			return proxyURL
		}
	}
	return strings.TrimSpace(systemProxyURL)
}

func clientAccountGroup(client *core.APIClient) string {
	if client == nil {
		return ""
	}
	return accountGroupName(client.AccountGroup)
}

func accountGroupName(group string) string {
	return core.NormalizeAccountGroupName(group)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (p *Pool) MarkSuccess(account core.Account) error {
	if current, err := p.repo.GetAccount(account.ID); err == nil {
		account = current
	}
	now := time.Now().UTC()
	account.Status = core.AccountStatusActive
	account.LastUsedAt = &now
	account.CooldownUntil = nil
	account.ConsecutiveFails = 0
	return p.upsertRuntimeAccount(account)
}

func (p *Pool) MarkFailure(account core.Account, status core.AccountStatus, cooldown time.Duration) error {
	now := time.Now().UTC()
	preserveCredentialUpdate := false
	if current, err := p.repo.GetAccount(account.ID); err == nil {
		if accountFailureAlreadyRecorded(current, now) {
			return nil
		}
		incomingCredential := account.Credential
		account = current
		if shouldPreserveFailureCredentialUpdate(incomingCredential, current.Credential, now) {
			account.Credential = incomingCredential
			preserveCredentialUpdate = true
		}
	}
	account.TotalFails++
	account.ConsecutiveFails++
	account.LastUsedAt = &now

	switch status {
	case core.AccountStatusCooling:
		until := now.Add(cooldown)
		account.Status = core.AccountStatusCooling
		account.CooldownUntil = &until
	case core.AccountStatusExpired:
		account.Status = core.AccountStatusExpired
		account.CooldownUntil = nil
	case core.AccountStatusBlocked:
		account.Status = core.AccountStatusBlocked
		account.CooldownUntil = nil
	case core.AccountStatusProviderBanned:
		account.Status = core.AccountStatusProviderBanned
		account.CooldownUntil = nil
	default:
		account.Status = status
		account.CooldownUntil = nil
	}
	if preserveCredentialUpdate {
		return p.repo.UpsertAccount(account)
	}
	return p.upsertRuntimeAccount(account)
}

func (p *Pool) MarkChatQuotaLimited(account core.Account, cooldown time.Duration) error {
	return p.markScopedQuotaLimited(account, cooldown, false)
}

func (p *Pool) MarkImageQuotaLimited(account core.Account, cooldown time.Duration) error {
	return p.markScopedQuotaLimited(account, cooldown, true)
}

func (p *Pool) MarkImageQuotaUsed(account core.Account, count int64) error {
	if count <= 0 {
		return nil
	}
	if current, err := p.repo.GetAccount(account.ID); err == nil {
		account = current
	}
	snapshot := core.ReadAccountQuota(account)
	if snapshot == nil || snapshot.Image == nil || snapshot.Image.Unknown || snapshot.Image.Remaining <= 0 {
		return nil
	}
	metadata := cloneAccountMetadata(account.Credential.Metadata)
	account.Credential.Metadata = metadata
	next := snapshot.Image.Remaining - count
	if next < 0 {
		next = 0
	}
	snapshot.Image.Remaining = next
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	metadata[core.AccountQuotaMetadataKey] = string(encoded)
	return p.upsertRuntimeAccount(account)
}

func (p *Pool) markScopedQuotaLimited(account core.Account, cooldown time.Duration, image bool) error {
	now := time.Now().UTC()
	if cooldown <= 0 {
		cooldown = 45 * time.Second
	}
	reset := now.Add(cooldown)
	if current, err := p.repo.GetAccount(account.ID); err == nil {
		account = current
	}
	metadata := cloneAccountMetadata(account.Credential.Metadata)
	account.Credential.Metadata = metadata
	snapshot := core.ReadAccountQuota(account)
	if snapshot == nil {
		snapshot = &core.AccountQuotaSnapshot{}
	}
	if snapshot.Additional == nil {
		snapshot.Additional = map[string]core.AccountQuotaSnapshot{}
	}
	scoped := core.AccountQuotaSnapshot{
		Source:      core.AccountQuotaRuntimeSource,
		RefreshedAt: &now,
	}
	if image {
		scoped.LimitID = core.AccountQuotaRuntimeImageLimitID
		scoped.Image = &core.AccountImageQuota{Remaining: 0, ResetsAt: &reset}
		snapshot.Additional[core.AccountQuotaRuntimeImageLimitID] = scoped
	} else {
		scoped.LimitID = core.AccountQuotaRuntimeChatLimitID
		scoped.Primary = &core.AccountQuotaWindow{
			Name:        "rate_limit",
			UsedPercent: 100,
			ResetsAt:    &reset,
		}
		scoped.ReachedType = "primary_window"
		snapshot.Additional[core.AccountQuotaRuntimeChatLimitID] = scoped
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	metadata[core.AccountQuotaMetadataKey] = string(encoded)
	return p.upsertRuntimeAccount(account)
}

func accountFailureAlreadyRecorded(account core.Account, now time.Time) bool {
	account = core.NormalizeAccountRuntimeState(account, now)
	switch account.Status {
	case core.AccountStatusBlocked,
		core.AccountStatusProviderBanned,
		core.AccountStatusExpired:
		return true
	case core.AccountStatusCooling:
		return account.CooldownUntil != nil && account.CooldownUntil.After(now)
	default:
		return false
	}
}

func shouldPreserveFailureCredentialUpdate(incoming, current core.Credential, now time.Time) bool {
	if incoming.ExpiresAt == nil || !incoming.ExpiresAt.Before(now) {
		return false
	}
	if incoming.AccessToken != current.AccessToken || incoming.RefreshToken != current.RefreshToken {
		return false
	}
	if current.ExpiresAt == nil {
		return true
	}
	return incoming.ExpiresAt.Before(*current.ExpiresAt)
}

func cloneAccountMetadata(metadata map[string]string) map[string]string {
	if metadata == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(metadata)+1)
	for key, value := range metadata {
		out[key] = value
	}
	return out
}

func (p *Pool) MarkRefreshing(account core.Account) error {
	until := time.Now().UTC().Add(refreshStateTTL)
	account.Status = core.AccountStatusRefreshing
	account.CooldownUntil = &until
	return p.upsertRuntimeAccount(account)
}

func (p *Pool) MarkCredentialRefreshed(account core.Account, credential core.Credential) (core.Account, error) {
	account.Credential = credential
	account.Status = core.AccountStatusActive
	account.CooldownUntil = nil
	account.ConsecutiveFails = 0
	if current, err := p.repo.GetAccount(account.ID); err == nil {
		account = core.PreserveAccountControlState(account, current)
	}
	return account, p.repo.UpsertAccount(account)
}

func (p *Pool) upsertRuntimeAccount(account core.Account) error {
	if current, err := p.repo.GetAccount(account.ID); err == nil {
		account = core.PreserveAccountControlState(account, current)
	}
	if repo, ok := p.repo.(runtimeAccountUpdater); ok {
		return repo.UpsertRuntimeAccount(account)
	}
	return p.repo.UpsertAccount(account)
}
