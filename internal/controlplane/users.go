package controlplane

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/storage"
)

const (
	passwordHashScheme     = "pbkdf2-sha256"
	passwordHashIterations = 210000
	passwordSaltBytes      = 16
	passwordKeyBytes       = 32
	sessionTokenBytes      = 32
	defaultSessionTTL      = 30 * 24 * time.Hour
)

var (
	ErrInvalidCredentials        = errors.New("invalid credentials")
	ErrInvalidUsernameCharacters = errors.New("username may only contain English letters, numbers, underscores, hyphens, and dots")
)

type UserInput struct {
	Username                          string
	Password                          string
	Role                              core.UserRole
	Enabled                           bool
	ConcurrentRequestLimitOverride    *int
	RequestRateLimitPerMinuteOverride *int
	BalanceNanoUSD                    int64
	Email                             string
	EmailVerified                     bool
	InviterUserID                     string
	RegistrationIP                    string
	RegistrationBrowserFingerprint    string
}

type UserBalanceAdjustment struct {
	AmountNanoUSD int64
	Reason        string
}

type UserListFilter struct {
	Query     string
	Role      core.UserRole
	Status    string
	Inviter   string
	Sort      string
	Direction string
	Page      int
	PageSize  int
}

type UserListItem struct {
	User         core.User
	InviteCount  int64
	SpendNanoUSD int64
}

