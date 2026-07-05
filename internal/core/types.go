package core

import (
	"encoding/json"
	"math"
	"strings"
	"time"
)

type ProviderKind string

const (
	ProviderOpenAI ProviderKind = "openai"
	ProviderClaude ProviderKind = "claude"
)

type AccountStatus string

const (
	AccountStatusActive         AccountStatus = "active"
	AccountStatusCooling        AccountStatus = "cooldown"
	AccountStatusExpired        AccountStatus = "expired"
	AccountStatusBlocked        AccountStatus = "blocked"
	AccountStatusProviderBanned AccountStatus = "provider_banned"
	AccountStatusRefreshing     AccountStatus = "refreshing"
)

type Credential struct {
	Mode         string
	AccessToken  string
	RefreshToken string
	SessionToken string
	ExpiresAt    *time.Time
	Metadata     map[string]string
}

type AccountQuotaWindow struct {
	Name          string
	UsedPercent   float64
	WindowMinutes int64
	ResetsAt      *time.Time
}

type AccountQuotaCredits struct {
	HasCredits bool
	Unlimited  bool
	Balance    *float64
}

type AccountImageQuota struct {
	Remaining  int64
	Unknown    bool
	ResetAfter string
	ResetsAt   *time.Time
}

type AccountQuotaSnapshot struct {
	LimitID     string
	LimitName   string
	Source      string
	Plan        string
	Primary     *AccountQuotaWindow
	Secondary   *AccountQuotaWindow
	Credits     *AccountQuotaCredits
	Image       *AccountImageQuota
	ReachedType string
	Additional  map[string]AccountQuotaSnapshot
	RefreshedAt *time.Time
}

const (
	AccountQuotaMetadataKey            = "quota_snapshot"
	AccountQuotaErrorMetadataKey       = "quota_refresh_error"
	AccountQuotaErrorAtMetadataKey     = "quota_refresh_error_at"
	AccountQuotaErrorCodeMetadataKey   = "quota_refresh_error_code"
	AccountQuotaErrorStatusMetadataKey = "quota_refresh_error_status"
	AccountQuotaRuntimeSource          = "ai_gateway_runtime"
	AccountQuotaRuntimeChatLimitID     = "runtime_chat_rate_limit"
	AccountQuotaRuntimeImageLimitID    = "runtime_image_rate_limit"
)

type Account struct {
	ID                string
	Provider          ProviderKind
	Label             string
	Remark            string
	Group             string
	ProxyURL          string
	EffectiveProxyURL string `json:"-"`
	Status            AccountStatus
	ControlDisabled   bool
	Backup            bool
	Weight            int
	Priority          int
	Tags              []string
	Credential        Credential
	LastUsedAt        *time.Time
	CooldownUntil     *time.Time
	ConsecutiveFails  int
	TotalFails        int
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type AccountGroup struct {
	ID                            string
	Name                          string
	Type                          string
	Remark                        string
	ProxyURL                      string
	ShowInClientEditor            *bool
	VisibleUserIDs                []string
	BillingMultiplierBps          int64
	PlanBillingMultiplierBps      int64
	PlanBillingEnabled            *bool
	TimedMultipliers              []AccountGroupTimedMultiplier
	InputPriceNanoUSDPer1M        int64
	CachedInputPriceNanoUSDPer1M  int64
	CacheWritePriceNanoUSDPer1M   int64
	CacheWrite5mPriceNanoUSDPer1M int64
	CacheWrite1hPriceNanoUSDPer1M int64
	OutputPriceNanoUSDPer1M       int64
	ImageOutputPriceNanoUSDPer1M  int64
	CreatedAt                     time.Time
	UpdatedAt                     time.Time
}

const (
	DefaultAccountGroupID   = "default"
	DefaultAccountGroupName = "Default"

	AccountGroupTypeMixed  = "mixed"
	AccountGroupTypeOpenAI = "openai"
	AccountGroupTypeClaude = "claude"
)

func NormalizeAccountGroupName(group string) string {
	group = strings.TrimSpace(group)
	if group == "" {
		return DefaultAccountGroupName
	}
	return group
}

func NormalizeAccountGroupType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case AccountGroupTypeOpenAI, "codex", "openai/codex", "openai_codex":
		return AccountGroupTypeOpenAI
	case AccountGroupTypeClaude, "anthropic":
		return AccountGroupTypeClaude
	default:
		return AccountGroupTypeMixed
	}
}

func AccountGroupTypeAllowsProvider(group AccountGroup, provider ProviderKind) bool {
	switch NormalizeAccountGroupType(group.Type) {
	case AccountGroupTypeOpenAI:
		return provider == ProviderOpenAI
	case AccountGroupTypeClaude:
		return provider == ProviderClaude
	default:
		return true
	}
}

func AccountGroupVisibleInClientEditor(group AccountGroup) bool {
	return group.ShowInClientEditor == nil || *group.ShowInClientEditor
}

func AccountGroupVisibleInClientEditorForUser(group AccountGroup, userID string) bool {
	if strings.EqualFold(strings.TrimSpace(group.ID), DefaultAccountGroupID) || strings.EqualFold(NormalizeAccountGroupName(group.Name), DefaultAccountGroupName) {
		return true
	}
	if AccountGroupVisibleInClientEditor(group) {
		return true
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false
	}
	for _, visibleUserID := range group.VisibleUserIDs {
		if strings.EqualFold(strings.TrimSpace(visibleUserID), userID) {
			return true
		}
	}
	return false
}

