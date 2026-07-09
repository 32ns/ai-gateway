package web

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/32ns/ai-gateway/internal/backup"
	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/failover"
	"github.com/32ns/ai-gateway/internal/gateway"
	"github.com/32ns/ai-gateway/internal/providers"
)

//go:embed templates/*.html static/*.css static/*.ico static/*.js static/*.png static/*.svg static/css/*.css static/js/*.js
var assets embed.FS

type Server struct {
	control                  *controlplane.Service
	gateway                  *gateway.Service
	imageLabJobs             *imageLabJobManager
	configPath               string
	statePath                string
	databaseBackend          string
	postgresDSN              string
	masterKey                string
	protocolRequestBodyLimit int64
	restoreBackup            func(string, backup.Options, string) (string, error)
	personalPayAndroid       http.Handler
	consoleEvents            *consoleEventBus
	support                  *supportHub
	allowPublicRegistration  bool
	trustedProxies           trustedProxySet
	assetFS                  fs.FS
	templates                map[string]*template.Template
	templateErr              error
	registrationLimiter      *ipRateLimiter
	loginLimiter             *ipRateLimiter
	oauthMergeMu             sync.Mutex
	oauthMergeStates         map[string]profileOAuthMergeState
}

const consoleCSRFCookieName = "ag_console_csrf"
const consoleSessionCookieName = "ag_console_session"
const defaultProtocolRequestBodyLimit = 64 << 20
const maxStreamResponseContentRunes = 4096
const defaultConsoleSessionMaxAge = 30 * 24 * time.Hour
const maxTemplateInt64 = int64(^uint64(0) >> 1)
const gatewayProtocolErrorType = "gateway_error"
const gatewayProtocolErrorMessage = "The gateway could not complete the request. Please retry later."

type consoleCSRFContextKey struct{}
type consoleUserContextKey struct{}
type siteMessageUnreadCountContextKey struct{}
type supportUnreadCountContextKey struct{}
type siteMessagePopupDeliveriesContextKey struct{}
type protocolClientContextKey struct{}

type limitedTextBuilder struct {
	builder   strings.Builder
	limit     int
	count     int
	truncated bool
}

func (b *limitedTextBuilder) WriteString(value string) {
	if value == "" || b.limit <= 0 || b.truncated {
		return
	}
	for _, r := range value {
		if b.count >= b.limit {
			b.truncated = true
			return
		}
		b.builder.WriteRune(r)
		b.count++
	}
}

func (b *limitedTextBuilder) String() string {
	if b.truncated {
		return b.builder.String() + "...[truncated]"
	}
	return b.builder.String()
}

func NewServer(control *controlplane.Service, gatewayService *gateway.Service, statePath string) *Server {
	return NewServerWithOptions(control, gatewayService, ServerOptions{StatePath: statePath})
}

type ServerOptions struct {
	ConfigPath               string
	StatePath                string
	DatabaseBackend          string
	PostgresDSN              string
	MasterKey                string
	ProtocolRequestBodyLimit int64
	RestoreBackup            func(string, backup.Options, string) (string, error)
	PersonalPayAndroid       http.Handler
	AllowPublicRegistration  bool
	TrustedProxyCIDRs        []string
}

func NewServerWithOptions(control *controlplane.Service, gatewayService *gateway.Service, options ServerOptions) *Server {
	assetFS := fs.FS(assets)
	templates, err := compileTemplates(assetFS)
	server := &Server{
		control:         control,
		gateway:         gatewayService,
		imageLabJobs:    newImageLabJobManager(),
		configPath:      strings.TrimSpace(options.ConfigPath),
		statePath:       strings.TrimSpace(options.StatePath),
		databaseBackend: strings.TrimSpace(options.DatabaseBackend),
		postgresDSN:     strings.TrimSpace(options.PostgresDSN),
		masterKey:       strings.TrimSpace(options.MasterKey),
		protocolRequestBodyLimit: func() int64 {
			if options.ProtocolRequestBodyLimit <= 0 {
				return defaultProtocolRequestBodyLimit
			}
			return options.ProtocolRequestBodyLimit
		}(),
		restoreBackup:           options.RestoreBackup,
		personalPayAndroid:      options.PersonalPayAndroid,
		consoleEvents:           newConsoleEventBus(),
		support:                 newSupportHub(),
		allowPublicRegistration: options.AllowPublicRegistration,
		trustedProxies:          newTrustedProxySet(options.TrustedProxyCIDRs),
		assetFS:                 assetFS,
		templates:               templates,
		templateErr:             err,
		registrationLimiter:     newIPRateLimiter(),
		loginLimiter:            newIPRateLimiter(),
		oauthMergeStates:        make(map[string]profileOAuthMergeState),
	}
	server.clearImageLabStoredResults()
	if gatewayService != nil {
		gatewayService.WithBillingEvents(func(event gateway.BillingEvent) {
			if billingEventShouldRefreshFinance(event.Reason) {
				server.publishFinanceChanged(event.Reason, event.RequestID)
			}
			server.publishUsageLogChanged(event.Reason, event.UserID, event.RequestID)
			server.publishBalanceUpdated(event.UserID)
		})
	}
	return server
}

func billingEventShouldRefreshFinance(reason string) bool {
	switch strings.TrimSpace(reason) {
	case "usage_settled", "usage_released", "usage_account_updated":
		return false
	default:
		return true
	}
}

func faviconICO() []byte {
	payload, err := assets.ReadFile("static/favicon.ico")
	if err != nil {
		return nil
	}
	return payload
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	locale := resolveLocale(w, r)
	data := withCSRFData(map[string]any{
		"TitleKey":                "page_title_home",
		"ActiveNav":               "home",
		"Locale":                  locale,
		"AllowPublicRegistration": s.publicRegistrationAllowed(),
		"Home":                    s.currentHomeSettings(),
	}, r)
	s.render(w, "home.html", locale, data)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	user, hasUser := currentUserFromContext(r.Context())
	var state controlplane.Dashboard
	var usage controlplane.UsageLogPage
	var chart controlplane.UsageCostChart
	var dashboardStatus controlplane.MonitorPage
	invitationLink := ""
	settings, err := s.control.GetSystemSettings()
	if err != nil {
		settings = core.DefaultSystemSettings()
	}
	inviteeReward := settings.Invitation.InviteeRewardNanoUSD
	if settings.Registration.NewUserRewardEnabled {
		inviteeReward += settings.Registration.NewUserRewardNanoUSD
	}
	if hasUser && !user.IsAdmin() {
		state = s.control.DashboardForUser(r.Context(), user)
		usage = s.control.UsageLogPage(r.Context(), user, controlplane.UsageLogFilter{Page: 1, PageSize: 5})
		chart = s.control.UsageCostChartForUser(r.Context(), user, time.Now())
		dashboardStatus = s.control.MonitorPage(false)
		if settings.Invitation.Enabled && user.Enabled {
			invitationLink = s.invitationLink(r, user)
		}
	} else {
		state = s.control.Dashboard(r.Context())
	}
	finance := controlplane.FinancePage{}
	if hasUser && user.IsAdmin() {
		finance = s.control.DashboardFinancePage()
	}
	locale := resolveLocale(w, r)
	data := withCSRFData(map[string]any{
		"TitleKey":         "page_title_dashboard",
		"ActiveNav":        "dashboard",
		"Locale":           locale,
		"Now":              time.Now().UTC(),
		"State":            state,
		"DashboardClients": limitDashboardClients(state.Clients),
		"Usage":            usage,
		"UsageChart":       chart,
		"DashboardStatus":  dashboardStatus,
		"Finance":          finance,
		"Payment":          settings.Payment,
		"PersonalPay":      s.control.PersonalPayRuntime(r.Context()),
		"Invitation":       settings.Invitation,
		"UserDashboard":    settings.UserDashboard,
		"InviteeReward":    inviteeReward,
		"InviteLink":       invitationLink,
	}, r)
	s.render(w, "dashboard.html", locale, data)
}

func limitDashboardClients(clients []core.APIClient) []core.APIClient {
	const dashboardClientPreviewLimit = 6
	if len(clients) <= dashboardClientPreviewLimit {
		return clients
	}
	return append([]core.APIClient(nil), clients[:dashboardClientPreviewLimit]...)
}

func (s *Server) invitationLink(r *http.Request, user core.User) string {
	if s == nil || s.control == nil {
		return ""
	}
	code := s.control.InvitationCodeForUser(user)
	if strings.TrimSpace(code) == "" {
		return ""
	}
	return requestOrigin(r) + "/register?invite=" + url.QueryEscape(code)
}

func (s *Server) userDisplayName(userID string) string {
	userID = strings.TrimSpace(userID)
	if userID == "" || s == nil || s.control == nil {
		return userID
	}
	user, err := s.control.GetUser(userID)
	if err != nil || strings.TrimSpace(user.Username) == "" {
		return userID
	}
	return user.Username
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	report := s.control.HealthReport(r.Context())
	status := http.StatusOK
	if report.Status == "error" {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, report)
}

func (s *Server) requireAPIKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, authHeaderOK := protocolAPIKeyFromRequest(r)
		if !authHeaderOK {
			writeProtocolAuthError(w, r, http.StatusUnauthorized, "auth_error", "missing or invalid api key")
			return
		}

		client, err := s.control.AuthorizeProtocolKeyPointer(token)
		if err != nil {
			status := http.StatusInternalServerError
			code := "internal_error"
			message := err.Error()
			var accessErr *controlplane.AccessError
			if errors.As(err, &accessErr) && accessErr != nil {
				status = accessErr.StatusCode
				code = accessErr.Code
				message = accessErr.Message
			}
			writeProtocolAuthError(w, r, status, code, message)
			return
		}

		next.ServeHTTP(w, r.WithContext(withProtocolClient(r.Context(), client)))
	})
}

