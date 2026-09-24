package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/hishamkaram/claude-code-router/internal/modelrouting"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestFamilyOverridesFollowSessionModelSelection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "startup", ProviderName: "fixture", ProviderModel: "model-startup", Status: "degraded"},
	)
	if err := s.AddModel(ctx, store.Model{Alias: "switched", ProviderName: "fixture", ProviderModel: "model-switched", Status: "degraded"}); err != nil {
		t.Fatalf("AddModel(switched) error = %v", err)
	}
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("startup")}

	for _, requested := range []string{"ccr-family-OPUS", "ccr-family-SONNET", "ccr-family-HAIKU"} {
		route, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", anthropicRequest{Model: requested})
		if validationErr != nil {
			t.Fatalf("family request %q error = %#v", requested, validationErr)
		}
		if route.firstPartyAnthropic || route.model.Alias != "startup" || route.model.ProviderModel != "model-startup" || route.responseModel != requested {
			t.Fatalf("family request %q route = %#v, want startup provider route", requested, route)
		}
	}

	if _, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", anthropicRequest{Model: "switched"}); validationErr != nil {
		t.Fatalf("switch request error = %#v", validationErr)
	}
	route, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", anthropicRequest{Model: "ccr-family-SONNET"})
	if validationErr != nil {
		t.Fatalf("family request after switch error = %#v", validationErr)
	}
	if route.firstPartyAnthropic || route.model.Alias != "switched" || route.model.ProviderModel != "model-switched" {
		t.Fatalf("family request after switch route = %#v, want switched provider route", route)
	}

	_, validationErr = h.selectMessageRouteForRequest(ctx, "session-a", anthropicRequest{Model: "sonnet"})
	if validationErr != nil {
		t.Fatalf("native switch request error = %#v", validationErr)
	}
	route, validationErr = h.selectMessageRouteForRequest(ctx, "session-a", anthropicRequest{Model: "ccr-family-OPUS[1m]"})
	if validationErr != nil {
		t.Fatalf("family request after native switch error = %#v", validationErr)
	}
	if !route.firstPartyAnthropic || route.responseModel != "opus[1m]" || route.model.Alias != "opus[1m]" || route.model.ProviderModel != "opus[1m]" {
		t.Fatalf("family request after native switch route = %#v, want first-party opus[1m]", route)
	}
}

func TestFamilyOverrideFailsClosedForUnroutableActiveAlias(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "blocked"},
	)
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("active")}

	_, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", anthropicRequest{Model: "ccr-family-HAIKU"})
	if validationErr == nil {
		t.Fatal("family request succeeded, want fail-closed rejection")
	}
	if validationErr.status != 403 || !strings.Contains(validationErr.message, "is blocked") ||
		!strings.Contains(validationErr.message, "first-party Anthropic fallback was refused") {
		t.Fatalf("family error = %#v, want blocked fail-closed error", validationErr)
	}
}

func TestFamilyOverrideNativeModelIDs(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		model string
		want  string
	}{
		{model: "ccr-family-OPUS", want: "opus"},
		{model: "ccr-family-SONNET[1m]", want: "sonnet[1m]"},
		{model: "ccr-family-HAIKU", want: "haiku"},
	} {
		test := test
		t.Run(test.model, func(t *testing.T) {
			parsed, ok := modelrouting.ParseOverride(test.model)
			if !ok {
				t.Fatalf("ParseOverride(%q) did not recognize family identifier", test.model)
			}
			if got := parsed.NativeModelID(); got != test.want {
				t.Fatalf("NativeModelID() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestLegacyLowercaseFamilyAliasRemainsRoutable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newGatewayStore(t,
		store.Provider{Name: "fixture", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1"},
		store.Model{Alias: "active", ProviderName: "fixture", ProviderModel: "model-active", Status: "degraded"},
	)
	if err := s.AddModel(ctx, store.Model{Alias: "ccr-family-opus", ProviderName: "fixture", ProviderModel: "legacy-opus", Status: "degraded"}); err != nil {
		t.Fatalf("AddModel(legacy family-looking alias) error = %v", err)
	}
	h := handler{cfg: Config{Store: s}, activeModel: newActiveModelSelection("active")}

	route, validationErr := h.selectMessageRouteForRequest(ctx, "session-a", anthropicRequest{Model: "ccr-family-opus"})
	if validationErr != nil {
		t.Fatalf("legacy alias route error = %#v", validationErr)
	}
	if route.firstPartyAnthropic || route.model.Alias != "ccr-family-opus" || route.model.ProviderModel != "legacy-opus" {
		t.Fatalf("legacy alias route = %#v, want configured legacy provider route", route)
	}
}
