package gateway

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"

	"github.com/hishamkaram/claude-code-router/internal/session"
)

const (
	observerTokenHeader = "X-CCR-Observer-Token"
	maxHookBodyBytes    = 64 << 10
)

func (h *handler) handleRuntimeRequest(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Tracker == nil {
		writeRuntimeError(w, http.StatusNotFound, "runtime observation is unavailable")
		return
	}
	if !requestIsLoopback(r) {
		writeRuntimeError(w, http.StatusForbidden, "runtime endpoint is loopback-only")
		return
	}
	if !h.observerAuthorized(r) {
		writeRuntimeError(w, http.StatusUnauthorized, "invalid observer token")
		return
	}
	switch r.URL.Path {
	case "/internal/v1/status":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeRuntimeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		writeJSON(w, http.StatusOK, h.cfg.Tracker.Snapshot())
	case "/internal/v1/hooks":
		h.handleHookRequest(w, r)
	default:
		writeRuntimeError(w, http.StatusNotFound, "runtime endpoint not found")
	}
}

func (h *handler) handleHookRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeRuntimeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		writeRuntimeError(w, http.StatusUnsupportedMediaType, "content type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxHookBodyBytes)
	decoder := json.NewDecoder(r.Body)
	var event session.HookEvent
	if err := decoder.Decode(&event); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeRuntimeError(w, http.StatusRequestEntityTooLarge, "hook body exceeds 64 KiB")
			return
		}
		writeRuntimeError(w, http.StatusBadRequest, "invalid hook body")
		return
	}
	if err := ensureJSONEOF(decoder); err != nil {
		writeRuntimeError(w, http.StatusBadRequest, "invalid hook body")
		return
	}
	if err := h.cfg.Tracker.HandleHook(r.Context(), event); err != nil {
		if errors.Is(err, session.ErrInvalidHook) {
			writeRuntimeError(w, http.StatusBadRequest, "invalid lifecycle hook")
			return
		}
		writeRuntimeError(w, http.StatusServiceUnavailable, "lifecycle persistence unavailable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) observerAuthorized(r *http.Request) bool {
	expected := strings.TrimSpace(h.cfg.ObserverToken)
	if expected == "" {
		return false
	}
	provided := strings.TrimSpace(r.Header.Get(observerTokenHeader))
	if provided == "" {
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if len(auth) > len("Bearer ") && strings.EqualFold(auth[:len("Bearer ")], "Bearer ") {
			provided = strings.TrimSpace(auth[len("Bearer "):])
		}
	}
	expectedHash := sha256.Sum256([]byte(expected))
	providedHash := sha256.Sum256([]byte(provided))
	return subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) == 1
}

func requestIsLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("unexpected trailing JSON value")
}

func writeRuntimeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"schema_version": 1,
		"error":          message,
	})
}
