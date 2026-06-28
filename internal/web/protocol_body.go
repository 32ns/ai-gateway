package web

import (
	"io"
	"net/http"
)

func (s *Server) protocolBodyLimit() int64 {
	if s == nil || s.protocolRequestBodyLimit <= 0 {
		return defaultProtocolRequestBodyLimit
	}
	return s.protocolRequestBodyLimit
}

func (s *Server) readProtocolBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(w, r.Body, s.protocolBodyLimit()))
}
