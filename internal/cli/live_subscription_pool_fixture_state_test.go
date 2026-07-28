//go:build live

package cli

import (
	"net/http"
	"testing"
)

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

func TestLiveSubscriptionFixtureAcceptsOldAccountDuringPendingRotation(t *testing.T) {
	t.Parallel()

	fixture := &liveSubscriptionFixture{
		responses: []liveSubscriptionResponse{
			{account: "personal", token: liveSubscriptionPersonalToken, status: http.StatusTooManyRequests},
			{account: "work", token: liveSubscriptionWorkToken},
		},
		repeatLast: true,
	}
	for _, token := range []string{
		liveSubscriptionPersonalToken,
		liveSubscriptionPersonalToken,
		liveSubscriptionWorkToken,
		liveSubscriptionWorkToken,
	} {
		if _, _, ok := fixture.nextResponse("Bearer " + token); !ok {
			t.Fatalf("fixture rejected expected token at call %d", fixture.CallCount()+1)
		}
	}
	fixture.AssertCalls(t, []string{"personal", "personal", "work", "work"})
	fixture.AssertRotationCalls(t, "personal", "work")
}

func TestLiveSubscriptionFixtureRejectsInvalidRotationOrder(t *testing.T) {
	t.Parallel()

	t.Run("work before personal", func(t *testing.T) {
		t.Parallel()
		fixture := &liveSubscriptionFixture{responses: []liveSubscriptionResponse{
			{account: "personal", token: liveSubscriptionPersonalToken, status: http.StatusTooManyRequests},
			{account: "work", token: liveSubscriptionWorkToken},
		}}
		if _, _, ok := fixture.nextResponse("Bearer " + liveSubscriptionWorkToken); ok {
			t.Fatal("fixture accepted work account before initial personal account")
		}
	})

	t.Run("personal after work", func(t *testing.T) {
		t.Parallel()
		fixture := &liveSubscriptionFixture{
			responses: []liveSubscriptionResponse{
				{account: "personal", token: liveSubscriptionPersonalToken, status: http.StatusTooManyRequests},
				{account: "work", token: liveSubscriptionWorkToken},
			},
			repeatLast: true,
		}
		for _, token := range []string{
			liveSubscriptionPersonalToken,
			liveSubscriptionWorkToken,
		} {
			if _, _, ok := fixture.nextResponse("Bearer " + token); !ok {
				t.Fatalf("fixture rejected expected token at call %d", fixture.CallCount()+1)
			}
		}
		if _, _, ok := fixture.nextResponse("Bearer " + liveSubscriptionPersonalToken); ok {
			t.Fatal("fixture accepted personal account after work account became active")
		}
	})

	t.Run("duplicate successful previous account", func(t *testing.T) {
		t.Parallel()
		fixture := &liveSubscriptionFixture{responses: []liveSubscriptionResponse{
			{account: "personal", token: liveSubscriptionPersonalToken},
			{account: "work", token: liveSubscriptionWorkToken},
		}}
		if _, _, ok := fixture.nextResponse("Bearer " + liveSubscriptionPersonalToken); !ok {
			t.Fatal("fixture rejected initial personal account")
		}
		if _, _, ok := fixture.nextResponse("Bearer " + liveSubscriptionPersonalToken); ok {
			t.Fatal("fixture accepted duplicate previous account without a rate-limit transition")
		}
	})
}
