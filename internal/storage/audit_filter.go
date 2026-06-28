package storage

import (
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

type GatewayAuditFilterRepository struct {
	Repository
}

func NewGatewayAuditFilterRepository(base Repository, gatewayEnabled bool) Repository {
	if base == nil || gatewayEnabled {
		return base
	}
	return &GatewayAuditFilterRepository{Repository: base}
}

func (r *GatewayAuditFilterRepository) AppendAudit(event core.AuditEvent) error {
	if event.EffectiveKind() == core.AuditKindGateway {
		return nil
	}
	return r.Repository.AppendAudit(event)
}

func (r *GatewayAuditFilterRepository) GetStartupSystemSettings() (core.SystemSettings, error) {
	return LoadStartupSystemSettings(r.Repository)
}

func (r *GatewayAuditFilterRepository) TouchUserLastUsedAt(userID string, usedAt time.Time) error {
	if repo, ok := r.Repository.(interface {
		TouchUserLastUsedAt(string, time.Time) error
	}); ok {
		return repo.TouchUserLastUsedAt(userID, usedAt)
	}
	user, err := r.Repository.GetUser(userID)
	if err != nil {
		return err
	}
	if usedAt.IsZero() {
		usedAt = time.Now().UTC()
	} else {
		usedAt = usedAt.UTC()
	}
	if user.LastLoginAt != nil && !usedAt.After(*user.LastLoginAt) {
		return nil
	}
	user.LastLoginAt = &usedAt
	return r.Repository.UpdateUserMetadata(user)
}

func (r *GatewayAuditFilterRepository) ConfigRevision() uint64 {
	if repo, ok := r.Repository.(interface{ ConfigRevision() uint64 }); ok {
		return repo.ConfigRevision()
	}
	return 0
}

func (r *GatewayAuditFilterRepository) AccountRevision() uint64 {
	if repo, ok := r.Repository.(interface{ AccountRevision() uint64 }); ok {
		return repo.AccountRevision()
	}
	return 0
}

func (r *GatewayAuditFilterRepository) ModelRevision() uint64 {
	if repo, ok := r.Repository.(interface{ ModelRevision() uint64 }); ok {
		return repo.ModelRevision()
	}
	return 0
}

func (r *GatewayAuditFilterRepository) ClientRevision() uint64 {
	if repo, ok := r.Repository.(interface{ ClientRevision() uint64 }); ok {
		return repo.ClientRevision()
	}
	return 0
}

func (r *GatewayAuditFilterRepository) ListClientSummariesPage(offset, limit int) ([]core.APIClient, int) {
	if repo, ok := r.Repository.(clientSummaryPager); ok {
		return repo.ListClientSummariesPage(offset, limit)
	}
	clients := listCachedClientSummaries(r.Repository)
	return clientSummaryPage(clients, offset, limit)
}

func (r *GatewayAuditFilterRepository) ListClientSummariesByOwnerPage(ownerUserID string, offset, limit int) ([]core.APIClient, int) {
	if repo, ok := r.Repository.(clientSummaryOwnerPager); ok {
		return repo.ListClientSummariesByOwnerPage(ownerUserID, offset, limit)
	}
	clients := r.Repository.ListClientsByOwner(ownerUserID)
	return clientSummaryPage(clients, offset, limit)
}

func (r *GatewayAuditFilterRepository) ListSiteMessageDeliveriesPage(userID string, includeDisabled bool, offset, limit int) ([]core.SiteMessageDelivery, int) {
	if repo, ok := r.Repository.(siteMessageDeliveryPager); ok {
		return repo.ListSiteMessageDeliveriesPage(userID, includeDisabled, offset, limit)
	}
	deliveries := r.Repository.ListSiteMessageDeliveries(userID, includeDisabled)
	return siteMessageDeliveryPage(deliveries, offset, limit)
}

func (r *GatewayAuditFilterRepository) ListVisibleSiteMessageDeliveriesPage(query SiteMessageVisibilityQuery) ([]core.SiteMessageDelivery, int) {
	if repo, ok := r.Repository.(SiteMessageVisibleDeliveryPager); ok {
		return repo.ListVisibleSiteMessageDeliveriesPage(query)
	}
	query = normalizeSiteMessageVisibilityQuery(query)
	deliveries := r.Repository.ListSiteMessageDeliveries(query.UserID, false)
	return visibleSiteMessageDeliveriesPage(deliveries, query)
}

func (r *GatewayAuditFilterRepository) VisibleSiteMessageUnreadCount(query SiteMessageVisibilityQuery) int {
	if repo, ok := r.Repository.(SiteMessageVisibleDeliveryPager); ok {
		return repo.VisibleSiteMessageUnreadCount(query)
	}
	query = normalizeSiteMessageVisibilityQuery(query)
	deliveries := r.Repository.ListSiteMessageDeliveries(query.UserID, false)
	return visibleSiteMessageUnreadCount(deliveries, query)
}

func (r *GatewayAuditFilterRepository) ListDocumentsPage(status core.DocumentStatus, seoOnly bool, offset, limit int) ([]core.Document, int) {
	if repo, ok := r.Repository.(documentPager); ok {
		return repo.ListDocumentsPage(status, seoOnly, offset, limit)
	}
	return documentPage(r.Repository.ListDocuments(), status, seoOnly, offset, limit)
}

func (r *GatewayAuditFilterRepository) SearchDocumentsPage(query string, status core.DocumentStatus, seoOnly bool, offset, limit int) ([]core.Document, int) {
	if repo, ok := r.Repository.(documentSearcher); ok {
		return repo.SearchDocumentsPage(query, status, seoOnly, offset, limit)
	}
	return documentSearchPage(r.Repository.ListDocuments(), query, status, seoOnly, offset, limit)
}
