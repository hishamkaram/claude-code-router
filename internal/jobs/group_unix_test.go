//go:build linux || darwin

package jobs

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestWaitGroupEmpty(t *testing.T) {
	live := Cleanup{Coverage: "partial", Survivors: []Survivor{{PID: 42, PGID: 42}, {PID: 43, PGID: 42}}, Observed: []string{"launch_pgid_members"}}
	remaining := Cleanup{Coverage: "partial", Survivors: []Survivor{{PID: 43, PGID: 42}}, Observed: []string{"launch_pgid_members"}}
	empty := Cleanup{Coverage: "partial", Survivors: []Survivor{}, Observed: []string{"launch_pgid_members"}}
	failed := Cleanup{Coverage: "unknown", Reason: "process-group observation failed"}
	for _, test := range []struct {
		name      string
		snapshots []Cleanup
		timeout   bool
	}{
		{"delayed exit", []Cleanup{live, remaining, empty}, false},
		{"already empty", []Cleanup{empty}, false},
		{"initial observation failure", []Cleanup{failed}, false},
		{"later observation failure", []Cleanup{live, failed}, false},
		{"deadline preserves latest survivors", []Cleanup{live, remaining}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
				defer cancel()
				calls := 0
				got := waitGroupEmpty(ctx, 42, func(observeCtx context.Context, group int) Cleanup {
					if observeCtx != ctx || group != 42 {
						t.Fatal("observer lost cleanup context or group identity")
					}
					snapshot := test.snapshots[min(calls, len(test.snapshots)-1)]
					calls++
					return snapshot
				})
				want := test.snapshots[len(test.snapshots)-1]
				if test.timeout {
					want.Reason = "process-group cleanup observation interrupted: context deadline exceeded"
				} else if calls != len(test.snapshots) {
					t.Fatalf("observations: got %d, want %d", calls, len(test.snapshots))
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("cleanup: got %+v, want %+v", got, want)
				}
			})
		})
	}
}

func TestWaitGroupEmptyInterruptedObservationKeepsSurvivors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		calls := 0
		got := waitGroupEmpty(ctx, 42, func(observeCtx context.Context, _ int) Cleanup {
			calls++
			if calls == 1 {
				return Cleanup{Coverage: "partial", Survivors: []Survivor{{PID: 43, PGID: 42}}}
			}
			<-observeCtx.Done()
			return Cleanup{Coverage: "unknown", Reason: "process-group observation failed"}
		})
		if got.Coverage != "partial" || len(got.Survivors) != 1 || got.Survivors[0].PID != 43 || !strings.Contains(got.Reason, "deadline exceeded") {
			t.Fatalf("deadline discarded last observed survivors: %+v", got)
		}
	})
}

func TestWaitGroupEmptyCancellationKeepsSurvivors(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	got := waitGroupEmpty(ctx, 42, func(context.Context, int) Cleanup {
		cancel()
		return Cleanup{Coverage: "partial", Survivors: []Survivor{{PID: 42, PGID: 42}}}
	})
	if got.Coverage != "partial" || len(got.Survivors) != 1 || !strings.Contains(got.Reason, "context canceled") {
		t.Fatalf("cancellation discarded last observed survivors: %+v", got)
	}
}