func writeProtocolAuthError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	if isAnthropicProtocolPath(r) {
		anthropicCode := "authentication_error"
		if status == http.StatusForbidden {
			anthropicCode = "permission_error"
		}
		if strings.TrimSpace(code) == "rate_limit_exceeded" {
			anthropicCode = "rate_limit_error"
		}
		writeJSON(w, status, map[string]any{
			"type": "error",
			"error": map[string]string{
				"type":    anthropicCode,
				"message": message,
			},
		})
		return
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"type":    code,
			"message": message,
		},
	})
}

func isAnthropicProtocolPath(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	switch strings.TrimRight(r.URL.Path, "/") {
	case "/v1/messages", "/v1/messages/count_tokens", "/api/v1/messages", "/api/v1/messages/count_tokens", "/anthropic/v1/messages", "/anthropic/v1/messages/count_tokens":
		return true
	default:
		return false
	}
}

func protocolAPIKeyFromRequest(r *http.Request) (string, bool) {
	if r == nil {
		return "", true
	}
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if authHeader == "" {
		return strings.TrimSpace(r.Header.Get("X-API-Key")), true
	}
	parts := strings.Fields(authHeader)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		return "", false
	}
	return strings.TrimSpace(parts[1]), true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json") ||
		strings.EqualFold(r.Header.Get("X-Requested-With"), "fetch")
}

func writeSSEJSON(w io.Writer, eventName string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if eventName != "" {
		if _, err := io.WriteString(w, "event: "); err != nil {
			return err
		}
		if _, err := io.WriteString(w, eventName); err != nil {
			return err
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
	}
	return writeSSEData(w, body)
}

func writeSSEData(w io.Writer, data []byte) error {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for _, line := range lines {
		if _, err := io.WriteString(w, "data: "); err != nil {
			return err
		}
		if _, err := io.WriteString(w, line); err != nil {
			return err
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "\n")
	return err
}

func writeSSERawEvent(w io.Writer, eventName string, data []byte) error {
	if eventName != "" {
		if _, err := io.WriteString(w, "event: "); err != nil {
			return err
		}
		if _, err := io.WriteString(w, eventName); err != nil {
			return err
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
	}
	return writeSSEData(w, data)
}

func (s *Server) writeGatewayError(w http.ResponseWriter, err error) {
	if errors.Is(err, gateway.ErrModelUnavailable) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"type":    "invalid_request_error",
				"message": err.Error(),
			},
		})
		return
	}
	var responsesBindingErr *gateway.ResponsesBindingError
	if errors.As(err, &responsesBindingErr) && responsesBindingErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"type":    "invalid_request_error",
				"message": responsesBindingErr.Error(),
			},
		})
		return
	}
	var concurrencyErr *gateway.ConcurrencyLimitError
	if errors.As(err, &concurrencyErr) && concurrencyErr != nil {
		writeJSON(w, concurrencyErr.StatusCode, map[string]any{
			"error": map[string]any{
				"type":    concurrencyErr.Code,
				"message": concurrencyErr.Error(),
			},
		})
		return
	}
	var billingErr *gateway.BillingError
	if errors.As(err, &billingErr) && billingErr != nil {
		writeJSON(w, billingErr.StatusCode, map[string]any{
			"error": map[string]any{
				"type":    billingErr.Code,
				"message": billingErr.Error(),
			},
		})
		return
	}
	var accessErr *gateway.AccessError
	if errors.As(err, &accessErr) && accessErr != nil {
		writeJSON(w, accessErr.StatusCode, map[string]any{
			"error": map[string]any{
				"type":    accessErr.Code,
				"message": accessErr.Error(),
			},
		})
		return
	}
	if message, ok := publicUpstreamRejectedMessage(err); ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"type":    "invalid_request_error",
				"code":    providers.ErrorCodeUpstreamRejected,
				"message": message,
			},
		})
		return
	}

	payload := map[string]any{
		"type":    gatewayProtocolErrorType,
		"message": gatewayProtocolErrorMessage,
	}

	writeJSON(w, http.StatusBadGateway, map[string]any{"error": payload})
}

func publicUpstreamRejectedMessage(err error) (string, bool) {
	var executionErr *failover.ExecutionError
	if !errors.As(err, &executionErr) || executionErr == nil {
		return "", false
	}
	for i := len(executionErr.Attempts) - 1; i >= 0; i-- {
		attempt := executionErr.Attempts[i]
		if strings.TrimSpace(attempt.ErrorCode) != providers.ErrorCodeUpstreamRejected {
			continue
		}
		message := strings.TrimSpace(attempt.ErrorMessage)
		if message == "" {
			continue
		}
		message = strings.TrimSpace(strings.TrimPrefix(message, providers.ErrorCodeUpstreamRejected+":"))
		if message == "" || !publicUpstreamRejectedText(message) {
			continue
		}
		return message, true
	}
	return "", false
}

func publicUpstreamRejectedText(message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	if normalized == "" {
		return false
	}
	if strings.Contains(normalized, "context window") ||
		strings.Contains(normalized, "context length") ||
		strings.Contains(normalized, "maximum context") ||
		strings.Contains(normalized, "too many tokens") {
		return true
	}
	return strings.Contains(normalized, "input exceeds") && strings.Contains(normalized, "model")
}

func (s *Server) writeAnthropicGatewayError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	errorType := "api_error"
	message := gatewayProtocolErrorMessage

	if errors.Is(err, gateway.ErrModelUnavailable) {
		status = http.StatusBadRequest
		errorType = "invalid_request_error"
		message = err.Error()
	} else {
		var concurrencyErr *gateway.ConcurrencyLimitError
		if errors.As(err, &concurrencyErr) && concurrencyErr != nil {
			status = concurrencyErr.StatusCode
			errorType = "rate_limit_error"
			message = concurrencyErr.Error()
		}
		var billingErr *gateway.BillingError
		if errors.As(err, &billingErr) && billingErr != nil {
			status = billingErr.StatusCode
			errorType = "permission_error"
			message = billingErr.Error()
		}
		var accessErr *gateway.AccessError
		if errors.As(err, &accessErr) && accessErr != nil {
			status = accessErr.StatusCode
			errorType = "permission_error"
			message = accessErr.Error()
		}
		if upstreamRejectedMessage, ok := publicUpstreamRejectedMessage(err); ok {
			status = http.StatusBadRequest
			errorType = "invalid_request_error"
			message = upstreamRejectedMessage
		}
	}

	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    errorType,
			"message": message,
		},
	})
}

func (s *Server) redirectWithNoticeError(w http.ResponseWriter, r *http.Request, target string, err error) {
	message := strings.TrimSpace(err.Error())
	if message == "" {
		message = "request failed"
	}
	redirectLocalSeeOther(w, r, appendNoticeError(target, message))
}

func redirectLocalSeeOther(w http.ResponseWriter, r *http.Request, target string) {
	http.Redirect(w, r, sanitizeLocalRedirectTarget(target), http.StatusSeeOther)
}

