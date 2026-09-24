package cli

const launchCommandLongHelp = `Launch Claude Code through the local router.

CCR owns --model, --auth-mode, --claude-account, --permission-mode, --print/-p,
and --db. All other options and positional arguments are passed to Claude Code
unchanged unless they would override CCR's selected model, generated model
allowlist, or tool-safety restrictions. For example ccr launch --chrome starts
Claude Code with its Chrome integration.

Use -- to end CCR option parsing and pass Claude Code options, for example:
  ccr launch --model <alias> -p -- --output-format stream-json --verbose
CCR consumes this separator and validates the forwarded options. CCR-owned
options, including --no-history, --no-lifecycle, --no-statusline and --ccr-cua-*
options, must precede it. A second -- is forwarded as Claude Code's literal
prompt boundary: ccr launch -p -- -- --prompt-starting-with-a-dash

Claude's fallback and background modes are rejected because they cannot preserve
CCR's selected route and local gateway ownership. Use CCR-owned detached jobs:
  ccr launch --model <alias> --detach -p --prompt-file prompt.txt
  ccr status <job_id> --json
  ccr cancel <job_id>
Use --submission-id=<persisted-token> to recover the original admission after a
lost receipt; ccr status --submission-id=<token> --json never starts work.
Use --resume=<sid> --expected-parent-job=<job> for a new job in the same registered
session, with --output-format=stream-json --verbose. Resolve its authoritative
head with ccr status --session-id=<sid> --json. Busy or unresolved heads refuse
continuation. CCR session options must precede the option separator.
Detached launch returns a durable job/session/submission receipt. Job status reports cleanup
coverage separately from workload exit; partial coverage does not exclude escape.
Detached Claude options cannot contain another standalone --; use --name=value
for option values and --prompt-file for literal prompt text.

By default, --auth-mode auto uses provider-only local gateway auth when
--model <alias> selects a registered provider-backed model, even if Claude
login is detected. That launch can use the selected provider without asking
Claude Code to authenticate to a subscription. For a first-party Anthropic
model that uses incoming Claude auth, auto preserves the login when available.
With no startup model, auto also preserves a working Claude login so registered
models can be selected from /model alongside first-party models. Without
--model and without Claude auth, CCR fails before starting Claude Code and tells
you which provider alias to select. Use --auth-mode preserve with a working
Claude login when a provider-backed startup model must also retain subscription
routes. CCR never chooses a provider implicitly for first-party Claude requests.
Provider-only startup disables first-party subscription routes; use
--auth-mode preserve with a working login when both route families are needed.

Use --auth-mode provider-only with --model <alias> to force provider-only local
gateway auth. The older spelling --auth-mode gateway-token is accepted for
compatibility.

Use --auth-mode subscription-pool to route first-party model requests through a
registered local Claude account. Claude authenticates only to the loopback
gateway; account OAuth tokens remain in CCR memory. On a confirmed account-wide
first-party quota response, the gateway cools the exhausted account, selects the
next usable account, and retries the same buffered request before returning any
response to Claude. The Claude process, gateway, session, tools, and browser
connection stay open. An explicit --claude-account pins one account. If no
replacement is usable, CCR forwards Anthropic's original limit response and
keeps Claude Code running.

When no status line is configured, CCR adds a launch-only account-aware line
with account=<name> and limits=unknown. An existing statusLine keeps its command
and output. Subscription-pool launches use a launch-only credential-isolation
wrapper: the command can read CCR_CLAUDE_ACCOUNT, but OAuth, API-key, gateway,
refresh, scope, and observer credentials are removed from its environment.
CCR_CLAUDE_ACCOUNT is resolved from the gateway on each status-line invocation,
so it follows in-process account rotation.
Generated launch settings are held in a private temporary file rather than
process arguments and are removed after Claude exits. An explicit statusLine:
null in a higher-precedence Claude settings file remains disabled.
On Windows, CCR visibly falls back to its account-aware line because the POSIX
credential-isolation wrapper is unavailable.
CCR does not use Claude Code's private advisory quota service for routing or
reuse shared-profile values. Run ccr claude-account test <name> --live for an
explicit best-effort quota check. Use --no-statusline only when you
intentionally want to opt out of CCR injection.

Use ccr launch --help for router-specific help. To ask Claude Code for its own
help, use ccr launch -- --help.`
