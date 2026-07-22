## Summary

- TBD

## Behavior

- TBD

## Architecture and Privacy

- TBD

## Verification

- [ ] `go test ./...`
- [ ] `go test -race -count=1 -p 4 ./...`
- [ ] `go vet ./...`
- [ ] `golangci-lint run ./...`
- [ ] `govulncheck ./...`
- [ ] `make test-cua-macos-fixture`
- [ ] `make test-live-fixture`
- [ ] `CCR_LIVE_REAL_MATRIX=1 make test-live-real`
- [ ] `CCR_LIVE_REAL_MATRIX=1 make test-live-matrix`
- [ ] `CCR_LIVE_REAL_MATRIX=1 make test-live-real-full` when optional real vision/CUA/executor environment is configured
- [ ] `CCR_LIVE_REAL_MATRIX=1 make test-live-matrix-full` when optional real vision/CUA/executor environment is configured
- [ ] `go test -tags=live -count=1 -p 1 ./...`
- [ ] `goreleaser check`
- [ ] `goreleaser release --snapshot --clean`

## Acceptance

- `docs/acceptance/vX.Y.Z.md`

## Release Notes

- `docs/releases/vX.Y.Z.md`
