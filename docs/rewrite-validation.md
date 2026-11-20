# Go cutover validation

## Baseline

The TypeScript base (`0ec42b6`) passed `pnpm typecheck`, `pnpm lint` and all 194
tests on Linux amd64 with Node 26.5.0. A disposable 100-note vault with watch
enabled and a mocked no-tool startup response measured **138,496 KiB RSS /
83,248 KiB PSS** from `/proc/self/smaps_rollup` one second after startup.
The Go watch fixture with the same corpus, reading from a local fake HTTP
provider in the test process, measured **11,832 KiB RSS / 10,363 KiB PSS**.
These are single idle observations, not peak measurements or a deployment
budget.

Before removing the TypeScript implementation, the Go and TypeScript
trigger analyzers agreed on 8,000 generated inputs covering comments,
Markdown code, nested containers, line endings, and default/custom triggers.

## Deployment checks

1. Build and run the tests on Linux amd64 and arm64; cross-building checks
   compilation, not runtime behavior.
2. Run `bobik check-sandbox` under the actual deployment policy, then run
   `BOBIK_REQUIRE_SANDBOX=1 go test ./internal/sandbox` with that same user
   and package set. The development host
   rejected network namespace loopback setup (`RTM_NEWADDR`: operation not
   permitted). A fail-closed result is expected here, but does not validate
   successful sandbox execution on a supported host.
3. Use a disposable vault and a live Fireworks key to verify tool calls,
   reasoning/usage, and strict no-fallback vision with the selected model.
   Include an image near the 10 MiB local limit; encoding adds size.
4. Compare idle watch RSS/PSS, no-tool completion, repeated shell calls,
   and a large image on the same corpus. Use a fresh cgroup's `memory.peak`
   per workload to include shell children. Set hard job
   budgets from deployment measurements, not process RSS alone.
5. Review private prompts, cron entries, and scripts for `--allow-path`,
   `--deny`, and assumptions about shell writes. Verify the chosen vault
   and directory exceptions with a disposable copy before deploying.
