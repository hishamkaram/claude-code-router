package modelcap

import (
	"reflect"
	"testing"
)

func TestEffectiveAppliesOverridesBeforeModelIDHint(t *testing.T) {
	t.Parallel()
	discovered, err := SnapshotFrom(Values{
		ContextWindowTokens: Int64(200_000),
		SupportsTools:       Bool(false),
	}, "fixture:/models")
	if err != nil {
		t.Fatalf("SnapshotFrom() error = %v", err)
	}
	effective, err := Effective(discovered, Values{
		ContextWindowTokens: Int64(1_000_000),
		SupportsTools:       Bool(true),
	}, "glm-5.2[1m]")
	if err != nil {
		t.Fatalf("Effective() error = %v", err)
	}
	if got := *effective.Values.ContextWindowTokens; got != 1_000_000 {
		t.Fatalf("ContextWindowTokens = %d", got)
	}
	if got := *effective.Values.SupportsTools; !got {
		t.Fatal("SupportsTools = false")
	}
	if effective.Sources["context_window_tokens"] != SourceOverride || effective.Sources["supports_tools"] != SourceOverride {
		t.Fatalf("Sources = %#v", effective.Sources)
	}
}

func TestEffectiveUsesOneMillionHintOnlyWhenContextUnknown(t *testing.T) {
	t.Parallel()
	effective, err := Effective(Snapshot{}, Values{}, "glm-5.2[1M]")
	if err != nil {
		t.Fatalf("Effective() error = %v", err)
	}
	if effective.Values.ContextWindowTokens == nil || *effective.Values.ContextWindowTokens != 1_000_000 {
		t.Fatalf("ContextWindowTokens = %#v", effective.Values.ContextWindowTokens)
	}
	if effective.Sources["context_window_tokens"] != SourceModelIDHint {
		t.Fatalf("Sources = %#v", effective.Sources)
	}
}

func TestNormalizeValuesPreservesUnknownAndExplicitFalse(t *testing.T) {
	t.Parallel()
	values, err := NormalizeValues(Values{
		InputModalities: []string{"Image", "text", "image"},
		SupportsTools:   Bool(false),
	})
	if err != nil {
		t.Fatalf("NormalizeValues() error = %v", err)
	}
	if values.SupportsTools == nil || *values.SupportsTools {
		t.Fatalf("SupportsTools = %#v", values.SupportsTools)
	}
	if values.SupportsThinking != nil {
		t.Fatalf("SupportsThinking = %#v, want unknown", values.SupportsThinking)
	}
	if !reflect.DeepEqual(values.InputModalities, []string{"image", "text"}) {
		t.Fatalf("InputModalities = %#v", values.InputModalities)
	}
	if got := PopulatedFields(values); !reflect.DeepEqual(got, []string{"input_modalities", "supports_tools"}) {
		t.Fatalf("PopulatedFields() = %#v", got)
	}
}

func TestNormalizeValuesRejectsInvalidData(t *testing.T) {
	t.Parallel()
	for name, values := range map[string]Values{
		"kind":       {Kind: "chat-control"},
		"tokens":     {MaxOutputTokens: Int64(0)},
		"modalities": {InputModalities: []string{"binary"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := NormalizeValues(values); err == nil {
				t.Fatal("NormalizeValues() succeeded")
			}
		})
	}
}

func TestIsRoutableKindKeepsUnknownAndRejectsNonChatKinds(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"", KindUnknown, KindChat, KindCompletion, KindResponses} {
		if !IsRoutableKind(kind) {
			t.Fatalf("IsRoutableKind(%q) = false", kind)
		}
	}
	for _, kind := range []string{KindEmbedding, KindRerank, KindImage, KindAudio, KindControl} {
		if IsRoutableKind(kind) {
			t.Fatalf("IsRoutableKind(%q) = true", kind)
		}
	}
}
