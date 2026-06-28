package web

import (
	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/failover"
	"github.com/32ns/ai-gateway/internal/gateway"
	"github.com/32ns/ai-gateway/internal/providers"
)

type gatewayErrorGroup struct {
	ID    string
	Title string
	Codes []gatewayErrorCode
}

type gatewayErrorCode struct {
	Code    string
	Kind    string
	Meaning string
}

func gatewayErrorCatalog(locale string) []gatewayErrorGroup {
	text := func(zh, en string) string {
		if locale == localeZH {
			return zh
		}
		return en
	}

	return []gatewayErrorGroup{
		{
			ID:    "upstream",
			Title: text("上游与传输", "Upstream and transport"),
			Codes: []gatewayErrorCode{
				errorCode(providers.ErrorCodeUpstreamTransportError, text("上游/网络", "Upstream/network"), text("连接、代理、DNS、TLS 或请求发送失败", "Connection, proxy, DNS, TLS, or request send failed")),
				errorCode(providers.ErrorCodeUpstreamReadError, text("上游/读取", "Upstream/read"), text("读取上游响应体或流失败", "Failed while reading the upstream body or stream")),
				errorCode(providers.ErrorCodeUpstreamServerError, text("上游/服务", "Upstream/service"), text("上游返回 5xx、HTML 挑战或网关层异常", "Upstream returned 5xx, HTML challenge, or gateway-layer failure")),
				errorCode(providers.ErrorCodeUpstreamTemporarilyUnavailable, text("上游/临时", "Upstream/temporary"), text("上游或中间层暂时不可用", "Upstream or middle layer is temporarily unavailable")),
				errorCode(providers.ErrorCodeUpstreamInvalidJSON, text("上游/响应", "Upstream/response"), text("上游返回的 JSON 无法解析", "Upstream returned invalid JSON")),
				errorCode(providers.ErrorCodeUpstreamInvalidStreamChunk, text("上游/流", "Upstream/stream"), text("流式响应在首个输出前出现非法分块", "Streaming response had an invalid chunk before first output")),
				errorCode(providers.ErrorCodeUpstreamEmptyResponse, text("上游/响应", "Upstream/response"), text("上游返回空响应", "Upstream returned an empty response")),
				errorCode(providers.ErrorCodeUpstreamRequestBuildFailed, text("请求", "Request"), text("构造上游请求失败", "Failed to build upstream request")),
				errorCode(providers.ErrorCodeUpstreamRejected, text("请求", "Request"), text("上游明确拒绝请求，通常不可重试", "Upstream rejected the request, usually non-retryable")),
				errorCode(providers.ErrorCodeUpstreamNotFound, text("请求", "Request"), text("上游资源、模型或响应 ID 不存在", "Upstream resource, model, or response ID was not found")),
			},
		},
		{
			ID:    "credential",
			Title: text("凭证与账号", "Credentials and accounts"),
			Codes: []gatewayErrorCode{
				errorCode(providers.ErrorCodeMissingCredential, text("账号", "Account"), text("账号没有可用访问凭证", "The account has no usable credential")),
				errorCode(providers.ErrorCodeCredentialExpired, text("账号", "Account"), text("账号凭证已过期", "The account credential has expired")),
				errorCode(providers.ErrorCodeMissingRefreshCredential, text("账号", "Account"), text("需要刷新但缺少 refresh token", "Refresh is required but refresh token is missing")),
				errorCode(providers.ErrorCodeCredentialRefreshNotSupported, text("账号", "Account"), text("当前供应商不支持自动刷新该凭证", "The provider cannot refresh this credential automatically")),
				errorCode(providers.ErrorCodeCredentialStateUpdateFailed, text("账号", "Account"), text("刷新成功后写入账号状态失败", "Failed to persist refreshed credential state")),
				errorCode(providers.ErrorCodeUpstreamAuthError, text("账号", "Account"), text("上游鉴权失败", "Upstream authentication failed")),
				errorCode(providers.ErrorCodeUpstreamForbidden, text("账号", "Account"), text("上游拒绝账号访问", "Upstream denied account access")),
				errorCode(providers.ErrorCodeUpstreamProviderBanned, text("账号", "Account"), text("供应商账号被封禁或限制", "Provider account is banned or restricted")),
				errorCode(providers.ErrorCodeGatewayAPIKeyDisabled, text("账号", "Account"), text("上游网关型 API Key 已禁用", "Upstream gateway-style API key is disabled")),
			},
		},
		{
			ID:    "quota",
			Title: text("额度、计费与限流", "Quota, billing, and rate limits"),
			Codes: []gatewayErrorCode{
				errorCode(providers.ErrorCodeUpstreamRateLimited, text("额度", "Quota"), text("上游限流或账号额度耗尽", "Upstream rate limit or account quota exhausted")),
				errorCode(gateway.ErrorCodePlanBillingDisabled, text("计费", "Billing"), text("该分组不允许套餐计费", "Plan billing is disabled for this account group")),
				errorCode(gateway.ErrorCodePlanQuotaExhausted, text("计费", "Billing"), text("用户套餐额度已耗尽", "User plan quota is exhausted")),
				errorCode(gateway.ErrorCodeQuotaError, text("计费", "Billing"), text("余额不足或客户端消费上限已达到", "Insufficient balance or client spend limit reached")),
				errorCode(gateway.ErrorCodeBillingAmountOverflow, text("计费", "Billing"), text("计费金额超过可处理范围", "Billing amount exceeds the supported range")),
				errorCode(gateway.ErrorCodeBillingOwnerMismatch, text("计费", "Billing"), text("API Key 归属用户和计费用户不一致", "API key owner does not match billing user")),
				errorCode(gateway.ErrorCodeBillingConflict, text("计费", "Billing"), text("计费请求存在冲突或重复结算", "Billing request conflict or duplicate settlement detected")),
				errorCode(gateway.ErrorCodeRateLimitExceeded, text("限流", "Rate limit"), text("用户或套餐并发请求数超过限制", "User or plan concurrent request limit exceeded")),
			},
		},
		{
			ID:    "image",
			Title: text("图像与多模态", "Image and multimodal"),
			Codes: []gatewayErrorCode{
				errorCode(providers.ErrorCodeImageEndpointUnsupported, text("能力", "Capability"), text("当前账号或接口不支持图像端点", "The current account or endpoint does not support image requests")),
				errorCode(providers.ErrorCodeImageModelUnsupported, text("能力", "Capability"), text("请求的图像模型不受支持", "Requested image model is unsupported")),
				errorCode(providers.ErrorCodeImageGenerationFailed, text("图像", "Image"), text("图像生成任务失败", "Image generation task failed")),
				errorCode(providers.ErrorCodeImageGenerationRejected, text("图像", "Image"), text("图像生成请求被拒绝", "Image generation request was rejected")),
				errorCode(providers.ErrorCodeImageGenerationNotStarted, text("图像", "Image"), text("图像生成任务未成功启动", "Image generation task did not start")),
				errorCode(providers.ErrorCodeImagePollTimeout, text("图像", "Image"), text("图像任务轮询超时", "Timed out while polling image task")),
				errorCode(providers.ErrorCodeImageUploadEmpty, text("图像", "Image"), text("上传的参考图片为空", "Uploaded reference image is empty")),
				errorCode(providers.ErrorCodeImageBackendRequiresOAuth, text("账号", "Account"), text("该图像后端需要 OAuth 账号", "This image backend requires an OAuth account")),
				errorCode(providers.ErrorCodeChatGPTArkoseRequired, text("账号", "Account"), text("ChatGPT 后端要求 Arkose 验证", "ChatGPT backend requires Arkose verification")),
				errorCode(providers.ErrorCodeChatGPTProofTokenFailed, text("账号", "Account"), text("ChatGPT proof token 获取失败", "Failed to obtain ChatGPT proof token")),
				errorCode(providers.ErrorCodeChatGPTRequirementsMissingToken, text("账号", "Account"), text("ChatGPT requirements 响应缺少必要 token", "ChatGPT requirements response is missing a required token")),
				errorCode(providers.ErrorCodeChatGPTConduitTokenMissing, text("账号", "Account"), text("ChatGPT conduit token 缺失", "ChatGPT conduit token is missing")),
				errorCode(providers.ErrorCodeChatGPTUploadURLMissing, text("图像", "Image"), text("上传参考图时缺少上传 URL", "Upload URL is missing for reference image upload")),
			},
		},
		{
			ID:    "routing",
			Title: text("路由与能力", "Routing and capabilities"),
			Codes: []gatewayErrorCode{
				errorCode(failover.ErrorCodeNoAdapter, text("路由", "Routing"), text("供应商适配器未注册", "Provider adapter is not registered")),
				errorCode(failover.ErrorCodeNoAccount, text("路由", "Routing"), text("供应商没有符合条件的账号", "Provider has no eligible account")),
				errorCode(failover.ErrorCodeBoundAccountUnavailable, text("路由", "Routing"), text("绑定的上一轮账号不可用", "The bound previous account is unavailable")),
				errorCode(failover.ErrorCodeResponsesNotSupported, text("能力", "Capability"), text("供应商不支持 Responses 普通请求", "Provider does not support regular Responses requests")),
				errorCode(failover.ErrorCodeResponsesStreamingNotSupported, text("能力", "Capability"), text("供应商不支持 Responses 流式请求", "Provider does not support Responses streaming")),
				errorCode(failover.ErrorCodeResponsesWebSocketNotSupported, text("能力", "Capability"), text("供应商不支持 Responses WebSocket", "Provider does not support Responses WebSocket")),
				errorCode(failover.ErrorCodeEmbeddingsNotSupported, text("能力", "Capability"), text("供应商不支持 Embeddings", "Provider does not support embeddings")),
				errorCode(failover.ErrorCodeModerationsNotSupported, text("能力", "Capability"), text("供应商不支持审核接口", "Provider does not support moderation")),
				errorCode(failover.ErrorCodeImageGenerationNotSupported, text("能力", "Capability"), text("供应商不支持图像生成", "Provider does not support image generation")),
				errorCode(failover.ErrorCodeImageGenerationStreamingNotSupported, text("能力", "Capability"), text("供应商不支持图像生成流式事件", "Provider does not support image generation streaming events")),
				errorCode(failover.ErrorCodeImageMultipartNotSupported, text("能力", "Capability"), text("供应商不支持图像 multipart 请求", "Provider does not support image multipart requests")),
				errorCode(failover.ErrorCodeImageMultipartStreamingNotSupported, text("能力", "Capability"), text("供应商不支持图像 multipart 流式请求", "Provider does not support image multipart streaming")),
				errorCode(failover.ErrorCodeAudioSpeechNotSupported, text("能力", "Capability"), text("供应商不支持语音合成", "Provider does not support speech synthesis")),
				errorCode(failover.ErrorCodeAudioMultipartNotSupported, text("能力", "Capability"), text("供应商不支持音频 multipart 请求", "Provider does not support audio multipart requests")),
				errorCode(failover.ErrorCodeTokenCountNotSupported, text("能力", "Capability"), text("供应商不支持 token 计数", "Provider does not support token counting")),
				errorCode(failover.ErrorCodeStreamingNotSupported, text("能力", "Capability"), text("供应商不支持 Chat Completions 流式输出", "Provider does not support Chat Completions streaming")),
			},
		},
		{
			ID:    "protocol",
			Title: text("协议与访问", "Protocol and access"),
			Codes: []gatewayErrorCode{
				errorCode(controlplane.ErrorCodeAuthError, text("访问", "Access"), text("API Key 缺失、无效、禁用或归属用户不可用", "API key is missing, invalid, disabled, or its owner is unavailable")),
				errorCode(controlplane.ErrorCodeForbidden, text("访问/MCP/客服", "Access/MCP/support"), text("无权访问资源、来源不被允许或客服操作被拒绝", "Resource access, request origin, or support action is forbidden")),
				errorCode(mcpHTTPErrorCodeInvalidRequest, text("JSON-RPC/MCP/客服", "JSON-RPC/MCP/support"), text("JSON-RPC 请求无效、MCP HTTP 请求或客服消息格式无效", "Invalid JSON-RPC request, MCP HTTP request, or support message")),
				errorCode(mcpHTTPErrorCodeUnauthorized, text("访问", "Access"), text("MCP bearer token 缺失或无效", "MCP bearer token is missing or invalid")),
				errorCode("parse_error", text("JSON-RPC", "JSON-RPC"), text("JSON-RPC 解析失败，对应 code -32700", "JSON-RPC parse failure, code -32700")),
				errorCode("method_not_found", text("JSON-RPC", "JSON-RPC"), text("MCP 方法不存在，对应 code -32601", "MCP method not found, code -32601")),
				errorCode("invalid_params", text("JSON-RPC", "JSON-RPC"), text("MCP 参数无效，对应 code -32602", "Invalid MCP params, code -32602")),
				errorCode("document_not_found", text("MCP", "MCP"), text("MCP 文档不存在，对应 code -32004", "MCP document was not found, code -32004")),
				errorCode("conflict", text("MCP", "MCP"), text("MCP 操作冲突，对应 code -32009", "MCP operation conflict, code -32009")),
				errorCode("internal_error", text("JSON-RPC", "JSON-RPC"), text("MCP 内部错误，对应 code -32603", "MCP internal error, code -32603")),
				errorCode(supportErrorCodeAuthRequired, text("客服", "Support"), text("客服连接登录状态失效", "Support connection authentication expired")),
				errorCode(supportErrorCodeNotFound, text("客服", "Support"), text("客服会话不存在", "Support ticket does not exist")),
				errorCode(supportErrorCodeDataConflict, text("客服", "Support"), text("客服数据状态冲突", "Support data state conflict")),
				errorCode(supportErrorCodeRequestFailed, text("客服", "Support"), text("客服请求失败", "Support request failed")),
				errorCode(supportErrorCodeUnsupportedEvent, text("客服", "Support"), text("客服事件类型不支持", "Support event type is unsupported")),
			},
		},
	}
}

func errorCode(code, kind, meaning string) gatewayErrorCode {
	return gatewayErrorCode{
		Code:    code,
		Kind:    kind,
		Meaning: meaning,
	}
}