type UserListPage struct {
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

type UserListResult struct {
	Items          []UserListItem
	Page           UserListPage
	FilteredCount  int
	TotalUserCount int
}

type DeleteUserOptions struct {
	DeleteInvitedUsers bool
}

type DeleteUserResult struct {
	User         core.User
	InvitedUsers []core.User
}

type InvitedUserResult struct {
	User    core.User
	Inviter *core.User
}

type OAuthUserInput struct {
	Provider                       string
	Subject                        string
	Email                          string
	EmailVerified                  bool
	Username                       string
	RegistrationIP                 string
	RegistrationBrowserFingerprint string
}

func (s *Service) EnsureAdminUser(username, password string) (core.User, bool, error) {
	username = strings.TrimSpace(username)
	defaultUsername := username == ""
	if username == "" {
		username = "root"
	}
	defaultPassword := strings.TrimSpace(password) == ""
	if defaultPassword {
		password = "toor"
	}
	if user, err := s.repo.FindUserByUsername(username); err == nil {
		if user.Role == core.UserRoleAdmin {
			return user, false, nil
		}
	} else if !errors.Is(err, storage.ErrNotFound) {
		return core.User{}, false, err
	}
	if user, ok := s.firstAdminUser("enabled"); ok {
		return user, false, nil
	}
	if user, ok := s.firstAdminUser(""); ok {
		return user, false, nil
	}

	user, err := s.CreateUser(UserInput{
		Username: username,
		Password: password,
		Role:     core.UserRoleAdmin,
		Enabled:  true,
	})
	if err != nil {
		return core.User{}, false, err
	}
	if defaultUsername && defaultPassword {
		user.ForcePasswordChange = true
		if err := s.saveUserMetadata(user); err != nil {
			return core.User{}, false, err
		}
		user, err = s.repo.GetUser(user.ID)
		if err != nil {
			return core.User{}, false, err
		}
	}
	return user, true, nil
}

func (s *Service) firstAdminUser(status string) (core.User, bool) {
	items, _, _ := s.repo.ListUsersPage(storage.UserListQuery{
		Role:      core.UserRoleAdmin,
		Status:    status,
		Sort:      "created_at",
		Direction: "asc",
		Limit:     1,
	})
	if len(items) == 0 {
		return core.User{}, false
	}
	return items[0].User, true
}

func (s *Service) ListUsers() []core.User {
	return s.repo.ListUsers()
}

func (s *Service) CountUsersByInviter(inviterID string) int {
	if s == nil || s.repo == nil {
		return 0
	}
	return s.repo.CountUsersByInviter(strings.TrimSpace(inviterID))
}

func (s *Service) ListUsersPage(filter UserListFilter) (UserListResult, bool) {
	if filter.PageSize <= 0 {
		filter.PageSize = 25
	}
	if filter.PageSize > 100 {
		filter.PageSize = 100
	}
	if filter.Page < 1 {
		filter.Page = 1
	}
	offset := (filter.Page - 1) * filter.PageSize
	items, filtered, totalUsers := s.repo.ListUsersPage(storage.UserListQuery{
		Query:     filter.Query,
		Role:      filter.Role,
		Status:    filter.Status,
		Inviter:   filter.Inviter,
		Sort:      filter.Sort,
		Direction: filter.Direction,
		Offset:    offset,
		Limit:     filter.PageSize,
	})
	lastPage := 1
	if filtered > 0 {
		lastPage = (filtered + filter.PageSize - 1) / filter.PageSize
	}
	if filter.Page > lastPage {
		filter.Page = lastPage
		offset = (filter.Page - 1) * filter.PageSize
		items, filtered, totalUsers = s.repo.ListUsersPage(storage.UserListQuery{
			Query:     filter.Query,
			Role:      filter.Role,
			Status:    filter.Status,
			Inviter:   filter.Inviter,
			Sort:      filter.Sort,
			Direction: filter.Direction,
			Offset:    offset,
			Limit:     filter.PageSize,
		})
	}
	end := offset + len(items)
	page := UserListPage{
		Page:      filter.Page,
		PageSize:  filter.PageSize,
		Total:     filtered,
		HasPrev:   filter.Page > 1,
		PrevPage:  filter.Page - 1,
		HasNext:   end < filtered,
		NextPage:  filter.Page + 1,
		FirstItem: offset + 1,
		LastItem:  end,
	}
	if filtered == 0 {
		page.FirstItem = 0
	}
	out := make([]UserListItem, 0, len(items))
	for _, item := range items {
		out = append(out, UserListItem{
			User:         item.User,
			InviteCount:  item.InviteCount,
			SpendNanoUSD: item.SpendNanoUSD,
		})
	}
	return UserListResult{
		Items:          out,
		Page:           page,
		FilteredCount:  filtered,
		TotalUserCount: totalUsers,
	}, true
}

func (s *Service) UserActualSpendTotal(user core.User) int64 {
	if s == nil || s.repo == nil {
		return 0
	}
	userID := strings.TrimSpace(user.ID)
	if userID == "" {
		return 0
	}
	return s.repo.UserActualSpendTotal(userID)
}

func (s *Service) UserBalanceSpendTotal(user core.User) int64 {
	return s.UserActualSpendTotal(user)
}

func (s *Service) UserPaidRechargeTotal(user core.User) int64 {
	if s == nil || s.repo == nil {
		return 0
	}
	userID := strings.TrimSpace(user.ID)
	if userID == "" {
		return 0
	}
	total := int64(0)
	for _, order := range s.repo.ListPaymentOrders(storage.PaymentOrderQuery{UserID: userID, Status: core.PaymentOrderPaid}) {
		if strings.TrimSpace(order.UserID) != userID || order.AmountNanoUSD <= 0 {
			continue
		}
		total = addNanoUSDSaturating(total, order.AmountNanoUSD)
	}
	return total
}

func (s *Service) UserPaidPaymentOrders(user core.User, limit int) []core.PaymentOrder {
	if s == nil || s.repo == nil {
		return nil
	}
	userID := strings.TrimSpace(user.ID)
	if userID == "" {
		return nil
	}
	if limit <= 0 {
		limit = 8
	}
	orders, _ := s.repo.ListPaymentOrdersPage(storage.PaymentOrderQuery{
		UserID: userID,
		Status: core.PaymentOrderPaid,
		Limit:  limit,
	})
	out := orders[:0]
	for _, order := range orders {
		if strings.TrimSpace(order.UserID) == userID {
			out = append(out, order)
		}
	}
	return out
}

func (s *Service) GetUser(id string) (core.User, error) {
	return s.repo.GetUser(strings.TrimSpace(id))
}

func (s *Service) CreateUser(input UserInput) (core.User, error) {
	username, err := normalizeUsername(input.Username)
	if err != nil {
		return core.User{}, err
	}
	if strings.TrimSpace(input.Password) == "" {
		return core.User{}, fmt.Errorf("password is required")
	}
	if input.BalanceNanoUSD < 0 {
		return core.User{}, fmt.Errorf("balance must be zero or greater")
	}
	if _, err := s.repo.FindUserByUsername(username); err == nil {
		return core.User{}, fmt.Errorf("username already exists")
	} else if !errors.Is(err, storage.ErrNotFound) {
		return core.User{}, err
	}
	passwordHash, err := hashPassword(input.Password)
	if err != nil {
		return core.User{}, err
	}
	user := core.User{
		ID:                                fmt.Sprintf("user_%d", time.Now().UnixNano()),
		Username:                          username,
		PasswordHash:                      passwordHash,
		Role:                              normalizeUserRole(input.Role),
		Enabled:                           input.Enabled,
		ConcurrentRequestLimitOverride:    cloneInt(input.ConcurrentRequestLimitOverride),
		RequestRateLimitPerMinuteOverride: cloneInt(input.RequestRateLimitPerMinuteOverride),
		BalanceNanoUSD:                    input.BalanceNanoUSD,
		Email:                             normalizeEmail(input.Email),
		EmailVerified:                     input.EmailVerified && normalizeEmail(input.Email) != "",
		InviterUserID:                     strings.TrimSpace(input.InviterUserID),
		RegistrationIP:                    normalizeRegistrationMetadata(input.RegistrationIP, 128),
		RegistrationBrowserFingerprint:    normalizeRegistrationMetadata(input.RegistrationBrowserFingerprint, 128),
	}
	if err := s.repo.UpsertUser(user); err != nil {
		return core.User{}, err
	}
	return s.repo.GetUser(user.ID)
}

func (s *Service) CreateInvitedUser(input UserInput, inviteCode string) (InvitedUserResult, error) {
	input.Role = core.UserRoleUser
	input.Enabled = true
	settings := s.currentSystemSettings()
	if err := ValidateUsername(input.Username); err != nil {
		return InvitedUserResult{}, err
	}
	if err := validateRegistrationUsernameLength(input.Username, settings.Registration.UsernameMinLength); err != nil {
		return InvitedUserResult{}, err
	}
	var inviter *core.User
	inviteCode = strings.TrimSpace(inviteCode)
	if settings.Registration.RequireInvitationCode {
		if inviteCode == "" || !settings.Invitation.Enabled {
			return InvitedUserResult{}, fmt.Errorf("invitation code is required")
		}
	}
	if inviteCode != "" && settings.Invitation.Enabled {
		resolved, err := s.ResolveInvitationCode(inviteCode)
		if err != nil {
			return InvitedUserResult{}, err
		}
		if resolved.Enabled {
			inviter = &resolved
		}
	}
	if settings.Registration.RequireInvitationCode && inviter == nil {
		return InvitedUserResult{}, fmt.Errorf("invitation code is invalid")
	}
	input.InviterUserID = ""
	if inviter != nil {
		input.InviterUserID = inviter.ID
	}

	user, err := s.CreateUser(input)
	if err != nil {
		return InvitedUserResult{}, err
	}
	if settings.Registration.NewUserRewardEnabled && settings.Registration.NewUserRewardNanoUSD > 0 {
		if _, _, err := s.repo.AdjustUserBalance(user.ID, settings.Registration.NewUserRewardNanoUSD, registrationRewardNote()); err != nil {
			return InvitedUserResult{}, err
		}
		if updated, err := s.repo.GetUser(user.ID); err == nil {
			user = updated
		}
	}
	if inviter == nil {
		return InvitedUserResult{User: user}, nil
	}

	if settings.Invitation.InviteeRewardNanoUSD > 0 {
		if _, _, err := s.repo.AdjustUserBalance(user.ID, settings.Invitation.InviteeRewardNanoUSD, inviteRewardNote("invitee", inviter.Username)); err != nil {
			return InvitedUserResult{}, err
		}
		if updated, err := s.repo.GetUser(user.ID); err == nil {
			user = updated
		}
	}
	return InvitedUserResult{User: user, Inviter: inviter}, nil
}

func (s *Service) InvitationCodeForUser(user core.User) string {
	if strings.TrimSpace(user.ID) == "" {
		return ""
	}
	if strings.TrimSpace(user.PasswordHash) == "" {
		stored, err := s.repo.GetUser(user.ID)
		if err != nil {
			return ""
		}
		user = stored
	}
	return "i_" + invitationShortSignature(user)
}

func (s *Service) ResolveInvitationCode(code string) (core.User, error) {
	code = strings.TrimSpace(code)
	if !strings.HasPrefix(code, "i_") {
		return core.User{}, fmt.Errorf("invitation link is invalid")
	}
	signature := strings.TrimPrefix(code, "i_")
	user, err := s.repo.FindUserByInvitationSignature(signature)
	if err != nil || !user.Enabled {
		return core.User{}, fmt.Errorf("invitation link is invalid")
	}
	return user, nil
}

func (s *Service) UpdateUser(id string, input UserInput) (core.User, error) {
	id = strings.TrimSpace(id)
	user, err := s.repo.GetUser(id)
	if err != nil {
		return core.User{}, err
	}
	username, err := normalizeUsername(input.Username)
	if err != nil {
		return core.User{}, err
	}
	if !strings.EqualFold(user.Username, username) {
		if _, err := s.repo.FindUserByUsername(username); err == nil {
			return core.User{}, fmt.Errorf("username already exists")
		} else if !errors.Is(err, storage.ErrNotFound) {
			return core.User{}, err
		}
	}
	nextRole := normalizeUserRole(input.Role)
	nextEnabled := input.Enabled
	if err := s.ensureUserUpdateKeepsAdmin(user, nextRole, nextEnabled); err != nil {
		return core.User{}, err
	}
	user.Username = username
	user.Role = nextRole
	user.Enabled = nextEnabled
	user.ConcurrentRequestLimitOverride = cloneInt(input.ConcurrentRequestLimitOverride)
	user.RequestRateLimitPerMinuteOverride = cloneInt(input.RequestRateLimitPerMinuteOverride)
	if strings.TrimSpace(input.Password) != "" {
		passwordHash, err := hashPassword(input.Password)
		if err != nil {
			return core.User{}, err
		}
		user.PasswordHash = passwordHash
		user.ForcePasswordChange = false
	}
	if err := s.saveUserMetadata(user); err != nil {
		return core.User{}, err
	}
	return s.repo.GetUser(user.ID)
}

func (s *Service) AdjustUserBalance(id string, input UserBalanceAdjustment) (core.User, int64, error) {
	id = strings.TrimSpace(id)
	if input.AmountNanoUSD == 0 {
		return core.User{}, 0, fmt.Errorf("amount must be non-zero")
	}
	previousBalance, _, err := s.repo.AdjustUserBalance(id, input.AmountNanoUSD, input.Reason)
	if errors.Is(err, storage.ErrInsufficientBalance) {
		return core.User{}, 0, fmt.Errorf("balance cannot be negative")
	}
	if err != nil {
		return core.User{}, 0, err
	}
	updated, err := s.repo.GetUser(id)
	if err != nil {
		return core.User{}, 0, err
	}
	return updated, previousBalance, nil
}

func (s *Service) ListBillingLedger(userID string, limit int) []core.BillingLedgerEntry {
	return s.repo.ListBillingLedger(strings.TrimSpace(userID), limit)
}

func (s *Service) ChangeUserPassword(userID, currentPassword, nextPassword string) error {
	user, err := s.repo.GetUser(strings.TrimSpace(userID))
	if err != nil {
		return err
	}
	if !user.Enabled || !verifyPassword(currentPassword, user.PasswordHash) {
		return ErrInvalidCredentials
	}
	if strings.TrimSpace(nextPassword) == "" {
		return fmt.Errorf("password is required")
	}
	passwordHash, err := hashPassword(nextPassword)
	if err != nil {
		return err
	}
	user.PasswordHash = passwordHash
	user.ForcePasswordChange = false
	return s.saveUserMetadata(user)
}

func (s *Service) DeleteUser(id string) (core.User, error) {
	result, err := s.DeleteUserWithOptions(id, DeleteUserOptions{})
	if err != nil {
		return core.User{}, err
	}
	return result.User, nil
}

func (s *Service) DeleteUserWithOptions(id string, options DeleteUserOptions) (DeleteUserResult, error) {
	id = strings.TrimSpace(id)
	user, err := s.repo.GetUser(id)
	if err != nil {
		return DeleteUserResult{}, err
	}

	invitedUsers := []core.User{}
	deleteIDs := map[string]struct{}{id: {}}
	if options.DeleteInvitedUsers {
		invitedUsers = s.directInvitedUsers(id)
		for _, invited := range invitedUsers {
			if invitedID := strings.TrimSpace(invited.ID); invitedID != "" {
				deleteIDs[invitedID] = struct{}{}
			}
		}
	}
	if err := s.ensureUserDeletionKeepsAdmin(deleteIDs); err != nil {
		return DeleteUserResult{}, err
	}

	for _, invited := range invitedUsers {
		if err := s.deleteSingleUser(invited.ID); err != nil {
			return DeleteUserResult{}, err
		}
	}
	if err := s.deleteSingleUser(id); err != nil {
		return DeleteUserResult{}, err
	}
	return DeleteUserResult{User: user, InvitedUsers: invitedUsers}, nil
}

func (s *Service) directInvitedUsers(inviterID string) []core.User {
	inviterID = strings.TrimSpace(inviterID)
	if inviterID == "" {
		return nil
	}
	return s.repo.ListUsersByInviter(inviterID)
}

func (s *Service) ensureUserDeletionKeepsAdmin(deleteIDs map[string]struct{}) error {
	deletesAdmin := false
	excludedIDs := make([]string, 0, len(deleteIDs))
	for userID := range deleteIDs {
		userID = strings.TrimSpace(userID)
		if userID == "" {
			continue
		}
		excludedIDs = append(excludedIDs, userID)
		if user, err := s.repo.GetUser(userID); err == nil && user.Role == core.UserRoleAdmin {
			deletesAdmin = true
		}
	}
	if deletesAdmin && s.repo.CountEnabledAdminsExcluding(excludedIDs) == 0 {
		return fmt.Errorf("cannot delete the last enabled admin user")
	}
	return nil
}

func (s *Service) deleteSingleUser(id string) error {
	id = strings.TrimSpace(id)
	if err := s.repo.DeleteUser(id); err != nil {
		return err
	}
	return nil
}

func (s *Service) AuthenticateUser(username, password string) (core.User, error) {
	user, err := s.findUserByLoginIdentifier(username)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return core.User{}, ErrInvalidCredentials
		}
		return core.User{}, err
	}
	if !user.Enabled || !verifyPassword(password, user.PasswordHash) {
		return core.User{}, ErrInvalidCredentials
	}
	return s.RecordUserLogin(user.ID)
}

