package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/storage"
)

const (
	mcpProtocolVersion  = "2025-06-18"
	mcpDocsResourceURI  = "aigateway-doc://docs/"
	maxMCPBatchRequests = 100
)

type mcpRPCRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

type mcpRPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Result  any              `json:"result,omitempty"`
	Error   *mcpRPCError     `json:"error,omitempty"`
}

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type mcpResourceReadParams struct {
	URI string `json:"uri"`
}

type mcpResourcesListParams struct {
	Cursor string `json:"cursor"`
}

type mcpDocsListParams struct {
	Limit  int    `json:"limit"`
	Status string `json:"status"`
}

type mcpDocsSearchParams struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

type mcpDocsReadParams struct {
	Slug string `json:"slug"`
}

type mcpDocsCreateDraftParams struct {
	Title   string `json:"title"`
	Body    string `json:"body"`
	Pinned  bool   `json:"pinned"`
	NoIndex bool   `json:"noindex"`
}

type mcpDocsUpdateParams struct {
	Slug              string `json:"slug"`
	Title             string `json:"title"`
	Body              string `json:"body"`
	Pinned            *bool  `json:"pinned"`
	NoIndex           *bool  `json:"noindex"`
	ExpectedUpdatedAt string `json:"expected_updated_at"`
}

type mcpDocsStatusParams struct {
	Slug              string `json:"slug"`
	ExpectedUpdatedAt string `json:"expected_updated_at"`
}

type mcpDocsPinnedParams struct {
	Slug              string `json:"slug"`
	Pinned            bool   `json:"pinned"`
	ExpectedUpdatedAt string `json:"expected_updated_at"`
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/mcp" {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodOptions {
		w.Header().Set("Allow", "POST, OPTIONS")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.mcpOriginAllowed(r) {
		writeMCPHTTPError(w, http.StatusForbidden, mcpHTTPErrorCodeForbidden, "origin is not allowed")
		return
	}
	rawToken, ok := mcpBearerTokenFromRequest(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="ai-gateway-mcp"`)
		writeMCPHTTPError(w, http.StatusUnauthorized, mcpHTTPErrorCodeUnauthorized, "missing or invalid MCP bearer token")
		return
	}
	auth, err := s.control.AuthorizeMCPToken(rawToken)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="ai-gateway-mcp"`)
		writeMCPHTTPError(w, http.StatusUnauthorized, mcpHTTPErrorCodeUnauthorized, "invalid MCP bearer token")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		writeMCPHTTPError(w, http.StatusBadRequest, mcpHTTPErrorCodeInvalidRequest, "request body is too large")
		return
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		writeMCPHTTPError(w, http.StatusBadRequest, mcpHTTPErrorCodeInvalidRequest, "request body is required")
		return
	}
	if body[0] == '[' {
		s.handleMCPBatch(w, r, auth, body)
		return
	}
	var request mcpRPCRequest
	if err := json.Unmarshal(body, &request); err != nil {
		writeMCPResponse(w, mcpRPCResponse{JSONRPC: "2.0", Error: &mcpRPCError{Code: mcpErrorCodeParseError, Message: "parse error"}})
		return
	}
	response, respond := s.dispatchMCPRequest(r, auth, request)
	if !respond {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeMCPResponse(w, response)
}

func (s *Server) handleMCPBatch(w http.ResponseWriter, r *http.Request, auth controlplane.MCPAuthorization, body []byte) {
	var requests []mcpRPCRequest
	if err := json.Unmarshal(body, &requests); err != nil {
		writeMCPResponse(w, mcpRPCResponse{JSONRPC: "2.0", Error: &mcpRPCError{Code: mcpErrorCodeParseError, Message: "parse error"}})
		return
	}
	if len(requests) > maxMCPBatchRequests {
		writeMCPHTTPError(w, http.StatusBadRequest, mcpHTTPErrorCodeInvalidRequest, "mcp batch request limit exceeded")
		return
	}
	responses := make([]mcpRPCResponse, 0, len(requests))
	for _, request := range requests {
		response, respond := s.dispatchMCPRequest(r, auth, request)
		if respond {
			responses = append(responses, response)
		}
	}
	if len(responses) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, http.StatusOK, responses)
}

