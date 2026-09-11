//go:build linux || darwin

package cli

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/hishamkaram/claude-code-router/internal/jobs"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func executionAvailabilityFixture(t *testing.T) (*options, launchInvocation, *atomic.Bool, *atomic.Int32) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "profile"))
	unavailable, requests := new(atomic.Bool), new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		if unavailable.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"}]}`))
	}))
	t.Cleanup(server.Close)
	opts := &options{dbPath: filepath.Join(root, "ccr.db")}
	s, _, err := openMigratedStore(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(s)
	if addErr := s.AddProvider(t.Context(), store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: server.URL}); addErr != nil {
		t.Fatal(addErr)
	}
	if addErr := s.AddModel(t.Context(), store.Model{Alias: "fixture", ProviderName: "fixture", ProviderModel: "gpt-4o", Status: "full"}); addErr != nil {
		t.Fatal(addErr)
	}
	inv, err := parseLaunchInvocation([]string{"--model=fixture", "--auth-mode=provider-only"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolveLaunch(t.Context(), Dependencies{}, s, inv); err != nil {
		t.Fatalf("successful preflight not exercised: %v", err)
	}
	return opts, inv, unavailable, requests
}

func TestExecutionFingerprintExcludesTransientDiscovery(t *testing.T) {
	opts, inv, unavailable, requests := executionAvailabilityFixture(t)
	before, err := executionFingerprint(t.Context(), opts, inv)
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatal("fingerprinting performed live discovery")
	}
	unavailable.Store(true)
	s, _, err := openMigratedStore(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(s)
	if _, resolveErr := resolveLaunch(t.Context(), Dependencies{}, s, inv); resolveErr == nil {
		t.Fatal("503 preflight did not fail")
	}
	after, err := executionFingerprint(t.Context(), opts, inv)
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatal("fingerprinting consulted transient provider availability")
	}
	if before != after {
		t.Fatal("transient discovery changed execution fingerprint")
	}
}

func TestPreparedRecoverySurvivesProviderOutage(t *testing.T) {
	opts, inv, unavailable, _ := executionAvailabilityFixture(t)
	registry, err := jobs.OpenRegistry(t.Context(), filepath.Join(t.TempDir(), "jobs"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = registry.Close() }()
	inv.submissionID = uuid.NewString()
	request := strings.Repeat("a", 64)
	before, lease, err := prepareDetachedAdmission(t.Context(), registry, opts, inv, request)
	if err != nil || lease == nil {
		t.Fatalf("prepare: %v", err)
	}
	if closeErr := lease.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	unavailable.Store(true)
	recovered, recoveredLease, err := prepareDetachedAdmission(t.Context(), registry, opts, inv, request)
	if err != nil || recoveredLease == nil {
		t.Fatalf("recover: %v", err)
	}
	defer func() { _ = recoveredLease.Close() }()
	if recovered.JobID != before.JobID || recovered.State != jobs.AdmissionPrepared {
		t.Fatalf("prepared admission changed: %+v", recovered)
	}
}
