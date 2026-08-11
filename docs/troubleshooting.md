# Troubleshooting

Start with local diagnostics:

```bash
ccr doctor
ccr status
ccr trace --since 30m
ccr provider list
ccr model list
```

## Claude Code Is Unavailable

`ccr doctor` reports whether the `claude` binary is available. Install Claude
Code, ensure it is on your `PATH`, then sign in before using first-party routes.

## Claude Subscription Needs Relogin

When a first-party Claude request returns an authentication failure, CCR keeps
registered provider aliases available. Check the current state and recovery
action with:

```bash
ccr status
ccr model list
```

Run `claude /login` and relaunch. For a subscription-pool launch, repair the
selected account explicitly:

```bash
ccr claude-account refresh <name> --from current
```

The first-party rows remain visible in `/model` and are marked as requiring
re-login after the failure. Select a registered alias such as
`/model anthropic.ccr.<alias>` to continue through its configured provider;
CCR never silently falls back from a failed Claude route.

## macOS Prints a MallocStackLogging Warning

A message such as the following is a macOS malloc diagnostic, not a CCR routing
failure:

```text
ccr(12345) MallocStackLogging: can't turn off malloc stack logging because it was not enabled.
```

macOS discovers stack-logging configuration by scanning the process environment
for `MallocStackLogging` variables. Setting a flag to `0` can still trigger that
initialization; remove the variable to disable it:

```bash
env | grep '^MallocStackLogging'
unset MallocStackLogging MallocStackLoggingNoCompact MallocStackLoggingDirectory
```

Remove matching exports from the shell or terminal profile that starts CCR,
then open a new terminal. The process name and PID in the warning identify the
process in which macOS printed the diagnostic; they do not mean CCR detected a
Go allocation failure. If no matching variables are present, reproduce from a
new terminal without memory-debugging or command-integration tooling enabled.

## A Provider Cannot Connect

Validate the configured provider and model separately:

```bash
ccr provider test <provider>
ccr model test <alias>
```

Check that the API-key environment variable is present in the shell that starts
CCR, or that the configured key file is a regular file with mode `0600`. Do not
print the key while debugging.

Use bounded live diagnostics when configuration checks pass but routing fails:

```bash
ccr doctor --live
ccr doctor --live --all
ccr conformance run <alias>
ccr conformance run --all
```

## The Desired Model Is Missing from `/model`

Run:

```bash
ccr model list
ccr status
ccr launch
```

A normal no-model launch preserves Claude Code's startup model and subscription
authentication, then adds configured, non-blocked routable aliases that are safe for a
tools-enabled session to the visual picker. It also prints their model IDs:

```text
/model anthropic.ccr.<alias>
```

An alias with a one-million-token effective context window is printed as:

```text
/model anthropic.ccr.<alias>[1m]
```

You can switch back to `opus`, `sonnet`, or another subscription model in the
same session. If an alias is absent, check that it is not `blocked`, that its
provider still exists, and that the provider protocol is Anthropic-compatible
or OpenAI-compatible. If it is `chat-only` or its provider mode disables tools,
start directly with `ccr launch --model <alias>`.

Claude Code organization policy can still restrict the model picker. CCR cannot
bypass that policy; use an allowed default model or ask the organization
administrator to permit the needed model option.

`--auth-mode gateway-token` requires `--model <alias>` and lets Claude Code
authenticate to CCR's `/v1/models` endpoint for discovery metadata. That mode
intentionally disables the original subscription and API-key authentication;
do not use it when first-party subscription routes must remain available.
This includes current Claude Code auto-mode safety classification for some
Agent and Workflow actions. Use `--auth-mode preserve`; CCR does not reroute or
bypass a safety classifier that cannot reach its required Anthropic model.

## The Picker Shows the Wrong Context Window

Inspect the effective value and where it came from:

```bash
ccr model show <alias> --json
ccr model refresh <alias>
```

If discovery is unavailable or incorrect, set a reviewed local override:

```bash
ccr model update <alias> --context-window 1000000
```

