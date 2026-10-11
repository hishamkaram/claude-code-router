package routeidentity

import (
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/modelcap"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestFingerprintBindsBehaviorAffectingRouteConfiguration(t *testing.T) {
	t.Parallel()
	provider := store.Provider{
		Name: "fixture", Type: "litellm", BaseURL: "https://fixture.example/v1",
		SecretRef: "env:FIXTURE_TOKEN", Protocol: "openai-compatible", Mode: "full",
		SupportsTools: true, SupportsStreaming: true, SupportsThinking: true,
		SupportsModelDiscovery: true, SupportsCountTokens: true, SupportsResponses: true,
	}
	model := store.Model{
		Alias: "coder", ProviderName: provider.Name, ProviderModel: "model-v1", Status: "full",
	}
	effective := modelcap.Values{Kind: modelcap.KindChat, SupportsTools: modelcap.Bool(true)}

	want, err := Fingerprint(model, provider, effective)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(want, "sha256:") || len(want) != len("sha256:")+sha256HexLength {
		t.Fatalf("Fingerprint() = %q", want)
	}
	duplicate, err := Fingerprint(model, provider, effective)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate != want {
		t.Fatalf("Fingerprint() is not deterministic: %q != %q", duplicate, want)
	}
	reordered := modelcap.CloneValues(effective)
	reordered.InputModalities = []string{"text", "image"}
	reordered.OutputModalities = []string{"text", "audio"}
	reorderedFingerprint, err := Fingerprint(model, provider, reordered)
	if err != nil {
		t.Fatal(err)
	}
	reordered.InputModalities = []string{"image", "text"}
	reordered.OutputModalities = []string{"audio", "text"}
	reorderedAgain, err := Fingerprint(model, provider, reordered)
	if err != nil {
		t.Fatal(err)
	}
	if reorderedFingerprint != reorderedAgain {
		t.Fatalf("Fingerprint() depends on modality ordering: %q != %q", reorderedFingerprint, reorderedAgain)
	}

	tests := []struct {
		name      string
		provider  store.Provider
		model     store.Model
		effective modelcap.Values
	}{
		{name: "base_url", provider: withProvider(provider, func(value *store.Provider) { value.BaseURL += "/changed" }), model: model, effective: effective},
		{name: "type", provider: withProvider(provider, func(value *store.Provider) { value.Type = "openrouter" }), model: model, effective: effective},
		{name: "secret_reference", provider: withProvider(provider, func(value *store.Provider) { value.SecretRef = "env:OTHER_TOKEN" }), model: model, effective: effective},
		{name: "provider_capability", provider: withProvider(provider, func(value *store.Provider) { value.SupportsTools = false }), model: model, effective: effective},
		{name: "provider_model", provider: provider, model: withModel(model, func(value *store.Model) { value.ProviderModel = "model-v2" }), effective: effective},
		{name: "effective_model_capability", provider: provider, model: model, effective: modelcap.Values{Kind: modelcap.KindChat, SupportsTools: modelcap.Bool(false)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, fingerprintErr := Fingerprint(test.model, test.provider, test.effective)
			if fingerprintErr != nil {
				t.Fatal(fingerprintErr)
			}
			if got == want {
				t.Fatalf("Fingerprint() did not change for %s", test.name)
			}
		})
	}
}

const sha256HexLength = 64

func withProvider(provider store.Provider, mutate func(*store.Provider)) store.Provider {
	mutate(&provider)
	return provider
}

func withModel(model store.Model, mutate func(*store.Model)) store.Model {
	mutate(&model)
	return model
}
