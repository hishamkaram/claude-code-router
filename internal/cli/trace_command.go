package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

type traceUsageView struct {
	Observed         bool  `json:"observed"`
	InputTokens      int64 `json:"input_tokens,omitempty"`
	OutputTokens     int64 `json:"output_tokens,omitempty"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
}

type traceRouteView struct {
	RequestID      string         `json:"request_id"`
	Operation      string         `json:"operation"`
	RequestedModel string         `json:"requested_model"`
	RouteKind      string         `json:"route_kind,omitempty"`
	ModelAlias     string         `json:"model_alias,omitempty"`
	ProviderName   string         `json:"provider_name,omitempty"`
	ProviderModel  string         `json:"provider_model,omitempty"`
	Protocol       string         `json:"protocol,omitempty"`
	Streaming      bool           `json:"streaming"`
	Tools          bool           `json:"tools"`
	Thinking       bool           `json:"thinking"`
	Status         string         `json:"status"`
	HTTPStatus     int            `json:"http_status,omitempty"`
	ErrorClass     string         `json:"error_class,omitempty"`
	LatencyMS      int64          `json:"latency_ms"`
	Usage          traceUsageView `json:"usage"`
}

type traceLifecycleView struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	ExternalID string `json:"external_id,omitempty"`
	ActorName  string `json:"actor_name,omitempty"`
	ActorKind  string `json:"actor_kind,omitempty"`
	TeamName   string `json:"team_name,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type traceEventView struct {
	ID          int64               `json:"id"`
	Kind        string              `json:"kind"`
	LaunchID    int64               `json:"launch_id"`
	SessionID   int64               `json:"session_id,omitempty"`
	Status      string              `json:"status"`
	OccurredAt  string              `json:"occurred_at"`
	CompletedAt string              `json:"completed_at,omitempty"`
	Route       *traceRouteView     `json:"route,omitempty"`
	Lifecycle   *traceLifecycleView `json:"lifecycle,omitempty"`
}

type traceDocument struct {
	SchemaVersion int              `json:"schema_version"`
	Events        []traceEventView `json:"events"`
}

type traceOptions struct {
	launchID        int64
	claudeSessionID string
	since           string
	limit           int
	follow          bool
	jsonOutput      bool
}

func newTraceCommand(ctx context.Context, opts *options) *cobra.Command {
	traceOpts := traceOptions{limit: 50}
	cmd := &cobra.Command{
		Use:   "trace",
		Short: "Show redacted route and lifecycle history",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTrace(ctx, cmd, opts, traceOpts)
		},
	}
	cmd.Flags().Int64Var(&traceOpts.launchID, "launch", 0, "Filter by launch ID")
	cmd.Flags().StringVar(&traceOpts.claudeSessionID, "session", "", "Filter by Claude session ID")
	cmd.Flags().StringVar(&traceOpts.since, "since", "", "Show events since a duration or RFC3339 timestamp")
	cmd.Flags().IntVar(&traceOpts.limit, "limit", 50, "Maximum events to return (1-1000)")
	cmd.Flags().BoolVar(&traceOpts.follow, "follow", false, "Follow new events until canceled")
	cmd.Flags().BoolVar(&traceOpts.jsonOutput, "json", false, "Emit schema-versioned JSON")
	cmd.AddCommand(newTracePurgeCommand(ctx, opts))
	return cmd
}

func runTrace(ctx context.Context, cmd *cobra.Command, opts *options, traceOpts traceOptions) error {
	if traceOpts.launchID < 0 {
		return fmt.Errorf("--launch must be positive")
	}
	if traceOpts.limit < 1 || traceOpts.limit > 1000 {
		return fmt.Errorf("--limit must be between 1 and 1000")
	}
	since, err := parseTimeSelector(traceOpts.since, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("invalid --since: %w", err)
	}
	s, _, err := openMigratedStore(ctx, opts)
	if err != nil {
		return err
	}
	defer closeStore(s)
	filter := store.TraceFilter{
		LaunchID: traceOpts.launchID, ClaudeSessionID: traceOpts.claudeSessionID,
		Since: since, Limit: traceOpts.limit,
	}
	events, err := s.ListTraceEvents(ctx, filter)
	if err != nil {
		return err
	}
	if traceOpts.follow {
		slices.Reverse(events)
		if err := writeFollowTraceEvents(cmd, events, traceOpts.jsonOutput, &filter.AfterID); err != nil {
			return err
		}
		filter.OldestFirst = true
		return followTrace(ctx, cmd, s, filter, traceOpts.jsonOutput)
	}
	views := newTraceEventViews(events)
	if traceOpts.jsonOutput {
		if err := writeVersionedJSON(cmd.OutOrStdout(), traceDocument{SchemaVersion: 1, Events: views}); err != nil {
			return err
		}
	} else {
		writeHumanTrace(cmd, views)
	}
	return nil
}

func followTrace(ctx context.Context, cmd *cobra.Command, s *store.Store, filter store.TraceFilter, jsonOutput bool) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := drainFollowTrace(ctx, cmd, s, &filter, jsonOutput); err != nil {
				return err
			}
		}
	}
}

func drainFollowTrace(ctx context.Context, cmd *cobra.Command, s *store.Store, filter *store.TraceFilter, jsonOutput bool) error {
	for {
		events, err := s.ListTraceEvents(ctx, *filter)
		if err != nil {
			return err
		}
		if err := writeFollowTraceEvents(cmd, events, jsonOutput, &filter.AfterID); err != nil {
			return err
		}
		if len(events) < filter.Limit {
			return nil
		}
	}
}

func writeFollowTraceEvents(cmd *cobra.Command, events []store.TraceEvent, jsonOutput bool, afterID *int64) error {
	for index := range events {
		event := &events[index]
		if event.ID > *afterID {
			*afterID = event.ID
		}
		view := newTraceEventView(*event)
		if !jsonOutput {
			writeHumanTraceEvent(cmd, view)
			continue
		}
		payload := struct {
			SchemaVersion int            `json:"schema_version"`
			Event         traceEventView `json:"event"`
		}{SchemaVersion: 1, Event: view}
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(payload); err != nil {
			return fmt.Errorf("writing trace stream JSON: %w", err)
		}
	}
	return nil
}

func newTracePurgeCommand(ctx context.Context, opts *options) *cobra.Command {
	var before string
	var all bool
	var yes bool
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "purge",
		Short: "Delete redacted trace history",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if all == (strings.TrimSpace(before) != "") {
				return fmt.Errorf("set exactly one of --before or --all")
			}
			if all && !yes {
				return fmt.Errorf("--all requires --yes")
			}
			cutoff, err := parseTimeSelector(before, time.Now().UTC())
			if err != nil {
				return fmt.Errorf("invalid --before: %w", err)
			}
			s, _, err := openMigratedStore(ctx, opts)
			if err != nil {
				return err
			}
			defer closeStore(s)
			deleted, err := s.PurgeEvents(ctx, cutoff, all)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeVersionedJSON(cmd.OutOrStdout(), struct {
					SchemaVersion int   `json:"schema_version"`
					Deleted       int64 `json:"deleted"`
				}{SchemaVersion: 1, Deleted: deleted})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted %d trace events.\n", deleted)
			return nil
		},
	}
	cmd.Flags().StringVar(&before, "before", "", "Delete events before a duration or RFC3339 timestamp")
	cmd.Flags().BoolVar(&all, "all", false, "Delete all trace events")
	cmd.Flags().BoolVar(&yes, "yes", false, "Confirm deletion of all trace events")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit schema-versioned JSON")
	return cmd
}

