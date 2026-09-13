package gateway

import "testing"

func TestLateSystemMessageIsFoldedIntoLeadingSystem(t *testing.T) {
	req := anthropicRequest{
		System: "lead system",
		Messages: []anthropicMessage{
			{Role: "user", Content: "hello"},
			{Role: "system", Content: "# Environment\nlate block"},
			{Role: "assistant", Content: "hi"},
		},
	}
	out, _, err := toOpenAIChatRequest(req, openAIModelRoute{alias: "a", providerName: "p", providerModel: "m"})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	roles := make([]string, 0, len(out.Messages))
	for _, m := range out.Messages {
		roles = append(roles, m.Role)
	}
	if len(roles) != 3 || roles[0] != "system" || roles[1] != "user" || roles[2] != "assistant" {
		t.Fatalf("unexpected role order %v", roles)
	}
	lead, _ := out.Messages[0].Content.(string)
	if lead != "lead system\n\n# Environment\nlate block" {
		t.Fatalf("late system not folded, got %q", lead)
	}
}

func TestLeadingSystemMessagesAreKept(t *testing.T) {
	req := anthropicRequest{
		Messages: []anthropicMessage{
			{Role: "system", Content: "only system"},
			{Role: "user", Content: "hello"},
		},
	}
	out, _, err := toOpenAIChatRequest(req, openAIModelRoute{alias: "a", providerName: "p", providerModel: "m"})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(out.Messages) != 2 || out.Messages[0].Role != "system" || out.Messages[1].Role != "user" {
		t.Fatalf("unexpected messages %+v", out.Messages)
	}
}