Relaunch Claude Code after changing capabilities because the picker allowlist is
created once per launch. Context below one million tokens intentionally has no
`[1m]` suffix. Use `--context-window 0` to clear the override.

For LiteLLM, a warning such as `LiteLLM capability metadata unavailable: HTTP
403 Forbidden` means optional `/model/info` metadata was unavailable. If the
alias line says `refreshed`, the refresh still completed using discovery,
retained facts, and local overrides.

## Doctor Reports a Live Failure

Doctor now prints the failed check, failure kind, safe gateway/provider HTTP
statuses, and an `action:` command. Aliases excluded from routing, such as a
provider control row or a Responses-only alias whose provider lacks
`--responses`, are marked skipped and are not sent to the provider. They are
also excluded from `/model` and aggregate conformance. Authentication and
provider HTTP failures point to `ccr provider test <provider>`, while missing
models point to fresh discovery. Provider response bodies and credentials are
never included in the diagnosis.

## Conformance Fails on Forced Tool Choice

When model metadata leaves tool choice unknown, `ccr conformance run` probes it
instead of assuming support. A provider that ignores a forced tool request
causes the `forced_tool` check to fail and CCR does not silently change the
model's compatibility or capabilities.

Review the effective facts first:

```bash
ccr model show <alias> --json
```

If you have verified that the provider does not support forced tool choice,
record that limitation explicitly, rerun conformance, and relaunch Claude Code
before relying on the alias in `/model`:

```bash
ccr model update <alias> --tool-choice false
ccr conformance run <alias>
ccr launch
```

Use `--tool-choice auto` to clear the override after provider metadata or
capability support changes.

## CCR Starts on an Unexpected Model

Without `--model`, CCR intentionally leaves Claude Code on its configured
startup model. Use `/model` to switch after launch, or pass an explicit startup
alias:

```bash
ccr launch --model <alias>
```

Confirm the actual route from another terminal with `ccr status` or
`ccr trace --follow`. Generated text claiming a model identity is not routing
evidence.

## Sessions or Agents Are Missing

Run `ccr sessions` and inspect the launch's observation state. CCR injects
launch-only lifecycle hooks while preserving existing hooks, but managed policy
may block the injected HTTP callbacks. Such a launch is reported as unobserved.
It is not represented as an observed session with zero agents.

`--no-lifecycle` deliberately disables this state. `--no-history` only disables
route history; it does not disable lifecycle observation. An unfinished session,
agent, or task is marked abandoned when Claude Code exits abruptly.

## The CCR Status Line Is Missing

CCR keeps an existing Claude Code status line's command and output. In
subscription-pool mode, a launch-only credential-isolation wrapper removes
OAuth, API-key, gateway, refresh, scope, and observer credentials before
execution; `CCR_CLAUDE_ACCOUNT` remains available for the selected local label.
On Windows, CCR visibly bypasses the existing command for that launch and uses
its account-aware line because the POSIX credential-isolation wrapper is
unavailable. CCR also injects that line when no status line is configured; it
shows the selected `account=<name>` and truthful `limits=unknown`. An explicit
`statusLine: null` in a higher-precedence Claude settings file remains disabled.
Generated launch settings use a private temporary file and are removed after
Claude exits; the existing command is never copied into process arguments. Run
`ccr status` to inspect the same route and selected launch account outside
Claude Code. Also check that the launch was not started with `--no-statusline`.

Claude Code's built-in usage display or an existing status line may use another
local profile or cached usage data. CCR cannot query fresh per-account
subscription quota with a Claude Code OAuth token. It therefore never labels
those numbers as belonging to the selected pool account.

## Remove Runtime History

Route and lifecycle history is redacted and bounded, but can be removed
explicitly:

```bash
ccr trace purge --all --yes
```

Start a one-off launch with `--no-history` when no route events should be
persisted. Prompts, responses, tool arguments, transcript paths, raw hook bodies,
authorization headers, and provider secret values are never part of history.
The default metadata retention window is 30 days and 10,000 combined route and
lifecycle events.

