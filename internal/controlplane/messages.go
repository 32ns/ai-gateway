package controlplane

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/storage"
)

type SiteMessageInput struct {
	Title               string
	Body                string
	Enabled             bool
	Popup               bool
	PublicPopup         bool
	TargetUserIDs       []string
	TargetAccountGroups []string
}

type siteMessageDeliveryPager interface {
	ListSiteMessageDeliveriesPage(userID string, includeDisabled bool, offset, limit int) ([]core.SiteMessageDelivery, int)
}

func (s *Service) CreateSiteMessage(actor core.User, input SiteMessageInput) (core.SiteMessage, error) {
	if !actor.IsAdmin() {
		return core.SiteMessage{}, fmt.Errorf("admin role required")
	}
	title := strings.TrimSpace(input.Title)
	body := strings.TrimSpace(input.Body)
	if title == "" || body == "" {
		return core.SiteMessage{}, fmt.Errorf("message title and body are required")
	}
	targetUsers, err := s.normalizeSiteMessageTargetUsers(input.TargetUserIDs)
	if err != nil {
		return core.SiteMessage{}, err
	}
	targetGroups, err := s.normalizeSiteMessageTargetGroups(input.TargetAccountGroups)
	if err != nil {
		return core.SiteMessage{}, err
	}
	if input.PublicPopup && (len(targetUsers) > 0 || len(targetGroups) > 0) {
		return core.SiteMessage{}, fmt.Errorf("website popup messages must be sent to all users")
	}
	now := time.Now().UTC()
	for range 3 {
		message := core.SiteMessage{
			ID:                  generateSiteMessageID(),
			Title:               title,
			Body:                body,
			CreatedBy:           actor.ID,
			Enabled:             input.Enabled,
			Popup:               input.Popup,
			PublicPopup:         input.PublicPopup,
			TargetUserIDs:       targetUsers,
			TargetAccountGroups: targetGroups,
			CreatedAt:           now,
			UpdatedAt:           now,
		}
		if err := s.repo.CreateSiteMessage(message); err != nil {
			if errors.Is(err, storage.ErrBillingRequestConflict) {
				continue
			}
			return core.SiteMessage{}, err
		}
		return message, nil
	}
	return core.SiteMessage{}, fmt.Errorf("message id conflict")
}

func (s *Service) UpdateSiteMessage(actor core.User, id string, input SiteMessageInput) (core.SiteMessage, error) {
	if !actor.IsAdmin() {
		return core.SiteMessage{}, fmt.Errorf("admin role required")
	}
	existing, err := s.repo.GetSiteMessage(strings.TrimSpace(id))
	if err != nil {
		return core.SiteMessage{}, err
	}
	existing.Title = strings.TrimSpace(input.Title)
	existing.Body = strings.TrimSpace(input.Body)
	existing.Enabled = input.Enabled
	existing.Popup = input.Popup
	existing.PublicPopup = input.PublicPopup
	existing.TargetUserIDs, err = s.normalizeSiteMessageTargetUsers(input.TargetUserIDs)
	if err != nil {
		return core.SiteMessage{}, err
	}
	existing.TargetAccountGroups, err = s.normalizeSiteMessageTargetGroups(input.TargetAccountGroups)
	if err != nil {
		return core.SiteMessage{}, err
	}
	if existing.PublicPopup && (len(existing.TargetUserIDs) > 0 || len(existing.TargetAccountGroups) > 0) {
		return core.SiteMessage{}, fmt.Errorf("website popup messages must be sent to all users")
	}
	if err := s.repo.UpdateSiteMessage(existing); err != nil {
		return core.SiteMessage{}, err
	}
	if err := s.repo.ClearSiteMessageReads(existing.ID); err != nil {
		return core.SiteMessage{}, err
	}
	return s.repo.GetSiteMessage(existing.ID)
}

func (s *Service) DeleteSiteMessage(actor core.User, id string) (core.SiteMessage, error) {
	if !actor.IsAdmin() {
		return core.SiteMessage{}, fmt.Errorf("admin role required")
	}
	message, err := s.repo.GetSiteMessage(strings.TrimSpace(id))
	if err != nil {
		return core.SiteMessage{}, err
	}
	if err := s.repo.DeleteSiteMessage(message.ID); err != nil {
		return core.SiteMessage{}, err
	}
	return message, nil
}

