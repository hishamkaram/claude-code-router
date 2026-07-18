package teamprofile

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/providers"
	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestBuildEncodeIsDeterministicAndRedactsLocalSecretReferences(t *testing.T) {
	t.Parallel()
	storedProviders := []store.Provider{
		profileProvider("openrouter", "openrouter", "https://openrouter.ai/api", secret.KeyringRef("openrouter")),
		profileProvider("anthropic", "anthropic", "https://api.anthropic.com", secret.EnvRef("ANTHROPIC_API_KEY")),
		profileProvider("litellm", "litellm", "http://localhost:4000", secret.FileRef("/private/team-api-key")),
	}
	storedModels := []store.Model{
		{Alias: "router-model", ProviderName: "openrouter", ProviderModel: "vendor/router-model", Status: "degraded"},
		{Alias: "local-model", ProviderName: "litellm", ProviderModel: "local/model", Status: "full"},
	}
	manifest, err := Build(storedProviders, storedModels)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	var first bytes.Buffer
	if err := Encode(&first, manifest); err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	var second bytes.Buffer
	if err := Encode(&second, manifest); err != nil {
		t.Fatalf("second Encode() error = %v", err)
	}
	if first.String() != second.String() {
		t.Fatal("Encode() output is not deterministic")
	}
	output := first.String()
	for _, forbidden := range []string{"/private/team-api-key", "provider/openrouter/api-key", "file:", "keyring:"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("Encode() leaked %q in %s", forbidden, output)
		}
	}
	if !strings.Contains(output, `"environment_variable": "ANTHROPIC_API_KEY"`) {
		t.Fatalf("Encode() did not preserve environment reference: %s", output)
	}
	if strings.Index(output, `"name": "anthropic"`) > strings.Index(output, `"name": "litellm"`) ||
		strings.Index(output, `"name": "litellm"`) > strings.Index(output, `"name": "openrouter"`) {
		t.Fatalf("providers are not sorted: %s", output)
	}
}

func TestDecodeRejectsInvalidProfiles(t *testing.T) {
	t.Parallel()
	valid := `{"schema_version":1,"kind":"ccr-team-profile","providers":[],"models":[]}`
	tests := map[string]string{
		"unknown field":   `{"schema_version":1,"kind":"ccr-team-profile","providers":[],"models":[],"extra":true}`,
		"duplicate field": `{"schema_version":1,"schema_version":1,"kind":"ccr-team-profile","providers":[],"models":[]}`,
		"wrong version":   `{"schema_version":2,"kind":"ccr-team-profile","providers":[],"models":[]}`,
		"wrong kind":      `{"schema_version":1,"kind":"other","providers":[],"models":[]}`,
		"trailing value":  valid + `{}`,
		"empty":           "  \n",
	}
	for name, input := range tests {
		input := input
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Decode(strings.NewReader(input)); err == nil {
				t.Fatalf("Decode(%q) succeeded", input)
			}
		})
	}
}

func TestDecodeRejectsOversizeInput(t *testing.T) {
	t.Parallel()
	if _, err := Decode(strings.NewReader(strings.Repeat(" ", MaxBytes+1))); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Decode() error = %v, want size error", err)
	}
}

func TestPlanImportPreservesEnvBindingsAndReportsUnboundCredentials(t *testing.T) {
	t.Parallel()
	manifest, err := Build(
		[]store.Provider{
			profileProvider("anthropic", "anthropic", "https://api.anthropic.com", secret.EnvRef("ORIGINAL_KEY")),
			profileProvider("litellm", "litellm", "http://localhost:4000", secret.FileRef("/private/key")),
		},
		[]store.Model{{Alias: "team-model", ProviderName: "litellm", ProviderModel: "team/model", Status: "full"}},
	)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	plan, err := manifest.PlanImport(map[string]string{"anthropic": "TEAM_ANTHROPIC_KEY"})
	if err != nil {
		t.Fatalf("PlanImport() error = %v", err)
	}
	if got := plan.Providers[0].SecretRef; got != secret.EnvRef("TEAM_ANTHROPIC_KEY") {
		t.Fatalf("anthropic SecretRef = %q", got)
	}
	if got := plan.Providers[1].SecretRef; got != "" {
		t.Fatalf("litellm SecretRef = %q, want unbound", got)
	}
	if len(plan.UnboundCredential) != 1 || plan.UnboundCredential[0] != "litellm" {
		t.Fatalf("UnboundCredential = %#v", plan.UnboundCredential)
	}
	if len(plan.Models) != 1 || plan.Models[0].ProviderName != "litellm" {
		t.Fatalf("Models = %#v", plan.Models)
	}
}

func TestPlanImportRejectsUnknownAndInvalidBindings(t *testing.T) {
	t.Parallel()
	manifest, err := Build(
		[]store.Provider{profileProvider("litellm", "litellm", "http://localhost:4000", "")},
		nil,
	)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	for _, bindings := range []map[string]string{
		{"missing": "MISSING_KEY"},
		{"litellm": "lowercase"},
	} {
		if _, err := manifest.PlanImport(bindings); err == nil {
			t.Fatalf("PlanImport(%v) succeeded", bindings)
		}
	}
}

func TestValidateRejectsCredentialBearingBaseURL(t *testing.T) {
	t.Parallel()
	provider := profileProvider("litellm", "litellm", "https://user:password@example.com/v1", "")
	if _, err := Build([]store.Provider{provider}, nil); err == nil {
		t.Fatal("Build() accepted credentials in base URL")
	}
	provider.BaseURL = "https://example.com/v1?api_key=secret"
	if _, err := Build([]store.Provider{provider}, nil); err == nil {
		t.Fatal("Build() accepted query parameters in base URL")
	}
}

func TestValidateRejectsInconsistentProviderSecurityAndCapabilities(t *testing.T) {
	t.Parallel()
	manifest, err := Build(
		[]store.Provider{profileProvider("openrouter", "openrouter", "https://openrouter.ai/api", secret.EnvRef("OPENROUTER_API_KEY"))},
		nil,
	)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	manifest.Providers[0].Credential = Credential{}
	if validationErr := manifest.Validate(); validationErr == nil || !strings.Contains(validationErr.Error(), "credential.required=true") {
		t.Fatalf("Validate() error = %v, want required credential error", validationErr)
	}

	manifest, err = Build(
		[]store.Provider{profileProvider("litellm", "litellm", "http://localhost:4000", "")},
		nil,
	)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	manifest.Providers[0].Mode = providers.ModeChatOnly
	manifest.Providers[0].Capabilities.Tools = true
	if validationErr := manifest.Validate(); validationErr == nil || !strings.Contains(validationErr.Error(), "cannot declare tools") {
		t.Fatalf("Validate() error = %v, want chat-only tools error", validationErr)
	}
}

func profileProvider(name, providerType, baseURL, secretRef string) store.Provider {
	caps := providers.DefaultCapabilities(providerType)
	return store.Provider{
		Name:                   name,
		Type:                   providerType,
		BaseURL:                baseURL,
		SecretRef:              secretRef,
		Protocol:               caps.Protocol,
		SupportsTools:          caps.SupportsTools,
		SupportsStreaming:      caps.SupportsStreaming,
		SupportsThinking:       caps.SupportsThinking,
		SupportsModelDiscovery: caps.SupportsModelDiscovery,
		SupportsCountTokens:    caps.SupportsCountTokens,
		Mode:                   caps.Mode,
	}
}
