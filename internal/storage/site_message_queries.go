package storage

import (
	"strings"

	"github.com/32ns/ai-gateway/internal/core"
)

type SiteMessageVisibilityQuery struct {
	UserID        string
	AccountGroups []string
	Offset        int
	Limit         int
}

type SiteMessageVisibleDeliveryPager interface {
	ListVisibleSiteMessageDeliveriesPage(query SiteMessageVisibilityQuery) ([]core.SiteMessageDelivery, int)
	VisibleSiteMessageUnreadCount(query SiteMessageVisibilityQuery) int
}

func normalizeSiteMessageVisibilityQuery(query SiteMessageVisibilityQuery) SiteMessageVisibilityQuery {
	query.UserID = strings.TrimSpace(query.UserID)
	if query.Offset < 0 {
		query.Offset = 0
	}
	if query.Limit <= 0 {
		query.Limit = 25
	}
	seen := make(map[string]struct{}, len(query.AccountGroups))
	groups := make([]string, 0, len(query.AccountGroups))
	for _, group := range query.AccountGroups {
		group = strings.ToLower(strings.TrimSpace(group))
		if group == "" {
			continue
		}
		if _, ok := seen[group]; ok {
			continue
		}
		seen[group] = struct{}{}
		groups = append(groups, group)
	}
	query.AccountGroups = groups
	return query
}

func siteMessageVisibleToQueryUser(message core.SiteMessage, query SiteMessageVisibilityQuery) bool {
	if strings.TrimSpace(query.UserID) == "" {
		return false
	}
	if len(message.TargetUserIDs) == 0 && len(message.TargetAccountGroups) == 0 {
		return true
	}
	for _, targetUserID := range message.TargetUserIDs {
		if strings.TrimSpace(targetUserID) == query.UserID {
			return true
		}
	}
	if len(message.TargetAccountGroups) == 0 {
		return false
	}
	groups := make(map[string]struct{}, len(query.AccountGroups))
	for _, group := range query.AccountGroups {
		group = strings.ToLower(strings.TrimSpace(group))
		if group != "" {
			groups[group] = struct{}{}
		}
	}
	for _, targetGroup := range message.TargetAccountGroups {
		if _, ok := groups[strings.ToLower(strings.TrimSpace(targetGroup))]; ok {
			return true
		}
	}
	return false
}

func visibleSiteMessageDeliveriesPage(deliveries []core.SiteMessageDelivery, query SiteMessageVisibilityQuery) ([]core.SiteMessageDelivery, int) {
	query = normalizeSiteMessageVisibilityQuery(query)
	visible := make([]core.SiteMessageDelivery, 0, len(deliveries))
	for _, delivery := range deliveries {
		if !delivery.Message.Enabled || delivery.Message.PublicPopup {
			continue
		}
		if siteMessageVisibleToQueryUser(delivery.Message, query) {
			visible = append(visible, delivery)
		}
	}
	return siteMessageDeliveryPage(visible, query.Offset, query.Limit)
}

func visibleSiteMessageUnreadCount(deliveries []core.SiteMessageDelivery, query SiteMessageVisibilityQuery) int {
	query = normalizeSiteMessageVisibilityQuery(query)
	count := 0
	for _, delivery := range deliveries {
		if !delivery.Message.Enabled || delivery.Message.PublicPopup || delivery.Read {
			continue
		}
		if siteMessageVisibleToQueryUser(delivery.Message, query) {
			count++
		}
	}
	return count
}