## Vision or Computer Use Is Rejected

Inspect the selected alias:

```bash
ccr model show <alias> --json
```

If the effective facts do not show image input or computer-use support, CCR
rejects the request before provider submission. Add a reviewed override only
when you have provider evidence. For managed computer use, also verify the
provider is OpenAI-compatible and Responses-capable, the model has effective
Responses and computer-use support, and the launch selected the intended
executor. OpenAI Responses computer use always requires that managed executor;
it is not delegated to Claude Code's client-managed tool loop. External managed
CUA requires a public HTTPS base URL with no
credentials, query, fragment, or redirects, plus
`--ccr-cua-external-token-env`. The macOS helper is source-built only, unsigned,
not packaged in Homebrew or release archives, and must be on `PATH` as
`ccr-cua-macos`. It requires Accessibility and Screen Recording permission for
the launching process; restart it after granting permissions. Prefer Docker for
reproducible local runs.

If a Responses provider returns `pending_safety_checks`, CCR returns a visible
rejection before running the executor. This prevents silent
`acknowledged_safety_checks` until those checks can be displayed in the approval
flow.

## `/compact` Does Not Reduce Context on an OpenAI-Compatible Alias

Claude Code compaction uses Anthropic `context_management` edits. CCR rejects
compaction edits on OpenAI-compatible routes instead of silently forwarding the
pre-compact transcript, because that would make the selected provider keep
seeing stale context. Use a first-party or Anthropic-compatible route for
sessions where `/compact` must work.

## First-Party Subscription Authentication Fails

Use the default `--auth-mode preserve` and verify the ordinary `claude` CLI is
signed in. `gateway-token` intentionally disables original Anthropic
subscription and API-key authentication, so it cannot use a first-party route.

## Claude Account Import Has No Linux Keychain

`--oauth-token-stdin` controls how the token enters CCR, but account credentials
still require an OS secret store. Headless Linux installations need a Secret
Service implementation such as GNOME Keyring:

```bash
sudo apt install gnome-keyring libsecret-tools
```

If the `login` collection already exists but is locked, unlock it with its
existing password:

```bash
read -rsp "Keyring password: " K; echo; printf '%s' "$K" | gnome-keyring-daemon --unlock; unset K
```

For a new, dedicated headless environment with no existing keyring data, start
one daemon and initialize its collection in the same shell:

```bash
pkill -u "$USER" -f '^gnome-keyring-daemon ' 2>/dev/null || true
read -rsp "New keyring password: " K; echo; eval "$(printf %s "$K" | gnome-keyring-daemon --login --components=secrets)"; eval "$(gnome-keyring-daemon --start --components=secrets)"; export GNOME_KEYRING_CONTROL; unset K
printf probe | secret-tool store --label=CCR service ccr-bootstrap account probe && secret-tool clear service ccr-bootstrap account probe
```

Do not stop or replace the keyring daemon on a desktop session or a machine
with existing keyring data. Use the desktop keyring manager or login password
to unlock that collection instead. CCR checks keychain availability before
reading stdin, so a failed preflight does not consume or persist the token.

## Claude Subscription Pool Has No Usable Account

Inspect local account state:

```bash
ccr claude-account list
ccr claude-account show <name>
ccr claude-account test <name>
ccr claude-account test --all --live
```

`list` and `show` report redacted metadata only. A status of `disabled`,
`expired`, or `cooldown` makes the account ineligible for automatic selection.
Enable or replace credentials explicitly:

```bash
ccr claude-account enable <name>
ccr claude-account clear-cooldown <name>
ccr claude-account clear-cooldown --all
ccr claude-account refresh <name> --from current
claude setup-token
ccr claude-account refresh <name> --oauth-token-stdin
```

`--from current` works on Linux and Windows when the current Claude credentials
file exists and has safe permissions. On macOS it is unsupported because Claude
stores the active login in Keychain; use `claude setup-token` and
`--oauth-token-stdin`.