func (s *Service) findUserByLoginIdentifier(identifier string) (core.User, error) {
	user, err := s.repo.FindUserByUsername(identifier)
	if err == nil {
		return user, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return core.User{}, err
	}
	if !strings.Contains(strings.TrimSpace(identifier), "@") {
		return core.User{}, storage.ErrNotFound
	}
	return s.repo.FindUserByEmail(identifier)
}

func (s *Service) AuthenticateOAuthUser(input OAuthUserInput) (core.User, bool, error) {
	provider := strings.ToLower(strings.TrimSpace(input.Provider))
	subject := strings.TrimSpace(input.Subject)
	if provider == "" || subject == "" {
		return core.User{}, false, fmt.Errorf("oauth identity is incomplete")
	}
	if user, ok := s.findUserByOAuthIdentity(provider, subject); ok {
		if !user.Enabled {
			return core.User{}, false, ErrInvalidCredentials
		}
		loggedIn, err := s.RecordUserLogin(user.ID)
		return loggedIn, false, err
	}
	settings := s.currentSystemSettings()
	if !settings.OAuth.LoginAutoCreateUser {
		return core.User{}, false, fmt.Errorf("oauth account is not linked to a user")
	}
	if settings.Registration.RequireInvitationCode || settings.Registration.TurnstileEnabled {
		return core.User{}, false, fmt.Errorf("oauth auto-create is disabled by registration security settings")
	}
	password, err := randomHex(24)
	if err != nil {
		return core.User{}, false, err
	}
	user, err := s.CreateUser(UserInput{
		Username:                       s.availableOAuthUsername(provider, input.Username, input.Email, subject),
		Password:                       password,
		Role:                           core.UserRoleUser,
		Enabled:                        true,
		Email:                          input.Email,
		EmailVerified:                  input.EmailVerified,
		RegistrationIP:                 input.RegistrationIP,
		RegistrationBrowserFingerprint: input.RegistrationBrowserFingerprint,
	})
	if err != nil {
		return core.User{}, false, err
	}
	user.OAuthIdentities = append(user.OAuthIdentities, core.UserOAuthIdentity{
		Provider: provider,
		Subject:  subject,
		Email:    strings.TrimSpace(input.Email),
		Username: strings.TrimSpace(input.Username),
		LinkedAt: time.Now().UTC(),
	})
	if err := s.saveUserMetadata(user); err != nil {
		return core.User{}, false, err
	}
	if settings.Registration.NewUserRewardEnabled && settings.Registration.NewUserRewardNanoUSD > 0 {
		if _, _, err := s.repo.AdjustUserBalance(user.ID, settings.Registration.NewUserRewardNanoUSD, registrationRewardNote()); err != nil {
			return core.User{}, false, err
		}
	}
	loggedIn, err := s.RecordUserLogin(user.ID)
	return loggedIn, true, err
}

