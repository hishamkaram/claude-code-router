# Security Policy

## Supported Versions

Security fixes are made on the current `main` branch and released in the next
appropriate version.

## Reporting a Vulnerability

Please use GitHub's private vulnerability-reporting flow for this repository:

<https://github.com/hishamkaram/claude-code-router/security/advisories/new>

Do not open a public issue for a suspected vulnerability. Include a clear impact
description, reproduction steps, affected version or commit, and any suggested
mitigation. Never include API keys, OAuth tokens, local session tokens, or other
credentials in the report.

You can expect an acknowledgement within seven days. We will coordinate a fix,
release, and disclosure timeline with the reporter where appropriate.

## Automated Protections

GitHub repository settings own dependency alerts, security-fix PRs, secret
scanning, push protection, and merge enforcement. The maintainer
(@hishamkaram) owns these settings and alert triage. Checked-in workflows own
scan execution; a successful scan does not mean the application has no flaws.

- Dependabot checks Go modules, GitHub Actions, and the managed-browser
  Docker image reference weekly. Security-fix PRs can open as soon as GitHub
  identifies a fix. Minor and patch version updates are grouped; majors and
  security fixes remain separate. Updates require review and passing CI;
  automatic merging is disabled.
- Dependency review blocks pull requests introducing high/critical dependency
  vulnerabilities. Existing strict `govulncheck` remains required through CI.
- CodeQL scans Go, browser JavaScript, Python, GitHub Actions, and the Swift
  helper on pull requests, main pushes, and weekly. Main's ruleset requires
  CodeQL results and blocks new high/critical security findings.
- Repository secret scanning and push protection detect supported credential
  patterns. A detected real credential must be revoked or rotated before
  cleanup; deleting a file alone does not invalidate the credential.
- OpenSSF Scorecard publishes weekly and on main pushes, with reports in
  GitHub code scanning, workflow artifacts, and the public badge. Its numeric
  score is advisory and does not block merging.

The maintainer investigates alerts, prioritizes confirmed high/critical impact,
and records any false-positive dismissal with a specific reason. Do not dismiss
alerts, add vulnerability allowlists, or bypass required checks merely to merge.
Use private reporting for sensitive findings; public PRs must omit exploit
details and credentials.

Dependabot's Docker updates track image references. They do not scan the
container's operating-system packages or Chromium for vulnerabilities. These
automated controls complement source review and live integration tests.