func (s *Server) dispatchMCPRequest(r *http.Request, auth controlplane.MCPAuthorization, request mcpRPCRequest) (mcpRPCResponse, bool) {
	response := mcpRPCResponse{JSONRPC: "2.0", ID: request.ID}
	if request.ID == nil && strings.HasPrefix(request.Method, "notifications/") {
		return response, false
	}
	if request.JSONRPC != "" && request.JSONRPC != "2.0" {
		response.Error = &mcpRPCError{Code: mcpErrorCodeInvalidRequest, Message: "invalid jsonrpc version"}
		return response, true
	}
	switch strings.TrimSpace(request.Method) {
	case "initialize":
		response.Result = map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities": map[string]any{
				"tools":     map[string]any{},
				"resources": map[string]any{},
			},
			"serverInfo": map[string]string{
				"name":    "ag-toos",
				"version": "0.1.0",
			},
		}
		s.recordMCPAudit(r, auth, "mcp.initialize", "", "", "", "ok", "", request.Params)
	case "ping":
		response.Result = map[string]any{}
	case "tools/list":
		response.Result = map[string]any{"tools": s.mcpToolsForAuth(auth)}
		s.recordMCPAudit(r, auth, "mcp.tools.list", "", "", "", "ok", "", request.Params)
	case "tools/call":
		result, err := s.handleMCPToolCall(r, auth, request.Params)
		if err != nil {
			response.Error = mcpErrorFromError(err)
			return response, true
		}
		response.Result = result
	case "resources/list":
		result, err := s.handleMCPResourcesList(r, auth, request.Params)
		if err != nil {
			response.Error = mcpErrorFromError(err)
			return response, true
		}
		response.Result = result
		s.recordMCPAudit(r, auth, "mcp.resources.list", "document", "", "", "ok", "", request.Params)
	case "resources/read":
		result, err := s.handleMCPResourceRead(r, auth, request.Params)
		if err != nil {
			response.Error = mcpErrorFromError(err)
			return response, true
		}
		response.Result = result
	case "resources/templates/list":
		response.Result = map[string]any{"resourceTemplates": []any{}}
	case "prompts/list":
		response.Result = map[string]any{"prompts": []any{}}
	default:
		response.Error = &mcpRPCError{Code: mcpErrorCodeMethodNotFound, Message: "method not found"}
	}
	return response, true
}

func (s *Server) handleMCPToolCall(r *http.Request, auth controlplane.MCPAuthorization, params json.RawMessage) (any, error) {
	call, err := mcpDecodeParams[mcpToolCallParams](params)
	if err != nil {
		return nil, err
	}
	call.Name = strings.TrimSpace(call.Name)
	if call.Name == "" {
		return nil, mcpInvalidParams("tool name is required")
	}
	switch call.Name {
	case "docs.list":
		return s.mcpToolDocsList(r, auth, call.Arguments)
	case "docs.search":
		return s.mcpToolDocsSearch(r, auth, call.Arguments)
	case "docs.read":
		return s.mcpToolDocsRead(r, auth, call.Arguments)
	case "docs.create_draft":
		return s.mcpToolDocsCreateDraft(r, auth, call.Arguments)
	case "docs.update":
		return s.mcpToolDocsUpdate(r, auth, call.Arguments)
	case "docs.publish":
		return s.mcpToolDocsSetStatus(r, auth, call.Arguments, core.DocumentStatusPublished, core.MCPTokenScopeDocsPublish, "docs.publish")
	case "docs.archive":
		return s.mcpToolDocsSetStatus(r, auth, call.Arguments, core.DocumentStatusArchived, core.MCPTokenScopeDocsArchive, "docs.archive")
	case "docs.set_pinned":
		return s.mcpToolDocsSetPinned(r, auth, call.Arguments)
	default:
		return nil, mcpInvalidParams("unknown tool: " + call.Name)
	}
}

func (s *Server) mcpToolDocsList(r *http.Request, auth controlplane.MCPAuthorization, params json.RawMessage) (any, error) {
	if err := s.requireMCPDocsRead(auth); err != nil {
		s.recordMCPAudit(r, auth, "docs.list", "document", "", "", "denied", err.Error(), params)
		return nil, err
	}
	input, err := mcpDecodeParams[mcpDocsListParams](params)
	if err != nil {
		return nil, err
	}
	limit := mcpDocumentLimit(input.Limit)
	documents := s.mcpDocumentsPageForAuth(auth, core.DocumentStatus(strings.TrimSpace(input.Status)), limit)
	payload := map[string]any{"documents": s.mcpDocumentSummaries(r, documents)}
	s.recordMCPAudit(r, auth, "docs.list", "document", "", "", "ok", fmt.Sprintf("returned %d document(s)", len(documents)), params)
	return mcpToolJSON(payload), nil
}

