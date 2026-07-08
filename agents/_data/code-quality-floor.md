# Code Quality Floor

The repo floor is enforced by `.golangci.yml`, `Makefile`, and CI.

Required local gate:

```bash
go vet ./...
golangci-lint run ./...
go test -race -count=1 -p 4 ./...
govulncheck ./...
```

Lint categories:

- Formatting: gofumpt.
- Correctness: errcheck, govet, staticcheck, ineffassign, nilerr, bodyclose, contextcheck, errorlint, forcetypeassert, unconvert, unparam.
- Maintainability: gocritic, prealloc, errname, misspell, nolintlint.
- Complexity: gocyclo, gocognit.

Every `//nolint` must name the exact linter and explain why.
