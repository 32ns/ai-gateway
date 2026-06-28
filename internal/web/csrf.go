package web

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

const (
	consoleMultipartFormBodyLimit   = 128 << 20
	consoleMultipartFormMemoryLimit = 8 << 20
	consoleURLEncodedFormBodyLimit  = 2 << 20
)

func (s *Server) ensureConsoleCSRFToken(w http.ResponseWriter, r *http.Request) (string, error) {
	if cookie, err := r.Cookie(consoleCSRFCookieName); err == nil {
		token := strings.TrimSpace(cookie.Value)
		if token != "" {
			return token, nil
		}
	}

	token, err := generateConsoleCSRFToken()
	if err != nil {
		return "", fmt.Errorf("generate csrf token: %w", err)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     consoleCSRFCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   requestIsHTTPS(r),
	})
	return token, nil
}

func readConsoleCSRFToken(w http.ResponseWriter, r *http.Request) (string, error) {
	multipartRequest := isMultipartFormRequest(r)
	if multipartRequest {
		r.Body = http.MaxBytesReader(w, r.Body, consoleMultipartFormBodyLimit)
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, consoleURLEncodedFormBodyLimit)
	}
	token := strings.TrimSpace(r.Header.Get("X-CSRF-Token"))
	if token != "" {
		return token, nil
	}
	if multipartRequest {
		if err := r.ParseMultipartForm(consoleMultipartFormMemoryLimit); err != nil {
			return "", err
		}
		if r.MultipartForm == nil {
			return "", nil
		}
		return firstCSRFFormValue(r.MultipartForm.Value["csrf_token"]), nil
	}
	if err := r.ParseForm(); err != nil {
		return "", err
	}
	return strings.TrimSpace(r.PostForm.Get("csrf_token")), nil
}

func isMultipartFormRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))), "multipart/form-data")
}

func cleanupMultipartForm(r *http.Request) {
	if r != nil && r.MultipartForm != nil {
		_ = r.MultipartForm.RemoveAll()
	}
}

func firstCSRFFormValue(values []string) string {
	for _, value := range values {
		if token := strings.TrimSpace(value); token != "" {
			return token
		}
	}
	return ""
}

func generateConsoleCSRFToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func isStateChangingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}
