package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/32ns/ai-gateway/internal/controlplane"
	"github.com/32ns/ai-gateway/internal/core"
	"github.com/32ns/ai-gateway/internal/failover"
	"github.com/32ns/ai-gateway/internal/gateway"
	"github.com/32ns/ai-gateway/internal/providers"
)

const (
	imageLabMaxCount        = 20
	imageLabMaxInputImages  = 8
	imageLabMaxImageBytes   = 12 << 20
	imageLabMaxTotalBytes   = 50 << 20
	imageLabMaxPromptRunes  = 8000
	imageLabRequestBodySize = 80 << 20
	imageLabProxyMaxBytes   = 20 << 20
	imageLabTaskTimeout     = 420 * time.Second
)

var imageLabProxyHTTPClient = newImageLabProxyHTTPClient()

func newImageLabProxyHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           imageLabProxyDialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			IdleConnTimeout:       30 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("图片 URL 重定向次数过多")
			}
			_, err := validateImageLabProxyURL(req.URL.String())
			return err
		},
	}
}

type imageLabGeneratePayload struct {
	ClientID    string                      `json:"client_id"`
	Prompt      string                      `json:"prompt"`
	Ratio       string                      `json:"ratio"`
	Resolution  string                      `json:"resolution"`
	Model       string                      `json:"model"`
	Count       int                         `json:"count"`
	InputImages []imageLabInputImagePayload `json:"input_images"`
}

type imageLabInputImagePayload struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	DataURL string `json:"data_url"`
	Size    int64  `json:"size"`
}

type imageLabGenerateOptions struct {
	Client      core.APIClient
	Prompt      string
	Ratio       string
	Resolution  string
	Model       string
	Count       int
	DisplaySize string
	APISize     string
	InputImages []imageLabDecodedImage
}

type imageLabDecodedImage struct {
	Name string
	MIME string
	Data []byte
}

type imageLabClientView struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	AccountGroup string `json:"account_group"`
	Enabled      bool   `json:"enabled"`
}

type imageLabModelView struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
}

type imageLabErrorResponse struct {
	OK      bool   `json:"ok"`
	Type    string `json:"type"`
	Message string `json:"message"`
	Status  int    `json:"status,omitempty"`
}

func (s *Server) handleImageLabPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/images" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	locale := resolveLocale(w, r)
	data := withCSRFData(map[string]any{
		"TitleKey":  "page_title_image_lab",
		"ActiveNav": "images",
		"Locale":    locale,
	}, r)
	s.render(w, "image_lab.html", locale, data)
}

func (s *Server) handleImageLabBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, _ := currentUserFromContext(r.Context())
	clients := s.control.ClientsForUser(user)
	clientViews := make([]imageLabClientView, 0, len(clients))
	modelsByClient := make(map[string][]imageLabModelView, len(clients))
	firstEnabledClientID := ""
	firstEnabledModel := ""
	defaultClientID := ""
	defaultModel := ""
	for i := range clients {
		client := clients[i]
		clientViews = append(clientViews, imageLabClientView{
			ID:           client.ID,
			Name:         strings.TrimSpace(client.Name),
			AccountGroup: strings.TrimSpace(client.AccountGroup),
			Enabled:      client.Enabled,
		})
		models := imageLabModelViews(s.control.ListModelsForClient(r.Context(), &client))
		modelsByClient[client.ID] = models
		if client.Enabled && firstEnabledClientID == "" {
			firstEnabledClientID = client.ID
			firstEnabledModel = imageLabDefaultModel(models)
		}
		if client.Enabled && defaultClientID == "" && len(models) > 0 {
			defaultClientID = client.ID
			defaultModel = imageLabDefaultModel(models)
		}
	}
	if defaultClientID == "" {
		defaultClientID = firstEnabledClientID
		defaultModel = firstEnabledModel
	}
	activeTasks := []imageLabTaskSnapshot(nil)
	if s.imageLabJobs != nil {
		s.imageLabJobs.cleanup(s, time.Now())
		activeTasks = s.imageLabJobs.List(user.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"clients":           clientViews,
		"models_by_client":  modelsByClient,
		"default_client_id": defaultClientID,
		"default_model":     defaultModel,
		"active_tasks":      activeTasks,
		"limits": map[string]int{
			"max_count":        imageLabMaxCount,
			"max_input_images": imageLabMaxInputImages,
			"max_image_bytes":  imageLabMaxImageBytes,
			"max_total_bytes":  imageLabMaxTotalBytes,
		},
	})
}