func (s *Service) LinkOAuthIdentity(userID string, input OAuthUserInput) (core.User, error) {
	userID = strings.TrimSpace(userID)
	provider := normalizeOAuthProvider(input.Provider)
	subject := strings.TrimSpace(input.Subject)
	if userID == "" {
		return core.User{}, fmt.Errorf("user id is required")
	}
	if provider == "" {
		return core.User{}, fmt.Errorf("oauth provider is not supported")
	}
	if subject == "" {
		return core.User{}, fmt.Errorf("oauth identity is incomplete")
	}
	user, err := s.repo.GetUser(userID)
	if err != nil {
		return core.User{}, err
	}
	if linked, ok := s.findUserByOAuthIdentity(provider, subject); ok && linked.ID != user.ID {
		return core.User{}, fmt.Errorf("oauth account is already linked to another user")
	}
	now := time.Now().UTC()
	identity := core.UserOAuthIdentity{
		Provider: provider,
		Subject:  subject,
		Email:    strings.TrimSpace(input.Email),
		Username: strings.TrimSpace(input.Username),
		LinkedAt: now,
	}
	replaced := false
	for i, existing := range user.OAuthIdentities {
		if strings.EqualFold(existing.Provider, provider) {
			if strings.TrimSpace(existing.Subject) != "" && strings.TrimSpace(existing.Subject) != subject {
				return core.User{}, fmt.Errorf("%s account is already linked to the current user", provider)
			}
			if !existing.LinkedAt.IsZero() {
				identity.LinkedAt = existing.LinkedAt
			}
			user.OAuthIdentities[i] = identity
			replaced = true
			break
		}
	}
	if !replaced {
		user.OAuthIdentities = append(user.OAuthIdentities, identity)
	}
	if err := s.saveUserMetadata(user); err != nil {
		return core.User{}, err
	}
	return s.repo.GetUser(user.ID)
}