func (s *Service) GetSiteMessage(user core.User, id string) (core.SiteMessage, error) {
	message, err := s.repo.GetSiteMessage(strings.TrimSpace(id))
	if err != nil {
		return core.SiteMessage{}, err
	}
	if user.IsAdmin() {
		return message, nil
	}
	if !message.Enabled || message.PublicPopup || !s.siteMessageVisibleToUser(message, user) {
		return core.SiteMessage{}, storage.ErrNotFound
	}
	return message, nil
}

func (s *Service) ListSiteMessages(user core.User) []core.SiteMessageDelivery {
	deliveries := s.repo.ListSiteMessageDeliveries(user.ID, user.IsAdmin())
	out := make([]core.SiteMessageDelivery, 0, len(deliveries))
	var userGroups map[string]struct{}
	for _, delivery := range deliveries {
		if user.IsAdmin() && !s.siteMessageDeliveryVisibleInAdminInbox(delivery, user) {
			continue
		}
		if user.IsAdmin() || s.siteMessageDeliveryVisibleInInbox(delivery, user, &userGroups) {
			out = append(out, delivery)
		}
	}
	return out
}

func (s *Service) ListSiteMessagesPage(user core.User, offset, limit int) ([]core.SiteMessageDelivery, int) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 25
	}
	if user.IsAdmin() {
		if pager, ok := s.repo.(siteMessageDeliveryPager); ok {
			return s.listAdminSiteMessagesPageFromPager(user, pager, offset, limit)
		}
	} else if pager, ok := s.repo.(storage.SiteMessageVisibleDeliveryPager); ok {
		return pager.ListVisibleSiteMessageDeliveriesPage(s.siteMessageVisibilityQuery(user, offset, limit))
	}
	deliveries := s.ListSiteMessages(user)
	return siteMessageDeliveriesPage(deliveries, offset, limit)
}

func (s *Service) listAdminSiteMessagesPageFromPager(user core.User, pager siteMessageDeliveryPager, offset, limit int) ([]core.SiteMessageDelivery, int) {
	pageSize := limit
	if pageSize < 25 {
		pageSize = 25
	}
	out := make([]core.SiteMessageDelivery, 0, limit)
	totalVisible := 0
	for pageOffset := 0; ; pageOffset += pageSize {
		deliveries, totalRaw := pager.ListSiteMessageDeliveriesPage(user.ID, true, pageOffset, pageSize)
		for _, delivery := range deliveries {
			if !s.siteMessageDeliveryVisibleInAdminInbox(delivery, user) {
				continue
			}
			if totalVisible >= offset && len(out) < limit {
				out = append(out, delivery)
			}
			totalVisible++
		}
		if len(deliveries) == 0 || pageOffset+len(deliveries) >= totalRaw {
			break
		}
	}
	return out, totalVisible
}

func (s *Service) MarkSiteMessageRead(user core.User, id string) error {
	message, err := s.repo.GetSiteMessage(strings.TrimSpace(id))
	if err != nil {
		return err
	}
	if !message.Enabled && !user.IsAdmin() {
		return fmt.Errorf("message is unavailable")
	}
	if message.PublicPopup && !user.IsAdmin() {
		return fmt.Errorf("message is unavailable")
	}
	if !user.IsAdmin() && !s.siteMessageVisibleToUser(message, user) {
		return fmt.Errorf("message is unavailable")
	}
	return s.repo.MarkSiteMessageRead(message.ID, user.ID, time.Now().UTC())
}

func (s *Service) SiteMessageUnreadCount(user core.User) int {
	if user.ID == "" {
		return 0
	}
	if user.IsAdmin() {
		return s.adminSiteMessageUnreadCount(user)
	}
	if pager, ok := s.repo.(storage.SiteMessageVisibleDeliveryPager); ok {
		return pager.VisibleSiteMessageUnreadCount(s.siteMessageVisibilityQuery(user, 0, 25))
	}
	count := 0
	for _, delivery := range s.ListSiteMessages(user) {
		if delivery.Message.Enabled && !delivery.Read {
			count++
		}
	}
	return count
}

