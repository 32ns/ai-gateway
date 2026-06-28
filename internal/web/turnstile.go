package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	turnstileVerifyEndpoint = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	turnstileHTTPClient     = &http.Client{Timeout: 5 * time.Second}
)

type turnstileVerifyResponse struct {
	Success    bool     `json:"success"`
	ErrorCodes []string `json:"error-codes"`
}

func verifyTurnstile(ctx context.Context, secret, token, remoteIP string) error {
	secret = strings.TrimSpace(secret)
	token = strings.TrimSpace(token)
	if secret == "" {
		return fmt.Errorf("turnstile secret key is not configured")
	}
	if token == "" {
		return fmt.Errorf("turnstile response is required")
	}
	values := url.Values{}
	values.Set("secret", secret)
	values.Set("response", token)
	if remoteIP = strings.TrimSpace(remoteIP); remoteIP != "" {
		values.Set("remoteip", remoteIP)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, turnstileVerifyEndpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := turnstileHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("turnstile verification returned status %d", resp.StatusCode)
	}
	var payload turnstileVerifyResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return err
	}
	if !payload.Success {
		if len(payload.ErrorCodes) > 0 {
			return fmt.Errorf("turnstile verification failed: %s", strings.Join(payload.ErrorCodes, ", "))
		}
		return fmt.Errorf("turnstile verification failed")
	}
	return nil
}