func (s *Service) OAuthIdentityOwner(provider, subject string) (core.User, bool) {
	provider = normalizeOAuthProvider(provider)
	subject = strings.TrimSpace(subject)
	if provider == "" || subject == "" {
		return core.User{}, false
	}
	return s.findUserByOAuthIdentity(provider, subject)
}

func (s *Service) MergeOAuthUser(targetUserID, sourceUserID, provider, subject string) (core.User, core.User, error) {
	targetUserID = strings.TrimSpace(targetUserID)
	sourceUserID = strings.TrimSpace(sourceUserID)
	provider = normalizeOAuthProvider(provider)
	subject = strings.TrimSpace(subject)
	if targetUserID == "" || sourceUserID == "" || targetUserID == sourceUserID {
		return core.User{}, core.User{}, fmt.Errorf("source and target users are required")
	}
	if provider == "" || subject == "" {
		return core.User{}, core.User{}, fmt.Errorf("oauth identity is incomplete")
	}
	target, err := s.repo.GetUser(targetUserID)
	if err != nil {
		return core.User{}, core.User{}, err
	}
	source, err := s.repo.GetUser(sourceUserID)
	if err != nil {
		return core.User{}, core.User{}, err
	}
	if owner, ok := s.findUserByOAuthIdentity(provider, subject); !ok || owner.ID != source.ID {
		return core.User{}, core.User{}, fmt.Errorf("oauth account is not linked to the source user")
	}
	mergedIdentities, err := mergeOAuthIdentities(target.OAuthIdentities, source.OAuthIdentities)
	if err != nil {
		return core.User{}, core.User{}, err
	}
	nextBalance, err := addUserBalances(target.BalanceNanoUSD, source.BalanceNanoUSD)
	if err != nil {
		return core.User{}, core.User{}, err
	}
	target.OAuthIdentities = mergedIdentities
	target.BalanceNanoUSD = nextBalance
	if strings.TrimSpace(target.Email) == "" {
		target.Email = strings.TrimSpace(source.Email)
		target.EmailVerified = source.EmailVerified
	}
	source.Enabled = false
	source.BalanceNanoUSD = 0
	source.OAuthIdentities = nil
	if err := s.repo.MergeUsers(source, target); err != nil {
		return core.User{}, core.User{}, err
	}
	updatedTarget, err := s.repo.GetUser(target.ID)
	if err != nil {
		return core.User{}, core.User{}, err
	}
	updatedSource, err := s.repo.GetUser(source.ID)
	if err != nil {
		return core.User{}, core.User{}, err
	}
	return updatedTarget, updatedSource, nil
}