func (s *Server) mcpToolDocsSearch(r *http.Request, auth controlplane.MCPAuthorization, params json.RawMessage) (any, error) {
	if err := s.requireMCPDocsRead(auth); err != nil {
		s.recordMCPAudit(r, auth, "docs.search", "document", "", "", "denied", err.Error(), params)
		return nil, err
	}
	input, err := mcpDecodeParams[mcpDocsSearchParams](params)
	if err != nil {
		return nil, err
	}
	query := strings.TrimSpace(input.Query)
	limit := mcpDocumentLimit(input.Limit)
	documents := s.mcpDocumentSearchPageForAuth(auth, query, limit)
	payload := map[string]any{
		"query":     query,
		"documents": s.mcpDocumentSummaries(r, documents),
	}
	s.recordMCPAudit(r, auth, "docs.search", "document", "", "", "ok", fmt.Sprintf("returned %d document(s)", len(documents)), params)
	return mcpToolJSON(payload), nil
}

func (s *Server) mcpToolDocsRead(r *http.Request, auth controlplane.MCPAuthorization, params json.RawMessage) (any, error) {
	if err := s.requireMCPDocsRead(auth); err != nil {
		s.recordMCPAudit(r, auth, "docs.read", "document", "", "", "denied", err.Error(), params)
		return nil, err
	}
	input, err := mcpDecodeParams[mcpDocsReadParams](params)
	if err != nil {
		return nil, err
	}
	document, err := s.mcpDocumentBySlug(auth, input.Slug)
	if err != nil {
		s.recordMCPAudit(r, auth, "docs.read", "document", "", strings.TrimSpace(input.Slug), "error", err.Error(), params)
		return nil, err
	}
	payload := s.mcpDocumentDetail(r, document)
	s.recordMCPAudit(r, auth, "docs.read", "document", document.ID, document.Title, "ok", "", params)
	return mcpToolJSON(payload), nil
}

func (s *Server) mcpToolDocsCreateDraft(r *http.Request, auth controlplane.MCPAuthorization, params json.RawMessage) (any, error) {
	if err := controlplane.RequireMCPAdminScope(auth, core.MCPTokenScopeDocsWrite); err != nil {
		s.recordMCPAudit(r, auth, "docs.create_draft", "document", "", "", "denied", err.Error(), params)
		return nil, err
	}
	input, err := mcpDecodeParams[mcpDocsCreateDraftParams](params)
	if err != nil {
		return nil, err
	}
	document, err := s.control.CreateDocument(auth.User, controlplane.DocumentInput{
		Title:   input.Title,
		Body:    input.Body,
		Pinned:  input.Pinned,
		NoIndex: input.NoIndex,
		Status:  core.DocumentStatusDraft,
	})
	if err != nil {
		s.recordMCPAudit(r, auth, "docs.create_draft", "document", "", strings.TrimSpace(input.Title), "error", err.Error(), params)
		return nil, err
	}
	s.recordMCPAudit(r, auth, "docs.create_draft", "document", document.ID, document.Title, "ok", "", params)
	return mcpToolJSON(s.mcpDocumentDetail(r, document)), nil
}

func (s *Server) mcpToolDocsUpdate(r *http.Request, auth controlplane.MCPAuthorization, params json.RawMessage) (any, error) {
	if err := controlplane.RequireMCPAdminScope(auth, core.MCPTokenScopeDocsWrite); err != nil {
		s.recordMCPAudit(r, auth, "docs.update", "document", "", "", "denied", err.Error(), params)
		return nil, err
	}
	input, err := mcpDecodeParams[mcpDocsUpdateParams](params)
	if err != nil {
		return nil, err
	}
	existing, err := s.mcpPrivateDocumentBySlug(auth, input.Slug)
	if err != nil {
		s.recordMCPAudit(r, auth, "docs.update", "document", "", strings.TrimSpace(input.Slug), "error", err.Error(), params)
		return nil, err
	}
	if err := mcpCheckExpectedUpdatedAt(existing, input.ExpectedUpdatedAt); err != nil {
		s.recordMCPAudit(r, auth, "docs.update", "document", existing.ID, existing.Title, "conflict", err.Error(), params)
		return nil, err
	}
	next := mcpDocumentInputFromExisting(existing)
	if strings.TrimSpace(input.Title) != "" {
		next.Title = input.Title
	}
	if strings.TrimSpace(input.Body) != "" {
		next.Body = input.Body
	}
	if input.Pinned != nil {
		next.Pinned = *input.Pinned
	}
	if input.NoIndex != nil {
		next.NoIndex = *input.NoIndex
	}
	document, err := s.control.UpdateDocument(auth.User, existing.ID, next)
	if err != nil {
		s.recordMCPAudit(r, auth, "docs.update", "document", existing.ID, existing.Title, "error", err.Error(), params)
		return nil, err
	}
	s.recordMCPAudit(r, auth, "docs.update", "document", document.ID, document.Title, "ok", auditDocumentUpdateChangeMessage(existing, document, true), params)
	return mcpToolJSON(s.mcpDocumentDetail(r, document)), nil
}

