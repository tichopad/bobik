# AGENTS.md

Bobik is a Linux-only CLI AI agent for Markdown notes. Typed tools and offline Bubblewrap Bash share a designated live vault with directory exceptions. Each run writes a JSONL audit log.

- Use Go 1.26+ and `gofmt`. The entrypoint is `cmd/bobik`; production packages live in `internal/`.
- Verify changes with `go test ./...`, `go test -race ./...`, `go vet ./...`, and `go build ./cmd/bobik`.
- Keep the Linux filesystem policy and shell isolation fail-closed. To test the real sandbox on a supported Linux host, run `BOBIK_REQUIRE_SANDBOX=1 go test ./internal/sandbox` and `bobik check-sandbox`.
