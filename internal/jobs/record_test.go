package jobs

import "testing"

func TestInvalidJobIDs(t *testing.T) {
	for _, id := range []string{"", "0", "1", "-1", "ccr-1", "../other", "ccr-00000000-0000-0000-0000-000000000000", "foo.scope", "ccr-88888888-8888-4888-8888-888888888888/../x"} {
		if err := ValidateID(id); err == nil {
			t.Errorf("accepted %q", id)
		}
	}
}

func TestPessimisticCoverage(t *testing.T) {
	for _, value := range []string{"", "clean", "COMPLETE", "future"} {
		c := Cleanup{Coverage: value}
		c.Normalize()
		if c.Coverage != "unknown" || c.Reason == "" || c.Survivors == nil {
			t.Fatalf("optimistic normalization: %+v", c)
		}
	}
}
