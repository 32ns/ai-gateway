package web

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/core"
)

func (s *Server) handleProtocolModelActions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	modelID := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/models/"), "/")
	if modelID == "" || strings.Contains(modelID, "/") {
		writeProtocolError(w, http.StatusNotFound, "model not found")
		return
	}
	model, ok := findProtocolModelObject(s.protocolModelObjects(r.Context()), modelID)
	if !ok {
		writeProtocolError(w, http.StatusNotFound, fmt.Sprintf("model %q not found", modelID))
		return
	}
	writeJSON(w, http.StatusOK, model)
}

func (s *Server) protocolModelObjects(ctx context.Context) []modelObject {
	models := s.control.ListModelsForClient(ctx, protocolClientPointerFromContext(ctx))
	response := make([]modelObject, 0, len(models))

	seen := map[string]bool{}
	for _, spec := range models {
		response = appendProtocolModelObject(response, seen, spec.Name, spec.Provider, spec.CreatedAt)
		for _, alias := range spec.Aliases {
			response = appendProtocolModelObject(response, seen, alias, spec.Provider, spec.CreatedAt)
		}
	}
	slices.SortFunc(response, func(a, b modelObject) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return response
}

func appendProtocolModelObject(response []modelObject, seen map[string]bool, id string, provider core.ProviderKind, createdAt time.Time) []modelObject {
	if seen[id] {
		return response
	}
	var created int64
	if !createdAt.IsZero() {
		created = createdAt.Unix()
	}
	response = append(response, modelObject{
		ID:      id,
		Object:  "model",
		Created: created,
		OwnedBy: string(provider),
	})
	seen[id] = true
	return response
}

func findProtocolModelObject(models []modelObject, modelID string) (modelObject, bool) {
	for _, model := range models {
		if model.ID != modelID {
			continue
		}
		return model, true
	}
	return modelObject{}, false
}

func anthropicProtocolMetadataForClient(r *http.Request, metadata map[string]string, _ *core.APIClient) map[string]string {
	out := metadata
	if r == nil {
		return out
	}
	for headerName, metadataKey := range map[string]string{
		"anthropic-version":   "anthropic_version",
		"anthropic-beta":      "anthropic_beta",
		"x-client-request-id": "x-client-request-id",
	} {
		if value := strings.TrimSpace(r.Header.Get(headerName)); value != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[metadataKey] = value
		}
	}
	for headerName, values := range r.Header {
		normalized := strings.ToLower(strings.TrimSpace(headerName))
		if !strings.HasPrefix(normalized, "x-claude-code-") || len(values) == 0 {
			continue
		}
		value := strings.TrimSpace(values[0])
		if value == "" {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[normalized] = value
	}
	return out
}
