package modelcap

import (
	"encoding/json"
	"reflect"
	"strings"
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

func TestValuesUnmarshalRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	var values Values
	err := json.Unmarshal([]byte(`{"supports_reponses": true}`), &values)
	if err == nil || !strings.Contains(err.Error(), `json: unknown field "supports_reponses"`) {
		t.Fatalf("json.Unmarshal() error = %v, want unknown field rejection", err)
	}
}

func TestCloneValuesPreservesExplicitNullPresenceIndependently(t *testing.T) {
	t.Parallel()
	var original Values
	if err := json.Unmarshal([]byte(`{"context_window_tokens":8192,"input_modalities":["text","image"],"supports_responses":null,"supports_computer_use":null}`), &original); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	cloned := CloneValues(original)

	if cloned.ContextWindowTokens == nil || *cloned.ContextWindowTokens != 8192 {
		t.Fatalf("cloned ContextWindowTokens = %#v", cloned.ContextWindowTokens)
	}
	if cloned.SupportsResponses != nil || cloned.SupportsComputerUse != nil {
		t.Fatalf("cloned explicit null fields = %#v, %#v", cloned.SupportsResponses, cloned.SupportsComputerUse)
	}
	if !UsesSchemaV3Fields(cloned) {
		t.Fatal("cloned explicit-null presence metadata was lost")
	}

	*original.ContextWindowTokens = 1
	original.InputModalities[0] = "audio"
	delete(original.presentFields, "supports_responses")
	if *cloned.ContextWindowTokens != 8192 || cloned.InputModalities[0] != "text" {
		t.Fatalf("clone changed with original mutation: %#v", cloned)
	}
	if _, ok := cloned.presentFields["supports_responses"]; !ok {
		t.Fatal("clone lost presence metadata after original mutation")
	}

	*cloned.ContextWindowTokens = 16384
	cloned.InputModalities[1] = "audio"
	delete(cloned.presentFields, "supports_computer_use")
	if *original.ContextWindowTokens != 1 || original.InputModalities[1] != "image" {
		t.Fatalf("original changed with clone mutation: %#v", original)
	}
	if _, ok := original.presentFields["supports_computer_use"]; !ok {
		t.Fatal("original presence metadata changed with clone mutation")
	}
}

func TestCloneSnapshotCopiesSourcesAndValuesIndependently(t *testing.T) {
	t.Parallel()
	snapshot := Snapshot{
		Values: Values{
			MaxOutputTokens:  Int64(2048),
			OutputModalities: []string{"text"},
			presentFields:    map[string]struct{}{"supports_responses": {}},
		},
		Sources: map[string]string{"max_output_tokens": SourceOpenAIModels},
	}
	cloned := CloneSnapshot(snapshot)

	*cloned.Values.MaxOutputTokens = 4096
	cloned.Values.OutputModalities[0] = "audio"
	cloned.Sources["max_output_tokens"] = "clone"
	delete(cloned.Values.presentFields, "supports_responses")
	if *snapshot.Values.MaxOutputTokens != 2048 || snapshot.Values.OutputModalities[0] != "text" {
		t.Fatalf("snapshot values changed with clone mutation: %#v", snapshot.Values)
	}
	if snapshot.Sources["max_output_tokens"] != SourceOpenAIModels {
		t.Fatalf("snapshot sources changed with clone mutation: %#v", snapshot.Sources)
	}
	if _, ok := snapshot.Values.presentFields["supports_responses"]; !ok {
		t.Fatal("snapshot presence metadata changed with clone mutation")
	}

	*snapshot.Values.MaxOutputTokens = 1
	snapshot.Values.OutputModalities[0] = "image"
	snapshot.Sources["max_output_tokens"] = "original"
	snapshot.Values.presentFields["supports_computer_use"] = struct{}{}
	if *cloned.Values.MaxOutputTokens != 4096 || cloned.Values.OutputModalities[0] != "audio" {
		t.Fatalf("clone values changed with snapshot mutation: %#v", cloned.Values)
	}
	if cloned.Sources["max_output_tokens"] != "clone" {
		t.Fatalf("clone sources changed with snapshot mutation: %#v", cloned.Sources)
	}
	if _, ok := cloned.Values.presentFields["supports_computer_use"]; ok {
		t.Fatal("clone presence metadata changed with snapshot mutation")
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
