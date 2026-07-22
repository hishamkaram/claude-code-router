package teamprofile

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
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
		"wrong version":   `{"schema_version":4,"kind":"ccr-team-profile","providers":[],"models":[]}`,
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

func TestDecodeRejectsUnknownNestedCapabilityFields(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"provider capability typo": `{
  "schema_version": 3,
  "kind": "ccr-team-profile",
  "providers": [{
    "name": "litellm",
    "type": "litellm",
    "base_url": "http://localhost:4000",
    "protocol": "openai-compatible",
    "mode": "degraded",
    "capabilities": {"tools": true, "streamng": true, "thinking": true, "model_discovery": true, "count_tokens": true},
    "credential": {"required": false}
  }],
  "models": []
}`,
		"model override typo": `{
  "schema_version": 3,
  "kind": "ccr-team-profile",
  "providers": [{
    "name": "litellm",
    "type": "litellm",
    "base_url": "http://localhost:4000",
    "protocol": "openai-compatible",
    "mode": "degraded",
    "capabilities": {"tools": true, "streaming": true, "thinking": true, "model_discovery": true, "count_tokens": true},
    "credential": {"required": false}
  }],
  "models": [{
    "alias": "glm",
    "provider": "litellm",
    "provider_model": "glm-5.2",
    "compatibility": "full",
    "capability_overrides": {"supports_reponses": true}
  }]
}`,
		"model discovered values typo": `{
  "schema_version": 3,
  "kind": "ccr-team-profile",
  "providers": [{
    "name": "litellm",
    "type": "litellm",
    "base_url": "http://localhost:4000",
    "protocol": "openai-compatible",
    "mode": "degraded",
    "capabilities": {"tools": true, "streaming": true, "thinking": true, "model_discovery": true, "count_tokens": true},
    "credential": {"required": false}
  }],
  "models": [{
    "alias": "glm",
    "provider": "litellm",
    "provider_model": "glm-5.2",
    "compatibility": "full",
    "discovered_capabilities": {"values": {"supports_reponses": true}}
  }]
}`,
	}
	for name, input := range tests {
		input := input
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Decode(strings.NewReader(input)); err == nil || !strings.Contains(err.Error(), "json: unknown field") {
				t.Fatalf("Decode() error = %v, want unknown field rejection", err)
			}
		})
	}
}

