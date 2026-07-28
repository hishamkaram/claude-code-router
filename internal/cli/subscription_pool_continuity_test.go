package cli

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/claude-code-router/internal/gateway"
	"github.com/hishamkaram/claude-code-router/internal/secret"
	"github.com/hishamkaram/claude-code-router/internal/store"
)

func TestSubscriptionPoolControllerRotatesWithoutRestartState(t *testing.T) {
	t.Parallel()

	controller, s, dbPath := newControllerTestPool(t, false, "personal", "work")
	failed, err := controller.CurrentCredential(context.Background())
	if err != nil {
		t.Fatalf("CurrentCredential() error = %v", err)
	}
	next, retry, err := controller.RotateCredential(
		context.Background(),
		failed,
		confirmedSubscriptionLimit(),
	)
	if err != nil || !retry || next.AccountName != "work" || next.Generation != 2 {
		t.Fatalf("RotateCredential() = %#v, %t, %v", next, retry, err)
	}
	if got := controller.ActiveAccount(); got != "work" {
		t.Fatalf("ActiveAccount() = %q, want work", got)
	}
	personal := getAccountForCLI(t, dbPath, "personal")
	if personal.LastError != "subscription_limit_five_hour" || personal.CooldownUntil == "" {
		t.Fatalf("personal failure state = %#v", personal)
	}
	launch, err := s.GetLaunch(context.Background(), 1)
	if err != nil || launch.ClaudeAccountName != "work" {
		t.Fatalf("launch account = %#v, %v", launch, err)
	}
	notice := receiveControllerNotice(t, controller)
	if !strings.Contains(notice, "existing gateway") || !strings.Contains(notice, "was not restarted") {
		t.Fatalf("rotation notice = %q", notice)
	}
}

func TestGatewaySubscriptionPoolKeepsNilControllerNil(t *testing.T) {
	t.Parallel()

	if pool := gatewaySubscriptionPool(nil); pool != nil {
		t.Fatalf("gateway subscription pool = %#v, want nil", pool)
	}
}

func TestSubscriptionPoolControllerCoalescesConcurrentStaleFailures(t *testing.T) {
	t.Parallel()

	controller, _, dbPath := newControllerTestPool(t, false, "personal", "work", "reserve")
	failed, err := controller.CurrentCredential(context.Background())
	if err != nil {
		t.Fatalf("CurrentCredential() error = %v", err)
	}
	type result struct {
		credential gateway.AnthropicSubscriptionCredential
		retry      bool
		err        error
	}
	results := make(chan result, 2)
	var start sync.WaitGroup
	start.Add(2)
	for range 2 {
		go func() {
			start.Done()
			start.Wait()
			credential, retry, rotateErr := controller.RotateCredential(
				context.Background(), failed, confirmedSubscriptionLimit(),
			)
			results <- result{credential: credential, retry: retry, err: rotateErr}
		}()
	}
	first, second := <-results, <-results
	for _, got := range []result{first, second} {
		if got.err != nil || !got.retry || got.credential.AccountName != "work" ||
			got.credential.Generation != 2 {
			t.Fatalf("concurrent rotation result = %#v", got)
		}
	}
	if work := getAccountForCLI(t, dbPath, "work"); work.LastError != "" {
		t.Fatalf("stale failure incorrectly marked replacement account: %#v", work)
	}
	if reserve := getAccountForCLI(t, dbPath, "reserve"); reserve.LastUsedAt != "" {
		t.Fatalf("stale failure skipped to reserve account: %#v", reserve)
	}
}

func TestSubscriptionPoolControllerForwardsLimitWhenNoReplacementExists(t *testing.T) {
	t.Parallel()

	controller, _, _ := newControllerTestPool(t, false, "personal")
	failed, err := controller.CurrentCredential(context.Background())
	if err != nil {
		t.Fatalf("CurrentCredential() error = %v", err)
	}
	next, retry, err := controller.RotateCredential(
		context.Background(), failed, confirmedSubscriptionLimit(),
	)
	if err != nil || retry || next != (gateway.AnthropicSubscriptionCredential{}) {
		t.Fatalf("RotateCredential() = %#v, %t, %v; want preserved limit", next, retry, err)
	}
	if got := controller.ActiveAccount(); got != "personal" {
		t.Fatalf("ActiveAccount() = %q, want personal", got)
	}
	notice := receiveControllerNotice(t, controller)
	if !strings.Contains(notice, "no replacement account is usable") ||
		!strings.Contains(notice, "kept Claude Code running") {
		t.Fatalf("no-replacement notice = %q", notice)
	}
}

func TestSubscriptionPoolControllerReadmitsAccountAfterCooldownClears(t *testing.T) {
	t.Parallel()

	controller, s, _ := newControllerTestPool(t, false, "personal", "work")
	personal, _ := controller.CurrentCredential(context.Background())
	work, retry, err := controller.RotateCredential(
		context.Background(), personal, confirmedSubscriptionLimit(),
	)
	if err != nil || !retry || work.AccountName != "work" {
		t.Fatalf("first RotateCredential() = %#v, %t, %v", work, retry, err)
	}
	if clearErr := s.ClearClaudeAccountFailure(context.Background(), "personal"); clearErr != nil {
		t.Fatalf("ClearClaudeAccountFailure() error = %v", clearErr)
	}
	personal, retry, err = controller.RotateCredential(
		context.Background(), work, confirmedSubscriptionLimit(),
	)
	if err != nil || !retry || personal.AccountName != "personal" || personal.Generation != 3 {
		t.Fatalf("second RotateCredential() = %#v, %t, %v", personal, retry, err)
	}
}