func (s *Server) handleImageLabGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	options, labErr := s.parseImageLabGenerateOptions(w, r)
	if labErr != nil {
		writeImageLabError(w, labErr)
		return
	}
	if s.imageLabJobs == nil {
		writeImageLabError(w, newImageLabHTTPError(http.StatusInternalServerError, "internal_error", "任务管理器未初始化"))
		return
	}
	user, _ := currentUserFromContext(r.Context())
	job, err := s.imageLabJobs.StartDetached(s, user.ID, options)
	if err != nil {
		writeImageLabError(w, newImageLabHTTPError(http.StatusInternalServerError, "internal_error", "创建生成任务失败"))
		return
	}
	writeJSON(w, http.StatusAccepted, job.snapshotCopy())
}

func (s *Server) handleImageLabJobsList(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/images/api/jobs" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, _ := currentUserFromContext(r.Context())
	activeTasks := []imageLabTaskSnapshot(nil)
	if s.imageLabJobs != nil {
		s.imageLabJobs.cleanup(s, time.Now())
		activeTasks = s.imageLabJobs.List(user.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"active_tasks": activeTasks})
}

func (s *Server) handleImageLabJobActions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodDelete && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.imageLabJobs == nil {
		writeImageLabError(w, newImageLabHTTPError(http.StatusInternalServerError, "internal_error", "任务管理器未初始化"))
		return
	}
	jobPath := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/images/api/jobs/"))
	isCancel := false
	if strings.HasSuffix(jobPath, "/cancel") {
		isCancel = true
		jobPath = strings.TrimSuffix(jobPath, "/cancel")
	}
	if strings.Contains(jobPath, "/results/") {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		s.handleImageLabResultFile(w, r, jobPath)
		return
	}
	jobID := strings.Trim(jobPath, "/")
	if jobID == "" || strings.Contains(jobID, "/") {
		http.NotFound(w, r)
		return
	}
	if isCancel && r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	user, _ := currentUserFromContext(r.Context())
	s.imageLabJobs.cleanup(s, time.Now())
	switch r.Method {
	case http.MethodGet:
		snapshot, ok := s.imageLabJobs.Get(user.ID, jobID)
		if !ok {
			writeJSON(w, http.StatusNotFound, imageLabErrorResponse{OK: false, Type: "not_found", Message: "生成任务不存在", Status: http.StatusNotFound})
			return
		}
		writeJSON(w, http.StatusOK, snapshot)
	case http.MethodDelete:
		snapshot, ok := s.imageLabJobs.Delete(user.ID, jobID)
		if !ok {
			writeJSON(w, http.StatusNotFound, imageLabErrorResponse{OK: false, Type: "not_found", Message: "生成任务不存在或尚未结束", Status: http.StatusNotFound})
			return
		}
		s.publishImageJobUpdated(snapshot)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case http.MethodPost:
		if !isCancel {
			http.NotFound(w, r)
			return
		}
		snapshot, ok := s.imageLabJobs.Cancel(user.ID, jobID)
		if !ok {
			writeJSON(w, http.StatusConflict, imageLabErrorResponse{OK: false, Type: "conflict", Message: "生成任务当前不能停止", Status: http.StatusConflict})
			return
		}
		s.publishImageJobUpdated(snapshot)
		writeJSON(w, http.StatusOK, snapshot)
	}
}

func (s *Server) handleImageLabResultFile(w http.ResponseWriter, r *http.Request, jobPath string) {
	_ = jobPath
	http.NotFound(w, r)
}