func (s *Service) UnlinkOAuthIdentity(userID, provider string) (core.User, error) {
	userID = strings.TrimSpace(userID)
	provider = normalizeOAuthProvider(provider)
	if userID == "" {
		return core.User{}, fmt.Errorf("user id is required")
	}
	if provider == "" {
		return core.User{}, fmt.Errorf("oauth provider is not supported")
	}
	user, err := s.repo.GetUser(userID)
	if err != nil {
		return core.User{}, err
	}
	next := user.OAuthIdentities[:0]
	for _, identity := range user.OAuthIdentities {
		if strings.EqualFold(identity.Provider, provider) {
			continue
		}
		next = append(next, identity)
	}
	user.OAuthIdentities = next
	if err := s.saveUserMetadata(user); err != nil {
		return core.User{}, err
	}
	return s.repo.GetUser(user.ID)
}

func (s *Service) RecordUserLogin(userID string) (core.User, error) {
	userID = strings.TrimSpace(userID)
	now := time.Now().UTC()
	if repo, ok := s.repo.(userLastUsedToucher); ok {
		if err := repo.TouchUserLastUsedAt(userID, now); err != nil {
			return core.User{}, err
		}
		return s.repo.GetUser(userID)
	}
	user, err := s.repo.GetUser(userID)
	if err != nil {
		return core.User{}, err
	}
	user.LastLoginAt = &now
	if err := s.saveUserMetadata(user); err != nil {
		return core.User{}, err
	}
	return s.repo.GetUser(user.ID)
}

