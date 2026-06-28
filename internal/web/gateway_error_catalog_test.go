package web

import (
	"sort"
	"testing"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/failover"
	"github.com/32ns/ai-gateway/internal/gateway"
	"github.com/32ns/ai-gateway/internal/providers"
)

func TestGatewayErrorCatalogCoversKnownErrorCodes(t *testing.T) {
	want := uniqueStrings([]string{
		providers.ErrorCodeMissingCredential,
		providers.ErrorCodeCredentialExpired,
		providers.ErrorCodeMissingRefreshCredential,
		providers.ErrorCodeCredentialRefreshNotSupported,
		providers.ErrorCodeCredentialStateUpdateFailed,
		providers.ErrorCodeGatewayAPIKeyDisabled,
		providers.ErrorCodeUpstreamAuthError,
		providers.ErrorCodeUpstreamEmptyResponse,
		providers.ErrorCodeUpstreamForbidden,
		providers.ErrorCodeUpstreamInvalidJSON,
		providers.ErrorCodeUpstreamInvalidStreamChunk,
		providers.ErrorCodeUpstreamNotFound,
		providers.ErrorCodeUpstreamProviderBanned,
		providers.ErrorCodeUpstreamRateLimited,
		providers.ErrorCodeUpstreamReadError,
		providers.ErrorCodeUpstreamRejected,
		providers.ErrorCodeUpstreamRequestBuildFailed,
		providers.ErrorCodeUpstreamServerError,
		providers.ErrorCodeUpstreamTemporarilyUnavailable,
		providers.ErrorCodeUpstreamTransportError,
		providers.ErrorCodeImageBackendRequiresOAuth,
		providers.ErrorCodeImageEndpointUnsupported,
		providers.ErrorCodeImageGenerationFailed,
		providers.ErrorCodeImageGenerationNotStarted,
		providers.ErrorCodeImageGenerationRejected,
		providers.ErrorCodeImageModelUnsupported,
		providers.ErrorCodeImagePollTimeout,
		providers.ErrorCodeImageUploadEmpty,
		providers.ErrorCodeChatGPTArkoseRequired,
		providers.ErrorCodeChatGPTConduitTokenMissing,
		providers.ErrorCodeChatGPTProofTokenFailed,
		providers.ErrorCodeChatGPTRequirementsMissingToken,
		providers.ErrorCodeChatGPTUploadURLMissing,
		gateway.ErrorCodeBillingAmountOverflow,
		gateway.ErrorCodeBillingConflict,
		gateway.ErrorCodeBillingOwnerMismatch,
		gateway.ErrorCodePlanBillingDisabled,
		gateway.ErrorCodePlanQuotaExhausted,
		gateway.ErrorCodeQuotaError,
		gateway.ErrorCodeRateLimitExceeded,
		controlplane.ErrorCodeAuthError,
		controlplane.ErrorCodeForbidden,
		failover.ErrorCodeAudioMultipartNotSupported,
		failover.ErrorCodeAudioSpeechNotSupported,
		failover.ErrorCodeBoundAccountUnavailable,
		failover.ErrorCodeEmbeddingsNotSupported,
		failover.ErrorCodeImageGenerationNotSupported,
		failover.ErrorCodeImageGenerationStreamingNotSupported,
		failover.ErrorCodeImageMultipartNotSupported,
		failover.ErrorCodeImageMultipartStreamingNotSupported,
		failover.ErrorCodeModerationsNotSupported,
		failover.ErrorCodeNoAccount,
		failover.ErrorCodeNoAdapter,
		failover.ErrorCodeResponsesNotSupported,
		failover.ErrorCodeResponsesStreamingNotSupported,
		failover.ErrorCodeResponsesWebSocketNotSupported,
		failover.ErrorCodeStreamingNotSupported,
		failover.ErrorCodeTokenCountNotSupported,
		mcpHTTPErrorCodeForbidden,
		mcpHTTPErrorCodeInvalidRequest,
		mcpHTTPErrorCodeUnauthorized,
		"parse_error",
		"method_not_found",
		"invalid_params",
		"document_not_found",
		"conflict",
		"internal_error",
		supportErrorCodeAuthRequired,
		supportErrorCodeDataConflict,
		supportErrorCodeNotFound,
		supportErrorCodeRequestFailed,
		supportErrorCodeUnsupportedEvent,
	})
	got := catalogErrorCodes(gatewayErrorCatalog(localeEN))
	if diff := stringSetDiff(want, got); len(diff) > 0 {
		t.Fatalf("gateway error catalog missing codes: %v", diff)
	}
	if diff := stringSetDiff(got, want); len(diff) > 0 {
		t.Fatalf("gateway error catalog has unknown codes: %v", diff)
	}
}

func catalogErrorCodes(groups []gatewayErrorGroup) []string {
	var codes []string
	seen := map[string]bool{}
	for _, group := range groups {
		for _, code := range group.Codes {
			if code.Code == "" {
				continue
			}
			if seen[code.Code] {
				continue
			}
			seen[code.Code] = true
			codes = append(codes, code.Code)
		}
	}
	sort.Strings(codes)
	return codes
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func stringSetDiff(left, right []string) []string {
	seen := map[string]bool{}
	for _, value := range right {
		seen[value] = true
	}
	var diff []string
	for _, value := range left {
		if !seen[value] {
			diff = append(diff, value)
		}
	}
	sort.Strings(diff)
	return diff
}