func (s *Server) handleImageLabProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	target, err := validateImageLabProxyURL(r.URL.Query().Get("url"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, imageLabErrorResponse{
			OK:      false,
			Type:    "bad_request",
			Message: err.Error(),
			Status:  http.StatusBadRequest,
		})
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, imageLabErrorResponse{
			OK:      false,
			Type:    "bad_request",
			Message: "图片 URL 无效",
			Status:  http.StatusBadRequest,
		})
		return
	}
	req.Header.Set("Accept", "image/avif,image/webp,image/png,image/jpeg,image/*;q=0.9,*/*;q=0.1")
	httpClient := imageLabProxyHTTPClient
	if httpClient == nil {
		httpClient = newImageLabProxyHTTPClient()
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, imageLabErrorResponse{
			OK:      false,
			Type:    gatewayProtocolErrorType,
			Message: "远程图片读取失败",
			Status:  http.StatusBadGateway,
		})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeJSON(w, http.StatusBadGateway, imageLabErrorResponse{
			OK:      false,
			Type:    gatewayProtocolErrorType,
			Message: "远程图片读取失败",
			Status:  http.StatusBadGateway,
		})
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, imageLabProxyMaxBytes+1))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, imageLabErrorResponse{
			OK:      false,
			Type:    gatewayProtocolErrorType,
			Message: "远程图片读取失败",
			Status:  http.StatusBadGateway,
		})
		return
	}
	if int64(len(body)) > imageLabProxyMaxBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, imageLabErrorResponse{
			OK:      false,
			Type:    "bad_request",
			Message: "远程图片超过 20MB",
			Status:  http.StatusRequestEntityTooLarge,
		})
		return
	}
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" || strings.EqualFold(contentType, "application/octet-stream") {
		contentType = http.DetectContentType(body)
	}
	contentType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if !imageLabProxyAllowedContentType(contentType) {
		writeJSON(w, http.StatusBadGateway, imageLabErrorResponse{
			OK:      false,
			Type:    gatewayProtocolErrorType,
			Message: "远程图片读取失败",
			Status:  http.StatusBadGateway,
		})
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("Content-Disposition", "inline")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func validateImageLabProxyURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("图片 URL 不能为空")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("图片 URL 无效")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, fmt.Errorf("图片 URL 仅支持 http 或 https")
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("图片 URL 不能包含认证信息")
	}
	if imageLabProxyBlockedHost(parsed.Hostname()) {
		return nil, fmt.Errorf("图片 URL 指向的主机不允许代理")
	}
	return parsed, nil
}

func imageLabProxyAllowedContentType(contentType string) bool {
	switch strings.ToLower(strings.TrimSpace(contentType)) {
	case "image/png", "image/jpeg", "image/jpg", "image/webp", "image/gif", "image/avif":
		return true
	default:
		return false
	}
}

func imageLabProxyDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if imageLabProxyBlockedHost(host) {
		return nil, fmt.Errorf("图片 URL 指向的主机不允许代理")
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("图片 URL 主机解析失败")
	}
	for _, ipAddr := range ips {
		if imageLabProxyBlockedIP(ipAddr.IP) {
			return nil, fmt.Errorf("图片 URL 指向的主机不允许代理")
		}
	}
	dialer := net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	var lastErr error
	for _, ipAddr := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ipAddr.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func imageLabProxyBlockedHost(host string) bool {
	host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return imageLabProxyBlockedIP(ip)
}

func imageLabProxyBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1]&0xc0 == 64 {
		return true
	}
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}

func (s *Server) parseImageLabGenerateOptions(w http.ResponseWriter, r *http.Request) (imageLabGenerateOptions, *imageLabHTTPError) {
	var payload imageLabGeneratePayload
	if err := decodeStrictJSONBody(w, r, imageLabRequestBodySize, &payload); err != nil {
		return imageLabGenerateOptions{}, newImageLabHTTPError(http.StatusBadRequest, "bad_request", "请求格式无效")
	}
	user, _ := currentUserFromContext(r.Context())
	client, clientErr := s.imageLabClientForUser(user, payload.ClientID)
	if clientErr != nil {
		return imageLabGenerateOptions{}, clientErr
	}
	if !client.Enabled {
		return imageLabGenerateOptions{}, newImageLabHTTPError(http.StatusForbidden, "forbidden", "当前 API 密钥已禁用")
	}

	models := imageLabModelViews(s.control.ListModelsForClient(r.Context(), &client))
	model := strings.TrimSpace(payload.Model)
	if model == "" {
		model = imageLabDefaultModel(models)
	}
	if model == "" {
		return imageLabGenerateOptions{}, newImageLabHTTPError(http.StatusBadRequest, "invalid_config", "当前 API 密钥没有可用模型")
	}
	if !imageLabModelVisible(models, model) {
		return imageLabGenerateOptions{}, newImageLabHTTPError(http.StatusForbidden, "forbidden", "模型对当前 API 密钥不可见或已禁用")
	}

	prompt := strings.TrimSpace(payload.Prompt)
	if prompt == "" {
		return imageLabGenerateOptions{}, newImageLabHTTPError(http.StatusBadRequest, "bad_request", "提示词不能为空")
	}
	if len([]rune(prompt)) > imageLabMaxPromptRunes {
		return imageLabGenerateOptions{}, newImageLabHTTPError(http.StatusBadRequest, "bad_request", fmt.Sprintf("提示词不能超过 %d 字", imageLabMaxPromptRunes))
	}
	count := payload.Count
	if count <= 0 {
		count = 1
	}
	if count < 1 || count > imageLabMaxCount {
		return imageLabGenerateOptions{}, newImageLabHTTPError(http.StatusBadRequest, "bad_request", fmt.Sprintf("生成张数必须在 1-%d 之间", imageLabMaxCount))
	}
	ratio, resolution, size, apiSize, sizeErr := normalizeImageLabSize(payload.Ratio, payload.Resolution)
	if sizeErr != nil {
		return imageLabGenerateOptions{}, newImageLabHTTPError(http.StatusBadRequest, "bad_request", sizeErr.Error())
	}
	images, decodeErr := decodeImageLabInputImages(payload.InputImages)
	if decodeErr != nil {
		return imageLabGenerateOptions{}, newImageLabHTTPError(http.StatusBadRequest, "bad_request", decodeErr.Error())
	}
	if s.gateway == nil {
		return imageLabGenerateOptions{}, newImageLabHTTPError(http.StatusInternalServerError, "invalid_config", "网关服务未初始化")
	}

	return imageLabGenerateOptions{
		Client:      client,
		Prompt:      prompt,
		Ratio:       ratio,
		Resolution:  resolution,
		Model:       model,
		Count:       count,
		DisplaySize: size,
		APISize:     apiSize,
		InputImages: images,
	}, nil
}

