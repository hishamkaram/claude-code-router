package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/cua"
)

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := s.validateHTTPBoundary(r); err != nil {
		writeApprovalError(w, err.status, err.message)
		return
	}
	switch r.URL.Path {
	case "/approve":
		s.handleApprovalPage(w, r)
	case "/api/request":
		s.handleRequestAPI(w, r)
	case "/api/decision":
		s.handleDecisionAPI(w, r)
	default:
		writeApprovalError(w, http.StatusNotFound, "approval endpoint not found")
	}
}

func (s *Server) handleApprovalPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeApprovalError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	pending, ok := s.lookupPendingFromRequest(w, r)
	if !ok {
		return
	}
	writeSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>CCR computer-use approval</title>
</head>
<body>
<main>
<h1>Computer-use approval</h1>
<dl>
<dt>Action</dt><dd>%s</dd>
<dt>Risk</dt><dd>%s</dd>
<dt>Executor</dt><dd>%s</dd>
<dt>Expires</dt><dd>%s</dd>
</dl>
<form method="post" action="/api/decision">
<input type="hidden" name="request_id" value="%s">
<input type="hidden" name="token" value="%s">
<button type="submit" name="decision" value="approve">Approve</button>
<button type="submit" name="decision" value="deny">Deny</button>
</form>
</main>
</body>
</html>`,
		html.EscapeString(string(pending.request.Kind)),
		html.EscapeString(string(pending.request.Risk)),
		html.EscapeString(pending.request.Executor),
		html.EscapeString(pending.expiresAt.UTC().Format(time.RFC3339)),
		html.EscapeString(pending.id),
		html.EscapeString(pending.token),
	)
}

func (s *Server) handleRequestAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeApprovalError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	pending, ok := s.lookupPendingFromRequest(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version": 1,
		"action_id":      pending.request.ActionID,
		"kind":           pending.request.Kind,
		"risk":           pending.request.Risk,
		"executor":       pending.request.Executor,
		"expires_at":     pending.expiresAt.UTC().Format(time.RFC3339Nano),
	})
}

func (s *Server) handleDecisionAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeApprovalError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	payload, err := decodeDecisionPayload(w, r)
	if err != nil {
		writeApprovalError(w, http.StatusBadRequest, "invalid decision request")
		return
	}
	decision, err := normalizeDecision(payload.Decision)
	if err != nil {
		writeApprovalError(w, http.StatusBadRequest, "invalid decision")
		return
	}
	pending, ok := s.lookupPending(payload.RequestID, payload.Token)
	if !ok {
		writeApprovalError(w, http.StatusUnauthorized, "invalid approval token")
		return
	}
	s.removePending(pending.id)
	pending.complete(decisionResult{decision: decision})
	if acceptsHTML(r) {
		writeSecurityHeaders(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<!doctype html><title>Decision recorded</title><p>Decision recorded.</p>")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version": 1,
		"decision":       decision,
	})
}

func (s *Server) lookupPendingFromRequest(w http.ResponseWriter, r *http.Request) (*pendingApproval, bool) {
	pending, ok := s.lookupPending(r.URL.Query().Get("request_id"), r.URL.Query().Get("token"))
	if !ok {
		writeApprovalError(w, http.StatusUnauthorized, "invalid approval token")
		return nil, false
	}
	return pending, true
}

func (s *Server) lookupPending(id, token string) (*pendingApproval, bool) {
	id = strings.TrimSpace(id)
	token = strings.TrimSpace(token)
	if id == "" || token == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, ok := s.pending[id]
	if !ok {
		return nil, false
	}
	if !sameSecret(pending.token, token) {
		return nil, false
	}
	if !s.now().Before(pending.expiresAt) {
		delete(s.pending, id)
		pending.complete(decisionResult{
			decision: cua.DecisionDeny,
			err:      fmt.Errorf("approval.Server.Approve: approval token expired: %w", context.DeadlineExceeded),
		})
		return nil, false
	}
	return pending, true
}

type boundaryError struct {
	status  int
	message string
}

func (s *Server) validateHTTPBoundary(r *http.Request) *boundaryError {
	if !requestIsLoopback(r) {
		return &boundaryError{status: http.StatusForbidden, message: "approval endpoint is loopback-only"}
	}
	if strings.TrimSpace(r.Host) != s.host {
		return &boundaryError{status: http.StatusForbidden, message: "invalid approval host"}
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin != "" && origin != s.origin {
		return &boundaryError{status: http.StatusForbidden, message: "invalid approval origin"}
	}
	if methodRequiresOrigin(r.Method) && origin == "" {
		return &boundaryError{status: http.StatusForbidden, message: "approval origin is required"}
	}
	return nil
}

func methodRequiresOrigin(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

type decisionPayload struct {
	RequestID string       `json:"request_id"`
	Token     string       `json:"token"`
	Decision  cua.Decision `json:"decision"`
}

func decodeDecisionPayload(w http.ResponseWriter, r *http.Request) (decisionPayload, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxDecisionBodyBytes)
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return decisionPayload{}, fmt.Errorf("decode decision payload: content type: %w", err)
	}
	switch {
	case strings.EqualFold(mediaType, "application/json"):
		var payload decisionPayload
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&payload); err != nil {
			return decisionPayload{}, fmt.Errorf("decode decision payload: json: %w", err)
		}
		if err := ensureJSONEOF(decoder); err != nil {
			return decisionPayload{}, fmt.Errorf("decode decision payload: json: %w", err)
		}
		return payload, nil
	case strings.EqualFold(mediaType, "application/x-www-form-urlencoded"):
		if err := r.ParseForm(); err != nil {
			return decisionPayload{}, fmt.Errorf("decode decision payload: form: %w", err)
		}
		return decisionPayload{
			RequestID: r.PostForm.Get("request_id"),
			Token:     r.PostForm.Get("token"),
			Decision:  cua.Decision(r.PostForm.Get("decision")),
		}, nil
	default:
		return decisionPayload{}, fmt.Errorf("decode decision payload: unsupported content type %q", mediaType)
	}
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

func requestIsLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func acceptsHTML(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html")
}

func writeSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; form-action 'self'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func writeApprovalError(w http.ResponseWriter, status int, message string) {
	writeSecurityHeaders(w)
	writeJSON(w, status, map[string]any{
		"schema_version": 1,
		"error":          message,
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	writeSecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		return
	}
}