func (s *Server) mcpToolDocsSetStatus(r *http.Request, auth controlplane.MCPAuthorization, params json.RawMessage, status core.DocumentStatus, scope string, action string) (any, error) {
	if err := controlplane.RequireMCPAdminScope(auth, scope); err != nil {
		s.recordMCPAudit(r, auth, action, "document", "", "", "denied", err.Error(), params)
		return nil, err
	}
	input, err := mcpDecodeParams[mcpDocsStatusParams](params)
	if err != nil {
		return nil, err
	}
	existing, err := s.mcpPrivateDocumentBySlug(auth, input.Slug)
	if err != nil {
		s.recordMCPAudit(r, auth, action, "document", "", strings.TrimSpace(input.Slug), "error", err.Error(), params)
		return nil, err
	}
	if err := mcpCheckExpectedUpdatedAt(existing, input.ExpectedUpdatedAt); err != nil {
		s.recordMCPAudit(r, auth, action, "document", existing.ID, existing.Title, "conflict", err.Error(), params)
		return nil, err
	}
	next := mcpDocumentInputFromExisting(existing)
	next.Status = status
	document, err := s.control.UpdateDocument(auth.User, existing.ID, next)
	if err != nil {
		s.recordMCPAudit(r, auth, action, "document", existing.ID, existing.Title, "error", err.Error(), params)
		return nil, err
	}
	s.recordMCPAudit(r, auth, action, "document", document.ID, document.Title, "ok", auditDocumentUpdateChangeMessage(existing, document, true), params)
	return mcpToolJSON(s.mcpDocumentDetail(r, document)), nil
}

func (s *Server) mcpToolDocsSetPinned(r *http.Request, auth controlplane.MCPAuthorization, params json.RawMessage) (any, error) {
	if err := controlplane.RequireMCPAdminScope(auth, core.MCPTokenScopeDocsPin); err != nil {
		s.recordMCPAudit(r, auth, "docs.set_pinned", "document", "", "", "denied", err.Error(), params)
		return nil, err
	}
	input, err := mcpDecodeParams[mcpDocsPinnedParams](params)
	if err != nil {
		return nil, err
	}
	existing, err := s.mcpPrivateDocumentBySlug(auth, input.Slug)
	if err != nil {
		s.recordMCPAudit(r, auth, "docs.set_pinned", "document", "", strings.TrimSpace(input.Slug), "error", err.Error(), params)
		return nil, err
	}
	if err := mcpCheckExpectedUpdatedAt(existing, input.ExpectedUpdatedAt); err != nil {
		s.recordMCPAudit(r, auth, "docs.set_pinned", "document", existing.ID, existing.Title, "conflict", err.Error(), params)
		return nil, err
	}
	next := mcpDocumentInputFromExisting(existing)
	next.Pinned = input.Pinned
	document, err := s.control.UpdateDocument(auth.User, existing.ID, next)
	if err != nil {
		s.recordMCPAudit(r, auth, "docs.set_pinned", "document", existing.ID, existing.Title, "error", err.Error(), params)
		return nil, err
	}
	s.recordMCPAudit(r, auth, "docs.set_pinned", "document", document.ID, document.Title, "ok", auditDocumentUpdateChangeMessage(existing, document, true), params)
	return mcpToolJSON(s.mcpDocumentDetail(r, document)), nil
}

func (s *Server) handleMCPResourcesList(r *http.Request, auth controlplane.MCPAuthorization, params json.RawMessage) (any, error) {
	if err := s.requireMCPDocsRead(auth); err != nil {
		return nil, err
	}
	input, err := mcpDecodeParams[mcpResourcesListParams](params)
	if err != nil {
		return nil, err
	}
	offset, err := mcpCursorOffset(input.Cursor)
	if err != nil {
		return nil, err
	}
	documents, total := s.mcpDocumentsPageWithTotalForAuth(auth, "", offset, mcpDocumentLimit(0))
	resources := make([]map[string]any, 0, len(documents)+1)
	if offset == 0 {
		resources = append(resources, map[string]any{
			"uri":         mcpDocsResourceURI + "index",
			"name":        "Documentation index",
			"description": "AI Gateway documentation index",
			"mimeType":    "application/json",
		})
	}
	for _, document := range documents {
		resources = append(resources, map[string]any{
			"uri":      mcpDocsResourceURI + document.Slug + ".md",
			"name":     document.Title,
			"mimeType": "text/markdown",
		})
	}
	payload := map[string]any{"resources": resources}
	if next := mcpNextCursor(offset, len(documents), total); next != "" {
		payload["nextCursor"] = next
	}
	return payload, nil
}