func parseTimeSelector(value string, now time.Time) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if strings.HasSuffix(value, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(value, "d"))
		if err != nil || days <= 0 {
			return "", fmt.Errorf("day duration must be a positive integer")
		}
		return now.Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339Nano), nil
	}
	if duration, err := time.ParseDuration(value); err == nil {
		if duration <= 0 {
			return "", fmt.Errorf("duration must be positive")
		}
		return now.Add(-duration).Format(time.RFC3339Nano), nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", fmt.Errorf("expected a duration such as 24h, days such as 30d, or RFC3339")
	}
	return parsed.UTC().Format(time.RFC3339Nano), nil
}

func newTraceEventViews(events []store.TraceEvent) []traceEventView {
	views := make([]traceEventView, 0, len(events))
	for index := range events {
		views = append(views, newTraceEventView(events[index]))
	}
	return views
}

func newTraceEventView(event store.TraceEvent) traceEventView {
	view := traceEventView{
		ID: event.ID, Kind: event.Kind, LaunchID: event.LaunchID,
		SessionID: event.SessionID, Status: event.Status,
		OccurredAt: event.OccurredAt, CompletedAt: event.CompletedAt,
	}
	switch event.Kind {
	case "route":
		route := newTraceRouteView(event.Route)
		view.Route = &route
	case "lifecycle":
		lifecycle := traceLifecycleView{
			Name: event.Lifecycle.Name, Status: event.Lifecycle.Status,
			ExternalID: event.Lifecycle.ExternalID, ActorName: event.Lifecycle.ActorName,
			ActorKind: event.Lifecycle.ActorKind, TeamName: event.Lifecycle.TeamName,
			Reason: event.Lifecycle.Reason,
		}
		view.Lifecycle = &lifecycle
	}
	return view
}

func newTraceRouteView(route store.RouteEvent) traceRouteView {
	return traceRouteView{
		RequestID: route.RequestID, Operation: route.Operation,
		RequestedModel: route.RequestedModel, RouteKind: route.RouteKind,
		ModelAlias: route.ModelAlias, ProviderName: route.ProviderName,
		ProviderModel: route.ProviderModel, Protocol: route.Protocol,
		Streaming: route.Streaming, Tools: route.Tools, Thinking: route.Thinking,
		Status: route.Status, HTTPStatus: route.HTTPStatus, ErrorClass: route.ErrorClass,
		LatencyMS: route.LatencyMS,
		Usage: traceUsageView{
			Observed: route.Usage.Observed, InputTokens: route.Usage.InputTokens,
			OutputTokens: route.Usage.OutputTokens, CacheReadTokens: route.Usage.CacheReadTokens,
			CacheWriteTokens: route.Usage.CacheWriteTokens,
		},
	}
}

func writeHumanTrace(cmd *cobra.Command, events []traceEventView) {
	if len(events) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No trace events found.")
		return
	}
	for _, event := range events {
		writeHumanTraceEvent(cmd, event)
	}
}

func writeHumanTraceEvent(cmd *cobra.Command, event traceEventView) {
	if event.Route != nil {
		route := event.Route
		usage := "tokens=unavailable"
		if route.Usage.Observed {
			usage = fmt.Sprintf("tokens=in:%d out:%d cache-read:%d cache-write:%d",
				route.Usage.InputTokens, route.Usage.OutputTokens,
				route.Usage.CacheReadTokens, route.Usage.CacheWriteTokens)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%d\t%s\troute\t%s\t%s/%s\tstatus=%s http=%d latency=%dms %s\n",
			event.ID, event.OccurredAt, route.ModelAlias, route.ProviderName,
			route.ProviderModel, route.Status, route.HTTPStatus, route.LatencyMS, usage)
		return
	}
	if event.Lifecycle != nil {
		lifecycle := event.Lifecycle
		fmt.Fprintf(cmd.OutOrStdout(), "%d\t%s\tlifecycle\t%s\tstatus=%s external=%s actor=%s\n",
			event.ID, event.OccurredAt, lifecycle.Name, lifecycle.Status,
			lifecycle.ExternalID, lifecycle.ActorName)
	}
}