func (s *Service) adminSiteMessageUnreadCount(user core.User) int {
	count := 0
	if pager, ok := s.repo.(siteMessageDeliveryPager); ok {
		const pageSize = 100
		for offset := 0; ; offset += pageSize {
			deliveries, totalRaw := pager.ListSiteMessageDeliveriesPage(user.ID, true, offset, pageSize)
			for _, delivery := range deliveries {
				if delivery.Message.Enabled && !delivery.Message.PublicPopup && !delivery.Read && s.siteMessageDeliveryVisibleInAdminInbox(delivery, user) {
					count++
				}
			}
			if len(deliveries) == 0 || offset+len(deliveries) >= totalRaw {
				break
			}
		}
		return count
	}
	for _, delivery := range s.ListSiteMessages(user) {
		if delivery.Message.Enabled && !delivery.Message.PublicPopup && !delivery.Read {
			count++
		}
	}
	return count
}

func (s *Service) ListUnreadPopupSiteMessages(user core.User, limit int) []core.SiteMessageDelivery {
	if strings.TrimSpace(user.ID) == "" {
		return nil
	}
	if limit <= 0 {
		limit = 5
	}
	out := make([]core.SiteMessageDelivery, 0, limit)
	const pageSize = 25
	for offset := 0; len(out) < limit; {
		deliveries, total := s.ListSiteMessagesPage(user, offset, pageSize)
		for _, delivery := range deliveries {
			if !delivery.Message.Enabled || !delivery.Message.Popup || delivery.Message.PublicPopup || delivery.Read {
				continue
			}
			out = append(out, delivery)
			if len(out) >= limit {
				break
			}
		}
		if len(deliveries) == 0 || offset+len(deliveries) >= total {
			break
		}
		offset += len(deliveries)
	}
	return out
}

