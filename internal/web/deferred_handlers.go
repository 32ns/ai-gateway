package web

import (
	"net/http"
	"strings"
)

const ajaxPartialHeader = "X-Ajax-Partial"

func deferredPartialURL(r *http.Request, partial string) string {
	values := r.URL.Query()
	values.Set("partial", partial)
	query := values.Encode()
	if query == "" {
		return r.URL.Path
	}
	return r.URL.Path + "?" + query
}

func deferredPartialRequested(r *http.Request, partial string) bool {
	partial = strings.TrimSpace(partial)
	if partial == "" || r == nil {
		return false
	}
	requested := strings.TrimSpace(r.URL.Query().Get("partial"))
	if requested != "" {
		return requested == partial
	}
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Requested-With")), "fetch") {
		return false
	}
	return strings.TrimSpace(r.Header.Get(ajaxPartialHeader)) == partial
}