CCR account names are local labels, not identities returned by Anthropic. If
several labels appear to use the same subscription, each setup token may have
authorized the same browser account. Generate a fresh token and select the
intended account in the setup-token OAuth flow before refreshing each label:

```bash
claude setup-token
ccr claude-account refresh <name> --oauth-token-stdin
```

`test --all --live` reports a non-reversible identity fingerprint for each
token and fails when fingerprints are duplicated. It also reports known
five-hour, seven-day, and model-specific utilization and reset windows. The
probe is advisory and can fail when Claude Code's private usage service changes;
pool routing does not depend on it.

An HTTP 403 from the private profile service does not prove that a setup token
is invalid for model requests. Some inference-valid tokens cannot access the
advisory profile endpoint, and the usage endpoint may be independently rate
limited. In that case CCR reports the diagnostic failure without changing
account state. Use a pinned `--claude-account <name> --print` request to verify
that exact identity, and rely on the gateway's first-party quota response for
cooldown classification.

`claude setup-token` does not replace the CLI's saved login. That shared login
can remain visible in the UI for every launch while model requests use the
selected label's stored token.

If every account is disabled, expired, cooling down, or has an unavailable
keychain credential, `ccr launch --auth-mode subscription-pool` fails visibly.
CCR does not silently fall back to your default Claude login or an Anthropic API
key.

Current CCR versions distinguish account-wide subscription claims from model
limits with fallback available and unclassified rejections. Only confirmed
account-wide claims create a quota cooldown; model-specific, unknown, and
ambiguous limits remain with Claude Code.
`claude-account list` shows `until=<timestamp>` and `reason=<class>` for every
active cooldown. Schema v8 automatically clears only the legacy generic
`rate_limited` marker so the next rejection can be classified accurately. For
any other state you have independently verified as stale, use `clear-cooldown`;
it does not contact Anthropic or change keychain credentials, expiry, or
enablement.

## Subscription Pool Did Not Rotate

Automatic in-gateway rotation requires a first-party Anthropic `/v1/messages`
HTTP 429
whose `anthropic-ratelimit-unified-status` response header is `rejected`.
A recognized representative account-wide claim must also be present, and
Anthropic must report no model fallback. Model-specific, unknown, token-count,
registered-provider, and temporary or ambiguous limits are left to Claude Code
without cooling or rotating an account.

```bash
ccr launch --auth-mode subscription-pool
```

Rotation happens inside the existing gateway and is independent of interactive,
`--print`, resume, worktree, prompt, or managed CUA launch shape. An explicit
`--claude-account` intentionally pins that account. Launch stderr reports the
decision.

```bash
ccr launch --auth-mode subscription-pool --claude-account other
```

On rotation, only outbound first-party OAuth authorization changes. CCR closes
the rejected response and retries the already-buffered request before Claude
sees it. The Claude PID, session, pending turn, tools, browser extension
connection, and gateway do not restart. If all replacements are unavailable,
CCR forwards Anthropic's original limit response and leaves Claude running.
Inspect `ccr claude-account list` for disabled, expired, cooling, duplicate, or
credential-unavailable accounts.

## Remove a Claude Subscription Account

Removal deletes the account's OS keychain credentials before deleting its
SQLite metadata:

```bash
ccr claude-account remove <name> --yes
```

The `--yes` confirmation is required. If keychain cleanup fails, CCR retains the
metadata and refs so the cleanup can be retried.

## Web Search Reports No Results

`WebSearch` and `WebFetch` are run by the Claude Code host, not by CCR or the
selected model provider. A zero-result search or a site fetch failure is usually
a host-service or target-site result. Configure a dedicated MCP search provider
when you need separate search behavior.

## Reset or Isolate Local State

`ccr init` prints the database path. Use `--db` to isolate a test or a separate
configuration without deleting your normal state:

```bash
ccr --db /tmp/ccr-test.db init
ccr --db /tmp/ccr-test.db provider list
```

Back up a database before manually removing it. The database contains provider
and session metadata, but not raw provider credentials.