func (s *Service) ListPublicPopupSiteMessages(limit int) []core.SiteMessage {
	if limit <= 0 {
		limit = 5
	}
	out := make([]core.SiteMessage, 0, limit)
	if pager, ok := s.repo.(siteMessageDeliveryPager); ok {
		const pageSize = 25
		for offset := 0; len(out) < limit; {
			deliveries, total := pager.ListSiteMessageDeliveriesPage("", false, offset, pageSize)
			for _, delivery := range deliveries {
				message := delivery.Message
				if !message.Enabled || !message.PublicPopup {
					continue
				}
				out = append(out, message)
				if len(out) >= limit {
					break
				}
			}
			if len(deliveries) == 0 || offset+len(deliveries) >= total {
				return out
			}
			offset += len(deliveries)
		}
		return out
	}
	for _, message := range s.repo.ListSiteMessages() {
		if message.Enabled && message.PublicPopup {
			out = append(out, message)
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

func (s *Service) siteMessageVisibilityQuery(user core.User, offset, limit int) storage.SiteMessageVisibilityQuery {
	groups := s.siteMessageUserAccountGroupList(user.ID)
	return storage.SiteMessageVisibilityQuery{
		UserID:        user.ID,
		AccountGroups: groups,
		Offset:        offset,
		Limit:         limit,
	}
}

func siteMessageDeliveriesPage(deliveries []core.SiteMessageDelivery, offset, limit int) ([]core.SiteMessageDelivery, int) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 25
	}
	total := len(deliveries)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return append([]core.SiteMessageDelivery(nil), deliveries[offset:end]...), total
}

func (s *Service) siteMessageVisibleToUser(message core.SiteMessage, user core.User) bool {
	return s.siteMessageVisibleToUserWithGroups(message, user, nil)
}

func (s *Service) siteMessageDeliveryVisibleInAdminInbox(delivery core.SiteMessageDelivery, user core.User) bool {
	message := delivery.Message
	if !user.IsAdmin() {
		return false
	}
	if !isSystemUserNotification(message) {
		return true
	}
	for _, targetUserID := range message.TargetUserIDs {
		if strings.TrimSpace(targetUserID) == user.ID {
			return true
		}
	}
	return false
}

func isSystemUserNotification(message core.SiteMessage) bool {
	if len(message.TargetUserIDs) == 0 {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(message.CreatedBy), "system") {
		return true
	}
	return strings.TrimSpace(message.Title) == "套餐赠送到账" && strings.Contains(message.Body, "管理员已赠送套餐")
}

func (s *Service) siteMessageDeliveryVisibleInInbox(delivery core.SiteMessageDelivery, user core.User, userGroups *map[string]struct{}) bool {
	message := delivery.Message
	if user.IsAdmin() && message.PublicPopup {
		return true
	}
	if message.PublicPopup {
		return false
	}
	var groups map[string]struct{}
	if userGroups != nil {
		groups = *userGroups
	}
	if len(message.TargetAccountGroups) > 0 && groups == nil {
		groups = s.siteMessageUserAccountGroups(user.ID)
		if userGroups != nil {
			*userGroups = groups
		}
	}
	return s.siteMessageVisibleToUserWithGroups(message, user, groups)
}

func (s *Service) siteMessageVisibleToUserWithGroups(message core.SiteMessage, user core.User, userGroups map[string]struct{}) bool {
	if strings.TrimSpace(user.ID) == "" {
		return false
	}
	if len(message.TargetUserIDs) == 0 && len(message.TargetAccountGroups) == 0 {
		return true
	}
	for _, targetUserID := range message.TargetUserIDs {
		if strings.TrimSpace(targetUserID) == user.ID {
			return true
		}
	}
	if len(message.TargetAccountGroups) == 0 {
		return false
	}
	if userGroups == nil {
		userGroups = s.siteMessageUserAccountGroups(user.ID)
	}
	for _, targetGroup := range message.TargetAccountGroups {
		if _, ok := userGroups[strings.ToLower(normalizeAccountGroup(targetGroup))]; ok {
			return true
		}
	}
	return false
}

func (s *Service) siteMessageUserAccountGroups(userID string) map[string]struct{} {
	userGroups := make(map[string]struct{})
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return userGroups
	}
	if pager, ok := s.repo.(clientSummaryOwnerPager); ok {
		const pageSize = 100
		for offset := 0; ; {
			clients, total := pager.ListClientSummariesByOwnerPage(userID, offset, pageSize)
			for _, client := range clients {
				userGroups[strings.ToLower(normalizeAccountGroup(client.AccountGroup))] = struct{}{}
			}
			if len(clients) == 0 || offset+len(clients) >= total {
				return userGroups
			}
			offset += len(clients)
		}
	}
	for _, client := range s.repo.ListClientsByOwner(userID) {
		userGroups[strings.ToLower(normalizeAccountGroup(client.AccountGroup))] = struct{}{}
	}
	return userGroups
}

func (s *Service) siteMessageUserAccountGroupList(userID string) []string {
	userGroups := s.siteMessageUserAccountGroups(userID)
	out := make([]string, 0, len(userGroups))
	for group := range userGroups {
		out = append(out, group)
	}
	return out
}

func (s *Service) normalizeSiteMessageTargetUsers(values []string) ([]string, error) {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(values))
	for _, value := range values {
		id, err := s.resolveSiteMessageTargetUserID(value)
		if err != nil {
			return nil, err
		}
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

func (s *Service) resolveSiteMessageTargetUserID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	user, err := s.repo.GetUser(value)
	if err == nil {
		return user.ID, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return "", err
	}
	user, err = s.repo.FindUserByUsername(value)
	if err != nil {
		return "", err
	}
	return user.ID, nil
}

func (s *Service) normalizeSiteMessageTargetGroups(values []string) ([]string, error) {
	available := map[string]string{}
	for _, group := range accountGroupsWithDefault(s.repo.ListAccountGroups()) {
		name := normalizeAccountGroup(group.Name)
		available[strings.ToLower(name)] = name
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, len(values))
	for _, value := range values {
		group := normalizeAccountGroup(value)
		if group == "" {
			return nil, fmt.Errorf("account group is required")
		}
		normalized, ok := available[strings.ToLower(group)]
		if !ok {
			return nil, fmt.Errorf("account group %q was not found", group)
		}
		key := strings.ToLower(normalized)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, normalized)
	}
	return out, nil
}

func generateSiteMessageID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("msg_%d", time.Now().UnixNano())
	}
	return "msg_" + hex.EncodeToString(buf)
}
