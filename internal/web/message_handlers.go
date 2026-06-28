package web

import (
	"net/http"
	"strings"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
)

const (
	messagePageSize                  = 25
	messageTargetUserCandidateLimit  = 100
	messageTargetUserSearchLimit     = 20
	messageTargetUserCandidateSort   = "username"
	messageTargetUserCandidateSortBy = "asc"
)

type messagePageInfo struct {
	Page      int
	PageSize  int
	Total     int
	HasPrev   bool
	PrevPage  int
	HasNext   bool
	NextPage  int
	FirstItem int
	LastItem  int
}

func (s *Server) handleMessagesPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/messages" {
		http.NotFound(w, r)
		return
	}
	user, ok := currentUserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		locale := resolveLocale(w, r)
		requestedPage := parsePositiveInt(r.URL.Query().Get("page"), 1)
		pageMessages, totalMessages := s.control.ListSiteMessagesPage(user, messagePageOffset(requestedPage, messagePageSize), messagePageSize)
		pageInfo := messagePageInfoForTotal(requestedPage, messagePageSize, totalMessages)
		if pageInfo.Page != requestedPage {
			pageMessages, totalMessages = s.control.ListSiteMessagesPage(user, messagePageOffset(pageInfo.Page, messagePageSize), messagePageSize)
			pageInfo = messagePageInfoForTotal(pageInfo.Page, messagePageSize, totalMessages)
		}
		users := []core.User{user}
		var accountGroups []core.AccountGroup
		if user.IsAdmin() {
			users = s.messageTargetUsersForPage(user, pageMessages)
			accountGroups = s.control.ListAccountGroups()
		}
		data := withCSRFData(map[string]any{
			"TitleKey":      "page_title_messages",
			"ActiveNav":     "messages",
			"Locale":        locale,
			"Messages":      pageMessages,
			"MessagePage":   pageInfo,
			"MessageTotal":  totalMessages,
			"Users":         users,
			"AccountGroups": accountGroups,
		}, r)
		s.render(w, "messages.html", locale, data)
	case http.MethodPost:
		if !user.IsAdmin() {
			s.writeAdminRequired(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			s.redirectWithNoticeError(w, r, "/messages", err)
			return
		}
		message, err := s.control.CreateSiteMessage(user, controlplane.SiteMessageInput{
			Title:               r.FormValue("title"),
			Body:                r.FormValue("body"),
			Enabled:             r.FormValue("enabled") != "",
			Popup:               siteMessagePopupFromForm(r),
			PublicPopup:         siteMessagePublicPopupFromForm(r),
			TargetUserIDs:       siteMessageTargetUsersFromForm(r),
			TargetAccountGroups: siteMessageTargetGroupsFromForm(r),
		})
		if err != nil {
			s.recordAdminAudit(r, "error", "site_message.create", "site_message", "", strings.TrimSpace(r.FormValue("title")), err.Error())
			s.redirectWithNoticeError(w, r, "/messages", err)
			return
		}
		s.recordAdminAudit(r, "ok", "site_message.create", "site_message", message.ID, message.Title, "")
		s.publishSiteMessagesChanged()
		http.Redirect(w, r, "/messages", http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleMessageUserSearch(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/messages/users/search" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, ok := currentUserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if !user.IsAdmin() {
		s.writeAdminRequired(w, r)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeJSON(w, http.StatusOK, map[string]any{"users": []map[string]string{}})
		return
	}
	result, _ := s.control.ListUsersPage(controlplane.UserListFilter{
		Query:     query,
		Page:      1,
		PageSize:  messageTargetUserSearchLimit,
		Sort:      messageTargetUserCandidateSort,
		Direction: messageTargetUserCandidateSortBy,
	})
	users := make([]map[string]string, 0, len(result.Items))
	for _, item := range result.Items {
		user := item.User
		id := strings.TrimSpace(user.ID)
		if id == "" {
			continue
		}
		username := strings.TrimSpace(user.Username)
		if username == "" {
			username = id
		}
		users = append(users, map[string]string{"id": id, "username": username})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

func (s *Server) messageTargetUsersForPage(currentUser core.User, deliveries []core.SiteMessageDelivery) []core.User {
	if !currentUser.IsAdmin() {
		return []core.User{currentUser}
	}
	seen := make(map[string]struct{}, messageTargetUserCandidateLimit+len(deliveries))
	users := make([]core.User, 0, messageTargetUserCandidateLimit)
	addUser := func(user core.User) {
		id := strings.TrimSpace(user.ID)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		users = append(users, user)
	}

	page, _ := s.control.ListUsersPage(controlplane.UserListFilter{
		Page:      1,
		PageSize:  messageTargetUserCandidateLimit,
		Sort:      messageTargetUserCandidateSort,
		Direction: messageTargetUserCandidateSortBy,
	})
	for _, item := range page.Items {
		addUser(item.User)
	}
	for _, delivery := range deliveries {
		for _, targetUserID := range delivery.Message.TargetUserIDs {
			targetUserID = strings.TrimSpace(targetUserID)
			if targetUserID == "" {
				continue
			}
			if _, ok := seen[targetUserID]; ok {
				continue
			}
			targetUser, err := s.control.GetUser(targetUserID)
			if err != nil {
				addUser(core.User{ID: targetUserID, Username: targetUserID})
				continue
			}
			addUser(targetUser)
		}
	}
	return users
}

func messagePageOffset(page, pageSize int) int {
	if pageSize <= 0 {
		pageSize = messagePageSize
	}
	if page < 1 {
		page = 1
	}
	return (page - 1) * pageSize
}

func messagePageInfoForTotal(page, pageSize, total int) messagePageInfo {
	if pageSize <= 0 {
		pageSize = messagePageSize
	}
	if page < 1 {
		page = 1
	}
	lastPage := 1
	if total > 0 {
		lastPage = (total + pageSize - 1) / pageSize
	}
	if page > lastPage {
		page = lastPage
	}
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	info := messagePageInfo{
		Page:      page,
		PageSize:  pageSize,
		Total:     total,
		HasPrev:   page > 1,
		PrevPage:  page - 1,
		HasNext:   end < total,
		NextPage:  page + 1,
		FirstItem: start + 1,
		LastItem:  end,
	}
	if total == 0 {
		info.FirstItem = 0
	}
	return info
}

func (s *Server) handleMessageActions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, ok := currentUserFromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/messages/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	switch parts[1] {
	case "read":
		if err := s.control.MarkSiteMessageRead(user, parts[0]); err != nil {
			s.redirectWithNoticeError(w, r, "/messages", err)
			return
		}
		s.publishSiteMessageUnread(user)
	case "update":
		if !user.IsAdmin() {
			s.writeAdminRequired(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			s.redirectWithNoticeError(w, r, "/messages", err)
			return
		}
		before, beforeErr := s.control.GetSiteMessage(user, parts[0])
		hasBefore := beforeErr == nil
		message, err := s.control.UpdateSiteMessage(user, parts[0], controlplane.SiteMessageInput{
			Title:               r.FormValue("title"),
			Body:                r.FormValue("body"),
			Enabled:             r.FormValue("enabled") != "",
			Popup:               siteMessagePopupFromForm(r),
			PublicPopup:         siteMessagePublicPopupFromForm(r),
			TargetUserIDs:       siteMessageTargetUsersFromForm(r),
			TargetAccountGroups: siteMessageTargetGroupsFromForm(r),
		})
		if err != nil {
			s.recordAdminAudit(r, "error", "site_message.update", "site_message", parts[0], strings.TrimSpace(r.FormValue("title")), err.Error())
			s.redirectWithNoticeError(w, r, "/messages", err)
			return
		}
		s.recordAdminAudit(r, "ok", "site_message.update", "site_message", message.ID, message.Title, auditSiteMessageUpdateChangeMessage(before, message, hasBefore))
		s.publishSiteMessagesChanged()
	case "delete":
		if !user.IsAdmin() {
			s.writeAdminRequired(w, r)
			return
		}
		message, err := s.control.DeleteSiteMessage(user, parts[0])
		if err != nil {
			s.recordAdminAudit(r, "error", "site_message.delete", "site_message", parts[0], "", err.Error())
			s.redirectWithNoticeError(w, r, "/messages", err)
			return
		}
		s.recordAdminAudit(r, "ok", "site_message.delete", "site_message", message.ID, message.Title, "")
		s.publishSiteMessagesChanged()
	default:
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/messages", http.StatusSeeOther)
}

func siteMessageTargetUsersFromForm(r *http.Request) []string {
	if strings.TrimSpace(r.FormValue("target_mode")) != "user" {
		return nil
	}
	return r.Form["target_user_id"]
}

func siteMessageTargetGroupsFromForm(r *http.Request) []string {
	if strings.TrimSpace(r.FormValue("target_mode")) != "group" {
		return nil
	}
	return r.Form["target_group"]
}

func siteMessagePublicPopupFromForm(r *http.Request) bool {
	return strings.TrimSpace(r.FormValue("target_mode")) == "website"
}

func siteMessagePopupFromForm(r *http.Request) bool {
	if siteMessagePublicPopupFromForm(r) {
		return false
	}
	return r.FormValue("popup") != ""
}
