package web

import (
	"errors"
	"net/http"

	"github.com/32ns/ai-gateway/internal/storage"
)

func writeProtocolNotFound(w http.ResponseWriter, err error, fallback string) {
	if errors.Is(err, storage.ErrNotFound) {
		writeProtocolError(w, http.StatusNotFound, fallback)
		return
	}
	writeProtocolError(w, http.StatusBadRequest, err.Error())
}

func writeProtocolError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"type":    "invalid_request_error",
			"message": message,
		},
	})
}
