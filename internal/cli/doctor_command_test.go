package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestDoctorIsOfflineByDefaultAndBoundsLiveTargets(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"model-a"},{"id":"model-b"}]}`)
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	dbPath := seedDoctorModels(t, provider.URL)

	out, _, err := runCommand(t, "--db", dbPath, "doctor", "--json")
	if err != nil {
		t.Fatalf("doctor offline error = %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("offline doctor made %d provider requests", requests.Load())
	}
	var offline doctorDocument
	if decodeErr := json.Unmarshal([]byte(out), &offline); decodeErr != nil {
		t.Fatalf("offline doctor JSON error = %v", decodeErr)
	}
	if offline.SchemaVersion != 1 || len(offline.Probes) != 0 || offline.Status != "passed" {
		t.Fatalf("offline doctor = %#v", offline)
	}

	out, errOut, err := runCommand(t, "--db", dbPath, "doctor", "--live", "--json")
	if err != nil {
		t.Fatalf("doctor --live error = %v\nstderr=%s", err, errOut)
	}
	var live doctorDocument
	if decodeErr := json.Unmarshal([]byte(out), &live); decodeErr != nil {
		t.Fatalf("live doctor JSON error = %v", decodeErr)
	}
	if len(live.Probes) != 1 || live.Probes[0].Alias != "alpha" || live.Probes[0].Status != "passed" {
		t.Fatalf("live doctor = %#v", live)
	}
	if !strings.Contains(errOut, "Doctor live target: alias=alpha") {
		t.Fatalf("live doctor target missing: %s", errOut)
	}

	out, _, err = runCommand(t, "--db", dbPath, "doctor", "--live", "--all", "--json")
	if err != nil {
		t.Fatalf("doctor --live --all error = %v", err)
	}
	var all doctorDocument
	if decodeErr := json.Unmarshal([]byte(out), &all); decodeErr != nil {
		t.Fatalf("all doctor JSON error = %v", decodeErr)
	}
	if len(all.Probes) != 2 || all.Probes[0].Alias != "alpha" || all.Probes[1].Alias != "beta" {
		t.Fatalf("all doctor = %#v", all)
	}
	if _, _, err := runCommand(t, "--db", dbPath, "doctor", "--all"); err == nil || !strings.Contains(err.Error(), "requires --live") {
		t.Fatalf("doctor --all error = %v", err)
	}
}

func TestDoctorLiveFailureReturnsNonzeroWithJSON(t *testing.T) {
	t.Parallel()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer provider.Close()
	dbPath := seedDoctorModels(t, provider.URL)
	out, _, err := runCommand(t, "--db", dbPath, "doctor", "--live", "--json")
	if err == nil || !strings.Contains(err.Error(), "required failures") {
		t.Fatalf("doctor --live error = %v", err)
	}
	var document doctorDocument
	if decodeErr := json.Unmarshal([]byte(out), &document); decodeErr != nil {
		t.Fatalf("doctor failure JSON error = %v\n%s", decodeErr, out)
	}
	if document.Status != "failed" || len(document.Probes) != 1 || document.Probes[0].Status != "failed" {
		t.Fatalf("doctor failure document = %#v", document)
	}
	probe := document.Probes[0]
	if probe.FailedCheck != "discovery" || probe.FailureKind != "provider_http_status" ||
		probe.ProviderHTTPStatus != http.StatusServiceUnavailable || probe.HTTPStatus != 0 {
		t.Fatalf("doctor failure diagnosis = %#v", probe)
	}
	if probe.Action != "ccr provider test fixture" || !strings.Contains(probe.Evidence, "HTTP 503") {
		t.Fatalf("doctor failure action = %#v", probe)
	}
	if strings.Contains(out, "unavailable") {
		t.Fatalf("doctor leaked provider response body: %s", out)
	}

	humanOut, _, humanErr := runCommand(t, "--db", dbPath, "doctor", "--live")
	if humanErr == nil {
		t.Fatalf("doctor human output unexpectedly passed")
	}
	for _, want := range []string{
		"failure: check=discovery kind=provider_http_status",
		"provider_http=503",
		"action: ccr provider test fixture",
	} {
		if !strings.Contains(humanOut, want) {
			t.Fatalf("doctor human output missing %q:\n%s", want, humanOut)
		}
	}
}

func TestDoctorLiveControlModelSuggestsRemoval(t *testing.T) {
	t.Parallel()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"model-a"},{"id":"model-b"},{"id":"all-proxy-models"}]}`)
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	dbPath := seedDoctorModels(t, provider.URL)
	addDoctorModel(t, dbPath, store.Model{
		Alias: "control", ProviderName: "fixture", ProviderModel: "all-proxy-models", Status: "degraded",
	})

	out, _, err := runCommand(t, "--db", dbPath, "doctor", "--live", "--all", "--json")
	if err == nil || !strings.Contains(err.Error(), "required failures") {
		t.Fatalf("doctor control-model error = %v", err)
	}
	var document doctorDocument
	if decodeErr := json.Unmarshal([]byte(out), &document); decodeErr != nil {
		t.Fatalf("doctor control-model JSON error = %v\n%s", decodeErr, out)
	}
	for _, probe := range document.Probes {
		if probe.Alias != "control" {
			continue
		}
		if probe.FailedCheck != "" || probe.FailureKind != "suite_start" ||
			probe.Action != "ccr model remove control --yes" {
			t.Fatalf("control-model diagnosis = %#v", probe)
		}
		return
	}
	t.Fatalf("control-model probe missing: %#v", document.Probes)
}

func seedDoctorModels(t *testing.T, baseURL string) string {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := s.AddProvider(ctx, store.Provider{
		Name: "fixture", Type: "litellm", BaseURL: baseURL,
	}); err != nil {
		t.Fatalf("AddProvider() error = %v", err)
	}
	for _, model := range []store.Model{
		{Alias: "alpha", ProviderName: "fixture", ProviderModel: "model-a", Status: "degraded"},
		{Alias: "beta", ProviderName: "fixture", ProviderModel: "model-b", Status: "degraded"},
	} {
		if err := s.AddModel(ctx, model); err != nil {
			t.Fatalf("AddModel(%s) error = %v", model.Alias, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	return dbPath
}

func addDoctorModel(t *testing.T, dbPath string, model store.Model) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer func() {
		if closeErr := s.Close(); closeErr != nil {
			t.Fatalf("Close() error = %v", closeErr)
		}
	}()
	if err := s.AddModel(ctx, model); err != nil {
		t.Fatalf("AddModel(%s) error = %v", model.Alias, err)
	}
}
