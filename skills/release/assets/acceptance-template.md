# CCR vX.Y.Z Acceptance Criteria

This file is the release source of truth. A checked item requires reproducible
evidence. Skipped live tests are not evidence.

## Behavior

- [ ] TBD

## Privacy and Security

- [ ] No raw provider secret, prompt, response, tool argument, screenshot,
  transcript path, authorization header, or raw hook body is stored or printed.

## Live Matrix

- [ ] Fixture live matrix passes without skips across Linux/macOS, pinned/latest
  Claude Code, and `openai-chat`/`anthropic-native`/`openai-responses`
  protocols.
- [ ] Local real matrix passes against first-party Anthropic and every
  configured non-blocked routable alias.
- [ ] Opt-in real routing, vision, CUA, and executor coverage is recorded when
  run; skipped real tests are not evidence.

## Release

- [ ] Draft release contains Linux/macOS amd64/arm64 archives and checksums.
- [ ] GitHub provenance attestation verifies.
- [ ] GHCR browser image is published and signed.
- [ ] Manual promotion publishes GitHub Release.
- [ ] Homebrew formula points to the promoted tag and matching source SHA-256.

## Evidence

- Date:
- Claude Code:
- Commands:
- CI:
- Draft release:
- Promotion:
