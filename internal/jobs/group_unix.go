//go:build linux || darwin

package jobs

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// waitGroupEmpty observes signal delivery without reaping the child that pins
// the group ID. The observer is injected so delayed exits can be tested reliably.
func waitGroupEmpty(ctx context.Context, group int, observe func(context.Context, int) Cleanup) Cleanup {
	cleanup := observe(ctx, group)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for cleanup.Coverage == "partial" && len(cleanup.Survivors) > 0 {
		select {
		case <-ctx.Done():
			cleanup.Reason = "process-group cleanup observation interrupted: " + ctx.Err().Error()
			return cleanup
		case <-ticker.C:
			next := observe(ctx, group)
			// A deadline can interrupt ps itself. Keep the last successful
			// snapshot instead of discarding known survivors in that case.
			if ctx.Err() == nil || next.Coverage == "partial" {
				cleanup = next
			}
		}
	}
	return cleanup
}

// Process-table output is diagnostic only. It never supplies signal targets.
func observeGroup(ctx context.Context, group int) Cleanup {
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,pgid=,stat=").Output()
	if err != nil {
		return Cleanup{Coverage: "unknown", Reason: "process-group observation failed"}
	}
	survivors, err := parseGroupSnapshot(string(out), group)
	if err != nil {
		return Cleanup{Coverage: "unknown", Reason: err.Error()}
	}
	return Cleanup{Coverage: "partial", Survivors: survivors, Observed: []string{"launch_pgid_members"}}
}

func parseGroupSnapshot(data string, group int) ([]Survivor, error) {
	if data == "" || len(data) > 4<<20 || !strings.HasSuffix(data, "\n") {
		return nil, fmt.Errorf("empty or truncated process-group observation")
	}
	result := []Survivor{}
	for _, line := range strings.Split(strings.TrimSuffix(data, "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("malformed process-group observation")
		}
		pid, pidErr := strconv.Atoi(fields[0])
		pgid, groupErr := strconv.Atoi(fields[1])
		if pidErr != nil || groupErr != nil || pid < 1 || pgid < 0 {
			return nil, fmt.Errorf("invalid process-group observation")
		}
		if pgid == group && !strings.HasPrefix(fields[2], "Z") {
			result = append(result, Survivor{PID: pid, PGID: pgid})
		}
	}
	return result, nil
}