func (s *Service) saveUserMetadata(user core.User) error {
	if repo, ok := s.repo.(userMetadataUpdater); ok {
		return repo.UpdateUserMetadata(user)
	}
	return s.repo.UpsertUser(user)
}

func (s *Service) CreateUserSession(userID string) (string, core.UserSession, error) {
	token, err := randomHex(sessionTokenBytes)
	if err != nil {
		return "", core.UserSession{}, err
	}
	now := time.Now().UTC()
	session := core.UserSession{
		TokenHash: sessionTokenHash(token),
		UserID:    strings.TrimSpace(userID),
		ExpiresAt: now.Add(defaultSessionTTL),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.repo.DeleteExpiredUserSessions(now); err != nil {
		return "", core.UserSession{}, err
	}
	if err := s.repo.UpsertUserSession(session); err != nil {
		return "", core.UserSession{}, err
	}
	return token, session, nil
}

func (s *Service) UserBySessionToken(token string) (core.User, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return core.User{}, storage.ErrNotFound
	}
	session, err := s.repo.GetUserSession(sessionTokenHash(token))
	if err != nil {
		return core.User{}, err
	}
	now := time.Now().UTC()
	if !session.ExpiresAt.After(now) {
		_ = s.repo.DeleteUserSession(session.TokenHash)
		return core.User{}, storage.ErrNotFound
	}
	user, err := s.repo.GetUser(session.UserID)
	if err != nil {
		return core.User{}, err
	}
	if !user.Enabled {
		_ = s.repo.DeleteUserSession(session.TokenHash)
		return core.User{}, ErrInvalidCredentials
	}
	return user, nil
}

func (s *Service) DeleteUserSessionToken(token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	if err := s.repo.DeleteUserSession(sessionTokenHash(token)); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	return nil
}

func (s *Service) ensureUserUpdateKeepsAdmin(user core.User, nextRole core.UserRole, nextEnabled bool) error {
	if user.Role != core.UserRoleAdmin {
		return nil
	}
	if nextRole == core.UserRoleAdmin && nextEnabled {
		return nil
	}
	if s.enabledAdminCountExcluding(user.ID) == 0 {
		return fmt.Errorf("cannot disable or demote the last enabled admin user")
	}
	return nil
}

func (s *Service) enabledAdminCountExcluding(excludedID string) int {
	return s.repo.CountEnabledAdminsExcluding([]string{excludedID})
}

func (s *Service) defaultAdminUserID() string {
	user, ok := s.firstAdminUser("enabled")
	if !ok {
		return ""
	}
	return user.ID
}

func normalizeUsername(username string) (string, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return "", fmt.Errorf("username is required")
	}
	if len(username) > 64 {
		return "", fmt.Errorf("username must be 64 characters or fewer")
	}
	for _, r := range username {
		if !isAllowedUsernameRune(r) {
			return "", ErrInvalidUsernameCharacters
		}
	}
	return username, nil
}

func ValidateUsername(username string) error {
	_, err := normalizeUsername(username)
	return err
}

func isAllowedUsernameRune(r rune) bool {
	return (r >= 'A' && r <= 'Z') ||
		(r >= 'a' && r <= 'z') ||
		(r >= '0' && r <= '9') ||
		r == '_' ||
		r == '-' ||
		r == '.'
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func validateRegistrationUsernameLength(username string, minLength int) error {
	username = strings.TrimSpace(username)
	if minLength <= 1 {
		return nil
	}
	if len([]rune(username)) < minLength {
		return fmt.Errorf("username must be at least %d characters", minLength)
	}
	return nil
}

func normalizeRegistrationMetadata(value string, maxLength int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if maxLength <= 0 || len(runes) <= maxLength {
		return value
	}
	return string(runes[:maxLength])
}

func normalizeUserRole(role core.UserRole) core.UserRole {
	switch role {
	case core.UserRoleAdmin:
		return core.UserRoleAdmin
	default:
		return core.UserRoleUser
	}
}

func normalizeOAuthProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "github":
		return "github"
	case "google":
		return "google"
	case "linuxdo":
		return "linuxdo"
	default:
		return ""
	}
}