func (s *Server) imageLabClientForUser(user core.User, clientID string) (core.APIClient, *imageLabHTTPError) {
	clientID = strings.TrimSpace(clientID)
	if clientID != "" {
		client, err := s.control.GetClient(clientID)
		if err == nil && s.control.CanUserManageClient(user, client) {
			return client, nil
		}
		return core.APIClient{}, newImageLabHTTPError(http.StatusForbidden, "forbidden", "无权使用该 API 密钥")
	}
	var firstEnabled *core.APIClient
	for _, client := range s.control.ClientsForUser(user) {
		if client.Enabled && firstEnabled == nil {
			copied := client
			firstEnabled = &copied
		}
	}
	if firstEnabled != nil {
		return *firstEnabled, nil
	}
	return core.APIClient{}, newImageLabHTTPError(http.StatusBadRequest, "invalid_config", "请先创建并启用一个 API 密钥")
}

func buildImageLabGenerationBody(options imageLabGenerateOptions) (map[string]json.RawMessage, []byte, error) {
	payload := map[string]any{
		"model":           options.Model,
		"prompt":          options.Prompt,
		"n":               1,
		"response_format": "b64_json",
	}
	if strings.TrimSpace(options.APISize) != "" {
		payload["size"] = options.APISize
	}
	rawBody, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	extra := make(map[string]json.RawMessage, len(payload))
	if err := json.Unmarshal(rawBody, &extra); err != nil {
		return nil, nil, err
	}
	return extra, rawBody, nil
}

func buildImageLabEditMultipart(options imageLabGenerateOptions) ([]byte, string, map[string]string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	fields := map[string]string{
		"model":           options.Model,
		"prompt":          options.Prompt,
		"n":               "1",
		"response_format": "b64_json",
	}
	if strings.TrimSpace(options.APISize) != "" {
		fields["size"] = options.APISize
	}
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			_ = writer.Close()
			return nil, "", nil, err
		}
	}
	for index, image := range options.InputImages {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{
			"name":     "image",
			"filename": imageLabInputFilename(image.Name, image.MIME, index),
		}))
		header.Set("Content-Type", image.MIME)
		part, err := writer.CreatePart(header)
		if err != nil {
			_ = writer.Close()
			return nil, "", nil, err
		}
		if _, err := part.Write(image.Data); err != nil {
			_ = writer.Close()
			return nil, "", nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", nil, err
	}
	return body.Bytes(), writer.FormDataContentType(), fields, nil
}

func (s *Server) runImageLabItem(ctx context.Context, options imageLabGenerateOptions, index int) imageLabResultEvent {
	started := time.Now()
	itemCtx, cancel := context.WithTimeout(ctx, imageLabTaskTimeout)
	defer cancel()

	var result imageLabResultEvent
	if len(options.InputImages) > 0 {
		result = s.generateImageLabEdit(itemCtx, options, index)
	} else {
		result = s.generateImageLabText(itemCtx, options, index)
	}
	result.ElapsedMS = time.Since(started).Milliseconds()
	return result
}

