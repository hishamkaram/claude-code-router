package agentinput

import "testing"

func TestNormalizeAcceptsProviderAgentVariants(t *testing.T) {
	t.Parallel()

	got := Normalize(map[string]any{
		"prompt":            "find latest chatgpt news",
		"agent_type":        "general-purpose",
		"model":             "glm",
		"run_in_background": "false",
		"extra":             "ignored",
	})

	if got["prompt"] != "find latest chatgpt news" ||
		got["description"] != "find latest chatgpt news" ||
		got["subagent_type"] != "general-purpose" ||
		got["run_in_background"] != false {
		t.Fatalf("Normalize() = %#v", got)
	}
	if _, found := got["model"]; found {
		t.Fatalf("Normalize() retained invalid model: %#v", got)
	}
	if _, found := got["extra"]; found {
		t.Fatalf("Normalize() retained unsupported field: %#v", got)
	}
}