func (s *Server) handleMCPResourceRead(r *http.Request, auth controlplane.MCPAuthorization, params json.RawMessage) (any, error) {
	if err := s.requireMCPDocsRead(auth); err != nil {
		s.recordMCPAudit(r, auth, "mcp.resources.read", "document", "", "", "denied", err.Error(), params)
		return nil, err
	}
	input, err := mcpDecodeParams[mcpResourceReadParams](params)
	if err != nil {
		return nil, err
	}
	uri := strings.TrimSpace(input.URI)
	if isIndex, cursor, err := mcpIndexResourceCursor(uri); err != nil {
		return nil, err
	} else if isIndex {
		offset, err := mcpCursorOffset(cursor)
		if err != nil {
			return nil, err
		}
		documents, total := s.mcpDocumentsPageWithTotalForAuth(auth, "", offset, mcpDocumentLimit(0))
		index := map[string]any{
			"documents": s.mcpDocumentSummaries(r, documents),
			"total":     total,
		}
		if next := mcpNextCursor(offset, len(documents), total); next != "" {
			index["next_cursor"] = next
			index["next_resource_uri"] = mcpDocsResourceURI + "index?cursor=" + url.QueryEscape(next)
		}
		body, _ := json.MarshalIndent(index, "", "  ")
		s.recordMCPAudit(r, auth, "mcp.resources.read", "document", "", "Documentation index", "ok", "", params)
		return map[string]any{"contents": []map[string]any{{
			"uri":      uri,
			"mimeType": "application/json",
			"text":     string(body),
		}}}, nil
	}
	if !strings.HasPrefix(uri, mcpDocsResourceURI) || !strings.HasSuffix(uri, ".md") {
		return nil, mcpInvalidParams("unsupported resource uri")
	}
	slug := strings.TrimSuffix(strings.TrimPrefix(uri, mcpDocsResourceURI), ".md")
	document, err := s.mcpDocumentBySlug(auth, slug)
	if err != nil {
		s.recordMCPAudit(r, auth, "mcp.resources.read", "document", "", slug, "error", err.Error(), params)
		return nil, err
	}
	s.recordMCPAudit(r, auth, "mcp.resources.read", "document", document.ID, document.Title, "ok", "", params)
	return map[string]any{"contents": []map[string]any{{
		"uri":      uri,
		"mimeType": "text/markdown",
		"text":     documentMarkdownForMCP(document),
	}}}, nil
}

func (s *Server) mcpToolsForAuth(auth controlplane.MCPAuthorization) []map[string]any {
	tools := make([]map[string]any, 0, 8)
	if s.mcpCanReadDocs(auth) {
		tools = append(tools,
			mcpToolSpec("docs.list", "List documentation pages visible to this MCP token.", map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
					"status": map[string]any{"type": "string", "enum": []string{"published", "draft", "archived"}},
				},
				"additionalProperties": false,
			}),
			mcpToolSpec("docs.search", "Search documentation by title, slug, metadata, and Markdown body.", map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string"},
					"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 50},
				},
				"additionalProperties": false,
			}),
			mcpToolSpec("docs.read", "Read one documentation page as Markdown.", map[string]any{
				"type": "object",
				"properties": map[string]any{
					"slug": map[string]any{"type": "string"},
				},
				"required":             []string{"slug"},
				"additionalProperties": false,
			}),
		)
	}
	if auth.User.IsAdmin() && auth.HasScope(core.MCPTokenScopeDocsWrite) {
		tools = append(tools,
			mcpToolSpec("docs.create_draft", "Create a draft documentation page. Slug is generated automatically.", map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title":   map[string]any{"type": "string"},
					"body":    map[string]any{"type": "string"},
					"pinned":  map[string]any{"type": "boolean"},
					"noindex": map[string]any{"type": "boolean"},
				},
				"required":             []string{"title", "body"},
				"additionalProperties": false,
			}),
			mcpToolSpec("docs.update", "Update title, body, pinned, or noindex for an existing document. Requires expected_updated_at from docs.read/list.", map[string]any{
				"type": "object",
				"properties": map[string]any{
					"slug":                map[string]any{"type": "string"},
					"title":               map[string]any{"type": "string"},
					"body":                map[string]any{"type": "string"},
					"pinned":              map[string]any{"type": "boolean"},
					"noindex":             map[string]any{"type": "boolean"},
					"expected_updated_at": map[string]any{"type": "string"},
				},
				"required":             []string{"slug", "expected_updated_at"},
				"additionalProperties": false,
			}),
		)
	}
	if auth.User.IsAdmin() && auth.HasScope(core.MCPTokenScopeDocsPublish) {
		tools = append(tools, mcpStatusToolSpec("docs.publish", "Publish a draft or archived document."))
	}
	if auth.User.IsAdmin() && auth.HasScope(core.MCPTokenScopeDocsArchive) {
		tools = append(tools, mcpStatusToolSpec("docs.archive", "Archive a document instead of deleting it."))
	}
	if auth.User.IsAdmin() && auth.HasScope(core.MCPTokenScopeDocsPin) {
		tools = append(tools, mcpToolSpec("docs.set_pinned", "Pin or unpin a document.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"slug":                map[string]any{"type": "string"},
				"pinned":              map[string]any{"type": "boolean"},
				"expected_updated_at": map[string]any{"type": "string"},
			},
			"required":             []string{"slug", "pinned", "expected_updated_at"},
			"additionalProperties": false,
		}))
	}
	return tools
}