func sanitizeLocalRedirectTarget(target string) string {
	target = strings.TrimSpace(target)
	if target == "" || strings.HasPrefix(target, "//") || strings.HasPrefix(target, `\`) {
		return "/"
	}
	parsed, err := url.Parse(target)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/") {
		return "/"
	}
	return parsed.String()
}

func appendNoticeError(target, message string) string {
	message = strings.TrimSpace(message)
	if len([]rune(message)) > 180 {
		runes := []rune(message)
		message = string(runes[:180]) + "..."
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return target
	}
	values := parsed.Query()
	values.Set("notice_error", message)
	parsed.RawQuery = values.Encode()
	return parsed.String()
}

func isAccessError(err error) bool {
	var accessErr *controlplane.AccessError
	return errors.As(err, &accessErr) && accessErr != nil
}

func formNanoUSDOrZero(r *http.Request, name string) int64 {
	value, err := core.ParseNanoUSDDecimal(r.FormValue(name))
	if err != nil {
		return 0
	}
	return value
}

func parsePercentBpsFormValue(r *http.Request, name string) int64 {
	value := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(r.FormValue(name)), "%"))
	if value == "" {
		return 0
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 {
		return 0
	}
	wholeText := parts[0]
	if wholeText == "" {
		wholeText = "0"
	}
	if !asciiDigitsOnly(wholeText) {
		return 0
	}
	whole, err := strconv.ParseInt(wholeText, 10, 64)
	if err != nil {
		return 0
	}
	fractionText := ""
	if len(parts) == 2 {
		fractionText = parts[1]
	}
	if len(fractionText) > 2 || !asciiDigitsOnly(fractionText) {
		return 0
	}
	for len(fractionText) < 2 {
		fractionText += "0"
	}
	var fraction int64
	if fractionText != "" {
		fraction, err = strconv.ParseInt(fractionText, 10, 64)
		if err != nil {
			return 0
		}
	}
	bps := whole*100 + fraction
	if bps < 0 {
		return 0
	}
	if bps > 10000 {
		return 10000
	}
	return bps
}

func asciiDigitsOnly(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func formatPercentBps(value any) string {
	var bps int64
	switch typed := value.(type) {
	case int:
		bps = int64(typed)
	case int8:
		bps = int64(typed)
	case int16:
		bps = int64(typed)
	case int32:
		bps = int64(typed)
	case int64:
		bps = typed
	case uint:
		if uint64(typed) > uint64(maxTemplateInt64) {
			return "0"
		}
		bps = int64(typed)
	case uint8:
		bps = int64(typed)
	case uint16:
		bps = int64(typed)
	case uint32:
		bps = int64(typed)
	case uint64:
		if typed > uint64(maxTemplateInt64) {
			return "0"
		}
		bps = int64(typed)
	default:
		return "0"
	}
	if bps < 0 {
		bps = 0
	}
	whole := bps / 100
	fraction := bps % 100
	if fraction == 0 {
		return strconv.FormatInt(whole, 10)
	}
	return fmt.Sprintf("%d.%s", whole, strings.TrimRight(fmt.Sprintf("%02d", fraction), "0"))
}

func maskProxyURL(proxyURL string) string {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return ""
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.User == nil {
		return proxyURL
	}
	if _, hasPassword := parsed.User.Password(); hasPassword {
		parsed.User = url.UserPassword(parsed.User.Username(), "****")
	}
	return parsed.String()
}

func accountStatusClass(status core.AccountStatus) string {
	switch status {
	case core.AccountStatusActive:
		return "app-label-success"
	case core.AccountStatusCooling, core.AccountStatusRefreshing:
		return "app-label-warning"
	case core.AccountStatusExpired, core.AccountStatusBlocked, core.AccountStatusProviderBanned:
		return "app-label-danger"
	default:
		return "app-label-info"
	}
}

func boolStateClass(enabled bool) string {
	if enabled {
		return "app-label-success"
	}
	return "app-label-muted"
}

func auditStatusClass(status string) string {
	if strings.EqualFold(status, "ok") || strings.EqualFold(status, "success") {
		return "app-label-success"
	}
	return "app-label-danger"
}

func joinProviders(providers []core.ProviderKind) string {
	out := make([]string, 0, len(providers))
	for _, provider := range providers {
		out = append(out, string(provider))
	}
	return strings.Join(out, " -> ")
}

func formatTime(value any) string {
	var ts time.Time
	switch typed := value.(type) {
	case nil:
		return "-"
	case time.Time:
		ts = typed
	case *time.Time:
		if typed == nil {
			return "-"
		}
		ts = *typed
	default:
		return "-"
	}
	if ts.IsZero() {
		return "-"
	}
	return ts.Local().Format("2006-01-02 15:04:05")
}

func cooldownText(account core.Account) string {
	if account.CooldownUntil == nil {
		return "-"
	}
	if time.Until(*account.CooldownUntil) <= 0 {
		return "-"
	}
	return time.Until(*account.CooldownUntil).Round(time.Second).String()
}

func quotaWindowSummary(window *core.AccountQuotaWindow) string {
	if window == nil {
		return "-"
	}
	now := time.Now().UTC()
	parts := []string{quotaPercentText(core.AccountQuotaWindowUsedPercent(window, now))}
	if window.WindowMinutes > 0 {
		parts = append(parts, quotaWindowDurationText(window.WindowMinutes))
	}
	if core.AccountQuotaWindowResetActive(window, now) {
		parts = append(parts, formatTime(window.ResetsAt))
	}
	return strings.Join(parts, " / ")
}

func quotaWindowPercent(window *core.AccountQuotaWindow) string {
	if window == nil {
		return "0"
	}
	value := core.AccountQuotaWindowUsedPercent(window, time.Now().UTC())
	value = math.Max(0, math.Min(100, value))
	return fmt.Sprintf("%.2f", value)
}

func quotaCreditsSummary(locale string, credits *core.AccountQuotaCredits) string {
	if credits == nil {
		return "-"
	}
	if credits.Unlimited {
		return translate(locale, "unlimited")
	}
	if credits.Balance != nil {
		return fmt.Sprintf("%.2f", *credits.Balance)
	}
	if credits.HasCredits {
		return translate(locale, "enabled")
	}
	return translate(locale, "none")
}

func quotaImageSummary(locale string, quota *core.AccountImageQuota) string {
	if quota == nil {
		return "-"
	}
	if quota.Unknown {
		return translate(locale, "unknown")
	}
	remaining := quota.Remaining
	if remaining < 0 {
		remaining = 0
	}
	parts := []string{fmt.Sprintf("%d", remaining)}
	if quota.ResetsAt != nil && !quota.ResetsAt.IsZero() {
		parts = append(parts, formatTime(quota.ResetsAt))
	} else if strings.TrimSpace(quota.ResetAfter) != "" {
		parts = append(parts, strings.TrimSpace(quota.ResetAfter))
	}
	return strings.Join(parts, " / ")
}

func quotaPercentText(value float64) string {
	value = math.Max(0, math.Min(100, value))
	return fmt.Sprintf("%.0f%%", value)
}

func quotaWindowDurationText(minutes int64) string {
	switch {
	case minutes <= 0:
		return "-"
	case minutes%(24*60) == 0:
		return fmt.Sprintf("%dd", minutes/(24*60))
	case minutes%60 == 0:
		return fmt.Sprintf("%dh", minutes/60)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

func statusTone(status core.AccountStatus) string {
	switch status {
	case core.AccountStatusActive:
		return "tone-good"
	case core.AccountStatusCooling:
		return "tone-warn"
	default:
		return "tone-bad"
	}
}

func accountRuntimeStatusTone(status string) string {
	switch strings.TrimSpace(status) {
	case string(core.AccountStatusActive):
		return "tone-good"
	case string(core.AccountStatusCooling),
		string(core.AccountStatusRefreshing):
		return "tone-warn"
	default:
		return "tone-bad"
	}
}

func containsTag(account core.Account, tag string) bool {
	for _, item := range account.Tags {
		if item == tag {
			return true
		}
	}
	return false
}

func (s *Server) recordAdminAudit(r *http.Request, status, action, resourceType, resourceID, resourceName, message string) {
	if s.control == nil {
		return
	}
	actor := consoleUsernameFromContext(r.Context())
	if actor == "" {
		actor = "system"
	}
	_ = s.control.AppendAdminAudit(actor, action, resourceType, resourceID, resourceName, status, message)
	s.publishAuditUpdated()
}

func auditRouteText(policy core.RoutePolicy) string {
	parts := []string{string(policy.DefaultProvider)}
	for _, provider := range policy.FallbackProviders {
		if provider == "" {
			continue
		}
		parts = append(parts, string(provider))
	}
	return strings.Join(parts, "->")
}

func auditClientScopeText(client core.APIClient) string {
	if groupName := strings.TrimSpace(client.AccountGroup); groupName != "" {
		return "group:" + groupName
	}
	return "all"
}

func parseIntFormValue(value string) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return parsed
}

func parseFloatFormValue(value string) float64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0
	}
	return parsed
}

func parseEmailListFormValue(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool {
		switch r {
		case ',', ';', '\n', '\r', '\t', ' ':
			return true
		default:
			return false
		}
	})
}

func joinLines(values []string) string {
	return strings.Join(values, "\n")
}

func systemSettingsFromForm(r *http.Request, existing core.SystemSettings) core.SystemSettings {
	if r != nil {
		_ = r.ParseForm()
	}
	input := existing
	responsesWebSocketUpstreamEnabled := existing.Runtime.ResponsesWebSocketUpstreamEnabled
	if r.Form.Has("responses_websocket_upstream_present") {
		enabled := r.FormValue("responses_websocket_upstream_enabled") != ""
		responsesWebSocketUpstreamEnabled = &enabled
	}
	input.Runtime = core.SystemRuntimeSettings{
		PublicBaseURL:                     r.FormValue("public_base_url"),
		AllowPublicRegistration:           r.FormValue("allow_public_registration") != "",
		RegistrationEmailAllowlistEnabled: r.FormValue("registration_email_allowlist_enabled") != "",
		RegistrationEmailAllowlist:        parseEmailListFormValue(r.FormValue("registration_email_allowlist")),
		UserConcurrentRequestLimit:        parseIntFormValue(r.FormValue("user_concurrent_request_limit")),
		PlanConcurrentRequestLimit:        parseIntFormValue(r.FormValue("plan_concurrent_request_limit")),
		UserRequestRateLimitPerMinute:     parseIntFormValue(r.FormValue("user_request_rate_limit_per_minute")),
		ResponsesWebSocketUpstreamEnabled: responsesWebSocketUpstreamEnabled,
	}
	input.Network = core.SystemNetworkSettings{
		SystemProxyURL: r.FormValue("system_proxy_url"),
	}
	if r.Form.Has("image_user_console_enabled") || r.Form.Has("image_backend") {
		imageUserConsoleEnabled := r.FormValue("image_user_console_enabled") != ""
		input.Image = core.SystemImageSettings{
			Backend:            r.FormValue("image_backend"),
			UserConsoleEnabled: &imageUserConsoleEnabled,
		}
	}
	input.OAuth = core.SystemOAuthSettings{
		OpenAIEnabled:       r.FormValue("openai_oauth_enabled") != "",
		ClaudeEnabled:       r.FormValue("claude_oauth_enabled") != "",
		GitHubLoginEnabled:  r.FormValue("github_login_enabled") != "",
		GitHubLoginClientID: r.FormValue("github_login_client_id"),
		GitHubLoginSecret:   r.FormValue("github_login_secret"),
		GoogleLoginEnabled:  r.FormValue("google_login_enabled") != "",
		GoogleLoginClientID: r.FormValue("google_login_client_id"),
		GoogleLoginSecret:   r.FormValue("google_login_secret"),
		LinuxDOLoginEnabled: r.FormValue("linuxdo_login_enabled") != "",
		LinuxDOClientID:     r.FormValue("linuxdo_login_client_id"),
		LinuxDOSecret:       r.FormValue("linuxdo_login_secret"),
		LoginAutoCreateUser: r.FormValue("login_oauth_auto_create_user") != "",
	}
	input.Email = core.SystemEmailSettings{
		RegistrationVerificationEnabled: r.FormValue("email_verify_on_register") != "",
		Provider:                        r.FormValue("email_provider"),
		SMTPHost:                        r.FormValue("smtp_host"),
		SMTPPort:                        parseIntFormValue(r.FormValue("smtp_port")),
		SMTPUsername:                    r.FormValue("smtp_username"),
		SMTPPassword:                    r.FormValue("smtp_password"),
		CloudMailBaseURL:                r.FormValue("cloudmail_base_url"),
		CloudMailEmail:                  r.FormValue("cloudmail_email"),
		CloudMailPassword:               r.FormValue("cloudmail_password"),
		CloudMailAccountID:              parseIntFormValue(r.FormValue("cloudmail_account_id")),
		FromEmail:                       r.FormValue("smtp_from_email"),
		FromName:                        r.FormValue("smtp_from_name"),
		VerificationSubjectTemplate:     r.FormValue("email_template_subject"),
		VerificationTextTemplate:        r.FormValue("email_template_text"),
		VerificationHTMLTemplate:        r.FormValue("email_template_html"),
		CodeTTLSeconds:                  parseIntFormValue(r.FormValue("email_code_ttl_seconds")),
		SendCooldownSeconds:             parseIntFormValue(r.FormValue("email_send_cooldown_seconds")),
		HourlySendLimit:                 parseIntFormValue(r.FormValue("email_hourly_send_limit")),
		MaxAttempts:                     parseIntFormValue(r.FormValue("email_max_attempts")),
	}
	input.Home = core.SystemHomeSettings{
		BrandTitle:      r.FormValue("home_brand_title"),
		BrandSubtitle:   r.FormValue("home_brand_subtitle"),
		Heading:         r.FormValue("home_heading"),
		Summary:         r.FormValue("home_summary"),
		AvailabilityKey: r.FormValue("home_availability_key"),
		Availability:    r.FormValue("home_availability"),
		CostKey:         r.FormValue("home_cost_key"),
		CostMultiplier:  r.FormValue("home_cost_multiplier"),
		LatencyKey:      r.FormValue("home_latency_key"),
		Latency:         r.FormValue("home_latency"),
		CapabilityKey:   r.FormValue("home_capability_key"),
		Capability:      r.FormValue("home_capability"),
	}
	input.Payment = core.SystemPaymentSettings{
		CNYPerUSD:          r.FormValue("payment_cny_per_usd"),
		RechargeInputMode:  r.FormValue("payment_recharge_input_mode"),
		MinRechargeNanoUSD: formNanoUSDOrZero(r, "payment_min_recharge_usd"),
		MaxRechargeNanoUSD: formNanoUSDOrZero(r, "payment_max_recharge_usd"),
		WeChatPay: core.WeChatPaySettings{
			Enabled:               r.FormValue("wechat_pay_enabled") != "",
			AppID:                 r.FormValue("wechat_pay_app_id"),
			MchID:                 r.FormValue("wechat_pay_mch_id"),
			APIV3Key:              r.FormValue("wechat_pay_api_v3_key"),
			MerchantSerialNo:      r.FormValue("wechat_pay_merchant_serial_no"),
			MerchantPrivateKeyPEM: r.FormValue("wechat_pay_merchant_private_key_pem"),
			WeChatPayPublicKeyID:  r.FormValue("wechat_pay_public_key_id"),
			WeChatPayPublicKeyPEM: r.FormValue("wechat_pay_public_key_pem"),
		},
		Alipay: core.AlipaySettings{
			Enabled:            r.FormValue("alipay_enabled") != "",
			AppID:              r.FormValue("alipay_app_id"),
			PrivateKeyPEM:      r.FormValue("alipay_private_key_pem"),
			AlipayPublicKeyPEM: r.FormValue("alipay_public_key_pem"),
			GatewayURL:         r.FormValue("alipay_gateway_url"),
			ReturnURL:          r.FormValue("alipay_return_url"),
			SignType:           r.FormValue("alipay_sign_type"),
		},
		PersonalPay: core.PersonalPaySettings{
			Enabled:        r.FormValue("personalpay_enabled") != "",
			AndroidToken:   r.FormValue("personalpay_android_token"),
			ExpireAfterSec: parseIntFormValue(r.FormValue("personalpay_expire_after_sec")),
		},
	}
	input.Registration = core.SystemRegistrationSettings{
		NewUserRewardEnabled:   r.FormValue("new_user_reward_enabled") != "",
		NewUserRewardNanoUSD:   formNanoUSDOrZero(r, "new_user_reward_usd"),
		RequireInvitationCode:  r.FormValue("require_invitation_code") != "",
		UsernameMinLength:      parseIntFormValue(r.FormValue("registration_username_min_length")),
		RegisterIPHourlyLimit:  parseIntFormValue(r.FormValue("register_ip_hourly_limit")),
		EmailCodeIPHourlyLimit: parseIntFormValue(r.FormValue("email_code_ip_hourly_limit")),
		TurnstileEnabled:       r.FormValue("turnstile_enabled") != "",
		TurnstileSiteKey:       r.FormValue("turnstile_site_key"),
		TurnstileSecretKey:     r.FormValue("turnstile_secret_key"),
	}
	input.Invitation = core.SystemInvitationSettings{
		Enabled:                  r.FormValue("invitation_enabled") != "",
		InviterRechargeRewardBps: parsePercentBpsFormValue(r, "inviter_recharge_reward_percent"),
		InviteeRewardNanoUSD:     formNanoUSDOrZero(r, "invitee_reward_usd"),
	}
	input.UserDashboard = core.SystemUserDashboardSettings{
		CustomPanelEnabled: r.FormValue("user_dashboard_custom_panel_enabled") != "",
		CustomPanelHTML:    r.FormValue("user_dashboard_custom_panel_html"),
	}
	input.Retention = core.SystemRetentionSettings{
		AuditLimit:                 parseIntFormValue(r.FormValue("audit_limit")),
		UsageLogMaxAgeDays:         parseIntFormValue(r.FormValue("usage_log_max_age_days")),
		BillingLedgerRetentionDays: parseIntFormValue(r.FormValue("billing_ledger_retention_days")),
		GatewayAuditErrors:         r.FormValue("gateway_audit_errors") != "",
		GatewayAuditRetentionDays:  parseIntFormValue(r.FormValue("gateway_audit_retention_days")),
	}
	if input.Payment.PersonalPay.Enabled {
		input.Backup = core.SystemBackupSettings{
			AndroidAutoEnabled: r.FormValue("backup_android_auto_enabled") != "",
			AndroidTimeOfDay:   r.FormValue("backup_android_time"),
			AndroidDataSets:    selectedBackupDataSets(r.Form["backup_android_data"]),
		}
	} else {
		input.Backup = core.SystemBackupSettings{}
	}
	return core.NormalizeSystemSettings(input)
}

type userRoleOption struct {
	Value core.UserRole
	Label string
}

func userRoleOptions() []userRoleOption {
	return []userRoleOption{
		{Value: core.UserRoleUser, Label: string(core.UserRoleUser)},
		{Value: core.UserRoleAdmin, Label: string(core.UserRoleAdmin)},
	}
}

func userInputFromForm(r *http.Request) (controlplane.UserInput, error) {
	concurrentRequestLimit, err := parseUserConcurrentRequestLimitOverride(r.FormValue("concurrent_request_limit"))
	if err != nil {
		return controlplane.UserInput{}, err
	}
	ipConcurrentRequestLimit, err := parseUserIPConcurrentRequestLimitOverride(r.FormValue("ip_concurrent_request_limit"))
	if err != nil {
		return controlplane.UserInput{}, err
	}
	requestRateLimit, err := parseUserRequestRateLimitOverride(r.FormValue("request_rate_limit_per_minute"))
	if err != nil {
		return controlplane.UserInput{}, err
	}
	return controlplane.UserInput{
		Username:                          r.FormValue("username"),
		Password:                          r.FormValue("password"),
		Role:                              core.UserRole(r.FormValue("role")),
		Enabled:                           r.FormValue("enabled") == "on",
		ConcurrentRequestLimitOverride:    concurrentRequestLimit,
		IPConcurrentRequestLimitOverride:  ipConcurrentRequestLimit,
		RequestRateLimitPerMinuteOverride: requestRateLimit,
	}, nil
}

func parseUserConcurrentRequestLimitOverride(value string) (*int, error) {
	return parseOptionalNonNegativeInt(value, "concurrent request limit")
}

func parseUserIPConcurrentRequestLimitOverride(value string) (*int, error) {
	return parseOptionalNonNegativeInt(value, "ip concurrent request limit")
}

func parseUserRequestRateLimitOverride(value string) (*int, error) {
	return parseOptionalNonNegativeInt(value, "request rate limit")
}

func parseOptionalNonNegativeInt(value, label string) (*int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return nil, fmt.Errorf("%s must be a number", label)
	}
	if parsed < 0 {
		return nil, fmt.Errorf("%s must be zero or greater", label)
	}
	if parsed > 100000 {
		return nil, fmt.Errorf("%s must be 100000 or less", label)
	}
	return &parsed, nil
}

func parseNanoUSDFormValue(r *http.Request, name string) (int64, error) {
	value, err := core.ParseNanoUSDDecimal(r.FormValue(name))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return value, nil
}

func parseMultiplierFormValue(r *http.Request, name string) (int64, error) {
	value, err := core.ParseMultiplierDecimal(r.FormValue(name))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return value, nil
}

func parsePricingTiersForm(r *http.Request) ([]core.ModelPricingTier, error) {
	names := r.Form["pricing_tier_name"]
	maxTokens := r.Form["pricing_tier_max_input_tokens"]
	inputs := r.Form["pricing_tier_input_price_usd_per_1m"]
	cachedInputs := r.Form["pricing_tier_cached_input_price_usd_per_1m"]
	outputs := r.Form["pricing_tier_output_price_usd_per_1m"]
	imageOutputs := r.Form["pricing_tier_image_output_price_usd_per_1m"]
	count := maxInt(len(names), len(maxTokens), len(inputs), len(cachedInputs), len(outputs), len(imageOutputs))
	tiers := make([]core.ModelPricingTier, 0, count)
	for i := 0; i < count; i++ {
		inputPrice, err := core.ParseNanoUSDDecimal(formValueAt(inputs, i))
		if err != nil {
			return nil, fmt.Errorf("pricing tier %d input price: %w", i+1, err)
		}
		cachedInputPrice, err := core.ParseNanoUSDDecimal(formValueAt(cachedInputs, i))
		if err != nil {
			return nil, fmt.Errorf("pricing tier %d cached input price: %w", i+1, err)
		}
		outputPrice, err := core.ParseNanoUSDDecimal(formValueAt(outputs, i))
		if err != nil {
			return nil, fmt.Errorf("pricing tier %d output price: %w", i+1, err)
		}
		imageOutputPrice, err := core.ParseNanoUSDDecimal(formValueAt(imageOutputs, i))
		if err != nil {
			return nil, fmt.Errorf("pricing tier %d image output price: %w", i+1, err)
		}
		maxInputTokens := parseIntFormValue(formValueAt(maxTokens, i))
		if maxInputTokens == 0 && inputPrice == 0 && cachedInputPrice == 0 && outputPrice == 0 && imageOutputPrice == 0 {
			continue
		}
		tiers = append(tiers, core.ModelPricingTier{
			Name:                    formValueAt(names, i),
			MaxInputTokens:          maxInputTokens,
			InputPriceNanoUSD:       inputPrice,
			CachedInputPriceNanoUSD: cachedInputPrice,
			OutputPriceNanoUSD:      outputPrice,
			ImageOutputPriceNanoUSD: imageOutputPrice,
		})
	}
	return tiers, nil
}

func formValueAt(values []string, index int) string {
	if index < 0 || index >= len(values) {
		return ""
	}
	return strings.TrimSpace(values[index])
}

func maxInt(values ...int) int {
	maximum := 0
	for _, value := range values {
		if value > maximum {
			maximum = value
		}
	}
	return maximum
}

func billingModeText(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case core.ModelBillingModeRequest:
		return "request"
	case core.ModelBillingModeTieredExpr:
		return "tiered"
	default:
		return "token"
	}
}

func pricingExpression(model core.ModelConfig) string {
	if strings.TrimSpace(model.BillingMode) != core.ModelBillingModeTieredExpr || len(model.PricingTiers) == 0 {
		return ""
	}
	parts := make([]string, 0, len(model.PricingTiers))
	for i, tier := range model.PricingTiers {
		name := strings.TrimSpace(tier.Name)
		if name == "" {
			name = fmt.Sprintf("tier_%d", i+1)
		}
		body := fmt.Sprintf("p * %s + cr * %s + c * %s + img * %s", core.FormatNanoUSD(tier.InputPriceNanoUSD), core.FormatNanoUSD(tier.CachedInputPriceNanoUSD), core.FormatNanoUSD(tier.OutputPriceNanoUSD), core.FormatNanoUSD(tier.ImageOutputPriceNanoUSD))
		call := fmt.Sprintf("tier(%q, %s)", name, body)
		if tier.MaxInputTokens > 0 {
			parts = append(parts, fmt.Sprintf("len <= %d ? %s", tier.MaxInputTokens, call))
			continue
		}
		parts = append(parts, call)
	}
	return strings.Join(parts, " : ")
}

type modelPricingBreakdownView struct {
	Line1 string
	Line2 string
}

func modelPricingBreakdown(model core.ModelConfig) modelPricingBreakdownView {
	switch strings.TrimSpace(model.BillingMode) {
	case core.ModelBillingModeRequest:
		return modelPricingBreakdownView{Line1: "request", Line2: "$" + core.FormatNanoUSD(model.RequestPriceNanoUSD) + " / request"}
	case core.ModelBillingModeTieredExpr:
		if len(model.PricingTiers) == 0 {
			return modelPricingBreakdownView{Line1: "-", Line2: "no tiers"}
		}
		return modelPricingBreakdownView{Line1: "-", Line2: fmt.Sprintf("%d tiers", len(model.PricingTiers))}
	default:
		input := fmt.Sprintf("in %s / cache %s", core.FormatNanoUSD(model.InputPriceNanoUSDPer1M), core.FormatNanoUSD(model.CachedInputPriceNanoUSDPer1M))
		output := fmt.Sprintf("out %s / image %s", core.FormatNanoUSD(model.OutputPriceNanoUSDPer1M), core.FormatNanoUSD(model.ImageOutputPriceNanoUSDPer1M))
		return modelPricingBreakdownView{Line1: input, Line2: output}
	}
}

func formatNanoUSDTemplate(value any) string {
	switch typed := value.(type) {
	case int:
		return core.FormatNanoUSD(int64(typed))
	case int8:
		return core.FormatNanoUSD(int64(typed))
	case int16:
		return core.FormatNanoUSD(int64(typed))
	case int32:
		return core.FormatNanoUSD(int64(typed))
	case int64:
		return core.FormatNanoUSD(typed)
	case uint:
		if uint64(typed) > uint64(maxTemplateInt64) {
			return core.FormatNanoUSD(0)
		}
		return core.FormatNanoUSD(int64(typed))
	case uint8:
		return core.FormatNanoUSD(int64(typed))
	case uint16:
		return core.FormatNanoUSD(int64(typed))
	case uint32:
		return core.FormatNanoUSD(int64(typed))
	case uint64:
		if typed > uint64(maxTemplateInt64) {
			return core.FormatNanoUSD(0)
		}
		return core.FormatNanoUSD(int64(typed))
	default:
		return core.FormatNanoUSD(0)
	}
}

func formatUSDDisplay(nanoUSD int64) string {
	if nanoUSD == 0 {
		return "$0.00"
	}
	sign := ""
	if nanoUSD < 0 {
		sign = "-"
		nanoUSD = -nanoUSD
	}
	if nanoUSD < core.NanoUSDPerUSD/100 {
		return sign + "$" + core.FormatNanoUSD(nanoUSD)
	}
	centNanoUSD := core.NanoUSDPerUSD / 100
	if nanoUSD <= maxTemplateInt64-centNanoUSD/2 {
		nanoUSD += centNanoUSD / 2
	}
	cents := nanoUSD / centNanoUSD
	return fmt.Sprintf("%s$%d.%02d", sign, cents/100, cents%100)
}

func formatCNYCentsDisplay(cents int64) string {
	if cents == 0 {
		return "\u00a50.00"
	}
	sign := ""
	if cents < 0 {
		sign = "-"
		cents = -cents
	}
	return fmt.Sprintf("%s\u00a5%d.%02d", sign, cents/100, cents%100)
}

func paymentExchangeRateText(order core.PaymentOrder) string {
	rateNano := paymentExchangeRateNano(order)
	if rateNano <= 0 {
		return "-"
	}
	reverseText := "-"
	if usdPerCNYNano := reciprocalNano(rateNano); usdPerCNYNano > 0 {
		reverseText = formatRateNanoDisplay("$", usdPerCNYNano)
	}
	return fmt.Sprintf("$1 = %s / \u00a51 = %s", formatRateNanoDisplay("\u00a5", rateNano), reverseText)
}

func paymentExchangeRateNano(order core.PaymentOrder) int64 {
	if rate, err := core.ParseNanoUSDDecimal(strings.TrimSpace(order.ExchangeRateCNYPerUSD)); err == nil && rate > 0 {
		return rate
	}
	if order.ProviderAmountCents <= 0 || order.AmountNanoUSD <= 0 {
		return 0
	}
	numerator := big.NewInt(order.ProviderAmountCents)
	numerator.Mul(numerator, big.NewInt(core.NanoUSDPerUSD))
	numerator.Mul(numerator, big.NewInt(core.NanoUSDPerUSD))
	denominator := big.NewInt(order.AmountNanoUSD)
	denominator.Mul(denominator, big.NewInt(100))
	rate, ok := roundedBigQuotientInt64(numerator, denominator)
	if !ok || rate <= 0 {
		return 0
	}
	return rate
}

func reciprocalNano(rateNano int64) int64 {
	if rateNano <= 0 {
		return 0
	}
	numerator := big.NewInt(core.NanoUSDPerUSD)
	numerator.Mul(numerator, big.NewInt(core.NanoUSDPerUSD))
	value, ok := roundedBigQuotientInt64(numerator, big.NewInt(rateNano))
	if !ok || value <= 0 {
		return 0
	}
	return value
}

func roundedBigQuotientInt64(numerator, denominator *big.Int) (int64, bool) {
	if numerator == nil || denominator == nil || denominator.Sign() <= 0 {
		return 0, false
	}
	quotient, remainder := new(big.Int).QuoRem(new(big.Int).Set(numerator), denominator, new(big.Int))
	remainder.Mul(remainder, big.NewInt(2))
	if remainder.Cmp(denominator) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() {
		return 0, false
	}
	return quotient.Int64(), true
}

func formatRateNanoDisplay(symbol string, nanoValue int64) string {
	if nanoValue <= 0 {
		return symbol + "0.00"
	}
	const scale = int64(10000)
	unit := core.NanoUSDPerUSD / scale
	units := (nanoValue + unit/2) / unit
	if units <= 0 {
		return symbol + core.FormatNanoUSD(nanoValue)
	}
	whole := units / scale
	fraction := fmt.Sprintf("%04d", units%scale)
	for len(fraction) > 2 && strings.HasSuffix(fraction, "0") {
		fraction = strings.TrimSuffix(fraction, "0")
	}
	return fmt.Sprintf("%s%d.%s", symbol, whole, fraction)
}

func formatUsageCostDisplay(nanoUSD int64) string {
	return formatUSDDisplay(nanoUSD)
}

func addDisplayNanoUSDSaturating(a, b int64) int64 {
	if a <= 0 {
		if b < 0 {
			return 0
		}
		return b
	}
	if b <= 0 {
		return a
	}
	const maxInt64 = int64(^uint64(0) >> 1)
	if a > maxInt64-b {
		return maxInt64
	}
	return a + b
}

func signedUSDDisplay(nanoUSD int64) string {
	if nanoUSD > 0 {
		return "+" + formatUSDDisplay(nanoUSD)
	}
	return formatUSDDisplay(nanoUSD)
}

func billingLedgerKindText(locale, kind string) string {
	switch kind {
	case "manual_credit":
		return translate(locale, "recharge")
	case "manual_debit":
		return translate(locale, "deduct")
	case "account_merge":
		return translate(locale, "account_merge")
	case "payment":
		return translate(locale, "payment_recharge")
	case "plan_purchase":
		return translate(locale, "plan_purchase")
	case "settle":
		return translate(locale, "completed")
	case "payment_refund":
		return translate(locale, "refund")
	default:
		return kind
	}
}

func modelGroupSelected(model core.ModelConfig, group string) bool {
	return groupSelected(model.VisibleGroups, group)
}

func groupSelected(groups []string, group string) bool {
	group = strings.TrimSpace(group)
	for _, visibleGroup := range groups {
		if strings.EqualFold(strings.TrimSpace(visibleGroup), group) {
			return true
		}
	}
	return false
}

func modelGroupListTextForLocale(locale string, groups []string, emptyLabel string) string {
	return modelGroupListTextWithLabels(groups, emptyLabel)
}

func modelGroupListTextWithLabels(groups []string, emptyLabel string) string {
	if len(groups) == 0 {
		return emptyLabel
	}
	labels := make([]string, 0, len(groups))
	for _, group := range groups {
		group = strings.TrimSpace(group)
		if group != "" {
			labels = append(labels, group)
		}
	}
	if len(labels) == 0 {
		return emptyLabel
	}
	return strings.Join(labels, ", ")
}

func siteMessageTargetMode(message any) string {
	siteMessage, ok := message.(core.SiteMessage)
	if !ok {
		return "all"
	}
	if siteMessage.PublicPopup {
		return "website"
	}
	if len(siteMessage.TargetUserIDs) > 0 {
		return "user"
	}
	if len(siteMessage.TargetAccountGroups) > 0 {
		return "group"
	}
	return "all"
}

func siteMessageTargetUserSelected(message any, userID string) bool {
	siteMessage, ok := message.(core.SiteMessage)
	if !ok {
		return false
	}
	userID = strings.TrimSpace(userID)
	for _, targetUserID := range siteMessage.TargetUserIDs {
		if strings.TrimSpace(targetUserID) == userID {
			return true
		}
	}
	return false
}

func siteMessageTargetGroupSelected(message any, group string) bool {
	siteMessage, ok := message.(core.SiteMessage)
	if !ok {
		return false
	}
	group = strings.TrimSpace(group)
	for _, targetGroup := range siteMessage.TargetAccountGroups {
		if strings.EqualFold(strings.TrimSpace(targetGroup), group) {
			return true
		}
	}
	return false
}

func siteMessageTargetText(locale string, message core.SiteMessage, users []core.User) string {
	if message.PublicPopup {
		return translate(locale, "message_target_website")
	}
	if len(message.TargetUserIDs) == 0 && len(message.TargetAccountGroups) == 0 {
		return translate(locale, "message_target_all")
	}
	parts := make([]string, 0, 2)
	if len(message.TargetUserIDs) > 0 {
		names := make([]string, 0, len(message.TargetUserIDs))
		for _, userID := range message.TargetUserIDs {
			names = append(names, siteMessageTargetUserText(userID, users))
		}
		parts = append(parts, translatef(locale, "message_target_users_label", strings.Join(names, ", ")))
	}
	if len(message.TargetAccountGroups) > 0 {
		groups := make([]string, 0, len(message.TargetAccountGroups))
		for _, group := range message.TargetAccountGroups {
			groups = append(groups, accountGroupLabelText(locale, group))
		}
		parts = append(parts, translatef(locale, "message_target_groups_label", strings.Join(groups, ", ")))
	}
	return strings.Join(parts, " · ")
}

func siteMessageTargetUserText(userID string, users []core.User) string {
	userID = strings.TrimSpace(userID)
	for _, user := range users {
		if user.ID == userID && strings.TrimSpace(user.Username) != "" {
			return user.Username
		}
	}
	return userID
}

func siteMessageBrowserReadKey(message core.SiteMessage) string {
	version := message.UpdatedAt
	if version.IsZero() {
		version = message.CreatedAt
	}
	return strings.TrimSpace(message.ID) + ":" + strconv.FormatInt(version.UTC().UnixNano(), 10)
}

func accountGroupLabelText(locale, group string) string {
	_ = locale
	return strings.TrimSpace(group)
}

func templateDict(values ...any) (map[string]any, error) {
	if len(values)%2 != 0 {
		return nil, fmt.Errorf("dict expects key/value pairs")
	}
	out := make(map[string]any, len(values)/2)
	for i := 0; i < len(values); i += 2 {
		key, ok := values[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict key must be string")
		}
		out[key] = values[i+1]
	}
	return out, nil
}

type clientBillingSelection struct {
	AccountGroup  string
	BillingSource string
}

func (s *Server) clientBillingSelectionFromForm(r *http.Request, availableGroups []core.AccountGroup) (clientBillingSelection, error) {
	groupName, billingSource := parseClientBillingChoice(r.FormValue("account_group"))
	if groupName == "" {
		return clientBillingSelection{}, fmt.Errorf("account group is required")
	}
	knownGroup := false
	for _, group := range availableGroups {
		if strings.EqualFold(strings.TrimSpace(group.Name), groupName) {
			knownGroup = true
			groupName = strings.TrimSpace(group.Name)
			break
		}
	}
	if !knownGroup {
		return clientBillingSelection{}, fmt.Errorf("account group %q does not exist", groupName)
	}
	if s.accountGroupsHaveAccounts([]string{groupName}) {
		return clientBillingSelection{AccountGroup: groupName, BillingSource: billingSource}, nil
	}
	return clientBillingSelection{}, fmt.Errorf("account group %q has no accounts", groupName)
}

func parseClientBillingChoice(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", core.ClientBillingSourceCash
	}
	parts := strings.SplitN(raw, "|", 2)
	if len(parts) != 2 {
		return raw, core.ClientBillingSourceCash
	}
	source := core.NormalizeClientBillingSource(parts[0])
	groupName, err := url.QueryUnescape(parts[1])
	if err != nil {
		groupName = parts[1]
	}
	return strings.TrimSpace(groupName), source
}

func accountGroupBillingOptionValue(group, billingSource string) string {
	return core.NormalizeClientBillingSource(billingSource) + "|" + url.QueryEscape(accountGroupQueryKey(group))
}

func normalizeAccountGroupForWeb(value string) string {
	return core.NormalizeAccountGroupName(value)
}

func (s *Server) accountGroupsHaveAccounts(groups []string) bool {
	allowed := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		allowed[strings.ToLower(normalizeAccountGroupForWeb(group))] = struct{}{}
	}
	for _, account := range s.control.ListAccounts() {
		if _, ok := allowed[strings.ToLower(normalizeAccountGroupForWeb(account.Group))]; ok {
			return true
		}
	}
	return false
}

func (s *Server) routeProvidersForAccountGroup(groupName string) (core.ProviderKind, []core.ProviderKind) {
	groupName = strings.TrimSpace(groupName)
	if groupName == "" {
		return "", nil
	}
	providers := make([]core.ProviderKind, 0, 2)
	for _, account := range s.control.ListAccounts() {
		if !strings.EqualFold(normalizeAccountGroupForWeb(account.Group), groupName) {
			continue
		}
		if !containsProvider(providers, account.Provider) {
			providers = append(providers, account.Provider)
		}
	}
	if len(providers) == 0 {
		return "", nil
	}
	for _, group := range s.control.ListAccountGroups() {
		if !strings.EqualFold(normalizeAccountGroupForWeb(group.Name), groupName) {
			continue
		}
		switch core.NormalizeAccountGroupType(group.Type) {
		case core.AccountGroupTypeOpenAI:
			return orderedRouteProvidersForPreferred(providers, core.ProviderOpenAI)
		case core.AccountGroupTypeClaude:
			return orderedRouteProvidersForPreferred(providers, core.ProviderClaude)
		}
		break
	}
	return providers[0], providers[1:]
}

func orderedRouteProvidersForPreferred(providers []core.ProviderKind, preferred core.ProviderKind) (core.ProviderKind, []core.ProviderKind) {
	if len(providers) == 0 {
		return "", nil
	}
	ordered := make([]core.ProviderKind, 0, len(providers))
	if containsProvider(providers, preferred) {
		ordered = append(ordered, preferred)
	}
	for _, provider := range providers {
		if !containsProvider(ordered, provider) {
			ordered = append(ordered, provider)
		}
	}
	if len(ordered) == 0 {
		return "", nil
	}
	return ordered[0], ordered[1:]
}

func containsProvider(providers []core.ProviderKind, target core.ProviderKind) bool {
	for _, provider := range providers {
		if provider == target {
			return true
		}
	}
	return false
}

func parsePositiveInt(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func parseOptionalTimestamp(raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, fmt.Errorf("expires_at must use RFC3339 format")
	}
	utc := value.UTC()
	return &utc, nil
}

func parseOptionalDateTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04",
		"2006-01-02",
	} {
		if value, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return value.UTC()
		}
	}
	return time.Time{}
}

func accountGroupQueryKey(key string) string {
	return strings.TrimSpace(key)
}

func accountGroupReturnKey(r *http.Request) string {
	if r == nil {
		return ""
	}
	if key := strings.TrimSpace(r.FormValue("current_group")); key != "" {
		return key
	}
	return strings.TrimSpace(r.URL.Query().Get("group"))
}

func accountsGroupHref(groupKey string) string {
	groupKey = strings.TrimSpace(groupKey)
	if groupKey == "" {
		return "/admin/accounts"
	}
	return "/admin/accounts?group=" + url.QueryEscape(groupKey)
}

func selectActiveAccountGroup(groups []controlplane.AccountGroupSection, requested string) (string, controlplane.AccountGroupSection) {
	if len(groups) == 0 {
		return "", controlplane.AccountGroupSection{}
	}
	requested = strings.TrimSpace(requested)
	if requested == "" {
		for _, group := range groups {
			if len(group.Accounts) > 0 {
				return group.Key, group
			}
		}
		return groups[0].Key, groups[0]
	}
	for _, group := range groups {
		if strings.EqualFold(group.Key, requested) {
			return group.Key, group
		}
	}
	for _, group := range groups {
		if len(group.Accounts) > 0 {
			return group.Key, group
		}
	}
	return groups[0].Key, groups[0]
}

func requestBaseURL(r *http.Request) string {
	return requestOrigin(r)
}

func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r != nil && r.TLS != nil {
		scheme = "https"
	}
	if forwarded := trustedForwardedProto(r); forwarded != "" {
		scheme = forwarded
	}
	host := requestDomain(r)
	return scheme + "://" + host
}

func requestDomain(r *http.Request) string {
	if r == nil {
		return ""
	}
	host := strings.TrimSpace(r.Host)
	if forwardedHost := trustedForwardedHost(r); forwardedHost != "" {
		host = forwardedHost
	}
	return host
}

func requestIsHTTPS(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	return trustedForwardedProto(r) == "https"
}

func trustedForwardedProto(r *http.Request) string {
	if !shouldTrustForwardedHeaders(r) {
		return ""
	}
	switch strings.ToLower(firstForwardedValue(r.Header.Get("X-Forwarded-Proto"))) {
	case "http":
		return "http"
	case "https":
		return "https"
	default:
		return ""
	}
}

func trustedForwardedHost(r *http.Request) string {
	if !shouldTrustForwardedHeaders(r) {
		return ""
	}
	return cleanForwardedHost(firstForwardedValue(r.Header.Get("X-Forwarded-Host")))
}

func shouldTrustForwardedHeaders(r *http.Request) bool {
	if r == nil {
		return false
	}
	return trustedProxiesFromRequest(r).contains(normalizeClientIP(r.RemoteAddr))
}

func cleanForwardedHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" || strings.ContainsAny(host, "/\\@ \t\r\n") {
		return ""
	}
	return host
}

func firstForwardedValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if before, _, ok := strings.Cut(value, ","); ok {
		return strings.TrimSpace(before)
	}
	return value
}

func (s *Server) claudeOAuthRedirectURI(r *http.Request) string {
	baseURL := strings.TrimRight(strings.TrimSpace(s.control.PublicBaseURL()), "/")
	if baseURL == "" {
		baseURL = requestBaseURL(r)
	}
	return baseURL + "/admin/connect/claude/oauth/callback"
}

func auditPageURL(filter controlplane.AuditFilter, page int) string {
	if page < 1 {
		page = 1
	}
	values := make([]string, 0, 5)
	if filter.Kind != "" {
		values = append(values, "kind="+url.QueryEscape(string(filter.Kind)))
	}
	if filter.Status != "" {
		values = append(values, "status="+url.QueryEscape(filter.Status))
	}
	if filter.Actor != "" {
		values = append(values, "actor="+url.QueryEscape(filter.Actor))
	}
	if filter.Resource != "" {
		values = append(values, "resource="+url.QueryEscape(filter.Resource))
	}
	values = append(values, "page="+strconv.Itoa(page))
	return "/admin/audit?" + strings.Join(values, "&")
}

func usageLogPageURL(filter controlplane.UsageLogFilter, page int) string {
	if page < 1 {
		page = 1
	}
	values := make([]string, 0, 7)
	if filter.UserID != "" {
		values = append(values, "user_id="+url.QueryEscape(filter.UserID))
	}
	if filter.ClientID != "" {
		values = append(values, "client_id="+url.QueryEscape(filter.ClientID))
	}
	if filter.Model != "" {
		values = append(values, "model="+url.QueryEscape(filter.Model))
	}
	if filter.Status != "" {
		values = append(values, "status="+url.QueryEscape(string(filter.Status)))
	}
	if !filter.StartedAt.IsZero() {
		values = append(values, "started_at="+url.QueryEscape(datetimeInputValue(filter.StartedAt)))
	}
	if !filter.EndedAt.IsZero() {
		values = append(values, "ended_at="+url.QueryEscape(datetimeInputValue(filter.EndedAt)))
	}
	values = append(values, "page="+strconv.Itoa(page))
	return "/logs?" + strings.Join(values, "&")
}

func paymentOrderPageURL(filter controlplane.PaymentOrderFilter, page int) string {
	if page < 1 {
		page = 1
	}
	values := make([]string, 0, 4)
	if filter.UserID != "" {
		values = append(values, "user_id="+url.QueryEscape(filter.UserID))
	}
	if filter.Provider != "" {
		values = append(values, "provider="+url.QueryEscape(string(filter.Provider)))
	}
	if filter.Status != "" {
		values = append(values, "status="+url.QueryEscape(string(filter.Status)))
	}
	values = append(values, "page="+strconv.Itoa(page))
	return "/payments?" + strings.Join(values, "&")
}

func financeUserPageURL(page int) string {
	if page < 1 {
		page = 1
	}
	return "/admin/finance?tab=users&user_page=" + strconv.Itoa(page)
}

func userPageURL(filter userPageFilter, page int) string {
	if page < 1 {
		page = 1
	}
	values := make([]string, 0, 7)
	if filter.Query != "" {
		values = append(values, "q="+url.QueryEscape(filter.Query))
	}
	if filter.Role != "" {
		values = append(values, "role="+url.QueryEscape(string(filter.Role)))
	}
	if filter.Status != "" {
		values = append(values, "status="+url.QueryEscape(filter.Status))
	}
	if filter.Inviter != "" {
		values = append(values, "inviter="+url.QueryEscape(filter.Inviter))
	}
	if filter.Sort != "" {
		values = append(values, "sort="+url.QueryEscape(filter.Sort))
	}
	if filter.Direction != "" {
		values = append(values, "direction="+url.QueryEscape(filter.Direction))
	}
	values = append(values, "page="+strconv.Itoa(page))
	return "/admin/users?" + strings.Join(values, "&")
}

func userSearchPageURL(userID string) string {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return "/admin/users"
	}
	return "/admin/users?q=" + url.QueryEscape(userID)
}

func financeUsageLogPageURL(filter controlplane.UsageLogFilter, page int) string {
	if page < 1 {
		page = 1
	}
	values := make([]string, 0, 7)
	values = append(values, "tab=usage")
	if filter.UserID != "" {
		values = append(values, "usage_user_id="+url.QueryEscape(filter.UserID))
	}
	if filter.ClientID != "" {
		values = append(values, "usage_client_id="+url.QueryEscape(filter.ClientID))
	}
	if filter.Model != "" {
		values = append(values, "usage_model="+url.QueryEscape(filter.Model))
	}
	if filter.Status != "" {
		values = append(values, "usage_status="+url.QueryEscape(string(filter.Status)))
	}
	if !filter.StartedAt.IsZero() {
		values = append(values, "usage_started_at="+url.QueryEscape(datetimeInputValue(filter.StartedAt)))
	}
	if !filter.EndedAt.IsZero() {
		values = append(values, "usage_ended_at="+url.QueryEscape(datetimeInputValue(filter.EndedAt)))
	}
	values = append(values, "usage_page="+strconv.Itoa(page))
	return "/admin/finance?" + strings.Join(values, "&")
}

func formatInteger(value any) string {
	var n int64
	switch v := value.(type) {
	case int:
		n = int64(v)
	case int64:
		n = v
	case int32:
		n = int64(v)
	case uint:
		if uint64(v) > uint64(^uint64(0)>>1) {
			n = int64(^uint64(0) >> 1)
		} else {
			n = int64(v)
		}
	case uint64:
		if v > uint64(^uint64(0)>>1) {
			n = int64(^uint64(0) >> 1)
		} else {
			n = int64(v)
		}
	default:
		return fmt.Sprint(value)
	}
	sign := ""
	if n < 0 {
		sign = "-"
		n = -n
	}
	raw := strconv.FormatInt(n, 10)
	if len(raw) <= 3 {
		return sign + raw
	}
	var b strings.Builder
	b.Grow(len(raw) + len(raw)/3 + len(sign))
	b.WriteString(sign)
	prefix := len(raw) % 3
	if prefix == 0 {
		prefix = 3
	}
	b.WriteString(raw[:prefix])
	for i := prefix; i < len(raw); i += 3 {
		b.WriteByte(',')
		b.WriteString(raw[i : i+3])
	}
	return b.String()
}

func financePaymentOrderPageURL(filter controlplane.PaymentOrderFilter, page int) string {
	if page < 1 {
		page = 1
	}
	values := make([]string, 0, 5)
	values = append(values, "tab=orders")
	if filter.UserID != "" {
		values = append(values, "order_user_id="+url.QueryEscape(filter.UserID))
	}
	if filter.Provider != "" {
		values = append(values, "order_provider="+url.QueryEscape(string(filter.Provider)))
	}
	if filter.Status != "" {
		values = append(values, "order_status="+url.QueryEscape(string(filter.Status)))
	}
	values = append(values, "order_page="+strconv.Itoa(page))
	return "/admin/finance?" + strings.Join(values, "&")
}

func datetimeInputValue(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	local := value.Local()
	if local.Nanosecond() != 0 {
		return local.Format("2006-01-02T15:04:05.999999999")
	}
	if local.Second() != 0 {
		return local.Format("2006-01-02T15:04:05")
	}
	return local.Format("2006-01-02T15:04")
}

func financeReconcileIssueKindText(locale, kind string) string {
	switch strings.TrimSpace(kind) {
	case "negative_balance":
		return translate(locale, "finance_issue_negative_balance")
	case "client_spend_over_limit":
		return translate(locale, "finance_issue_client_spend_over_limit")
	case "stale_reserved_request":
		return translate(locale, "finance_issue_stale_reserved_request")
	default:
		if strings.TrimSpace(kind) == "" {
			return "-"
		}
		return kind
	}
}

func financeReconcileSeverityText(locale, severity string) string {
	switch strings.TrimSpace(severity) {
	case "error":
		return translate(locale, "severity_error")
	case "warning":
		return translate(locale, "severity_warning")
	default:
		if strings.TrimSpace(severity) == "" {
			return "-"
		}
		return severity
	}
}

func financeReconcileSeverityClass(severity string) string {
	switch strings.TrimSpace(severity) {
	case "error":
		return "tone-bad"
	case "warning":
		return "tone-warn"
	default:
		return "tone-muted"
	}
}

func usageStatusText(locale string, status core.BillingRequestStatus) string {
	switch status {
	case core.BillingRequestReserved:
		return translate(locale, "usage_status_reserved")
	case core.BillingRequestSettled:
		return translate(locale, "usage_status_settled")
	case core.BillingRequestReleased:
		return translate(locale, "usage_status_released")
	case core.BillingRequestUsageMissing:
		return translate(locale, "usage_status_usage_missing")
	default:
		if strings.TrimSpace(string(status)) == "" {
			return "-"
		}
		return string(status)
	}
}

func usageStatusClass(status core.BillingRequestStatus) string {
	switch status {
	case core.BillingRequestSettled:
		return "tone-good"
	case core.BillingRequestReserved, core.BillingRequestUsageMissing:
		return "tone-warn"
	case core.BillingRequestReleased:
		return "tone-muted"
	default:
		return "tone-muted"
	}
}

func paymentProviderText(locale string, provider core.PaymentProvider) string {
	switch provider {
	case core.PaymentProviderAlipay:
		return translate(locale, "alipay")
	case core.PaymentProviderWeChatPay:
		return translate(locale, "wechat_pay")
	case core.PaymentProviderPersonalPay:
		return translate(locale, "personalpay_wechat")
	default:
		if strings.TrimSpace(string(provider)) == "" {
			return "-"
		}
		return string(provider)
	}
}

func paymentOrderMethodText(locale string, order core.PaymentOrder) string {
	return paymentProviderText(locale, order.Provider)
}

func paymentOrderTypeText(locale string, order core.PaymentOrder) string {
	switch order.Provider {
	case core.PaymentProviderAlipay:
		switch order.Channel {
		case core.PaymentChannelWAP:
			return translate(locale, "alipay_wap")
		case core.PaymentChannelPage:
			return translate(locale, "alipay_page")
		}
	case core.PaymentProviderWeChatPay:
		switch order.Channel {
		case core.PaymentChannelNative:
			return translate(locale, "wechat_native")
		case core.PaymentChannelWAP:
			return translate(locale, "wechat_wap")
		case core.PaymentChannelJSAPI:
			return translate(locale, "wechat_jsapi")
		}
	case core.PaymentProviderPersonalPay:
		switch order.Channel {
		case core.PaymentChannelWeChat:
			return translate(locale, "personalpay_temporary_wechat")
		case core.PaymentChannelAlipay:
			return translate(locale, "personalpay_alipay")
		}
	}
	if strings.TrimSpace(string(order.Channel)) == "" {
		return "-"
	}
	return string(order.Channel)
}

func paymentStatusText(locale string, status core.PaymentOrderStatus) string {
	switch status {
	case core.PaymentOrderPending:
		return translate(locale, "payment_status_pending")
	case core.PaymentOrderPaid:
		return translate(locale, "payment_status_paid")
	case core.PaymentOrderClosed:
		return translate(locale, "payment_status_closed")
	case core.PaymentOrderFailed:
		return translate(locale, "payment_status_failed")
	default:
		if strings.TrimSpace(string(status)) == "" {
			return "-"
		}
		return string(status)
	}
}

func paymentStatusClass(status core.PaymentOrderStatus) string {
	switch status {
	case core.PaymentOrderPaid:
		return "tone-good"
	case core.PaymentOrderPending:
		return "tone-warn"
	case core.PaymentOrderClosed:
		return "tone-muted"
	case core.PaymentOrderFailed:
		return "tone-bad"
	default:
		return "tone-muted"
	}
}

func paymentRefundStatusText(locale string, status core.PaymentRefundStatus) string {
	switch status {
	case core.PaymentRefundPending:
		return translate(locale, "payment_refund_status_pending")
	case core.PaymentRefundDone:
		return translate(locale, "payment_refund_status_done")
	case core.PaymentRefundFailed:
		return translate(locale, "payment_refund_status_failed")
	default:
		if strings.TrimSpace(string(status)) == "" {
			return "-"
		}
		return string(status)
	}
}

func paymentRefundStatusClass(status core.PaymentRefundStatus) string {
	switch status {
	case core.PaymentRefundDone:
		return "tone-good"
	case core.PaymentRefundPending:
		return "tone-warn"
	case core.PaymentRefundFailed:
		return "tone-bad"
	default:
		return "tone-muted"
	}
}

func usageBillingAmountText(locale string, request core.BillingReservation) string {
	switch request.Status {
	case core.BillingRequestReleased:
		return translate(locale, "not_charged")
	case core.BillingRequestReserved:
		return "-"
	default:
		return formatUsageCostDisplay(request.ActualNanoUSD)
	}
}

func usageFirstTokenText(request core.BillingReservation) string {
	if request.FirstTokenMS <= 0 {
		return "-"
	}
	if request.FirstTokenMS >= 1000 {
		seconds := float64(request.FirstTokenMS) / 1000
		return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(seconds, 'f', 1, 64), "0"), ".") + " s"
	}
	return strconv.FormatInt(request.FirstTokenMS, 10) + " ms"
}

func usageAccountGroupLabel(locale, group string, request core.BillingReservation) string {
	label := accountGroupLabelText(locale, group)
	if core.NormalizeClientBillingSource(request.BillingSource) != core.ClientBillingSourcePlan {
		return label
	}
	if label == "" {
		return "[T]"
	}
	return label + " [T]"
}

func usageFailedAccountTitle(_ string, labels []string) string {
	values := make([]string, 0, len(labels))
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label != "" {
			values = append(values, label)
		}
	}
	if len(values) == 0 {
		return ""
	}
	return strings.Join(values, "\n")
}

func usageLogModelText(request core.BillingReservation) string {
	model := strings.TrimSpace(request.Model)
	if !request.FastMode {
		return model
	}
	if model == "" {
		return "[fast]"
	}
	return model + " [fast]"
}
