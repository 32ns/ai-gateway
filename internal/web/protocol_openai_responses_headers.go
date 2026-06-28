package web

import (
	"net/http"
	"strings"
)

var openAIResponsesRequestHeaderNames = []string{
	"User-Agent",
	"originator",
	"Accept-Language",
	"session-id",
	"thread-id",
	"x-client-request-id",
	"x-codex-installation-id",
	"x-codex-beta-features",
	"x-codex-parent-thread-id",
	"x-codex-window-id",
	"x-openai-memgen-request",
	"x-openai-subagent",
	"x-openai-internal-codex-responses-lite",
	"x-oai-attestation",
	"x-responsesapi-include-timing-metrics",
	"session_id",
	"conversation_id",
	"x-codex-turn-state",
	"x-codex-turn-metadata",
}

func openAIResponsesHeadersFromRequest(r *http.Request) map[string]string {
	if r == nil {
		return nil
	}
	headers := map[string]string{}
	for _, key := range openAIResponsesRequestHeaderNames {
		if value := strings.TrimSpace(r.Header.Get(key)); value != "" {
			headers[key] = value
		}
	}
	if len(headers) == 0 {
		return nil
	}
	return headers
}