func TestSubscriptionPoolControllerHonorsPinnedAccount(t *testing.T) {
	t.Parallel()

	controller, _, dbPath := newControllerTestPool(t, true, "personal", "work")
	failed, _ := controller.CurrentCredential(context.Background())
	_, retry, err := controller.RotateCredential(
		context.Background(), failed, confirmedSubscriptionLimit(),
	)
	if err != nil || retry || controller.ActiveAccount() != "personal" {
		t.Fatalf("pinned rotation retry=%t account=%q error=%v", retry, controller.ActiveAccount(), err)
	}
	if work := getAccountForCLI(t, dbPath, "work"); work.LastUsedAt != "" {
		t.Fatalf("pinned account selected a replacement: %#v", work)
	}
}

func TestSubscriptionPoolControllerRotationWaitHonorsCancellation(t *testing.T) {
	t.Parallel()

	controller, _, _ := newControllerTestPool(t, false, "personal", "work")
	<-controller.rotation
	defer func() { controller.rotation <- struct{}{} }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	failed, _ := controller.CurrentCredential(context.Background())
	_, retry, err := controller.RotateCredential(ctx, failed, confirmedSubscriptionLimit())
	if err == nil || retry {
		t.Fatalf("canceled RotateCredential() retry=%t error=%v", retry, err)
	}
}

func TestSubscriptionPoolControllerReportsNoticeSaturation(t *testing.T) {
	t.Parallel()

	controller, _, _ := newControllerTestPool(t, false, "personal")
	for index := range subscriptionPoolNoticeBuffer {
		controller.notice(fmt.Sprintf("notice-%d", index))
	}
	controller.notice("latest account transition")

	notices := make([]string, 0, subscriptionPoolNoticeBuffer)
	for range subscriptionPoolNoticeBuffer {
		notices = append(notices, <-controller.Notices())
	}
	combined := strings.Join(notices, "\n")
	if strings.Contains(combined, "notice-0") ||
		!strings.Contains(combined, "output delivery was saturated") ||
		!strings.Contains(combined, "latest account transition") {
		t.Fatalf("saturated notices = %q", combined)
	}
}

func TestWaitForClaudeProcessWritesNoticesWithoutStopping(t *testing.T) {
	t.Parallel()

	process := newContinuityTestProcess()
	notices := make(chan string, 1)
	notices <- "account rotated"
	var out strings.Builder
	go func() {
		time.Sleep(10 * time.Millisecond)
		process.Complete(nil)
	}()
	waitErr, stopErr := waitForClaudeProcess(context.Background(), process, notices, &out)
	if waitErr != nil || stopErr != nil {
		t.Fatalf("waitForClaudeProcess() = %v, %v", waitErr, stopErr)
	}
	if process.StopCalls() != 0 || !strings.Contains(out.String(), "account rotated") {
		t.Fatalf("process stops=%d output=%q", process.StopCalls(), out.String())
	}
}

func newControllerTestPool(
	t *testing.T,
	pinned bool,
	names ...string,
) (*subscriptionPoolController, *store.Store, string) {
	t.Helper()
	fixtures := make([]subscriptionAccountFixture, 0, len(names))
	values := make(map[string]string, len(names))
	for _, name := range names {
		token := name + "-oauth-token"
		fixtures = append(fixtures, subscriptionAccountFixture{name: name, token: token})
		values[secret.ClaudeAccountAccessTokenRef(name)] = token
	}
	dbPath := seedSubscriptionAccounts(t, fixtures)
	s, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	launchID, err := s.CreateLaunchWithAuth(
		context.Background(), "", "disabled", "disabled", launchAuthModeSubscriptionPool, names[0],
	)
	if err != nil {
		t.Fatalf("CreateLaunchWithAuth() error = %v", err)
	}
	initial, err := s.GetClaudeAccount(context.Background(), names[0])
	if err != nil {
		t.Fatalf("GetClaudeAccount() error = %v", err)
	}
	controller := newSubscriptionPoolController(
		Dependencies{Secrets: &accountTestSecrets{values: values}},
		selectedClaudeAccount{Account: initial, OAuthToken: names[0] + "-oauth-token"},
		pinned,
	)
	controller.bindLaunch(s, launchID)
	return controller, s, dbPath
}

func confirmedSubscriptionLimit() gateway.AnthropicSubscriptionExhaustionEvent {
	return gateway.AnthropicSubscriptionExhaustionEvent{
		StatusCode:          429,
		RetryAfterDuration:  time.Hour,
		RepresentativeClaim: gateway.AnthropicRateLimitClaimFiveHour,
	}
}

func receiveControllerNotice(t *testing.T, controller *subscriptionPoolController) string {
	t.Helper()
	select {
	case notice := <-controller.Notices():
		return notice
	case <-time.After(time.Second):
		t.Fatal("controller notice was not delivered")
		return ""
	}
}

type continuityTestProcess struct {
	done      chan error
	finish    sync.Once
	mu        sync.Mutex
	stopCalls int
}

func newContinuityTestProcess() *continuityTestProcess {
	return &continuityTestProcess{done: make(chan error, 1)}
}

func (p *continuityTestProcess) PID() int {
	return 9100
}

func (p *continuityTestProcess) Done() <-chan error {
	return p.done
}

func (p *continuityTestProcess) Stop() error {
	p.mu.Lock()
	p.stopCalls++
	p.mu.Unlock()
	p.Complete(nil)
	return nil
}

func (p *continuityTestProcess) Complete(err error) {
	p.finish.Do(func() {
		p.done <- err
		close(p.done)
	})
}

func (p *continuityTestProcess) StopCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopCalls
}
