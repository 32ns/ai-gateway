package web

type gatewayAPIGroup struct {
	ID          string
	Title       string
	Description string
	Endpoints   []gatewayAPIEndpoint
}

type gatewayAPIEndpoint struct {
	ID          string
	Method      string
	Path        string
	Badge       string
	Title       string
	Summary     string
	Auth        string
	ContentType string
	Example     string
	Notes       []string
}

func gatewayAPICatalog(locale string) []gatewayAPIGroup {
	text := func(zh, en string) string {
		if locale == localeZH {
			return zh
		}
		return en
	}
	apiKeyAuth := text("API Key：Authorization: Bearer <API_KEY>，也支持 X-API-Key", "API key: Authorization: Bearer <API_KEY>; X-API-Key is also supported")
	mcpAuth := text("MCP Bearer Token：Authorization: Bearer <MCP_TOKEN>", "MCP bearer token: Authorization: Bearer <MCP_TOKEN>")
	publicAuth := text("无需认证", "No authentication")
	jsonContent := "application/json"
	multipartContent := "multipart/form-data"
	wsContent := text("WebSocket Upgrade", "WebSocket upgrade")

	return []gatewayAPIGroup{
		{
			ID:          "models",
			Title:       text("模型发现", "Model discovery"),
			Description: text("查询当前 API Key 可用的 OpenAI 兼容模型。", "List OpenAI-compatible models available to the current API key."),
			Endpoints: []gatewayAPIEndpoint{
				{
					ID:          "models-list",
					Method:      "GET",
					Path:        "/v1/models",
					Badge:       text("OpenAI 兼容", "OpenAI compatible"),
					Title:       text("获取模型列表", "List models"),
					Summary:     text("返回当前客户端可见的 OpenAI 兼容模型和别名。", "Returns OpenAI-compatible models and aliases visible to the current client."),
					Auth:        apiKeyAuth,
					ContentType: jsonContent,
					Example:     "curl https://gateway.example.com/v1/models \\\n  -H \"Authorization: Bearer $API_KEY\"",
					Notes:       []string{text("只返回 OpenAI provider 的模型；Claude 模型通过 Anthropic Messages 路径使用。", "Only OpenAI provider models are returned; Claude models are used through Anthropic Messages endpoints.")},
				},
				{
					ID:          "models-get",
					Method:      "GET",
					Path:        "/v1/models/{model}",
					Badge:       text("OpenAI 兼容", "OpenAI compatible"),
					Title:       text("获取单个模型", "Retrieve a model"),
					Summary:     text("查询指定模型 ID 是否对当前客户端可用。", "Checks whether a model ID is available to the current client."),
					Auth:        apiKeyAuth,
					ContentType: jsonContent,
					Example:     "curl https://gateway.example.com/v1/models/gpt-4.1 \\\n  -H \"Authorization: Bearer $API_KEY\"",
					Notes:       []string{text("不存在或不可见时返回协议格式的 404。", "Returns a protocol-formatted 404 when the model is missing or not visible.")},
				},
			},
		},
		{
			ID:          "openai-text",
			Title:       text("OpenAI 文本与对话", "OpenAI text and conversation"),
			Description: text("Chat Completions 与 Responses 入口，支持普通请求、SSE 和 WebSocket。", "Chat Completions and Responses entry points with regular, SSE, and WebSocket modes."),
			Endpoints: []gatewayAPIEndpoint{
				{
					ID:          "chat-completions",
					Method:      "POST",
					Path:        "/v1/chat/completions",
					Badge:       text("OpenAI 兼容", "OpenAI compatible"),
					Title:       text("Chat Completions", "Chat Completions"),
					Summary:     text("OpenAI Chat Completions 兼容入口，支持流式和非流式输出。", "OpenAI Chat Completions compatible endpoint with streaming and non-streaming output."),
					Auth:        apiKeyAuth,
					ContentType: jsonContent,
					Example:     "curl https://gateway.example.com/v1/chat/completions \\\n  -H \"Authorization: Bearer $API_KEY\" \\\n  -H \"Content-Type: application/json\" \\\n  -d '{\"model\":\"gpt-4.1\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}'",
					Notes:       []string{text("请求体会保留原始字段，额外参数会转交给上游适配器。", "The raw request body is preserved and extra fields are forwarded to the upstream adapter.")},
				},
				{
					ID:          "responses-create",
					Method:      "POST",
					Path:        "/v1/responses",
					Badge:       text("OpenAI 兼容", "OpenAI compatible"),
					Title:       text("Responses", "Responses"),
					Summary:     text("OpenAI Responses 兼容入口，支持普通响应和 SSE 流式响应。", "OpenAI Responses compatible endpoint with regular and SSE streaming responses."),
					Auth:        apiKeyAuth,
					ContentType: jsonContent,
					Example:     "curl https://gateway.example.com/v1/responses \\\n  -H \"Authorization: Bearer $API_KEY\" \\\n  -H \"Content-Type: application/json\" \\\n  -d '{\"model\":\"gpt-4.1\",\"input\":\"hello\"}'",
					Notes:       []string{text("请求体必须包含 model。设置 stream=true 时走 SSE。", "The request body must include model. Set stream=true for SSE.")},
				},
				{
					ID:          "responses-websocket",
					Method:      "GET",
					Path:        "/v1/responses",
					Badge:       text("OpenAI WebSocket", "OpenAI WebSocket"),
					Title:       text("Responses WebSocket", "Responses WebSocket"),
					Summary:     text("同一路径在 WebSocket Upgrade 时进入 Responses WebSocket 会话。", "The same path enters a Responses WebSocket session when requested as a WebSocket upgrade."),
					Auth:        apiKeyAuth,
					ContentType: wsContent,
					Example:     "websocat -H \"Authorization: Bearer $API_KEY\" wss://gateway.example.com/v1/responses\n> {\"type\":\"response.create\",\"model\":\"gpt-4.1\",\"input\":\"hello\"}",
					Notes:       []string{text("连接建立后发送 response.create、session.update、response.cancel 等事件。浏览器 WebSocket 无法设置 Authorization 请求头，需要服务端或支持自定义请求头的客户端。", "After connecting, send events such as response.create, session.update, and response.cancel. Browser WebSocket cannot set Authorization headers; use a server-side or custom-header-capable client.")},
				},
				{
					ID:          "responses-compact",
					Method:      "POST",
					Path:        "/v1/responses/compact",
					Badge:       text("OpenAI 扩展", "OpenAI extension"),
					Title:       text("Responses 压缩", "Responses compaction"),
					Summary:     text("触发 Responses 上下文压缩。该入口不支持 stream。", "Triggers Responses context compaction. This endpoint does not support stream."),
					Auth:        apiKeyAuth,
					ContentType: jsonContent,
					Example:     "curl https://gateway.example.com/v1/responses/compact \\\n  -H \"Authorization: Bearer $API_KEY\" \\\n  -H \"Content-Type: application/json\" \\\n  -d '{\"model\":\"gpt-4.1\",\"input\":\"compact this conversation\",\"previous_response_id\":\"resp_123\"}'",
					Notes:       []string{text("stream=true 会返回 400。", "stream=true returns 400.")},
				},
				{
					ID:          "responses-input-tokens",
					Method:      "POST",
					Path:        "/v1/responses/input_tokens",
					Badge:       text("OpenAI 扩展", "OpenAI extension"),
					Title:       text("Responses 输入 Token 统计", "Responses input token count"),
					Summary:     text("代理上游的 Responses 输入 token 统计接口。", "Proxies the upstream Responses input token counting endpoint."),
					Auth:        apiKeyAuth,
					ContentType: jsonContent,
					Example:     "curl https://gateway.example.com/v1/responses/input_tokens \\\n  -H \"Authorization: Bearer $API_KEY\" \\\n  -H \"Content-Type: application/json\" \\\n  -d '{\"model\":\"gpt-4.1\",\"input\":\"count me\"}'",
					Notes:       []string{text("该接口会选择当前客户端可用的 OpenAI 账号直接代理。", "This endpoint selects an OpenAI account available to the current client and proxies directly.")},
				},
				{
					ID:          "responses-get",
					Method:      "GET",
					Path:        "/v1/responses/{response_id}",
					Badge:       text("OpenAI 兼容", "OpenAI compatible"),
					Title:       text("读取 Response", "Retrieve a response"),
					Summary:     text("通过已绑定的上游账号读取指定 response。", "Retrieves a response through the bound upstream account."),
					Auth:        apiKeyAuth,
					ContentType: jsonContent,
					Example:     "curl https://gateway.example.com/v1/responses/resp_123 \\\n  -H \"Authorization: Bearer $API_KEY\"",
					Notes:       []string{text("只能读取当前客户端创建并已绑定的 response。", "Only responses created by and bound to the current client can be retrieved.")},
				},
				{
					ID:          "responses-input-items",
					Method:      "GET",
					Path:        "/v1/responses/{response_id}/input_items",
					Badge:       text("OpenAI 兼容", "OpenAI compatible"),
					Title:       text("读取 Response 输入项", "List response input items"),
					Summary:     text("读取指定 response 的 input_items 资源。", "Lists input_items for a response."),
					Auth:        apiKeyAuth,
					ContentType: jsonContent,
					Example:     "curl 'https://gateway.example.com/v1/responses/resp_123/input_items?after=item_1' \\\n  -H \"Authorization: Bearer $API_KEY\"",
					Notes:       []string{text("查询参数会原样转发给上游。", "Query parameters are forwarded to the upstream unchanged.")},
				},
				{
					ID:          "responses-delete",
					Method:      "DELETE",
					Path:        "/v1/responses/{response_id}",
					Badge:       text("OpenAI 兼容", "OpenAI compatible"),
					Title:       text("删除 Response", "Delete a response"),
					Summary:     text("删除指定 response 资源。", "Deletes a response resource."),
					Auth:        apiKeyAuth,
					ContentType: jsonContent,
					Example:     "curl -X DELETE https://gateway.example.com/v1/responses/resp_123 \\\n  -H \"Authorization: Bearer $API_KEY\"",
					Notes:       []string{text("路由层允许 DELETE /v1/responses/{response_path...}，常用路径是 response_id。", "The router allows DELETE /v1/responses/{response_path...}; response_id is the common path.")},
				},
				{
					ID:          "responses-cancel",
					Method:      "POST",
					Path:        "/v1/responses/{response_id}/cancel",
					Badge:       text("OpenAI 兼容", "OpenAI compatible"),
					Title:       text("取消 Response", "Cancel a response"),
					Summary:     text("取消指定 response。", "Cancels a response."),
					Auth:        apiKeyAuth,
					ContentType: jsonContent,
					Example:     "curl -X POST https://gateway.example.com/v1/responses/resp_123/cancel \\\n  -H \"Authorization: Bearer $API_KEY\" \\\n  -H \"Content-Type: application/json\" \\\n  -d '{}'",
					Notes:       []string{text("POST 只允许用于 /cancel 子路径。", "POST is only allowed on the /cancel subpath.")},
				},
			},
		},
		{
			ID:          "openai-media",
			Title:       text("OpenAI 多模态", "OpenAI multimodal"),
			Description: text("Embedding、审核、图像和音频接口。", "Embeddings, moderation, image, and audio endpoints."),
			Endpoints: []gatewayAPIEndpoint{
				{
					ID:          "embeddings",
					Method:      "POST",
					Path:        "/v1/embeddings",
					Badge:       text("OpenAI 兼容", "OpenAI compatible"),
					Title:       text("Embeddings", "Embeddings"),
					Summary:     text("生成文本或 token 输入的向量。", "Creates embeddings for text or token input."),
					Auth:        apiKeyAuth,
					ContentType: jsonContent,
					Example:     "curl https://gateway.example.com/v1/embeddings \\\n  -H \"Authorization: Bearer $API_KEY\" \\\n  -H \"Content-Type: application/json\" \\\n  -d '{\"model\":\"text-embedding-3-small\",\"input\":\"alpha\"}'",
					Notes:       []string{text("支持 encoding_format=float 或 base64。", "Supports encoding_format=float or base64.")},
				},
				{
					ID:          "moderations",
					Method:      "POST",
					Path:        "/v1/moderations",
					Badge:       text("OpenAI 兼容", "OpenAI compatible"),
					Title:       text("Moderations", "Moderations"),
					Summary:     text("内容安全审核。", "Runs content safety moderation."),
					Auth:        apiKeyAuth,
					ContentType: jsonContent,
					Example:     "curl https://gateway.example.com/v1/moderations \\\n  -H \"Authorization: Bearer $API_KEY\" \\\n  -H \"Content-Type: application/json\" \\\n  -d '{\"model\":\"omni-moderation-latest\",\"input\":\"hello\"}'",
					Notes:       []string{text("未传 model 时默认使用 omni-moderation-latest。", "Defaults to omni-moderation-latest when model is omitted.")},
				},
				{
					ID:          "images-generations",
					Method:      "POST",
					Path:        "/v1/images/generations",
					Badge:       text("OpenAI 兼容", "OpenAI compatible"),
					Title:       text("图片生成", "Image generations"),
					Summary:     text("根据 prompt 生成图片，支持流式事件。", "Generates images from a prompt and supports streaming events."),
					Auth:        apiKeyAuth,
					ContentType: jsonContent,
					Example:     "curl https://gateway.example.com/v1/images/generations \\\n  -H \"Authorization: Bearer $API_KEY\" \\\n  -H \"Content-Type: application/json\" \\\n  -d '{\"model\":\"gpt-image-2\",\"prompt\":\"a clean product photo\"}'",
					Notes:       []string{text("未传 model 时默认使用 gpt-image-2。", "Defaults to gpt-image-2 when model is omitted.")},
				},
				{
					ID:          "images-edits",
					Method:      "POST",
					Path:        "/v1/images/edits",
					Badge:       text("OpenAI 兼容", "OpenAI compatible"),
					Title:       text("图片编辑", "Image edits"),
					Summary:     text("通过 multipart 上传图片并编辑。", "Edits images through multipart upload."),
					Auth:        apiKeyAuth,
					ContentType: multipartContent,
					Example:     "curl https://gateway.example.com/v1/images/edits \\\n  -H \"Authorization: Bearer $API_KEY\" \\\n  -F model=gpt-image-2 \\\n  -F prompt=\"make the background clean\" \\\n  -F image=@input.png",
					Notes:       []string{text("未传 model 时默认使用 gpt-image-2。", "Defaults to gpt-image-2 when model is omitted.")},
				},
				{
					ID:          "audio-speech",
					Method:      "POST",
					Path:        "/v1/audio/speech",
					Badge:       text("OpenAI 兼容", "OpenAI compatible"),
					Title:       text("文本转语音", "Text to speech"),
					Summary:     text("把输入文本转换为音频。", "Converts input text into audio."),
					Auth:        apiKeyAuth,
					ContentType: jsonContent,
					Example:     "curl https://gateway.example.com/v1/audio/speech \\\n  -H \"Authorization: Bearer $API_KEY\" \\\n  -H \"Content-Type: application/json\" \\\n  -d '{\"model\":\"gpt-4o-mini-tts\",\"voice\":\"alloy\",\"input\":\"hello\"}' \\\n  --output speech.mp3",
					Notes:       []string{text("model、voice、input 都是必填。", "model, voice, and input are required.")},
				},
				{
					ID:          "audio-transcriptions",
					Method:      "POST",
					Path:        "/v1/audio/transcriptions",
					Badge:       text("OpenAI 兼容", "OpenAI compatible"),
					Title:       text("音频转写", "Audio transcriptions"),
					Summary:     text("把音频转写为文本。", "Transcribes audio to text."),
					Auth:        apiKeyAuth,
					ContentType: multipartContent,
					Example:     "curl https://gateway.example.com/v1/audio/transcriptions \\\n  -H \"Authorization: Bearer $API_KEY\" \\\n  -F model=gpt-4o-transcribe \\\n  -F file=@speech.mp3",
					Notes:       []string{text("multipart 表单中必须包含 model。", "The multipart form must include model.")},
				},
				{
					ID:          "audio-translations",
					Method:      "POST",
					Path:        "/v1/audio/translations",
					Badge:       text("OpenAI 兼容", "OpenAI compatible"),
					Title:       text("音频翻译", "Audio translations"),
					Summary:     text("把音频翻译为文本。", "Translates audio to text."),
					Auth:        apiKeyAuth,
					ContentType: multipartContent,
					Example:     "curl https://gateway.example.com/v1/audio/translations \\\n  -H \"Authorization: Bearer $API_KEY\" \\\n  -F model=gpt-4o-transcribe \\\n  -F file=@speech.mp3",
					Notes:       []string{text("multipart 表单中必须包含 model。", "The multipart form must include model.")},
				},
			},
		},
		{
			ID:          "anthropic",
			Title:       text("Anthropic Messages", "Anthropic Messages"),
			Description: text("Claude Messages 和 token 统计，提供三组兼容路径。", "Claude Messages and token counting with three compatible path groups."),
			Endpoints: []gatewayAPIEndpoint{
				anthropicMessageEndpoint(text, apiKeyAuth, "/v1/messages", "anthropic-messages-v1"),
				anthropicTokenEndpoint(text, apiKeyAuth, "/v1/messages/count_tokens", "anthropic-count-v1"),
				anthropicMessageEndpoint(text, apiKeyAuth, "/api/v1/messages", "anthropic-messages-api-v1"),
				anthropicTokenEndpoint(text, apiKeyAuth, "/api/v1/messages/count_tokens", "anthropic-count-api-v1"),
				anthropicMessageEndpoint(text, apiKeyAuth, "/anthropic/v1/messages", "anthropic-messages-anthropic-v1"),
				anthropicTokenEndpoint(text, apiKeyAuth, "/anthropic/v1/messages/count_tokens", "anthropic-count-anthropic-v1"),
			},
		},
		{
			ID:          "gateway-private",
			Title:       text("网关私有额度接口", "Gateway quota APIs"),
			Description: text("给客户端查询余额、限额和 dashboard 兼容账单信息。", "Client quota, balance, and dashboard-compatible billing information."),
			Endpoints: []gatewayAPIEndpoint{
				{
					ID:          "quota",
					Method:      "GET",
					Path:        "/ag/v1/account/quota",
					Badge:       text("网关私有", "Gateway private"),
					Title:       text("查询客户端额度", "Client quota"),
					Summary:     text("返回当前 API Key 对应客户端的额度、余额和消耗。", "Returns quota, balance, and spend information for the current API key client."),
					Auth:        apiKeyAuth,
					ContentType: jsonContent,
					Example:     "curl https://gateway.example.com/ag/v1/account/quota \\\n  -H \"Authorization: Bearer $API_KEY\"",
					Notes:       []string{text("响应结构来自控制面 GetClientQuota。", "The response shape is returned by the control plane GetClientQuota call.")},
				},
				{
					ID:          "billing-subscription",
					Method:      "GET",
					Path:        "/ag/v1/dashboard/billing/subscription",
					Badge:       text("Dashboard 兼容", "Dashboard compatible"),
					Title:       text("账单订阅信息", "Billing subscription"),
					Summary:     text("返回 OpenAI dashboard 兼容的限额字段。", "Returns OpenAI dashboard-compatible limit fields."),
					Auth:        apiKeyAuth,
					ContentType: jsonContent,
					Example:     "curl https://gateway.example.com/ag/v1/dashboard/billing/subscription \\\n  -H \"Authorization: Bearer $API_KEY\"",
					Notes:       []string{text("主要用于兼容会读取 dashboard billing 的客户端。", "Mainly used for clients that read dashboard billing endpoints.")},
				},
				{
					ID:          "billing-usage",
					Method:      "GET",
					Path:        "/ag/v1/dashboard/billing/usage",
					Badge:       text("Dashboard 兼容", "Dashboard compatible"),
					Title:       text("账单用量信息", "Billing usage"),
					Summary:     text("返回 OpenAI dashboard 兼容的用量字段。", "Returns OpenAI dashboard-compatible usage fields."),
					Auth:        apiKeyAuth,
					ContentType: jsonContent,
					Example:     "curl https://gateway.example.com/ag/v1/dashboard/billing/usage \\\n  -H \"Authorization: Bearer $API_KEY\"",
					Notes:       []string{text("total_usage 为美分口径，total_usage_usd 为美元口径。", "total_usage is cent-denominated; total_usage_usd is USD-denominated.")},
				},
			},
		},
		{
			ID:          "mcp",
			Title:       "MCP",
			Description: text("MCP JSON-RPC 入口，用于文档资源和文档管理工具。", "MCP JSON-RPC entry point for documentation resources and document management tools."),
			Endpoints: []gatewayAPIEndpoint{
				{
					ID:          "mcp-options",
					Method:      "OPTIONS",
					Path:        "/mcp",
					Badge:       "MCP",
					Title:       text("MCP 预检", "MCP preflight"),
					Summary:     text("返回 MCP 入口允许的方法。", "Returns allowed methods for the MCP endpoint."),
					Auth:        publicAuth,
					ContentType: text("无请求体", "No request body"),
					Example:     "curl -i -X OPTIONS https://gateway.example.com/mcp",
					Notes:       []string{text("响应 Allow: POST, OPTIONS。", "Responds with Allow: POST, OPTIONS.")},
				},
				{
					ID:          "mcp-post",
					Method:      "POST",
					Path:        "/mcp",
					Badge:       "MCP",
					Title:       text("MCP JSON-RPC", "MCP JSON-RPC"),
					Summary:     text("执行 MCP initialize、tools/list、tools/call、resources/list、resources/read 等方法。", "Executes MCP methods such as initialize, tools/list, tools/call, resources/list, and resources/read."),
					Auth:        mcpAuth,
					ContentType: jsonContent,
					Example:     "curl https://gateway.example.com/mcp \\\n  -H \"Authorization: Bearer $MCP_TOKEN\" \\\n  -H \"Content-Type: application/json\" \\\n  -d '{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\",\"params\":{}}'",
					Notes:       []string{text("当前工具包括 docs.list、docs.search、docs.read、docs.create_draft、docs.update、docs.publish、docs.archive、docs.set_pinned。", "Current tools include docs.list, docs.search, docs.read, docs.create_draft, docs.update, docs.publish, docs.archive, and docs.set_pinned.")},
				},
			},
		},
		{
			ID:          "health",
			Title:       text("健康检查", "Health checks"),
			Description: text("部署和负载均衡健康探测。", "Deployment and load balancer health probes."),
			Endpoints: []gatewayAPIEndpoint{
				healthEndpoint(text, publicAuth, "/health", "health"),
				healthEndpoint(text, publicAuth, "/healthz", "healthz"),
			},
		},
	}
}