func (s *Server) generateImageLabText(ctx context.Context, options imageLabGenerateOptions, index int) imageLabResultEvent {
	extra, rawBody, err := buildImageLabGenerationBody(options)
	if err != nil {
		return imageLabResultEvent{
			Index:  index,
			OK:     false,
			Error:  "生成请求构造失败",
			Status: http.StatusBadRequest,
		}
	}
	resp, err := s.gateway.GenerateImage(ctx, &core.ImageGenerationRequest{
		Model:    options.Model,
		Prompt:   options.Prompt,
		Client:   &options.Client,
		Extra:    extra,
		RawBody:  json.RawMessage(rawBody),
		Metadata: imageLabRequestMetadata(options, index, "generation"),
	})
	if err != nil {
		status, message := imageLabGatewayError(err)
		return imageLabResultEvent{Index: index, OK: false, Status: status, Error: message}
	}
	return imageLabResultFromImageBody(index, resp.Body)
}

func (s *Server) generateImageLabEdit(ctx context.Context, options imageLabGenerateOptions, index int) imageLabResultEvent {
	body, contentType, formFields, err := buildImageLabEditMultipart(options)
	if err != nil {
		return imageLabResultEvent{
			Index:  index,
			OK:     false,
			Error:  "生成请求构造失败",
			Status: http.StatusBadRequest,
		}
	}
	resp, err := s.gateway.ProcessImageMultipart(ctx, &core.ImageMultipartRequest{
		Model:       options.Model,
		Endpoint:    "/v1/images/edits",
		ContentType: contentType,
		Body:        body,
		FormFields:  formFields,
		Client:      &options.Client,
		Metadata:    imageLabRequestMetadata(options, index, "edit"),
	})
	if err != nil {
		status, message := imageLabGatewayError(err)
		return imageLabResultEvent{Index: index, OK: false, Status: status, Error: message}
	}
	return imageLabResultFromImageBody(index, resp.Body)
}

func imageLabRequestMetadata(options imageLabGenerateOptions, index int, kind string) map[string]string {
	return map[string]string{
		"surface":         "console_image_lab",
		"image_kind":      strings.TrimSpace(kind),
		"image_prompt":    strings.TrimSpace(options.Prompt),
		"image_ratio":     strings.TrimSpace(options.Ratio),
		"image_size":      strings.TrimSpace(options.APISize),
		"image_display":   strings.TrimSpace(options.DisplaySize),
		"image_count":     strconv.Itoa(options.Count),
		"image_index":     strconv.Itoa(index + 1),
		"image_has_input": strconv.FormatBool(len(options.InputImages) > 0),
	}
}