func mergeOAuthIdentities(target []core.UserOAuthIdentity, source []core.UserOAuthIdentity) ([]core.UserOAuthIdentity, error) {
	out := append([]core.UserOAuthIdentity(nil), target...)
	for _, sourceIdentity := range source {
		sourceProvider := normalizeOAuthProvider(sourceIdentity.Provider)
		sourceSubject := strings.TrimSpace(sourceIdentity.Subject)
		if sourceProvider == "" || sourceSubject == "" {
			continue
		}
		sourceIdentity.Provider = sourceProvider
		sourceIdentity.Subject = sourceSubject
		found := false
		for _, targetIdentity := range out {
			if !strings.EqualFold(targetIdentity.Provider, sourceProvider) {
				continue
			}
			found = true
			if strings.TrimSpace(targetIdentity.Subject) != sourceSubject {
				return nil, fmt.Errorf("%s account is already linked to the current user", sourceProvider)
			}
			break
		}
		if !found {
			out = append(out, sourceIdentity)
		}
	}
	return out, nil
}

func addUserBalances(a, b int64) (int64, error) {
	if b > 0 && a > int64(^uint64(0)>>1)-b {
		return 0, fmt.Errorf("amount overflow")
	}
	if b < 0 && a < -int64(^uint64(0)>>1)-1-b {
		return 0, fmt.Errorf("amount overflow")
	}
	return a + b, nil
}

func (s *Service) findUserByOAuthIdentity(provider, subject string) (core.User, bool) {
	user, err := s.repo.FindUserByOAuthIdentity(provider, strings.TrimSpace(subject))
	if err != nil {
		return core.User{}, false
	}
	return user, true
}

func (s *Service) availableOAuthUsername(provider, username, email, subject string) string {
	base := strings.TrimSpace(username)
	if base == "" && strings.Contains(email, "@") {
		base = strings.Split(strings.TrimSpace(email), "@")[0]
	}
	base = oauthUsernameToken(base)
	if base == "" {
		base = oauthUsernameToken(shortID(subject))
	}
	if base == "" {
		base = "user"
	}
	prefix := oauthUsernameToken(provider)
	if prefix == "" {
		prefix = "oauth"
	}
	suffix := oauthUsernameToken(shortID(subject))
	if suffix == "" {
		suffix = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	candidates := []string{
		oauthUsernameCandidate(prefix, base, ""),
		oauthUsernameCandidate(prefix, base, suffix),
	}
	for i := 2; i <= 100; i++ {
		candidates = append(candidates, oauthUsernameCandidate(prefix, base, fmt.Sprintf("%s%d", suffix, i)))
	}
	for _, candidate := range candidates {
		if _, err := s.repo.FindUserByUsername(candidate); errors.Is(err, storage.ErrNotFound) {
			return candidate
		}
	}
	return oauthUsernameCandidate(prefix, base, fmt.Sprintf("%d", time.Now().UnixNano()))
}

func oauthUsernameCandidate(prefix, base, suffix string) string {
	extra := 1
	if suffix != "" {
		extra += len(suffix) + 1
	}
	maxBaseLen := 64 - len(prefix) - extra
	if maxBaseLen < 1 {
		maxBaseLen = 1
	}
	if len(base) > maxBaseLen {
		base = base[:maxBaseLen]
	}
	if suffix == "" {
		return prefix + "_" + base
	}
	return prefix + "_" + base + "_" + suffix
}

func oauthUsernameToken(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	for _, r := range value {
		switch {
		case isAllowedUsernameRune(r):
			builder.WriteRune(r)
		}
	}
	return strings.Trim(builder.String(), "._-")
}

func hashPassword(password string) (string, error) {
	salt := make([]byte, passwordSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, passwordHashIterations, passwordKeyBytes)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{
		passwordHashScheme,
		strconv.Itoa(passwordHashIterations),
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	}, "$"), nil
}

func verifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != passwordHashScheme {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations <= 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(expected) == 0 {
		return false
	}
	actual, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(expected))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func randomHex(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func sessionTokenHash(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

func invitationShortSignature(user core.User) string {
	return core.UserInvitationSignature(user)
}

func inviteRewardNote(kind, username string) string {
	username = strings.TrimSpace(username)
	if username == "" {
		return "invite reward: " + strings.TrimSpace(kind)
	}
	return fmt.Sprintf("invite reward: %s %s", strings.TrimSpace(kind), username)
}

func registrationRewardNote() string {
	return "new user reward"
}