func TestDecodeAcceptsVersionOneProfile(t *testing.T) {
	t.Parallel()
	input := `{"schema_version":1,"kind":"ccr-team-profile","providers":[],"models":[]}`
	manifest, err := Decode(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if manifest.SchemaVersion != 1 {
		t.Fatalf("SchemaVersion = %d, want 1", manifest.SchemaVersion)
	}
}

func TestBuildPlanImportRoundTripsModelCapabilities(t *testing.T) {
	t.Parallel()
	discovered, err := modelcap.SnapshotFrom(modelcap.Values{
		Kind:                modelcap.KindChat,
		ContextWindowTokens: modelcap.Int64(1_000_000),
		SupportsTools:       modelcap.Bool(true),
	}, modelcap.SourceLiteLLMInfo)
	if err != nil {
		t.Fatalf("SnapshotFrom() error = %v", err)
	}
	discovered.Values.SupportsStreaming = modelcap.Bool(true)
	discovered.Sources["supports_streaming"] = modelcap.SourceOpenAIAdapter
	stored := store.Model{
		Alias:                   "glm-5-2",
		ProviderName:            "litellm",
		ProviderModel:           "glm-5.2[1m]",
		Status:                  "full",
		DiscoveredCapabilities:  discovered,
		CapabilityOverrides:     modelcap.Values{MaxOutputTokens: modelcap.Int64(64_000)},
		CapabilitiesRefreshedAt: "2026-07-18T12:00:00Z",
	}
	manifest, err := Build(
		[]store.Provider{profileProvider("litellm", "litellm", "http://localhost:4000", "")},
		[]store.Model{stored},
	)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if manifest.SchemaVersion != 3 || manifest.Models[0].DiscoveredCapabilities == nil || manifest.Models[0].CapabilityOverrides == nil {
		t.Fatalf("Build() manifest = %#v", manifest)
	}
	plan, err := manifest.PlanImport(nil)
	if err != nil {
		t.Fatalf("PlanImport() error = %v", err)
	}
	got := plan.Models[0]
	if got.DiscoveredCapabilities.Values.ContextWindowTokens == nil ||
		*got.DiscoveredCapabilities.Values.ContextWindowTokens != 1_000_000 ||
		got.DiscoveredCapabilities.Values.SupportsStreaming == nil ||
		!*got.DiscoveredCapabilities.Values.SupportsStreaming ||
		got.DiscoveredCapabilities.Sources["supports_streaming"] != modelcap.SourceOpenAIAdapter ||
		got.CapabilityOverrides.MaxOutputTokens == nil || *got.CapabilityOverrides.MaxOutputTokens != 64_000 ||
		got.CapabilitiesRefreshedAt != stored.CapabilitiesRefreshedAt {
		t.Fatalf("PlanImport() model = %#v", got)
	}
}

func TestBuildPlanImportRoundTripsProviderResponsesCapability(t *testing.T) {
	t.Parallel()

	provider := profileProvider("responses", "openai-compatible", "https://responses.example", "")
	provider.SupportsResponses = true
	manifest, err := Build([]store.Provider{provider}, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if manifest.SchemaVersion != 3 || !manifest.Providers[0].Capabilities.Responses {
		t.Fatalf("Build() manifest = %#v", manifest)
	}
	plan, err := manifest.PlanImport(nil)
	if err != nil {
		t.Fatalf("PlanImport() error = %v", err)
	}
	if len(plan.Providers) != 1 || !plan.Providers[0].SupportsResponses {
		t.Fatalf("PlanImport() providers = %#v", plan.Providers)
	}
}

func TestValidateRejectsResponsesCapabilityBeforeSchemaVersionThree(t *testing.T) {
	t.Parallel()

	provider := profileProvider("responses", "openai-compatible", "https://responses.example", "")
	provider.SupportsResponses = true
	manifest, err := Build([]store.Provider{provider}, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	manifest.SchemaVersion = 2
	if validationErr := manifest.Validate(); validationErr == nil || !strings.Contains(validationErr.Error(), "responses capability requires schema_version 3") {
		t.Fatalf("Validate() error = %v, want schema-version rejection", validationErr)
	}
}

func TestDecodeRejectsProviderResponsesFieldBeforeSchemaVersionThree(t *testing.T) {
	t.Parallel()

	input := `{
  "schema_version": 2,
  "kind": "ccr-team-profile",
  "providers": [{
    "name": "litellm",
    "type": "litellm",
    "base_url": "http://localhost:4000",
    "protocol": "openai-compatible",
    "mode": "degraded",
    "capabilities": {"tools": true, "streaming": true, "thinking": true, "model_discovery": true, "count_tokens": true, "responses": false},
    "credential": {"required": false}
  }],
  "models": []
}`
	if _, err := Decode(strings.NewReader(input)); err == nil || !strings.Contains(err.Error(), "responses capability requires schema_version 3") {
		t.Fatalf("Decode() error = %v, want provider schema-version rejection", err)
	}
}

func TestValidateRejectsModelSchemaV3CapabilitiesBeforeSchemaVersionThree(t *testing.T) {
	t.Parallel()

	manifest, err := Build(
		[]store.Provider{profileProvider("litellm", "litellm", "http://localhost:4000", "")},
		[]store.Model{{
			Alias: "glm", ProviderName: "litellm", ProviderModel: "glm-5.2", Status: "full",
			CapabilityOverrides: modelcap.Values{SupportsComputerUse: modelcap.Bool(true)},
		}},
	)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	manifest.SchemaVersion = 2
	if validationErr := manifest.Validate(); validationErr == nil || !strings.Contains(validationErr.Error(), "require schema_version 3") {
		t.Fatalf("Validate() error = %v, want schema-version rejection", validationErr)
	}
}

func TestDecodeRejectsNullModelSchemaV3CapabilitiesBeforeSchemaVersionThree(t *testing.T) {
	t.Parallel()

	input := `{
  "schema_version": 2,
  "kind": "ccr-team-profile",
  "providers": [{
    "name": "litellm",
    "type": "litellm",
    "base_url": "http://localhost:4000",
    "protocol": "openai-compatible",
    "mode": "degraded",
    "capabilities": {"tools": true, "streaming": true, "thinking": true, "model_discovery": true, "count_tokens": true},
    "credential": {"required": false}
  }],
  "models": [{
    "alias": "glm",
    "provider": "litellm",
    "provider_model": "glm-5.2",
    "compatibility": "full",
    "capability_overrides": {"supports_responses": null}
  }]
}`
	if _, err := Decode(strings.NewReader(input)); err == nil || !strings.Contains(err.Error(), "require schema_version 3") {
		t.Fatalf("Decode() error = %v, want schema-version rejection", err)
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

	manifest, err = Build(
		[]store.Provider{profileProvider("anthropic", "anthropic", "https://api.anthropic.com", secret.EnvRef("ANTHROPIC_API_KEY"))},
		nil,
	)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	manifest.Providers[0].Capabilities.Responses = true
	if validationErr := manifest.Validate(); validationErr == nil || !strings.Contains(validationErr.Error(), `responses API capability requires provider protocol "openai-compatible"`) {
		t.Fatalf("Validate() error = %v, want Responses protocol error", validationErr)
	}
}

func TestValidateRejectsControlModelsAndUnknownCapabilitySources(t *testing.T) {
	t.Parallel()
	manifest, err := Build(
		[]store.Provider{profileProvider("litellm", "litellm", "http://localhost:4000", "")},
		[]store.Model{{Alias: "chat", ProviderName: "litellm", ProviderModel: "chat-model", Status: "degraded"}},
	)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	manifest.Models[0].ProviderModel = "all-proxy-models"
	if validationErr := manifest.Validate(); validationErr == nil || !strings.Contains(validationErr.Error(), "control model") {
		t.Fatalf("Validate() control-model error = %v", validationErr)
	}

	manifest.Models[0].ProviderModel = "chat-model"
	manifest.Models[0].DiscoveredCapabilities = &modelcap.Snapshot{
		Values:  modelcap.Values{SupportsTools: modelcap.Bool(true)},
		Sources: map[string]string{"supports_tools": "untrusted:secret"},
	}
	if validationErr := manifest.Validate(); validationErr == nil || !strings.Contains(validationErr.Error(), "invalid discovered_capabilities source") {
		t.Fatalf("Validate() capability-source error = %v", validationErr)
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