func imageLabResultFromImageBody(index int, body []byte) imageLabResultEvent {
	result := imageLabResultEvent{Index: index}
	var payload struct {
		Data []struct {
			B64JSON       string `json:"b64_json"`
			URL           string `json:"url"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		result.OK = false
		result.Status = http.StatusBadGateway
		result.Error = gatewayProtocolErrorMessage
		return result
	}
	if len(payload.Data) == 0 {
		result.OK = false
		result.Status = http.StatusBadGateway
		result.Error = gatewayProtocolErrorMessage
		return result
	}
	item := payload.Data[0]
	result.Text = strings.TrimSpace(item.RevisedPrompt)
	if value := strings.TrimSpace(item.B64JSON); value != "" {
		result.Image = imageLabResultDataURL(value)
		result.MIME = imageLabDataURLMIME(result.Image)
		if result.MIME == "" {
			result.MIME = "image/png"
		}
		result.OK = true
		return result
	}
	if value := strings.TrimSpace(item.URL); value != "" {
		result.Image = value
		if strings.HasPrefix(strings.ToLower(value), "data:") {
			result.MIME = imageLabDataURLMIME(value)
			if result.MIME == "" {
				result.MIME = "image/png"
			}
		} else {
			result.RemoteURL = value
		}
		result.OK = true
		return result
	}
	result.OK = false
	result.Status = http.StatusBadGateway
	result.Error = gatewayProtocolErrorMessage
	return result
}

func imageLabResultDataURL(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "data:") {
		return value
	}
	return "data:image/png;base64," + value
}

func imageLabDataURLMIME(dataURL string) string {
	dataURL = strings.TrimSpace(dataURL)
	if !strings.HasPrefix(strings.ToLower(dataURL), "data:") {
		return ""
	}
	comma := strings.IndexByte(dataURL, ',')
	if comma < 0 {
		return ""
	}
	meta := dataURL[5:comma]
	return strings.TrimSpace(strings.Split(meta, ";")[0])
}

func imageLabInputFilename(name, mimeType string, index int) string {
	name = strings.TrimSpace(name)
	replacer := strings.NewReplacer("\\", "_", "/", "_", "\r", "_", "\n", "_", "\t", "_")
	name = replacer.Replace(name)
	if name == "" {
		name = fmt.Sprintf("image_%d", index+1)
	}
	if !strings.Contains(name, ".") {
		name += imageLabExtensionForMIME(mimeType)
	}
	return name
}

func imageLabExtensionForMIME(mimeType string) string {
	switch normalizeImageLabMIME(mimeType) {
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

func imageLabModelViews(models []core.ModelSpec) []imageLabModelView {
	out := make([]imageLabModelView, 0, len(models))
	for _, model := range models {
		name := strings.TrimSpace(model.Name)
		if name == "" {
			continue
		}
		if core.NormalizeModelType(model.Type, name) != core.ModelTypeImage {
			continue
		}
		out = append(out, imageLabModelView{
			ID:       name,
			Provider: string(model.Provider),
		})
	}
	return out
}

func imageLabDefaultModel(models []imageLabModelView) string {
	if len(models) == 0 {
		return ""
	}
	for _, preferred := range []string{"gpt-image-2"} {
		for _, model := range models {
			if strings.EqualFold(strings.TrimSpace(model.ID), preferred) {
				return model.ID
			}
		}
	}
	return models[0].ID
}

func imageLabModelVisible(models []imageLabModelView, model string) bool {
	for _, candidate := range models {
		if candidate.ID == model {
			return true
		}
	}
	return false
}

var imageLabSizes = map[string]map[string]string{
	"standard": {
		"1:1":  "1024x1024",
		"2:3":  "1024x1536",
		"3:2":  "1536x1024",
		"3:4":  "768x1024",
		"4:3":  "1024x768",
		"9:16": "1008x1792",
		"16:9": "1792x1008",
	},
	"2k": {
		"1:1":  "2048x2048",
		"2:3":  "1344x2016",
		"3:2":  "2016x1344",
		"3:4":  "1536x2048",
		"4:3":  "2048x1536",
		"9:16": "1152x2048",
		"16:9": "2048x1152",
	},
	"4k": {
		"1:1":  "2880x2880",
		"2:3":  "2336x3504",
		"3:2":  "3504x2336",
		"3:4":  "2448x3264",
		"4:3":  "3264x2448",
		"9:16": "2160x3840",
		"16:9": "3840x2160",
	},
}

func normalizeImageLabSize(ratio, resolution string) (string, string, string, string, error) {
	ratio = strings.TrimSpace(ratio)
	resolution = strings.ToLower(strings.TrimSpace(resolution))
	if ratio == "" {
		ratio = "auto"
	}
	if resolution == "" {
		resolution = "standard"
	}
	if resolution != "auto" && resolution != "standard" && resolution != "2k" && resolution != "4k" {
		return "", "", "", "", fmt.Errorf("分辨率无效")
	}
	if ratio == "auto" {
		if resolution != "auto" {
			ratio = "1:1"
		} else {
			return "auto", resolution, "自动", "", nil
		}
	}
	tier := resolution
	if tier == "auto" {
		tier = "standard"
	}
	size := imageLabSizes[tier][ratio]
	if size == "" {
		return "", "", "", "", fmt.Errorf("画幅比例无效")
	}
	return ratio, resolution, size, size, nil
}

func decodeImageLabInputImages(images []imageLabInputImagePayload) ([]imageLabDecodedImage, error) {
	if len(images) == 0 {
		return nil, nil
	}
	if len(images) > imageLabMaxInputImages {
		return nil, fmt.Errorf("参考图不能超过 %d 张", imageLabMaxInputImages)
	}
	out := make([]imageLabDecodedImage, 0, len(images))
	total := int64(0)
	for index, image := range images {
		data, mimeType, err := decodeImageLabDataURL(image.DataURL)
		if err != nil {
			return nil, fmt.Errorf("第 %d 张参考图无效：%w", index+1, err)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("第 %d 张参考图为空", index+1)
		}
		if int64(len(data)) > imageLabMaxImageBytes {
			return nil, fmt.Errorf("第 %d 张参考图超过 12MB", index+1)
		}
		total += int64(len(data))
		if total > imageLabMaxTotalBytes {
			return nil, fmt.Errorf("参考图总大小不能超过 50MB")
		}
		out = append(out, imageLabDecodedImage{
			Name: strings.TrimSpace(image.Name),
			MIME: mimeType,
			Data: data,
		})
	}
	return out, nil
}

func decodeImageLabDataURL(dataURL string) ([]byte, string, error) {
	dataURL = strings.TrimSpace(dataURL)
	if !strings.HasPrefix(strings.ToLower(dataURL), "data:") {
		return nil, "", fmt.Errorf("必须是 data URL")
	}
	comma := strings.IndexByte(dataURL, ',')
	if comma < 0 {
		return nil, "", fmt.Errorf("缺少图片数据")
	}
	meta := dataURL[5:comma]
	if !strings.Contains(strings.ToLower(meta), ";base64") {
		return nil, "", fmt.Errorf("仅支持 base64 图片")
	}
	mimeType := strings.TrimSpace(strings.Split(meta, ";")[0])
	raw := strings.TrimSpace(dataURL[comma+1:])
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, "", fmt.Errorf("base64 解码失败")
	}
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = http.DetectContentType(data)
	}
	mimeType = normalizeImageLabMIME(mimeType)
	if mimeType == "" {
		return nil, "", fmt.Errorf("仅支持 PNG、JPEG 或 WebP")
	}
	return data, mimeType, nil
}

func normalizeImageLabMIME(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/png":
		return "image/png"
	case "image/jpeg", "image/jpg":
		return "image/jpeg"
	case "image/webp":
		return "image/webp"
	default:
		return ""
	}
}

type imageLabHTTPError struct {
	status  int
	code    string
	message string
}

func newImageLabHTTPError(status int, code, message string) *imageLabHTTPError {
	return &imageLabHTTPError{status: status, code: code, message: strings.TrimSpace(message)}
}

func writeImageLabError(w http.ResponseWriter, err *imageLabHTTPError) {
	if err == nil {
		err = newImageLabHTTPError(http.StatusInternalServerError, "internal_error", "请求失败")
	}
	writeJSON(w, err.status, imageLabErrorResponse{
		OK:      false,
		Type:    err.code,
		Message: err.message,
		Status:  err.status,
	})
}

func imageLabGatewayError(err error) (int, string) {
	if err == nil {
		return http.StatusInternalServerError, "请求失败"
	}
	if errors.Is(err, context.Canceled) {
		return 499, "请求已取消"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, "请求超时：生图通常需要 100-300 秒，请稍后重试"
	}
	if errors.Is(err, gateway.ErrModelUnavailable) {
		return http.StatusBadRequest, err.Error()
	}
	var concurrencyErr *gateway.ConcurrencyLimitError
	if errors.As(err, &concurrencyErr) && concurrencyErr != nil {
		return concurrencyErr.StatusCode, concurrencyErr.Error()
	}
	var billingErr *gateway.BillingError
	if errors.As(err, &billingErr) && billingErr != nil {
		return billingErr.StatusCode, billingErr.Error()
	}
	var accessErr *controlplane.AccessError
	if errors.As(err, &accessErr) && accessErr != nil {
		return accessErr.StatusCode, accessErr.Message
	}
	var executionErr *failover.ExecutionError
	if errors.As(err, &executionErr) && executionErr != nil {
		return imageLabExecutionErrorStatusMessage(executionErr)
	}
	return http.StatusBadGateway, gatewayProtocolErrorMessage
}

func imageLabExecutionErrorStatusMessage(err *failover.ExecutionError) (int, string) {
	if err == nil {
		return http.StatusBadGateway, gatewayProtocolErrorMessage
	}
	bestPriority := -1
	bestStatus := 0
	bestMessage := ""
	for index := len(err.Attempts) - 1; index >= 0; index-- {
		attempt := err.Attempts[index]
		status, message, ok := imageLabAttemptStatusMessage(attempt)
		if !ok {
			continue
		}
		priority := imageLabAttemptMessagePriority(attempt)
		if priority > bestPriority {
			bestPriority = priority
			bestStatus = status
			bestMessage = message
		}
	}
	if bestMessage != "" {
		return bestStatus, bestMessage
	}
	return http.StatusBadGateway, gatewayProtocolErrorMessage
}

func imageLabAttemptStatusMessage(attempt core.AttemptRecord) (int, string, bool) {
	code := strings.TrimSpace(attempt.ErrorCode)
	detail := imageLabAttemptDetail(attempt)
	normalizedDetail := strings.ToLower(detail)
	if strings.Contains(normalizedDetail, "context deadline exceeded") ||
		strings.Contains(normalizedDetail, "client.timeout") ||
		strings.Contains(normalizedDetail, "timeout") {
		return http.StatusGatewayTimeout, "请求超时：生图通常需要 100-300 秒，请稍后重试", true
	}
	if strings.Contains(normalizedDetail, "524") || strings.Contains(normalizedDetail, "cloudflare") {
		return http.StatusGatewayTimeout, gatewayProtocolErrorMessage, true
	}
	if strings.Contains(normalizedDetail, "413") || strings.Contains(normalizedDetail, "too large") || strings.Contains(normalizedDetail, "request entity too large") {
		return http.StatusRequestEntityTooLarge, "图片太大，请压缩图片、减少参考图或降低分辨率后重试", true
	}

	switch code {
	case providers.ErrorCodeImageGenerationRejected:
		return http.StatusBadRequest, "图片请求未通过策略检查，请调整提示词或参考图后重试", true
	case providers.ErrorCodeImageGenerationNotStarted:
		return http.StatusBadRequest, "图片生成未启动，请调整提示词或参考图后重试", true
	case providers.ErrorCodeImageGenerationFailed:
		return http.StatusBadRequest, "图片生成失败，请调整提示词或参考图后重试", true
	case providers.ErrorCodeUpstreamAuthError, providers.ErrorCodeCredentialExpired, providers.ErrorCodeMissingCredential:
		return http.StatusBadGateway, gatewayProtocolErrorMessage, true
	case providers.ErrorCodeGatewayAPIKeyDisabled:
		return http.StatusBadGateway, gatewayProtocolErrorMessage, true
	case providers.ErrorCodeUpstreamForbidden:
		return http.StatusBadGateway, gatewayProtocolErrorMessage, true
	case providers.ErrorCodeImageBackendRequiresOAuth:
		return http.StatusBadGateway, gatewayProtocolErrorMessage, true
	case providers.ErrorCodeUpstreamRateLimited:
		return http.StatusTooManyRequests, gatewayProtocolErrorMessage, true
	case providers.ErrorCodeUpstreamServerError:
		return http.StatusBadGateway, gatewayProtocolErrorMessage, true
	case providers.ErrorCodeUpstreamTemporarilyUnavailable:
		return http.StatusBadGateway, gatewayProtocolErrorMessage, true
	case providers.ErrorCodeUpstreamRejected:
		return http.StatusBadRequest, "图片请求未通过策略检查，请调整提示词或参考图后重试", true
	case providers.ErrorCodeUpstreamNotFound:
		return http.StatusBadGateway, gatewayProtocolErrorMessage, true
	case providers.ErrorCodeUpstreamTransportError, providers.ErrorCodeUpstreamReadError:
		return http.StatusBadGateway, gatewayProtocolErrorMessage, true
	default:
		if strings.TrimSpace(detail) != "" {
			return http.StatusBadGateway, gatewayProtocolErrorMessage, true
		}
		return 0, "", false
	}
}

func imageLabAttemptMessagePriority(attempt core.AttemptRecord) int {
	code := strings.TrimSpace(attempt.ErrorCode)
	switch code {
	case providers.ErrorCodeImageGenerationRejected, providers.ErrorCodeImageGenerationFailed, providers.ErrorCodeImageGenerationNotStarted, providers.ErrorCodeUpstreamRejected:
		return 100
	case providers.ErrorCodeUpstreamAuthError, providers.ErrorCodeCredentialExpired, providers.ErrorCodeMissingCredential, providers.ErrorCodeUpstreamForbidden, providers.ErrorCodeImageBackendRequiresOAuth, providers.ErrorCodeUpstreamNotFound:
		return 80
	case providers.ErrorCodeGatewayAPIKeyDisabled:
		return 70
	case providers.ErrorCodeUpstreamRateLimited:
		return 70
	case providers.ErrorCodeUpstreamServerError, providers.ErrorCodeUpstreamTemporarilyUnavailable:
		return 60
	case providers.ErrorCodeUpstreamTransportError, providers.ErrorCodeUpstreamReadError:
		return 20
	default:
		if strings.TrimSpace(imageLabAttemptDetail(attempt)) != "" {
			return 50
		}
		return 0
	}
}

func imageLabAttemptDetail(attempt core.AttemptRecord) string {
	_ = attempt
	return ""
}