type AccountGroupTimedMultiplier struct {
	ID            string
	Name          string
	Enabled       bool
	MultiplierBps int64
	Weekdays      []int
	StartDate     string
	EndDate       string
	StartTime     string
	EndTime       string
	Priority      int
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type GatewayRequest struct {
	Model                 string                     `json:"model"`
	Messages              []Message                  `json:"messages"`
	RawMessages           json.RawMessage            `json:"-"`
	RawBody               json.RawMessage            `json:"-"`
	Client                *APIClient                 `json:"-"`
	UpstreamMode          string                     `json:"-"`
	Stream                bool                       `json:"stream"`
	StreamIncludeUsage    *bool                      `json:"-"`
	MaxTokens             *int                       `json:"max_tokens,omitempty"`
	MaxCompletionTokens   *int                       `json:"max_completion_tokens,omitempty"`
	ServiceTier           string                     `json:"service_tier,omitempty"`
	ReasoningEffort       string                     `json:"reasoning_effort,omitempty"`
	PromptCacheKey        string                     `json:"prompt_cache_key,omitempty"`
	Temperature           *float64                   `json:"temperature,omitempty"`
	TopP                  *float64                   `json:"top_p,omitempty"`
	Stop                  []string                   `json:"stop,omitempty"`
	Metadata              map[string]string          `json:"metadata,omitempty"`
	Extra                 map[string]json.RawMessage `json:"extra,omitempty"`
	StrictAccountAffinity bool                       `json:"-"`
	PreferredAccountID    string                     `json:"-"`
	ExcludedAccountIDs    []string                   `json:"-"`
}

type ResponsesTransport string

const (
	ResponsesTransportHTTP      ResponsesTransport = "http"
	ResponsesTransportSSE       ResponsesTransport = "sse"
	ResponsesTransportWebSocket ResponsesTransport = "websocket"
)

type ResponsesRequest struct {
	Model                 string             `json:"model"`
	RawBody               json.RawMessage    `json:"-"`
	Client                *APIClient         `json:"-"`
	Transport             ResponsesTransport `json:"-"`
	Stream                bool               `json:"stream"`
	Compact               bool               `json:"-"`
	Generate              *bool              `json:"generate,omitempty"`
	MaxOutputTokens       *int               `json:"max_output_tokens,omitempty"`
	ServiceTier           string             `json:"service_tier,omitempty"`
	PreviousResponseID    string             `json:"previous_response_id,omitempty"`
	PromptCacheKey        string             `json:"prompt_cache_key,omitempty"`
	Metadata              map[string]string  `json:"metadata,omitempty"`
	Headers               map[string]string  `json:"-"`
	StrictAccountAffinity bool               `json:"-"`
	PreferredAccountID    string             `json:"-"`
	ExcludedAccountIDs    []string           `json:"-"`
}

type Usage struct {
	PromptTokens          int `json:"prompt_tokens"`
	CachedPromptTokens    int `json:"cached_prompt_tokens,omitempty"`
	CacheCreationTokens   int `json:"cache_creation_tokens,omitempty"`
	CacheCreation5mTokens int `json:"cache_creation_5m_tokens,omitempty"`
	CacheCreation1hTokens int `json:"cache_creation_1h_tokens,omitempty"`
	CompletionTokens      int `json:"completion_tokens"`
	ImageOutputTokens     int `json:"image_output_tokens,omitempty"`
	TotalTokens           int `json:"total_tokens"`
}

const NanoUSDPerUSD int64 = 1_000_000_000

type GatewayResponse struct {
	ID           string       `json:"id"`
	Model        string       `json:"model"`
	Provider     ProviderKind `json:"provider"`
	AccountID    string       `json:"account_id"`
	AccountLabel string       `json:"account_label"`
	ServiceTier  string       `json:"service_tier,omitempty"`
	Content      string       `json:"content"`
	FinishReason string       `json:"finish_reason"`
	CreatedAt    time.Time    `json:"created_at"`
	Usage        Usage        `json:"usage"`
	RawBody      []byte       `json:"-"`
	FirstTokenMS int64        `json:"-"`
}

type OpenAIResponseBinding struct {
	ResponseID     string
	AccountID      string
	ClientID       string
	PromptCacheKey string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type EmbeddingRequest struct {
	Model          string            `json:"model"`
	Input          []string          `json:"input"`
	RawInput       json.RawMessage   `json:"-"`
	RawBody        json.RawMessage   `json:"-"`
	EncodingFormat string            `json:"encoding_format,omitempty"`
	Dimensions     *int              `json:"dimensions,omitempty"`
	User           string            `json:"user,omitempty"`
	Client         *APIClient        `json:"-"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

type EmbeddingObject struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

type EmbeddingResponse struct {
	Model        string            `json:"model"`
	Provider     ProviderKind      `json:"provider"`
	AccountID    string            `json:"account_id"`
	AccountLabel string            `json:"account_label"`
	Data         []EmbeddingObject `json:"data"`
	Usage        Usage             `json:"usage"`
	RawBody      []byte            `json:"-"`
}

type ModerationRequest struct {
	Model    string            `json:"model"`
	Input    any               `json:"input"`
	Client   *APIClient        `json:"-"`
	Metadata map[string]string `json:"metadata,omitempty"`
	RawBody  json.RawMessage   `json:"-"`
}

type ModerationResponse struct {
	Model        string       `json:"model"`
	Provider     ProviderKind `json:"provider"`
	AccountID    string       `json:"account_id"`
	AccountLabel string       `json:"account_label"`
	Body         []byte       `json:"-"`
}

type ImageGenerationRequest struct {
	Model    string                     `json:"model"`
	Prompt   string                     `json:"prompt"`
	Client   *APIClient                 `json:"-"`
	Metadata map[string]string          `json:"metadata,omitempty"`
	Extra    map[string]json.RawMessage `json:"extra,omitempty"`
	RawBody  json.RawMessage            `json:"-"`
}

type ImageGenerationResponse struct {
	RequestID    string       `json:"-"`
	Model        string       `json:"model"`
	Provider     ProviderKind `json:"provider"`
	AccountID    string       `json:"account_id"`
	AccountLabel string       `json:"account_label"`
	Body         []byte       `json:"-"`
}

type ImageMultipartRequest struct {
	Model       string            `json:"model"`
	Endpoint    string            `json:"endpoint"`
	ContentType string            `json:"content_type"`
	Body        []byte            `json:"-"`
	Client      *APIClient        `json:"-"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	FormFields  map[string]string `json:"-"`
}

type ImageMultipartResponse struct {
	RequestID    string       `json:"-"`
	Model        string       `json:"model"`
	Provider     ProviderKind `json:"provider"`
	AccountID    string       `json:"account_id"`
	AccountLabel string       `json:"account_label"`
	Body         []byte       `json:"-"`
	ContentType  string       `json:"content_type"`
}

type AudioSpeechRequest struct {
	Model    string                     `json:"model"`
	Input    string                     `json:"input"`
	Voice    string                     `json:"voice"`
	Client   *APIClient                 `json:"-"`
	Metadata map[string]string          `json:"metadata,omitempty"`
	Extra    map[string]json.RawMessage `json:"extra,omitempty"`
	RawBody  json.RawMessage            `json:"-"`
}

type AudioSpeechResponse struct {
	Model        string       `json:"model"`
	Provider     ProviderKind `json:"provider"`
	AccountID    string       `json:"account_id"`
	AccountLabel string       `json:"account_label"`
	Body         []byte       `json:"-"`
	ContentType  string       `json:"content_type"`
}

type AudioMultipartRequest struct {
	Model       string            `json:"model"`
	Endpoint    string            `json:"endpoint"`
	ContentType string            `json:"content_type"`
	Body        []byte            `json:"-"`
	Client      *APIClient        `json:"-"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	FormFields  map[string]string `json:"-"`
}

type AudioMultipartResponse struct {
	Model        string       `json:"model"`
	Provider     ProviderKind `json:"provider"`
	AccountID    string       `json:"account_id"`
	AccountLabel string       `json:"account_label"`
	Body         []byte       `json:"-"`
	ContentType  string       `json:"content_type"`
}

type TokenCountRequest struct {
	Model    string            `json:"model"`
	Client   *APIClient        `json:"-"`
	Metadata map[string]string `json:"metadata,omitempty"`
	RawBody  json.RawMessage   `json:"-"`
}

type TokenCountResponse struct {
	Model        string       `json:"model"`
	Provider     ProviderKind `json:"provider"`
	AccountID    string       `json:"account_id"`
	AccountLabel string       `json:"account_label"`
	Body         []byte       `json:"-"`
}

type StreamEvent struct {
	Started      bool   `json:"started,omitempty"`
	Delta        string `json:"delta,omitempty"`
	FirstOutput  bool   `json:"-"`
	FinishReason string `json:"finish_reason,omitempty"`
	Usage        *Usage `json:"usage,omitempty"`
	Done         bool   `json:"done,omitempty"`
	RawEvent     string `json:"-"`
	RawData      []byte `json:"-"`
}

type ModelSpec struct {
	Name      string
	Provider  ProviderKind
	Type      string
	Aliases   []string
	CreatedAt time.Time
}

type ModelSource string

const (
	ModelSourceManual   ModelSource = "manual"
	ModelSourceUpstream ModelSource = "upstream"
)

const (
	ModelTypeText      = "text"
	ModelTypeImage     = "image"
	ModelTypeEmbedding = "embedding"
	ModelTypeAudio     = "audio"
	ModelTypeVideo     = "video"
)

const (
	BillingModalityText  = "text"
	BillingModalityAudio = "audio"
	BillingModalityImage = "image"
	BillingModalityVideo = "video"
	BillingModalityTool  = "tool"

	BillingUnitSecond  = "second"
	BillingUnitRequest = "request"
	BillingUnitSession = "session"
)

const (
	ModelBillingModeToken      = "token"
	ModelBillingModeRequest    = "request"
	ModelBillingModeTieredExpr = "tiered_expr"
)

type ModelPricingTier struct {
	Name                     string `json:"name,omitempty"`
	MaxInputTokens           int    `json:"max_input_tokens,omitempty"`
	InputPriceNanoUSD        int64  `json:"input_price_nano_usd,omitempty"`
	CachedInputPriceNanoUSD  int64  `json:"cached_input_price_nano_usd,omitempty"`
	CacheWritePriceNanoUSD   int64  `json:"cache_write_price_nano_usd,omitempty"`
	CacheWrite5mPriceNanoUSD int64  `json:"cache_write_5m_price_nano_usd,omitempty"`
	CacheWrite1hPriceNanoUSD int64  `json:"cache_write_1h_price_nano_usd,omitempty"`
	OutputPriceNanoUSD       int64  `json:"output_price_nano_usd,omitempty"`
	ImageOutputPriceNanoUSD  int64  `json:"image_output_price_nano_usd,omitempty"`
}

type ModelConfig struct {
	ID                            string
	Provider                      ProviderKind
	Type                          string
	UpstreamID                    string
	DisplayName                   string
	OwnedBy                       string
	Source                        ModelSource
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
	PricingTiers                  []ModelPricingTier
	LastSyncedAt                  *time.Time
	CreatedAt                     time.Time
	UpdatedAt                     time.Time
}

func NormalizeModelType(value string, modelID string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "chat", "completion", "completions", "response", "responses", ModelTypeText:
		if strings.TrimSpace(value) == "" {
			return InferModelType(modelID)
		}
		return ModelTypeText
	case ModelTypeImage, "images", "vision-generation":
		return ModelTypeImage
	case ModelTypeEmbedding, "embeddings", "embed":
		return ModelTypeEmbedding
	case ModelTypeAudio, "speech", "tts", "transcription", "transcriptions", "translation", "translations":
		return ModelTypeAudio
	case ModelTypeVideo, "videos", "sora", "veo":
		return ModelTypeVideo
	default:
		return InferModelType(modelID)
	}
}

func InferModelType(modelID string) string {
	normalized := strings.ToLower(strings.TrimSpace(modelID))
	switch {
	case strings.Contains(normalized, "image") ||
		strings.Contains(normalized, "dall-e") ||
		strings.Contains(normalized, "imagen"):
		return ModelTypeImage
	case strings.Contains(normalized, "embedding") ||
		strings.HasPrefix(normalized, "text-embedding-"):
		return ModelTypeEmbedding
	case strings.Contains(normalized, "whisper") ||
		strings.Contains(normalized, "tts") ||
		strings.Contains(normalized, "audio") ||
		strings.Contains(normalized, "speech") ||
		strings.Contains(normalized, "transcribe"):
		return ModelTypeAudio
	case strings.Contains(normalized, "video") ||
		strings.Contains(normalized, "sora") ||
		strings.Contains(normalized, "veo"):
		return ModelTypeVideo
	default:
		return ModelTypeText
	}
}

type AttemptRecord struct {
	Provider     ProviderKind `json:"provider"`
	AccountID    string       `json:"account_id,omitempty"`
	AccountLabel string       `json:"account_label,omitempty"`
	Status       string       `json:"status"`
	ErrorCode    string       `json:"error_code,omitempty"`
	ErrorMessage string       `json:"error_message,omitempty"`
	Temporary    bool         `json:"temporary,omitempty"`
}

type MonitorStatus string

const (
	MonitorStatusUnknown  MonitorStatus = "unknown"
	MonitorStatusOK       MonitorStatus = "ok"
	MonitorStatusDegraded MonitorStatus = "degraded"
	MonitorStatusFailed   MonitorStatus = "failed"
)

type MonitorTarget struct {
	ID              string
	Name            string
	AccountGroup    string
	Model           string
	Enabled         bool
	PublicVisible   bool
	IntervalSeconds int
	TimeoutSeconds  int
	Prompt          string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type MonitorResult struct {
	ID           string
	TargetID     string
	Status       MonitorStatus
	LatencyMS    int64
	Provider     ProviderKind
	AccountID    string
	AccountLabel string
	Attempts     []AttemptRecord
	ErrorCode    string
	ErrorMessage string
	CheckedAt    time.Time
}

type RouteRule struct {
	ModelPrefix        string
	PreferredProviders []ProviderKind
}

type RoutePolicy struct {
	DefaultProvider   ProviderKind
	FallbackProviders []ProviderKind
	Rules             []RouteRule
}

type RoutePlan struct {
	Providers []ProviderKind
	Model     string
	Reason    string
}

type RouteDecision struct {
	Provider ProviderKind
	Account  Account
	Model    string
	Reason   string
}

type SystemSettings struct {
	Runtime       SystemRuntimeSettings
	Network       SystemNetworkSettings
	Image         SystemImageSettings
	OAuth         SystemOAuthSettings
	Email         SystemEmailSettings
	Home          SystemHomeSettings
	Payment       SystemPaymentSettings
	Registration  SystemRegistrationSettings
	Invitation    SystemInvitationSettings
	UserDashboard SystemUserDashboardSettings
	Retention     SystemRetentionSettings
	Backup        SystemBackupSettings
	UpdatedAt     time.Time
}

type SystemRuntimeSettings struct {
	PublicBaseURL                     string
	AllowPublicRegistration           bool
	RegistrationEmailAllowlistEnabled bool
	RegistrationEmailAllowlist        []string
	UserConcurrentRequestLimit        int
	PlanConcurrentRequestLimit        int
	UserRequestRateLimitPerMinute     int
	ResponsesWebSocketUpstreamEnabled *bool
}

type SystemNetworkSettings struct {
	SystemProxyURL string
}

type SystemImageSettings struct {
	Backend            string
	UserConsoleEnabled *bool
}

type SystemOAuthSettings struct {
	OpenAIEnabled       bool
	ClaudeEnabled       bool
	GitHubLoginEnabled  bool
	GitHubLoginClientID string
	GitHubLoginSecret   string
	GoogleLoginEnabled  bool
	GoogleLoginClientID string
	GoogleLoginSecret   string
	LinuxDOLoginEnabled bool
	LinuxDOClientID     string
	LinuxDOSecret       string
	LoginAutoCreateUser bool
}

type SystemEmailSettings struct {
	RegistrationVerificationEnabled bool
	Provider                        string
	SMTPHost                        string
	SMTPPort                        int
	SMTPUsername                    string
	SMTPPassword                    string
	CloudMailBaseURL                string
	CloudMailEmail                  string
	CloudMailPassword               string
	CloudMailAccountID              int
	FromEmail                       string
	FromName                        string
	VerificationSubjectTemplate     string
	VerificationTextTemplate        string
	VerificationHTMLTemplate        string
	CodeTTLSeconds                  int
	SendCooldownSeconds             int
	HourlySendLimit                 int
	MaxAttempts                     int
}

type SystemHomeSettings struct {
	BrandTitle      string
	BrandSubtitle   string
	Heading         string
	Summary         string
	Availability    string
	CostMultiplier  string
	Latency         string
	Capability      string
	AvailabilityKey string
	CostKey         string
	LatencyKey      string
	CapabilityKey   string
}

type SystemPaymentSettings struct {
	CNYPerUSD          string
	RechargeInputMode  string
	MinRechargeNanoUSD int64
	MaxRechargeNanoUSD int64
	WeChatPay          WeChatPaySettings
	Alipay             AlipaySettings
	PersonalPay        PersonalPaySettings
}

type SystemRegistrationSettings struct {
	NewUserRewardEnabled   bool
	NewUserRewardNanoUSD   int64
	RequireInvitationCode  bool
	UsernameMinLength      int
	RegisterIPHourlyLimit  int
	EmailCodeIPHourlyLimit int
	TurnstileEnabled       bool
	TurnstileSiteKey       string
	TurnstileSecretKey     string
}

type SystemInvitationSettings struct {
	Enabled                  bool
	InviterRewardNanoUSD     int64
	InviterRechargeRewardBps int64
	InviteeRewardNanoUSD     int64
}

type SystemUserDashboardSettings struct {
	CustomPanelEnabled bool
	CustomPanelHTML    string
}

type WeChatPaySettings struct {
	Enabled               bool
	AppID                 string
	MchID                 string
	APIV3Key              string
	MerchantSerialNo      string
	MerchantPrivateKeyPEM string
	WeChatPayPublicKeyID  string
	WeChatPayPublicKeyPEM string
	NotifyURL             string
}

type AlipaySettings struct {
	Enabled            bool
	AppID              string
	PrivateKeyPEM      string
	AlipayPublicKeyPEM string
	GatewayURL         string
	NotifyURL          string
	ReturnURL          string
	SignType           string
}

type PersonalPaySettings struct {
	Enabled        bool
	AndroidToken   string
	ExpireAfterSec int
}

type SystemRetentionSettings struct {
	AuditLimit                 int
	UsageLogMaxAgeDays         int
	BillingLedgerRetentionDays int
}

type SystemBackupSettings struct {
	AndroidAutoEnabled bool
	AndroidTimeOfDay   string
	AndroidDataSets    []string
}

const (
	DefaultAuditRetentionLimit        = 512
	MinimumAuditRetentionLimit        = 1
	MaximumAuditRetentionLimit        = 100000
	DefaultUsageLogMaxAgeDays         = 1
	MinimumUsageLogMaxAgeDays         = 0
	MaximumUsageLogMaxAgeDays         = 365
	DefaultBillingLedgerRetentionDays = 30
	MinimumBillingLedgerRetentionDays = 3
	MaximumBillingLedgerRetentionDays = 365
	DefaultPersonalPayExpireAfterSec  = 180
	EmailProviderSMTP                 = "smtp"
	EmailProviderCloudMail            = "cloudmail"
	DefaultSMTPPort                   = 465
	DefaultEmailCodeTTLSeconds        = 600
	DefaultEmailSendCooldownSeconds   = 60
	DefaultEmailHourlySendLimit       = 5
	DefaultEmailMaxAttempts           = 5
	DefaultEmailSubjectTemplate       = "Email verification code"
	DefaultEmailTextTemplate          = "Your verification code is {{code}}. It expires in {{minutes}} minutes."
	DefaultEmailHTMLTemplate          = "<p>Your verification code is <strong>{{code}}</strong>. It expires in {{minutes}} minutes.</p>"
	DefaultUsernameMinLength          = 1
	MaximumUsernameMinLength          = 64
	DefaultRegisterIPHourlyLimit      = 20
	DefaultEmailCodeIPHourlyLimit     = 10
	ImageBackendAuto                  = "auto"
	ImageBackendOfficial              = "official"
	DefaultAndroidBackupTimeOfDay     = "03:00"
	RechargeInputModeBalanceUSD       = "balance_usd"
	RechargeInputModePaymentCNY       = "payment_cny"
)

var knownBackupDataSets = []string{"settings", "users", "accounts", "models", "clients", "billing", "messages", "documents", "audit"}

func DefaultSystemHomeSettings() SystemHomeSettings {
	return SystemHomeSettings{
		BrandTitle:      "AI Gateway",
		BrandSubtitle:   "统一 AI 接入与运营控制台",
		Heading:         "自建官方号池,无上游中间商,安全,稳定,极速",
		Summary:         "自建号池，直连官方服务器，数据不再层层转手，安全有保障",
		AvailabilityKey: "可用性",
		Availability:    "99.98%",
		CostKey:         "成本倍率",
		CostMultiplier:  "≤0.1x",
		LatencyKey:      "延迟",
		Latency:         "≤0.25s",
		CapabilityKey:   "能力",
		Capability:      "不降智",
	}
}

func NormalizeSystemHomeSettings(settings SystemHomeSettings) SystemHomeSettings {
	defaults := DefaultSystemHomeSettings()
	settings.BrandTitle = strings.TrimSpace(settings.BrandTitle)
	if settings.BrandTitle == "" {
		settings.BrandTitle = defaults.BrandTitle
	}
	settings.BrandSubtitle = strings.TrimSpace(settings.BrandSubtitle)
	if settings.BrandSubtitle == "" {
		settings.BrandSubtitle = defaults.BrandSubtitle
	}
	settings.Heading = strings.TrimSpace(settings.Heading)
	if settings.Heading == "" {
		settings.Heading = defaults.Heading
	}
	settings.Summary = strings.TrimSpace(strings.ReplaceAll(settings.Summary, "\r\n", "\n"))
	settings.Summary = strings.ReplaceAll(settings.Summary, "\r", "\n")
	settings.Summary = strings.ReplaceAll(settings.Summary, `\n`, "\n")
	if settings.Summary == "" {
		settings.Summary = defaults.Summary
	}
	settings.AvailabilityKey = strings.TrimSpace(settings.AvailabilityKey)
	if settings.AvailabilityKey == "" {
		settings.AvailabilityKey = defaults.AvailabilityKey
	}
	settings.Availability = strings.TrimSpace(settings.Availability)
	if settings.Availability == "" {
		settings.Availability = defaults.Availability
	}
	settings.CostKey = strings.TrimSpace(settings.CostKey)
	if settings.CostKey == "" {
		settings.CostKey = defaults.CostKey
	}
	settings.CostMultiplier = strings.TrimSpace(settings.CostMultiplier)
	if settings.CostMultiplier == "" {
		settings.CostMultiplier = defaults.CostMultiplier
	}
	settings.LatencyKey = strings.TrimSpace(settings.LatencyKey)
	if settings.LatencyKey == "" {
		settings.LatencyKey = defaults.LatencyKey
	}
	settings.Latency = strings.TrimSpace(settings.Latency)
	if settings.Latency == "" {
		settings.Latency = defaults.Latency
	}
	settings.CapabilityKey = strings.TrimSpace(settings.CapabilityKey)
	if settings.CapabilityKey == "" {
		settings.CapabilityKey = defaults.CapabilityKey
	}
	settings.Capability = strings.TrimSpace(settings.Capability)
	if settings.Capability == "" {
		settings.Capability = defaults.Capability
	}
	return settings
}

func DefaultSystemSettings() SystemSettings {
	return SystemSettings{
		Runtime: SystemRuntimeSettings{
			ResponsesWebSocketUpstreamEnabled: boolPtr(true),
		},
		OAuth: SystemOAuthSettings{
			OpenAIEnabled: true,
			ClaudeEnabled: true,
		},
		Email: SystemEmailSettings{
			Provider:                    EmailProviderSMTP,
			SMTPPort:                    DefaultSMTPPort,
			VerificationSubjectTemplate: DefaultEmailSubjectTemplate,
			VerificationTextTemplate:    DefaultEmailTextTemplate,
			VerificationHTMLTemplate:    DefaultEmailHTMLTemplate,
		},
		Retention: SystemRetentionSettings{
			AuditLimit:                 DefaultAuditRetentionLimit,
			UsageLogMaxAgeDays:         DefaultUsageLogMaxAgeDays,
			BillingLedgerRetentionDays: DefaultBillingLedgerRetentionDays,
		},
		Image: SystemImageSettings{
			Backend:            ImageBackendAuto,
			UserConsoleEnabled: boolPtr(true),
		},
		Registration: SystemRegistrationSettings{
			UsernameMinLength:      DefaultUsernameMinLength,
			RegisterIPHourlyLimit:  DefaultRegisterIPHourlyLimit,
			EmailCodeIPHourlyLimit: DefaultEmailCodeIPHourlyLimit,
		},
		Home: DefaultSystemHomeSettings(),
		Backup: SystemBackupSettings{
			AndroidTimeOfDay: DefaultAndroidBackupTimeOfDay,
			AndroidDataSets:  []string{"billing"},
		},
		Payment: SystemPaymentSettings{
			RechargeInputMode: RechargeInputModePaymentCNY,
			PersonalPay:       PersonalPaySettings{ExpireAfterSec: DefaultPersonalPayExpireAfterSec},
		},
	}
}

func NormalizeSystemSettings(settings SystemSettings) SystemSettings {
	settings.Runtime.PublicBaseURL = strings.TrimRight(strings.TrimSpace(settings.Runtime.PublicBaseURL), "/")
	settings.Runtime.RegistrationEmailAllowlist = normalizeEmailDomainList(settings.Runtime.RegistrationEmailAllowlist)
	if settings.Runtime.UserConcurrentRequestLimit < 0 {
		settings.Runtime.UserConcurrentRequestLimit = 0
	}
	if settings.Runtime.PlanConcurrentRequestLimit < 0 {
		settings.Runtime.PlanConcurrentRequestLimit = 0
	}
	if settings.Runtime.UserRequestRateLimitPerMinute < 0 {
		settings.Runtime.UserRequestRateLimitPerMinute = 0
	}
	if settings.Runtime.ResponsesWebSocketUpstreamEnabled == nil {
		settings.Runtime.ResponsesWebSocketUpstreamEnabled = boolPtr(true)
	}
	settings.Backup = NormalizeSystemBackupSettings(settings.Backup)
	settings.Network.SystemProxyURL = strings.TrimSpace(settings.Network.SystemProxyURL)
	settings.Image.Backend = normalizeImageBackend(settings.Image.Backend)
	if settings.Image.UserConsoleEnabled == nil {
		settings.Image.UserConsoleEnabled = boolPtr(true)
	}
	settings.OAuth.GitHubLoginClientID = strings.TrimSpace(settings.OAuth.GitHubLoginClientID)
	settings.OAuth.GitHubLoginSecret = strings.TrimSpace(settings.OAuth.GitHubLoginSecret)
	settings.OAuth.GoogleLoginClientID = strings.TrimSpace(settings.OAuth.GoogleLoginClientID)
	settings.OAuth.GoogleLoginSecret = strings.TrimSpace(settings.OAuth.GoogleLoginSecret)
	settings.OAuth.LinuxDOClientID = strings.TrimSpace(settings.OAuth.LinuxDOClientID)
	settings.OAuth.LinuxDOSecret = strings.TrimSpace(settings.OAuth.LinuxDOSecret)
	settings.Email.Provider = strings.ToLower(strings.TrimSpace(settings.Email.Provider))
	if settings.Email.Provider == "" {
		settings.Email.Provider = EmailProviderSMTP
	}
	settings.Email.SMTPHost = strings.TrimSpace(settings.Email.SMTPHost)
	settings.Email.SMTPUsername = strings.TrimSpace(settings.Email.SMTPUsername)
	settings.Email.SMTPPassword = strings.TrimSpace(settings.Email.SMTPPassword)
	settings.Email.CloudMailBaseURL = strings.TrimRight(strings.TrimSpace(settings.Email.CloudMailBaseURL), "/")
	settings.Email.CloudMailEmail = strings.ToLower(strings.TrimSpace(settings.Email.CloudMailEmail))
	settings.Email.CloudMailPassword = strings.TrimSpace(settings.Email.CloudMailPassword)
	settings.Email.FromEmail = strings.TrimSpace(settings.Email.FromEmail)
	settings.Email.FromName = strings.TrimSpace(settings.Email.FromName)
	settings.Email.VerificationSubjectTemplate = strings.TrimSpace(settings.Email.VerificationSubjectTemplate)
	if settings.Email.VerificationSubjectTemplate == "" {
		settings.Email.VerificationSubjectTemplate = DefaultEmailSubjectTemplate
	}
	settings.Email.VerificationTextTemplate = normalizeMultilineSystemValue(settings.Email.VerificationTextTemplate)
	if settings.Email.VerificationTextTemplate == "" {
		settings.Email.VerificationTextTemplate = DefaultEmailTextTemplate
	}
	settings.Email.VerificationHTMLTemplate = normalizeMultilineSystemValue(settings.Email.VerificationHTMLTemplate)
	if settings.Email.VerificationHTMLTemplate == "" {
		settings.Email.VerificationHTMLTemplate = DefaultEmailHTMLTemplate
	}
	if settings.Email.SMTPPort <= 0 {
		settings.Email.SMTPPort = DefaultSMTPPort
	}
	if settings.Email.CloudMailAccountID < 0 {
		settings.Email.CloudMailAccountID = 0
	}
	settings.Email.CodeTTLSeconds = clampInt(settings.Email.CodeTTLSeconds, 60, 3600, DefaultEmailCodeTTLSeconds)
	settings.Email.SendCooldownSeconds = clampInt(settings.Email.SendCooldownSeconds, 10, 3600, DefaultEmailSendCooldownSeconds)
	settings.Email.HourlySendLimit = clampInt(settings.Email.HourlySendLimit, 1, 100, DefaultEmailHourlySendLimit)
	settings.Email.MaxAttempts = clampInt(settings.Email.MaxAttempts, 1, 20, DefaultEmailMaxAttempts)
	settings.Home = NormalizeSystemHomeSettings(settings.Home)
	settings.Payment.CNYPerUSD = strings.TrimSpace(settings.Payment.CNYPerUSD)
	if settings.Payment.CNYPerUSD == "" {
		settings.Payment.CNYPerUSD = "1"
	}
	if settings.Payment.MinRechargeNanoUSD < 0 {
		settings.Payment.MinRechargeNanoUSD = 0
	}
	if settings.Payment.MaxRechargeNanoUSD < 0 {
		settings.Payment.MaxRechargeNanoUSD = 0
	}
	settings.Payment.RechargeInputMode = strings.ToLower(strings.TrimSpace(settings.Payment.RechargeInputMode))
	switch settings.Payment.RechargeInputMode {
	case RechargeInputModeBalanceUSD:
	case RechargeInputModePaymentCNY:
	default:
		settings.Payment.RechargeInputMode = RechargeInputModePaymentCNY
	}
	settings.Payment.WeChatPay.AppID = strings.TrimSpace(settings.Payment.WeChatPay.AppID)
	settings.Payment.WeChatPay.MchID = strings.TrimSpace(settings.Payment.WeChatPay.MchID)
	settings.Payment.WeChatPay.APIV3Key = strings.TrimSpace(settings.Payment.WeChatPay.APIV3Key)
	settings.Payment.WeChatPay.MerchantSerialNo = strings.TrimSpace(settings.Payment.WeChatPay.MerchantSerialNo)
	settings.Payment.WeChatPay.MerchantPrivateKeyPEM = strings.TrimSpace(settings.Payment.WeChatPay.MerchantPrivateKeyPEM)
	settings.Payment.WeChatPay.WeChatPayPublicKeyID = strings.TrimSpace(settings.Payment.WeChatPay.WeChatPayPublicKeyID)
	settings.Payment.WeChatPay.WeChatPayPublicKeyPEM = strings.TrimSpace(settings.Payment.WeChatPay.WeChatPayPublicKeyPEM)
	settings.Payment.WeChatPay.NotifyURL = strings.TrimSpace(settings.Payment.WeChatPay.NotifyURL)
	settings.Payment.Alipay.AppID = strings.TrimSpace(settings.Payment.Alipay.AppID)
	settings.Payment.Alipay.PrivateKeyPEM = strings.TrimSpace(settings.Payment.Alipay.PrivateKeyPEM)
	settings.Payment.Alipay.AlipayPublicKeyPEM = strings.TrimSpace(settings.Payment.Alipay.AlipayPublicKeyPEM)
	settings.Payment.Alipay.GatewayURL = strings.TrimSpace(settings.Payment.Alipay.GatewayURL)
	if settings.Payment.Alipay.GatewayURL == "" {
		settings.Payment.Alipay.GatewayURL = "https://openapi.alipay.com/gateway.do"
	}
	settings.Payment.Alipay.NotifyURL = strings.TrimSpace(settings.Payment.Alipay.NotifyURL)
	settings.Payment.Alipay.ReturnURL = strings.TrimSpace(settings.Payment.Alipay.ReturnURL)
	settings.Payment.Alipay.SignType = strings.ToUpper(strings.TrimSpace(settings.Payment.Alipay.SignType))
	if settings.Payment.Alipay.SignType == "" {
		settings.Payment.Alipay.SignType = "RSA2"
	}
	settings.Payment.PersonalPay.AndroidToken = strings.TrimSpace(settings.Payment.PersonalPay.AndroidToken)
	if settings.Payment.PersonalPay.ExpireAfterSec <= 0 {
		settings.Payment.PersonalPay.ExpireAfterSec = DefaultPersonalPayExpireAfterSec
	}
	if settings.Registration.NewUserRewardNanoUSD < 0 {
		settings.Registration.NewUserRewardNanoUSD = 0
	}
	settings.Registration.UsernameMinLength = clampInt(settings.Registration.UsernameMinLength, 1, MaximumUsernameMinLength, DefaultUsernameMinLength)
	settings.Registration.RegisterIPHourlyLimit = clampInt(settings.Registration.RegisterIPHourlyLimit, 1, 1000, DefaultRegisterIPHourlyLimit)
	settings.Registration.EmailCodeIPHourlyLimit = clampInt(settings.Registration.EmailCodeIPHourlyLimit, 1, 1000, DefaultEmailCodeIPHourlyLimit)
	settings.Registration.TurnstileSiteKey = strings.TrimSpace(settings.Registration.TurnstileSiteKey)
	settings.Registration.TurnstileSecretKey = strings.TrimSpace(settings.Registration.TurnstileSecretKey)
	if settings.Invitation.InviterRewardNanoUSD < 0 {
		settings.Invitation.InviterRewardNanoUSD = 0
	}
	if settings.Invitation.InviterRechargeRewardBps < 0 {
		settings.Invitation.InviterRechargeRewardBps = 0
	}
	if settings.Invitation.InviterRechargeRewardBps > 10000 {
		settings.Invitation.InviterRechargeRewardBps = 10000
	}
	if settings.Invitation.InviteeRewardNanoUSD < 0 {
		settings.Invitation.InviteeRewardNanoUSD = 0
	}
	settings.UserDashboard.CustomPanelHTML = normalizeMultilineSystemValue(settings.UserDashboard.CustomPanelHTML)
	if settings.Retention == (SystemRetentionSettings{}) {
		settings.Retention = DefaultSystemSettings().Retention
	}
	settings.Retention.AuditLimit = clampInt(settings.Retention.AuditLimit, MinimumAuditRetentionLimit, MaximumAuditRetentionLimit, DefaultAuditRetentionLimit)
	settings.Retention.UsageLogMaxAgeDays = clampDisableableInt(settings.Retention.UsageLogMaxAgeDays, MaximumUsageLogMaxAgeDays, DefaultUsageLogMaxAgeDays)
	settings.Retention.BillingLedgerRetentionDays = clampInt(settings.Retention.BillingLedgerRetentionDays, MinimumBillingLedgerRetentionDays, MaximumBillingLedgerRetentionDays, DefaultBillingLedgerRetentionDays)
	return settings
}

func ResponsesWebSocketUpstreamEnabled(settings SystemRuntimeSettings) bool {
	return settings.ResponsesWebSocketUpstreamEnabled == nil || *settings.ResponsesWebSocketUpstreamEnabled
}

func StartupSystemSettingsFrom(settings SystemSettings) SystemSettings {
	settings = NormalizeSystemSettings(settings)
	runtime := settings.Runtime
	runtime.RegistrationEmailAllowlist = append([]string(nil), runtime.RegistrationEmailAllowlist...)
	backup := settings.Backup
	backup.AndroidDataSets = append([]string(nil), backup.AndroidDataSets...)
	return NormalizeSystemSettings(SystemSettings{
		Runtime: runtime,
		Image:   settings.Image,
		Payment: SystemPaymentSettings{
			PersonalPay: settings.Payment.PersonalPay,
		},
		Retention: settings.Retention,
		Backup:    backup,
		UpdatedAt: settings.UpdatedAt,
	})
}

func NormalizeSystemBackupSettings(settings SystemBackupSettings) SystemBackupSettings {
	settings.AndroidTimeOfDay = normalizeHHMM(settings.AndroidTimeOfDay, DefaultAndroidBackupTimeOfDay)
	settings.AndroidDataSets = normalizeBackupDataSets(settings.AndroidDataSets)
	if len(settings.AndroidDataSets) == 0 {
		settings.AndroidDataSets = []string{"billing"}
	}
	return settings
}

func ImageUserConsoleEnabled(settings SystemImageSettings) bool {
	return settings.UserConsoleEnabled == nil || *settings.UserConsoleEnabled
}

func normalizeBackupDataSets(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, raw := range values {
		value := strings.ToLower(strings.TrimSpace(raw))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		for _, known := range knownBackupDataSets {
			if value == known {
				seen[value] = struct{}{}
				out = append(out, value)
				break
			}
		}
	}
	return out
}

func normalizeHHMM(value string, fallback string) string {
	raw := strings.TrimSpace(value)
	if len(raw) != 5 || raw[2] != ':' {
		return fallback
	}
	hour, ok := twoDigits(raw[:2])
	if !ok || hour > 23 {
		return fallback
	}
	minute, ok := twoDigits(raw[3:])
	if !ok || minute > 59 {
		return fallback
	}
	return raw
}

func twoDigits(value string) (int, bool) {
	if len(value) != 2 {
		return 0, false
	}
	left := value[0]
	right := value[1]
	if left < '0' || left > '9' || right < '0' || right > '9' {
		return 0, false
	}
	return int(left-'0')*10 + int(right-'0'), true
}

func normalizeImageBackend(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "", ImageBackendAuto:
		return ImageBackendAuto
	case ImageBackendOfficial:
		return ImageBackendOfficial
	default:
		return normalized
	}
}

func boolPtr(value bool) *bool {
	return &value
}

func normalizeEmailDomainList(values []string) []string {
	seen := map[string]struct{}{}
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		domain := strings.ToLower(strings.TrimSpace(value))
		if domain == "" {
			continue
		}
		domain = strings.TrimPrefix(domain, "*")
		if !strings.HasPrefix(domain, "@") {
			domain = "@" + domain
		}
		if domain == "@" {
			continue
		}
		if _, ok := seen[domain]; ok {
			continue
		}
		seen[domain] = struct{}{}
		normalized = append(normalized, domain)
	}
	return normalized
}

func normalizeMultilineSystemValue(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.TrimSpace(value)
}

func clampInt(value, min, max, fallback int) int {
	if value <= 0 {
		value = fallback
	}
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func clampDisableableInt(value, max, fallback int) int {
	if value < 0 {
		value = fallback
	}
	if value > max {
		return max
	}
	return value
}

func clampFloat64(value, min, fallback float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		value = fallback
	}
	if value < min {
		return min
	}
	return value
}

type AuditKind string

const (
	AuditKindGateway AuditKind = "gateway"
	AuditKindAdmin   AuditKind = "admin"
)

type UserRole string

const (
	UserRoleAdmin UserRole = "admin"
	UserRoleUser  UserRole = "user"
)

type User struct {
	ID                                string
	Username                          string
	PasswordHash                      string
	ForcePasswordChange               bool
	Role                              UserRole
	Enabled                           bool
	ConcurrentRequestLimitOverride    *int
	RequestRateLimitPerMinuteOverride *int
	BalanceNanoUSD                    int64
	Email                             string
	EmailVerified                     bool
	InviterUserID                     string
	RegistrationIP                    string
	RegistrationBrowserFingerprint    string
	OAuthIdentities                   []UserOAuthIdentity
	LastLoginAt                       *time.Time
	CreatedAt                         time.Time
	UpdatedAt                         time.Time
}

type EmailVerificationCode struct {
	ID          string
	Purpose     string
	Email       string
	CodeHash    string
	Attempts    int
	MaxAttempts int
	ExpiresAt   time.Time
	UsedAt      *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type PasswordResetToken struct {
	ID        string
	UserID    string
	Email     string
	TokenHash string
	ExpiresAt time.Time
	UsedAt    *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

type UserOAuthIdentity struct {
	Provider string
	Subject  string
	Email    string
	Username string
	LinkedAt time.Time
}

func (u User) IsAdmin() bool {
	return u.Role == UserRoleAdmin
}

func (u User) IsEnabled() bool {
	return u.Enabled
}

type UserSession struct {
	TokenHash string
	UserID    string
	ExpiresAt time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

const (
	MCPTokenScopeConnect     = "mcp:connect"
	MCPTokenScopeDocsRead    = "docs:read"
	MCPTokenScopeDocsPrivate = "docs:read_private"
	MCPTokenScopeDocsWrite   = "docs:write"
	MCPTokenScopeDocsPublish = "docs:publish"
	MCPTokenScopeDocsArchive = "docs:archive"
	MCPTokenScopeDocsPin     = "docs:pin"
)

type MCPToken struct {
	ID          string
	Name        string
	TokenHash   string
	OwnerUserID string
	Scopes      []string
	Enabled     bool
	ExpiresAt   *time.Time
	LastUsedAt  *time.Time
	RevokedAt   *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (t MCPToken) HasScope(scope string) bool {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return false
	}
	for _, value := range t.Scopes {
		if strings.EqualFold(strings.TrimSpace(value), scope) {
			return true
		}
	}
	return false
}

type APIClient struct {
	ID                 string
	Name               string
	APIKey             string
	OwnerUserID        string
	Enabled            bool
	SpendLimitNanoUSD  int64
	RoutePolicy        RoutePolicy
	AccountGroup       string
	BillingSource      string
	RouteAffinityKey   string `json:"-"`
	CacheAffinityRoute bool   `json:"-"`
	LastUsedAt         *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

const (
	ClientBillingSourceCash = "cash"
	ClientBillingSourcePlan = "plan"
)

func NormalizeClientBillingSource(source string) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case ClientBillingSourcePlan, "package", "subscription", "entitlement", "套餐":
		return ClientBillingSourcePlan
	default:
		return ClientBillingSourceCash
	}
}

type ClientSpend struct {
	ClientID          string
	SpendLimitNanoUSD int64
	SpendUsedNanoUSD  int64
	UpdatedAt         time.Time
}

type BillingRequestStatus string

const (
	BillingRequestReserved     BillingRequestStatus = "reserved"
	BillingRequestSettled      BillingRequestStatus = "settled"
	BillingRequestReleased     BillingRequestStatus = "released"
	BillingRequestUsageMissing BillingRequestStatus = "usage_missing"
)

type BillingReservationInput struct {
	RequestID                     string
	ClientID                      string
	ClientName                    string
	UserID                        string
	AccountID                     string
	AccountLabel                  string
	FailedAccountLabels           []string
	AccountGroup                  string
	AccountGroupMultiplierBps     int64
	BillingSource                 string
	Provider                      ProviderKind
	Model                         string
	FastMode                      bool
	EstimatedPromptTokens         int
	EstimatedCompletionTokens     int
	InputPriceNanoUSDPer1M        int64
	CachedInputPriceNanoUSDPer1M  int64
	CacheWritePriceNanoUSDPer1M   int64
	CacheWrite5mPriceNanoUSDPer1M int64
	CacheWrite1hPriceNanoUSDPer1M int64
	OutputPriceNanoUSDPer1M       int64
	ImageOutputPriceNanoUSDPer1M  int64
	ReservedNanoUSD               int64
	Fingerprint                   string
	CacheDiagnostics              BillingCacheDiagnostics
}

type BillingReservation struct {
	ID                            string
	RequestID                     string
	ClientID                      string
	ClientName                    string
	UserID                        string
	AccountID                     string
	AccountLabel                  string
	FailedAccountLabels           []string
	AccountGroup                  string
	AccountGroupMultiplierBps     int64
	BillingSource                 string
	Provider                      ProviderKind
	Model                         string
	FastMode                      bool
	Status                        BillingRequestStatus
	EstimatedPromptTokens         int
	EstimatedCompletionTokens     int
	PromptTokens                  int
	CachedPromptTokens            int
	CacheCreationTokens           int
	CacheCreation5mTokens         int
	CacheCreation1hTokens         int
	CompletionTokens              int
	ImageOutputTokens             int
	TotalTokens                   int
	InputPriceNanoUSDPer1M        int64
	CachedInputPriceNanoUSDPer1M  int64
	CacheWritePriceNanoUSDPer1M   int64
	CacheWrite5mPriceNanoUSDPer1M int64
	CacheWrite1hPriceNanoUSDPer1M int64
	OutputPriceNanoUSDPer1M       int64
	ImageOutputPriceNanoUSDPer1M  int64
	ReservedNanoUSD               int64
	ActualNanoUSD                 int64
	FirstTokenMS                  int64
	Fingerprint                   string
	CacheDiagnostics              BillingCacheDiagnostics
	CreatedAt                     time.Time
	SettledAt                     *time.Time
}

type BillingSettlementInput struct {
	RequestID                     string
	ClientID                      string
	AccountID                     string
	AccountLabel                  string
	AccountGroup                  string
	AccountGroupMultiplierBps     int64
	BillingSource                 string
	Provider                      ProviderKind
	Model                         string
	FastMode                      bool
	Usage                         Usage
	InputPriceNanoUSDPer1M        int64
	CachedInputPriceNanoUSDPer1M  int64
	CacheWritePriceNanoUSDPer1M   int64
	CacheWrite5mPriceNanoUSDPer1M int64
	CacheWrite1hPriceNanoUSDPer1M int64
	OutputPriceNanoUSDPer1M       int64
	ImageOutputPriceNanoUSDPer1M  int64
	ActualNanoUSD                 int64
	FirstTokenMS                  int64
	MissingUsage                  bool
}

type BillingAccountUpdateInput struct {
	RequestID           string
	ClientID            string
	AccountID           string
	AccountLabel        string
	FailedAccountLabels []string
}

type BillingSettlement struct {
	Request             BillingReservation
	DeltaNanoUSD        int64
	BalanceAfterNanoUSD int64
}

type BillingReleaseInput struct {
	RequestID string
	ClientID  string
	Reason    string
}

type BillingCacheDiagnostics struct {
	RequestShape           string `json:"request_shape,omitempty"`
	PromptCacheKeySource   string `json:"prompt_cache_key_source,omitempty"`
	PromptCacheKeyHash     string `json:"prompt_cache_key_hash,omitempty"`
	RouteAffinityHash      string `json:"route_affinity_hash,omitempty"`
	SessionAffinityHash    string `json:"session_affinity_hash,omitempty"`
	PromptCacheKeyPresent  bool   `json:"prompt_cache_key_present,omitempty"`
	RouteAffinityPresent   bool   `json:"route_affinity_present,omitempty"`
	SessionAffinityPresent bool   `json:"session_affinity_present,omitempty"`
}

type BillingLedgerEntry struct {
	ID                  string
	UserID              string
	ClientID            string
	RequestID           string
	Kind                string
	AmountNanoUSD       int64
	BalanceAfterNanoUSD int64
	Note                string
	CreatedAt           time.Time
}

type BillingPlanGroup struct {
	ID              string
	Name            string
	SaleDisabled    bool
	QuotaPriceRatio string
	SortOrder       int
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func NormalizeBillingPlanGroup(group string) string {
	return strings.TrimSpace(group)
}

type BillingPlan struct {
	ID                 string
	Name               string
	Description        string
	Group              string
	Enabled            bool
	PriceNanoUSD       int64
	PeriodQuotaNanoUSD int64
	PeriodDurationSec  int64
	PeriodCount        int
	SortOrder          int
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type UserPlanEntitlementStatus string

const (
	UserPlanEntitlementActive    UserPlanEntitlementStatus = "active"
	UserPlanEntitlementExpired   UserPlanEntitlementStatus = "expired"
	UserPlanEntitlementCancelled UserPlanEntitlementStatus = "cancelled"
)

type UserPlanEntitlement struct {
	ID                     string
	UserID                 string
	PlanID                 string
	PlanName               string
	Status                 UserPlanEntitlementStatus
	PriceNanoUSD           int64
	PeriodQuotaNanoUSD     int64
	BasePeriodQuotaNanoUSD int64
	PeriodDurationSec      int64
	TotalPeriods           int
	RemainingPeriods       int
	CurrentQuotaNanoUSD    int64
	Priority               int
	CurrentPeriodStartedAt time.Time
	CurrentPeriodEndsAt    time.Time
	ExpiresAt              time.Time
	PurchasedAt            time.Time
	UpdatedAt              time.Time
}

type PlanQuotaLedgerEntry struct {
	ID                string
	EntitlementID     string
	UserID            string
	ClientID          string
	RequestID         string
	Kind              string
	AmountNanoUSD     int64
	QuotaAfterNanoUSD int64
	Note              string
	CreatedAt         time.Time
}

const (
	BillingFundingSourcePlan = "plan"
	BillingFundingSourceCash = "cash"
)

type BillingFundingAllocation struct {
	ID              string
	RequestID       string
	ClientID        string
	UserID          string
	Source          string
	EntitlementID   string
	ReservedNanoUSD int64
	ActualNanoUSD   int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type BillingPlanPurchaseInput struct {
	UserID              string
	PlanID              string
	Mode                BillingPlanPurchaseMode
	TargetEntitlementID string
}

type BillingPlanPurchase struct {
	Plan                 BillingPlan
	Entitlement          UserPlanEntitlement
	BalanceBeforeNanoUSD int64
	BalanceAfterNanoUSD  int64
}

type BillingPlanGrantInput struct {
	UserID string
	PlanID string
	Note   string
}

type BillingPlanPurchaseMode string

const (
	BillingPlanPurchaseSeparate     BillingPlanPurchaseMode = "separate"
	BillingPlanPurchaseMergeQuota   BillingPlanPurchaseMode = "merge_quota"
	BillingPlanPurchaseExtendPeriod BillingPlanPurchaseMode = "extend_period"
)

type PaymentProvider string

const (
	PaymentProviderWeChatPay   PaymentProvider = "wechatpay"
	PaymentProviderAlipay      PaymentProvider = "alipay"
	PaymentProviderPersonalPay PaymentProvider = "personalpay"

	DefaultPaymentOrderPendingTTL = 2 * time.Hour
)

type PaymentChannel string

const (
	PaymentChannelNative PaymentChannel = "native"
	PaymentChannelJSAPI  PaymentChannel = "jsapi"
	PaymentChannelWAP    PaymentChannel = "wap"
	PaymentChannelPage   PaymentChannel = "page"
	PaymentChannelWeChat PaymentChannel = "wechat"
	PaymentChannelAlipay PaymentChannel = "alipay"
)

type PaymentOrderStatus string

const (
	PaymentOrderPending PaymentOrderStatus = "pending"
	PaymentOrderPaid    PaymentOrderStatus = "paid"
	PaymentOrderClosed  PaymentOrderStatus = "closed"
	PaymentOrderFailed  PaymentOrderStatus = "failed"
)

type PaymentOrder struct {
	ID                    string
	OutTradeNo            string
	UserID                string
	Provider              PaymentProvider
	Channel               PaymentChannel
	AmountNanoUSD         int64
	Currency              string
	ProviderAmountCents   int64
	ProviderCurrency      string
	ExchangeRateCNYPerUSD string
	Subject               string
	Status                PaymentOrderStatus
	ProviderStatus        string
	ProviderTradeNo       string
	CodeURL               string
	PayURL                string
	PrepayID              string
	RawRequest            string
	RawResponse           string
	CreatedAt             time.Time
	PaidAt                *time.Time
	UpdatedAt             time.Time
}

type PaymentOrderBalanceCredit struct {
	UserID        string
	Kind          string
	LedgerID      string
	AmountNanoUSD int64
	Note          string
}

type PaymentOrderProviderUpdate struct {
	Status          PaymentOrderStatus
	ProviderStatus  string
	ProviderTradeNo string
	CodeURL         string
	PayURL          string
	PrepayID        string
	RawResponse     string
	PaidAt          *time.Time
}

type PaymentRefundStatus string

const (
	PaymentRefundPending PaymentRefundStatus = "pending"
	PaymentRefundDone    PaymentRefundStatus = "done"
	PaymentRefundFailed  PaymentRefundStatus = "failed"
)

type PaymentRefund struct {
	ID                    string
	OrderID               string
	OutTradeNo            string
	UserID                string
	Provider              PaymentProvider
	ProviderRefundNo      string
	AmountNanoUSD         int64
	ProviderAmountCents   int64
	ProviderCurrency      string
	ExchangeRateCNYPerUSD string
	Status                PaymentRefundStatus
	Reason                string
	ManualPayoutRef       string
	ManualPayoutNote      string
	ManualPayoutAt        *time.Time
	RawRequest            string
	RawResponse           string
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

type AuditEvent struct {
	ID           string
	Kind         AuditKind
	Actor        string
	Action       string
	ResourceType string
	ResourceID   string
	ResourceName string
	RequestID    string
	ClientID     string
	ClientName   string
	Provider     ProviderKind
	AccountID    string
	Model        string
	Status       string
	Message      string
	RequestBody  string
	Attempts     []AttemptRecord
	DurationMS   int64
	CreatedAt    time.Time
}

type SiteMessage struct {
	ID                  string
	Title               string
	Body                string
	CreatedBy           string
	Enabled             bool
	Popup               bool
	PublicPopup         bool
	TargetUserIDs       []string
	TargetAccountGroups []string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type SiteMessageRead struct {
	MessageID string
	UserID    string
	ReadAt    time.Time
}

type SiteMessageDelivery struct {
	Message SiteMessage
	Read    bool
	ReadAt  *time.Time
}

type SupportTicketStatus string

const (
	SupportTicketOpen         SupportTicketStatus = "open"
	SupportTicketPendingAdmin SupportTicketStatus = "pending_admin"
	SupportTicketPendingUser  SupportTicketStatus = "pending_user"
	SupportTicketResolved     SupportTicketStatus = "resolved"
	SupportTicketClosed       SupportTicketStatus = "closed"
)

type SupportTicket struct {
	ID                string              `json:"id"`
	UserID            string              `json:"user_id"`
	Username          string              `json:"username"`
	Title             string              `json:"title"`
	Status            SupportTicketStatus `json:"status"`
	LastMessage       string              `json:"last_message"`
	LastActorID       string              `json:"last_actor_id"`
	LastReadByUserAt  *time.Time          `json:"last_read_by_user_at,omitempty"`
	LastReadByAdminAt *time.Time          `json:"last_read_by_admin_at,omitempty"`
	UnreadCount       int                 `json:"unread_count,omitempty"`
	CreatedAt         time.Time           `json:"created_at"`
	UpdatedAt         time.Time           `json:"updated_at"`
	ClosedAt          *time.Time          `json:"closed_at,omitempty"`
}

type SupportMessage struct {
	ID        string    `json:"id"`
	TicketID  string    `json:"ticket_id"`
	ActorID   string    `json:"actor_id"`
	ActorRole UserRole  `json:"actor_role"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

type DocumentStatus string

const (
	DocumentStatusDraft     DocumentStatus = "draft"
	DocumentStatusPublished DocumentStatus = "published"
	DocumentStatusArchived  DocumentStatus = "archived"
)

type Document struct {
	ID              string
	Slug            string
	Title           string
	Body            string
	MetaTitle       string
	MetaDescription string
	CanonicalURL    string
	Pinned          bool
	NoIndex         bool
	Status          DocumentStatus
	CreatedBy       string
	UpdatedBy       string
	PublishedAt     *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type DocumentRedirect struct {
	FromSlug   string
	ToSlug     string
	StatusCode int
	CreatedAt  time.Time
}

func (e AuditEvent) EffectiveKind() AuditKind {
	if e.Kind == "" {
		return AuditKindGateway
	}
	return e.Kind
}

func DefaultRoutePolicy() RoutePolicy {
	return RoutePolicy{
		DefaultProvider:   ProviderOpenAI,
		FallbackProviders: []ProviderKind{ProviderClaude},
		Rules: []RouteRule{
			{ModelPrefix: "gpt-", PreferredProviders: []ProviderKind{ProviderOpenAI, ProviderClaude}},
			{ModelPrefix: "text-embedding-", PreferredProviders: []ProviderKind{ProviderOpenAI, ProviderClaude}},
			{ModelPrefix: "claude-", PreferredProviders: []ProviderKind{ProviderClaude, ProviderOpenAI}},
		},
	}
}
