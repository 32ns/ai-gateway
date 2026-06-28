package web

const (
	mcpErrorCodeParseError       = -32700
	mcpErrorCodeInvalidRequest   = -32600
	mcpErrorCodeMethodNotFound   = -32601
	mcpErrorCodeInvalidParams    = -32602
	mcpErrorCodeInternalError    = -32603
	mcpErrorCodeDocumentNotFound = -32004
	mcpErrorCodeConflict         = -32009

	mcpHTTPErrorCodeForbidden      = "forbidden"
	mcpHTTPErrorCodeInvalidRequest = "invalid_request"
	mcpHTTPErrorCodeUnauthorized   = "unauthorized"

	supportErrorCodeAuthRequired     = "auth_required"
	supportErrorCodeDataConflict     = "data_conflict"
	supportErrorCodeForbidden        = "forbidden"
	supportErrorCodeInvalidRequest   = "invalid_request"
	supportErrorCodeNotFound         = "not_found"
	supportErrorCodeRequestFailed    = "request_failed"
	supportErrorCodeUnsupportedEvent = "unsupported_event"
)