func anthropicMessageEndpoint(text func(string, string) string, auth, path, id string) gatewayAPIEndpoint {
	return gatewayAPIEndpoint{
		ID:          id,
		Method:      "POST",
		Path:        path,
		Badge:       text("Anthropic 兼容", "Anthropic compatible"),
		Title:       text("Claude Messages", "Claude Messages"),
		Summary:     text("Anthropic Messages 兼容入口，支持流式和非流式输出。", "Anthropic Messages compatible endpoint with streaming and non-streaming output."),
		Auth:        auth,
		ContentType: "application/json",
		Example:     "curl https://gateway.example.com" + path + " \\\n  -H \"Authorization: Bearer $API_KEY\" \\\n  -H \"Content-Type: application/json\" \\\n  -H \"anthropic-version: 2023-06-01\" \\\n  -d '{\"model\":\"claude-sonnet-4-0\",\"max_tokens\":1024,\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}'",
		Notes:       []string{text("anthropic-version、anthropic-beta 和 x-claude-code-* 请求头会写入协议元数据。", "anthropic-version, anthropic-beta, and x-claude-code-* headers are captured as protocol metadata.")},
	}
}

func anthropicTokenEndpoint(text func(string, string) string, auth, path, id string) gatewayAPIEndpoint {
	return gatewayAPIEndpoint{
		ID:          id,
		Method:      "POST",
		Path:        path,
		Badge:       text("Anthropic 兼容", "Anthropic compatible"),
		Title:       text("Claude Token 统计", "Claude token count"),
		Summary:     text("统计 Anthropic Messages 请求的 token。", "Counts tokens for an Anthropic Messages request."),
		Auth:        auth,
		ContentType: "application/json",
		Example:     "curl https://gateway.example.com" + path + " \\\n  -H \"Authorization: Bearer $API_KEY\" \\\n  -H \"Content-Type: application/json\" \\\n  -H \"anthropic-version: 2023-06-01\" \\\n  -d '{\"model\":\"claude-sonnet-4-0\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}'",
		Notes:       []string{text("model 和 messages 都是必填。", "model and messages are required.")},
	}
}

func healthEndpoint(text func(string, string) string, auth, path, id string) gatewayAPIEndpoint {
	return gatewayAPIEndpoint{
		ID:          id,
		Method:      "GET",
		Path:        path,
		Badge:       text("公开", "Public"),
		Title:       text("健康状态", "Health status"),
		Summary:     text("返回控制面的健康报告。", "Returns the control plane health report."),
		Auth:        auth,
		ContentType: "application/json",
		Example:     "curl https://gateway.example.com" + path,
		Notes:       []string{text("状态为 error 时 HTTP 状态码为 503。", "Returns HTTP 503 when status is error.")},
	}
}
