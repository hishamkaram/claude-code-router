//go:build live

package cli

import "testing"

func TestLiveSubscriptionFixtureClaimsResponsesInOrder(t *testing.T) {
	t.Parallel()

	fixture := &liveSubscriptionFixture{responses: []liveSubscriptionResponse{
		{account: "personal", token: liveSubscriptionPersonalToken},
		{account: "work", token: liveSubscriptionWorkToken},
	}}
	for _, token := range []string{
		liveSubscriptionPersonalToken,
		liveSubscriptionWorkToken,
	} {
		if _, _, ok := fixture.nextResponse("Bearer " + token); !ok {
			t.Fatalf("fixture rejected expected token at call %d", fixture.CallCount()+1)
		}
	}
	fixture.AssertCalls(t, []string{"personal", "work"})
}