func mcpStatusToolSpec(name, description string) map[string]any {
	return mcpToolSpec(name, description, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"slug":                map[string]any{"type": "string"},
			"expected_updated_at": map[string]any{"type": "string"},
		},
		"required":             []string{"slug", "expected_updated_at"},
		"additionalProperties": false,
	})
}

func mcpToolSpec(name, description string, inputSchema map[string]any) map[string]any {
	return map[string]any{
		"name":        name,
		"description": description,
		"inputSchema": inputSchema,
	}
}

func (s *Server) requireMCPDocsRead(auth controlplane.MCPAuthorization) error {
	if !s.mcpCanReadDocs(auth) {
		return fmt.Errorf("mcp token missing required scope: %s", core.MCPTokenScopeDocsRead)
	}
	return nil
}

func (s *Server) mcpCanReadDocs(auth controlplane.MCPAuthorization) bool {
	if auth.HasScope(core.MCPTokenScopeDocsRead) {
		return true
	}
	return auth.User.IsAdmin() && auth.HasScope(core.MCPTokenScopeDocsPrivate)
}

func (s *Server) mcpDocumentsPageForAuth(auth controlplane.MCPAuthorization, status core.DocumentStatus, limit int) []core.Document {
	documents, _ := s.mcpDocumentsPageWithTotalForAuth(auth, status, 0, limit)
	return documents
}

func (s *Server) mcpDocumentsPageWithTotalForAuth(auth controlplane.MCPAuthorization, status core.DocumentStatus, offset, limit int) ([]core.Document, int) {
	if limit <= 0 {
		limit = mcpDocumentLimit(limit)
	}
	if auth.User.IsAdmin() && auth.HasScope(core.MCPTokenScopeDocsPrivate) {
		return s.control.ListDocumentsPage(auth.User, status, offset, limit)
	}
	return s.control.ListPublicDocumentsForSEOPage(status, offset, limit)
}

func (s *Server) mcpDocumentSearchPageForAuth(auth controlplane.MCPAuthorization, query string, limit int) []core.Document {
	if limit <= 0 {
		limit = mcpDocumentLimit(limit)
	}
	if auth.User.IsAdmin() && auth.HasScope(core.MCPTokenScopeDocsPrivate) {
		documents, _ := s.control.SearchDocumentsPage(auth.User, query, "", 0, limit)
		return documents
	}
	documents, _ := s.control.SearchPublicDocumentsForSEOPage(query, 0, limit)
	return documents
}

func (s *Server) mcpDocumentBySlug(auth controlplane.MCPAuthorization, rawSlug string) (core.Document, error) {
	if auth.User.IsAdmin() && auth.HasScope(core.MCPTokenScopeDocsPrivate) {
		return s.mcpPrivateDocumentBySlug(auth, rawSlug)
	}
	slug, err := controlplane.NormalizeDocumentSlug(rawSlug)
	if err != nil {
		return core.Document{}, err
	}
	document, err := s.control.GetDocumentForUser(slug, core.User{})
	if err != nil {
		return core.Document{}, storage.ErrNotFound
	}
	if !controlplane.DocumentSEOIndexable(document) {
		return core.Document{}, storage.ErrNotFound
	}
	return document, nil
}

