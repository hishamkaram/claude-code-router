package conformance

import "testing"

func TestTextStreamRequiresCompletedVisibleText(t *testing.T) {
	t.Parallel()
	start := "data: {\"type\":\"message_start\"}\n\n"
	text := "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\n"
	stop := "data: {\"type\":\"message_stop\"}\n\n"
	for _, test := range []struct {
		name, stream string
		valid        bool
	}{
		{"success", start + text + stop, true},
		{"empty", start + stop, false},
		{"incomplete", start + text, false},
		{"unordered", text + start + stop, false},
		{"trailing content", start + text + stop + text, false},
		{"error", start + text + "data: {\"type\":\"error\"}\n\n" + stop, false},
		{"malformed", start + "data: not-json\n\n" + stop, false},
		{"comments", ": ping\n\n" + start + text + stop, true},
		{"multiline CRLF", start + "data: {\"type\":\"content_block_delta\",\r\ndata: \"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\r\n\r\n" + stop, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := requireTextStream([]byte(test.stream)); (err == nil) != test.valid {
				t.Fatalf("stream validation = %v", err)
			}
		})
	}
}
