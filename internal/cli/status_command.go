package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/hishamkaram/claude-code-router/internal/store"
)

type statusDocument struct {
	SchemaVersion int                 `json:"schema_version"`
	Database      string              `json:"database"`
	StoreSchema   int                 `json:"store_schema"`
	Providers     []providerView      `json:"providers"`
	Models        []modelView         `json:"models"`
	LatestLaunch  *launchView         `json:"latest_launch,omitempty"`
	Sessions      []claudeSessionView `json:"sessions"`
	LastRoute     *traceRouteView     `json:"last_route,omitempty"`
}

func newStatusCommand(ctx context.Context, opts *options) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show configuration and the latest runtime route",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			document, err := loadStatusDocument(ctx, opts)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeVersionedJSON(cmd.OutOrStdout(), document)
			}
			writeHumanStatus(cmd, document)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit schema-versioned JSON")
	return cmd
}

func loadStatusDocument(ctx context.Context, opts *options) (statusDocument, error) {
	s, dbPath, err := openMigratedStore(ctx, opts)
	if err != nil {
		return statusDocument{}, err
	}
	defer closeStore(s)
	document := statusDocument{
		SchemaVersion: 1, Database: dbPath,
		Providers: []providerView{}, Models: []modelView{}, Sessions: []claudeSessionView{},
	}
	if document.StoreSchema, err = s.SchemaVersion(ctx); err != nil {
		return statusDocument{}, err
	}
	providers, err := s.ListProviders(ctx)
	if err != nil {
		return statusDocument{}, err
	}
	for index := range providers {
		document.Providers = append(document.Providers, newProviderView(providers[index]))
	}
	models, err := s.ListModels(ctx)
	if err != nil {
		return statusDocument{}, err
	}
	for index := range models {
		document.Models = append(document.Models, newModelView(models[index]))
	}
	return loadLatestRuntimeStatus(ctx, s, document)
}

func loadLatestRuntimeStatus(ctx context.Context, s *store.Store, document statusDocument) (statusDocument, error) {
	launches, err := s.ListLaunches(ctx)
	if err != nil {
		return statusDocument{}, err
	}
	if len(launches) == 0 {
		return document, nil
	}
	latest := newLaunchView(launches[0])
	document.LatestLaunch = &latest
	sessions, err := s.ListClaudeSessions(ctx, latest.ID, false)
	if err != nil {
		return statusDocument{}, err
	}
	for index := range sessions {
		document.Sessions = append(document.Sessions, newClaudeSessionView(sessions[index]))
	}
	events, err := s.ListTraceEvents(ctx, store.TraceFilter{
		LaunchID: latest.ID, Kind: "route", Limit: 1,
	})
	if err != nil {
		return statusDocument{}, err
	}
	if len(events) == 1 {
		route := newTraceRouteView(events[0].Route)
		document.LastRoute = &route
	}
	return document, nil
}

func writeHumanStatus(cmd *cobra.Command, document statusDocument) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Database: %s (schema=%d)\n", document.Database, document.StoreSchema)
	fmt.Fprintf(out, "Providers: %d\nModels: %d\n", len(document.Providers), len(document.Models))
	if document.LatestLaunch == nil {
		fmt.Fprintln(out, "Latest launch: none")
	} else {
		launch := document.LatestLaunch
		fmt.Fprintf(out, "Latest launch: %d state=%s process=%s lifecycle=%s statusline=%s\n",
			launch.ID, launch.State, launch.ProcessState, launch.LifecycleState, launch.StatuslineState)
	}
	if document.LastRoute == nil {
		fmt.Fprintln(out, "Last route: none observed")
	} else {
		route := document.LastRoute
		fmt.Fprintf(out, "Last route: alias=%s provider=%s model=%s protocol=%s status=%s\n",
			route.ModelAlias, route.ProviderName, route.ProviderModel, route.Protocol, route.Status)
	}
	for _, provider := range document.Providers {
		tokenCount := "estimated"
		if provider.SupportsCountTokens {
			tokenCount = "provider"
		}
		fmt.Fprintf(out, "Provider %s: type=%s protocol=%s mode=%s token-count=%s\n",
			provider.Name, provider.Type, provider.Protocol, provider.Mode, tokenCount)
	}
	for _, model := range document.Models {
		fmt.Fprintf(out, "Model %s: provider=%s model=%s compat=%s\n",
			model.Alias, model.ProviderName, model.ProviderModel, model.Compatibility)
	}
}