func (s *Server) mcpPrivateDocumentBySlug(auth controlplane.MCPAuthorization, rawSlug string) (core.Document, error) {
	slug, err := controlplane.NormalizeDocumentSlug(rawSlug)
	if err != nil {
		return core.Document{}, err
	}
	if !auth.User.IsAdmin() {
		return core.Document{}, fmt.Errorf("admin role required")
	}
	return s.control.GetDocumentForUser(slug, auth.User)
}

func (s *Server) mcpDocumentSummaries(r *http.Request, documents []core.Document) []map[string]any {
	out := make([]map[string]any, 0, len(documents))
	baseURL := s.publicDocsBaseURL(r)
	for _, document := range documents {
		out = append(out, map[string]any{
			"id":                  document.ID,
			"slug":                document.Slug,
			"title":               document.Title,
			"status":              document.Status,
			"pinned":              document.Pinned,
			"noindex":             document.NoIndex,
			"updated_at":          mcpTime(document.UpdatedAt),
			"published_at":        mcpTimePtr(document.PublishedAt),
			"canonical_url":       documentCanonicalURL(document, baseURL),
			"resource_uri":        mcpDocsResourceURI + document.Slug + ".md",
			"expected_updated_at": mcpTime(document.UpdatedAt),
		})
	}
	return out
}

func (s *Server) mcpDocumentDetail(r *http.Request, document core.Document) map[string]any {
	baseURL := s.publicDocsBaseURL(r)
	return map[string]any{
		"id":                  document.ID,
		"slug":                document.Slug,
		"title":               document.Title,
		"status":              document.Status,
		"body":                document.Body,
		"markdown":            documentMarkdownForMCP(document),
		"pinned":              document.Pinned,
		"noindex":             document.NoIndex,
		"updated_at":          mcpTime(document.UpdatedAt),
		"published_at":        mcpTimePtr(document.PublishedAt),
		"canonical_url":       documentCanonicalURL(document, baseURL),
		"resource_uri":        mcpDocsResourceURI + document.Slug + ".md",
		"expected_updated_at": mcpTime(document.UpdatedAt),
	}
}

func mcpDocumentInputFromExisting(document core.Document) controlplane.DocumentInput {
	return controlplane.DocumentInput{
		Title:           document.Title,
		Body:            document.Body,
		MetaTitle:       document.MetaTitle,
		MetaDescription: document.MetaDescription,
		CanonicalURL:    document.CanonicalURL,
		Pinned:          document.Pinned,
		NoIndex:         document.NoIndex,
		Status:          document.Status,
	}
}

func mcpDocumentLimit(limit int) int {
	if limit <= 0 {
		return 20
	}
	return min(limit, 100)
}

func mcpCursorOffset(cursor string) (int, error) {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return 0, nil
	}
	offset, err := strconv.Atoi(cursor)
	if err != nil || offset < 0 {
		return 0, mcpInvalidParams("invalid cursor")
	}
	return offset, nil
}

func mcpNextCursor(offset, count, total int) string {
	next := offset + count
	if next >= total {
		return ""
	}
	return strconv.Itoa(next)
}

func mcpIndexResourceCursor(uri string) (bool, string, error) {
	rest, ok := strings.CutPrefix(uri, mcpDocsResourceURI)
	if !ok {
		return false, "", nil
	}
	if rest == "index" {
		return true, "", nil
	}
	if !strings.HasPrefix(rest, "index?") {
		return false, "", nil
	}
	values, err := url.ParseQuery(strings.TrimPrefix(rest, "index?"))
	if err != nil {
		return true, "", mcpInvalidParams("invalid index resource uri")
	}
	return true, values.Get("cursor"), nil
}

func documentMarkdownForMCP(document core.Document) string {
	var builder strings.Builder
	builder.WriteString("# ")
	builder.WriteString(documentSEOSnippet(document.Title, 160))
	builder.WriteString("\n\n")
	builder.WriteString(strings.TrimSpace(document.Body))
	builder.WriteString("\n")
	return builder.String()
}

func mcpCheckExpectedUpdatedAt(document core.Document, expected string) error {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return mcpInvalidParams("expected_updated_at is required")
	}
	parsed, err := time.Parse(time.RFC3339Nano, expected)
	if err != nil {
		return mcpInvalidParams("expected_updated_at must be RFC3339")
	}
	if !document.UpdatedAt.UTC().Equal(parsed.UTC()) {
		return mcpConflict("document was updated after the MCP client read it")
	}
	return nil
}

