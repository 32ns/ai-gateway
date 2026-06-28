package web

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"strings"
)

func flattenAnthropicContent(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := block["text"].(string); ok {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func flattenOpenAIMessageContent(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := block["text"].(string); ok && strings.TrimSpace(text) != "" {
				parts = append(parts, text)
				continue
			}
			if nested, ok := block["content"].(string); ok && strings.TrimSpace(nested) != "" {
				parts = append(parts, nested)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func anthropicStopReason(reason string) string {
	switch strings.TrimSpace(reason) {
	case "", "completed", "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	default:
		return strings.TrimSpace(reason)
	}
}

func multipartFormFieldValues(body []byte, contentType string, names ...string) (map[string]string, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		return nil, fmt.Errorf("content type must be multipart/form-data")
	}
	boundary := strings.TrimSpace(params["boundary"])
	if boundary == "" {
		return nil, fmt.Errorf("multipart boundary is required")
	}

	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		if strings.TrimSpace(name) != "" {
			wanted[name] = struct{}{}
		}
	}
	values := make(map[string]string, len(wanted))
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return values, nil
		}
		if err != nil {
			return nil, err
		}
		name := part.FormName()
		if _, ok := wanted[name]; !ok {
			continue
		}
		if _, exists := values[name]; exists {
			continue
		}
		value, err := io.ReadAll(io.LimitReader(part, 4096))
		if err != nil {
			return nil, err
		}
		values[name] = strings.TrimSpace(string(value))
	}
}
