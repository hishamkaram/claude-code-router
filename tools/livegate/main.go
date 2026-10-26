package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

type testEvent struct {
	Action  string
	Package string
	Test    string
}

type liveContract struct {
	required       []string
	requiredPasses int
	optional       map[string]string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: livegate <full-suite-json-log> <picker-json-log>")
	}
	if err := checkFile(args[0], releaseLiveContract()); err != nil {
		return fmt.Errorf("full live release gate: %w", err)
	}
	if err := checkFile(args[1], pickerLiveContract()); err != nil {
		return fmt.Errorf("repeated picker release gate: %w", err)
	}
	fmt.Println("full live release gate passed: required evidence present; ten picker passes; no unexpected skips")
	return nil
}

func checkFile(path string, contract liveContract) error {
	input, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening live test evidence: %w", err)
	}
	defer func() { _ = input.Close() }()
	return checkEvents(input, contract)
}

func checkEvents(input io.Reader, contract liveContract) error {
	decoder := json.NewDecoder(input)
	passed := make(map[string]int, len(contract.required))
	var failures []string
	for {
		var event testEvent
		if err := decoder.Decode(&event); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("decoding live test evidence: %w", err)
		}
		name := event.Package + "/" + event.Test
		switch event.Action {
		case "pass":
			passed[name]++
		case "fail", "build-fail":
			failures = append(failures, "failed: "+name)
		case "skip":
			if event.Test != "" && contract.optional[name] == "" {
				failures = append(failures, "unexpected skip: "+name)
			}
		}
	}
	for _, name := range contract.required {
		if count := passed[name]; count != contract.requiredPasses {
			failures = append(failures, fmt.Sprintf("passing evidence count for %s: got %d, want %d", name, count, contract.requiredPasses))
		}
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return fmt.Errorf("%s", strings.Join(failures, "\n"))
	}
	return nil
}

func releaseLiveContract() liveContract {
	const root = "github.com/hishamkaram/claude-code-router/internal/"
	const cli = root + "cli/"
	return liveContract{
		requiredPasses: 1,
		required: []string{
			cli + "TestLiveLaunchModelPickerShowsAnthropicAndRegisteredModels",
			cli + "TestLiveDetachedClaudeContinuation",
			cli + "TestLiveDetachedClaudeJobs",
			cli + "TestLiveClaudeConformanceMatrix",
			cli + "TestLiveFixtureMatrix",
			root + "jobs/TestLiveDirectChildMigrationFinishesWithoutExternalSignal",
			root + "jobs/TestLiveLostBusDoesNotSignalMigratedChild",
			root + "jobs/TestLiveScopeContainment",
		},
		optional: map[string]string{
			cli + "TestLiveRealClaudeAccountDiagnostics":                                   "requires a real registered subscription account",
			cli + "TestLiveHTTP2BuiltBinary":                                               "requires an explicitly selected protected deployment",
			cli + "TestLiveHTTP2BuiltBinaryCancellation":                                   "requires an explicitly selected protected deployment",
			cli + "TestLiveConfiguredProviderIdleTurn":                                     "requires configured real-provider credentials",
			cli + "TestLiveLaunchNoStartupModelCanSelectConfiguredOpenAIAlias":             "requires native first-party authentication",
			cli + "TestLiveLaunchPreserveAuthRoutesThroughFakeAnthropicCompatibleProvider": "requires native first-party authentication",
			cli + "TestLiveLocalRealConfiguredVision":                                      "opt-in paid real-provider vision matrix",
			cli + "TestLiveLocalRealAnthropicCUA":                                          "opt-in paid Anthropic computer use",
			cli + "TestLiveLocalRealOpenAIResponsesCUA":                                    "opt-in paid OpenAI computer use",
			cli + "TestLiveLocalRealCUAExecutors":                                          "opt-in native executor permissions and paid provider",
			cli + "TestLiveRealProviderMatrix":                                             "requires configured real-provider credentials",
			cli + "TestLiveConfiguredProviderAutoModeAgentWebFetch":                        "requires configured real-provider credentials",
			cli + "TestLiveConfiguredProviderAutoModeWorkflow":                             "requires configured real-provider credentials",
			cli + "TestLiveLocalRealSubscriptionPoolAccount":                               "opt-in real subscription account",
			root + "cua/executor/TestDockerBrowserSmoke":                                   "covered by the separate required Docker fixture job",
			root + "gateway/TestLiveHTTP2IdleClaudeCLI":                                    "opt-in protected HTTP2 idle deployment",
		},
	}
}

func pickerLiveContract() liveContract {
	return liveContract{
		required:       []string{"github.com/hishamkaram/claude-code-router/internal/cli/TestLiveLaunchModelPickerShowsAnthropicAndRegisteredModels"},
		requiredPasses: 10,
	}
}