func mcpDecodeParams[T any](params json.RawMessage) (T, error) {
	var out T
	params = bytes.TrimSpace(params)
	if len(params) == 0 || bytes.Equal(params, []byte("null")) {
		params = []byte("{}")
	}
	if err := json.Unmarshal(params, &out); err != nil {
		return out, mcpInvalidParams(err.Error())
	}
	return out, nil
}

func mcpToolJSON(payload any) map[string]any {
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		body = []byte(`{"error":"failed to encode MCP tool response"}`)
	}
	return map[string]any{
		"content": []map[string]string{{
			"type": "text",
			"text": string(body),
		}},
	}
}

func mcpInvalidParams(message string) error {
	return mcpTypedError{code: mcpErrorCodeInvalidParams, message: message}
}

func mcpConflict(message string) error {
	return mcpTypedError{code: mcpErrorCodeConflict, message: message}
}

type mcpTypedError struct {
	code    int
	message string
}

func (e mcpTypedError) Error() string {
	return e.message
}

func mcpErrorFromError(err error) *mcpRPCError {
	if err == nil {
		return nil
	}
	var typed mcpTypedError
	if errors.As(err, &typed) {
		return &mcpRPCError{Code: typed.code, Message: typed.message}
	}
	if errors.Is(err, storage.ErrNotFound) {
		return &mcpRPCError{Code: mcpErrorCodeDocumentNotFound, Message: "document not found"}
	}
	return &mcpRPCError{Code: mcpErrorCodeInternalError, Message: "internal error"}
}

func writeMCPResponse(w http.ResponseWriter, response mcpRPCResponse) {
	writeJSON(w, http.StatusOK, response)
}

func writeMCPHTTPError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}

func mcpBearerTokenFromRequest(r *http.Request) (string, bool) {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if authHeader == "" {
		return "", false
	}
	parts := strings.Fields(authHeader)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		return "", false
	}
	return strings.TrimSpace(parts[1]), true
}

func (s *Server) mcpOriginAllowed(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	originURL, err := url.Parse(origin)
	if err != nil {
		return false
	}
	originHost := normalizeMCPHost(originURL.Host)
	requestHost := normalizeMCPHost(r.Host)
	if originHost != "" && originHost == requestHost {
		return true
	}
	baseURL := s.publicDocsBaseURL(r)
	parsedBase, err := url.Parse(baseURL)
	if err == nil && normalizeMCPHost(parsedBase.Host) == originHost {
		return true
	}
	return false
}

func normalizeMCPHost(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(value)
	if err == nil {
		if port == "80" || port == "443" {
			return strings.Trim(host, "[]")
		}
		return strings.Trim(host, "[]") + ":" + port
	}
	return strings.Trim(value, "[]")
}

func (s *Server) recordMCPAudit(r *http.Request, auth controlplane.MCPAuthorization, action, resourceType, resourceID, resourceName, status, message string, params json.RawMessage) {
	if s == nil || s.control == nil {
		return
	}
	ip := clientIP(r)
	if ip != "" {
		if strings.TrimSpace(message) != "" {
			message += "; "
		}
		message += "ip=" + ip
	}
	_ = s.control.AppendMCPAudit(auth, action, resourceType, resourceID, resourceName, status, message, mcpAuditParams(params))
}

func mcpAuditParams(params json.RawMessage) string {
	params = bytes.TrimSpace(params)
	if len(params) == 0 || bytes.Equal(params, []byte("null")) {
		return ""
	}
	var payload any
	if err := json.Unmarshal(params, &payload); err != nil {
		if len(params) > 512 {
			return string(params[:512]) + "...[truncated]"
		}
		return string(params)
	}
	sanitized := sanitizeMCPAuditValue(payload)
	body, err := json.Marshal(sanitized)
	if err != nil {
		return ""
	}
	if len(body) > 1024 {
		return string(body[:1024]) + "...[truncated]"
	}
	return string(body)
}

func sanitizeMCPAuditValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			if strings.EqualFold(key, "body") {
				if text, ok := item.(string); ok {
					out[key] = "[omitted " + strconv.Itoa(len(text)) + " bytes]"
					continue
				}
				out[key] = "[omitted]"
				continue
			}
			out[key] = sanitizeMCPAuditValue(item)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, sanitizeMCPAuditValue(item))
		}
		return out
	default:
		return value
	}
}

func mcpTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func mcpTimePtr(value *time.Time) string {
	if value == nil {
		return ""
	}
	return mcpTime(*value)
}
