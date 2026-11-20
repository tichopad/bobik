# Bobik Go rewrite: research and cutover plan

**Recommendation:** Rewrite Bobik as a Linux-only, pure-Go CLI. Keep Bash for expressive `rg`/`grep` searches, but run it inside Bubblewrap against a **filtered, read-only copy** of the notes. Keep every live note write in Go. Port the one-shot run first, then watch mode, and release both in a single cutover.

**Dependency rule:** Write the small, Bobik-specific parts. Keep a focused library when replacing it would mean reimplementing Markdown grammar, glob semantics, or a Linux syscall interface. Aim for fewer dependencies, not zero at any cost.

## 1. Decide what changes before coding

| Keep | Change deliberately |
| --- | --- |
| Existing CLI output split, config/key precedence, model flags, and documented exit codes. | **Bash becomes read-only for notes.** Keep pipelines, substitutions, shell logic, and search tools. Direct note edits, redirection into notes, and deletion through Bash stop working. |
| One-shot and watch modes, tool error recovery, step and time limits, and JSONL audit events. | **Add `deleteFile`** to preserve single-file deletion. Add `moveFile` only if real workflows depend on it; it must check both endpoints and draft protection. Do not add recursive mutations in the first release. |
| Allow-path filtering, hidden-file behavior, symlink rejection, audit exclusion, and draft protection. | **Retire `--deny`.** Reject it with a migration message instead of silently accepting it. The curated shell toolset and filesystem isolation do the real work; don't rebuild the AST deny-list as a security control. |
| `readFile` missing-file hints, literal `editFile` replacements, and `viewMedia` behavior. | Document any narrowed glob syntax and new resource limits; reject unsupported patterns at startup rather than interpreting them broadly. |

The recommendations above change the shell contract documented in [README.md](README.md). Audit real prompts, CRON entries, and examples for `--deny`, shell mutations, and implicit utility dependencies before cutover. Don't carry over the old emulator-specific system prompt. The repo's [AGENTS.md](AGENTS.md) and release workflow also need updating at cutover: native Go binaries do require a compile step.

## 2. Shell: Bubblewrap around Bash, not around Bobik

- **Host:** The Go process owns the Fireworks key, network access, audit log, and live note policy. The shell gets none of those. Treat every model-generated command as untrusted.
- **Search view:** For each Bash call, walk the allowed base directory through the same note policy the typed tools use. Copy eligible regular files into a private, disk-backed temporary directory, preserving relative paths. Skip symlinks and special files. Never expose the live notes tree or use hard links to it. Mount the staged view read-only. Per-call copies cost disk I/O but avoid up-front work when the model never uses Bash and reflect completed edits on the next call. This is not an atomic snapshot of changes made by other programs.
- **Runtime:** Expose a curated, read-only runtime with Bash, `rg`, `grep`, and a small set of useful utilities plus their required libraries. Run from the staged notes directory with a clean environment and fixed `PATH`, private PID/network namespaces, no host home or sockets, and only intentional descriptors and mounts. Use tool-specific scratch only if needed, with a size limit. Don't bind the host root for convenience. Prefer packaged host tools initially; pin a runtime tree only if host version differences actually cause problems.
- **Limits:** Apply the 30-second call deadline to staging and execution. Bound command bytes, staged files/bytes, and retained stdout/stderr. Refuse a search if staging exceeds its budget rather than reporting results from a silently incomplete view. Keep draining after output truncation. `exec.CommandContext` alone kills the direct child; use Bubblewrap's lifecycle options (`--die-with-parent`, private PID namespace) and verify the whole process tree stops. Use cgroups for hard job-wide memory/process limits; Go's `GOMEMLIMIT` is soft and excludes child processes.
- **Preflight:** Smoke-test the actual isolation profile and runtime on the target distro. Upstream Bubblewrap removed setuid support in v0.12.0; verify the host permits user namespaces under its security policy. **If isolation cannot start, fail closed; never run the command directly.**
- **Tool result:** Return bounded stdout/stderr, truncation, and exit status separately. `rg` exit `1` means no match, not a failed agent run. Hidden/ignore rules in ripgrep affect search results, not authorization. A `.gitignore` omitted from the staged view may change search results; document that instead of copying otherwise excluded files.

Start with the per-call copy. Avoid tmpfs by default: it stores file data in virtual memory and can swap. If copies prove too costly, benchmark per-file read-only binds. They avoid copying bytes but add mount work per file and have different freshness semantics: a bind can still reference an old file after atomic replacement. Do not replace a filtered view with a broad read-only mount.

Sources: [Bubblewrap security model](https://github.com/containers/bubblewrap#readme), [Bubblewrap options](https://github.com/containers/bubblewrap/blob/main/bwrap.xml), [v0.12.0 release](https://github.com/containers/bubblewrap/releases/tag/v0.12.0), [Go `os/exec`](https://pkg.go.dev/os/exec#CommandContext), [Go memory limit](https://pkg.go.dev/runtime/debug#SetMemoryLimit), [Linux cgroup v2](https://docs.kernel.org/admin-guide/cgroup-v2.html), [tmpfs](https://docs.kernel.org/filesystems/tmpfs.html), [bind mounts](https://man7.org/linux/man-pages/man8/mount.8.html#Bind_mount_operation), [rename semantics](https://man7.org/linux/man-pages/man2/rename.2.html), [ripgrep guide](https://github.com/BurntSushi/ripgrep/blob/master/GUIDE.md).

## 3. Go modules and error handling

| Module | Owns |
| --- | --- |
| CLI | Argument parsing, XDG config and key permissions, stdout/stderr, exit codes, signals. Standard library. |
| Notes | Path normalization, glob matching, safe file access, draft checks, exact edits, and the filtered search view. Watcher reads also go through this module. |
| Run loop + Fireworks | A small model seam for HTTP and fake responses. The run loop owns history, tool dispatch, deadlines, step count, and usage. Fireworks wire structs stay private to the HTTP adapter. |
| Audit | Serialize JSONL as work happens; keep neither a duplicate transcript nor an unbounded write queue. Preserve event names and keep raw provider errors, image bytes, and credentials out of logs. |
| Watch | A single owner of pending paths, timers, retries, and shutdown state. Keep event ingestion separate from scheduling so tests don't depend on sleep timing. |

**Return `(RunSummary, error)`.** Preserve partial steps, writes, usage, and log path on failure. Wrap underlying causes with context and classify with `errors.Is`/`errors.As`, not string matching. Usage/config errors map to exit `2`; unrecoverable run errors to `1`; recoverable tool errors become safe model-facing results. Propagate cancel/deadline with `context.Context`. If audit finalization also fails, preserve the original cause, report both, and never claim the audit completed.

### Notes and glob policy

- Pin a supported Go release. `os.Root` gives root containment, **not** a no-symlink switch: it allows symlinks that resolve within the root and does not forbid crossing mounts. For the stricter Bobik policy, evaluate Linux `openat2` via `golang.org/x/sys/unix` (Linux 5.6+) for root-relative, no-symlink resolution, with mutations relative to safely opened parent directories. Review mount and alias handling explicitly. Verify regular-file type on the opened object before reading or copying; don't block on a FIFO. Don't fall back to a separate string check followed by an ordinary open on older hosts.
- Recheck the current note for a draft immediately before edits, replacement, moves, or deletion. On unreadable or unclassifiable content, deny the mutation. Prefer same-directory temporary writes and atomic publication. Serialize Bobik's own mutations; document that rechecking is not a distributed lock against Obsidian Sync or another external writer.
- Preserve literal paths, `*`, `?`, classes, `**`, brace alternatives (including `**/*.{md,png}`), escaped names, and explicit dot-path behavior. [Go's standard matcher](https://pkg.go.dev/path#Match) does not supply globstar or braces. [Doublestar](https://github.com/bmatcuk/doublestar#patterns) is a candidate matcher, **not** a replacement for Bobik's traversal and authorization logic or all of picomatch. Extract fixtures from existing behavior; explicitly reject unsupported extglobs or defer release if real configs need them.

Sources: [Go traversal-resistant file APIs](https://go.dev/blog/osroot), [Go `os.Root`](https://pkg.go.dev/os#Root), [Linux `openat2`](https://man7.org/linux/man-pages/man2/openat2.2.html), [Go syscall wrapper](https://pkg.go.dev/golang.org/x/sys/unix#Openat2), [Go `errors`](https://pkg.go.dev/errors), [Go `context`](https://pkg.go.dev/context); current policy: [src/allowlist.ts](src/allowlist.ts), [src/allowlist-fs.ts](src/allowlist-fs.ts), [src/edit.ts](src/edit.ts).

### Trigger detection: a scanner, yes; one regex, no

Bobik does more than detect an uncommented tag. The same function decides whether an entire note is **read-only**. It ignores code, including nested and indented blocks; `%%` inside code must not toggle comment state. An unclosed comment extends to EOF, and **any** commented trigger overrides a ready trigger elsewhere in the note.

Split the work in two:

1. **Small standard-library scanner:** Fast substring check; then scan prose for `%%` and the trigger. For `#bobik`, check adjacent runes explicitly, using the current Unicode and punctuation rules as fixtures. For custom triggers use literal, case-sensitive matching. Keep `{ready, draft}` as the shared result for watching and mutation checks.
2. **Code masking:** Start with a CommonMark parser candidate such as Goldmark. Verify that the adapter can mask the full source ranges, including fence info strings and multi-line code spans. Goldmark exposes code nodes and source segments, but not all of the same complete ranges as the current mdast nodes; test that seam before choosing it. Hand-roll masking only if its supported grammar is explicit and differential tests verify both draft and ready outcomes. If a scanner needs to implement nested CommonMark containers to pass, keep the parser.

Seed fixtures with [tests/trigger.test.ts](tests/trigger.test.ts) and [tests/draft-protection.test.ts](tests/draft-protection.test.ts). Add CRLF, escaped and unequal-length backtick runs, fenced info strings, list/quote boundaries, Unicode whitespace and adjacent letters, and simultaneous ready/draft tags. Fuzz against fixtures generated by the TypeScript analyzer before removing it. Regex is optional for the local tag check; Go's RE2 syntax does not support the current lookbehind.

Sources: [current analyzer](src/trigger.ts), CommonMark [indented code](https://spec.commonmark.org/0.31.2/#indented-code-blocks), [fenced code](https://spec.commonmark.org/0.31.2/#fenced-code-blocks), [code spans](https://spec.commonmark.org/0.31.2/#code-spans), [Goldmark code-span parser](https://github.com/yuin/goldmark/blob/master/parser/code_span.go), [Goldmark fence parser](https://github.com/yuin/goldmark/blob/master/parser/fcode_block.go), [RE2 syntax](https://github.com/google/re2/wiki/Syntax).

### Fireworks, tools, and vision

- Use `net/http` and `encoding/json` for Fireworks' non-streaming Chat Completions endpoint. Send tool schemas; preserve assistant tool calls and append tool responses linked by `tool_call_id`. Validate arguments independently of the provider. Execute tool calls serially even if the model returns several; measure latency before introducing concurrency. Audit calls before dispatch and results as they happen. Don't blindly replay mutations on a transport or audit failure. Cover unknown tools, malformed argument JSON, empty choices, provider errors, finish reasons, and step exhaustion with a local `httptest` server.
- Keep only the history needed for the next request and bound response, tool-output, and aggregate history size. Document limit behavior; don't silently drop an earlier instruction or orphan a tool result. Preserve stable prompt prefixes and verify reasoning and token-usage mapping against the configured Fireworks model, not just a fake.
- Keep vision as one tool-free request with local bytes, a MIME-typed data URI, and a text-only result. Preserve the 10 MiB local gate and current format checks initially. Fireworks also documents a base64 request limit: test the real service near the boundary, since base64 makes the payload larger. Cap before buffering; avoid retaining the encoded payload or provider error body. Preserve no-fallback behavior, incomplete-output warnings, and separate usage accounting including usage reported on failures.

Sources: Fireworks [Chat Completions](https://docs.fireworks.ai/api-reference/post-chatcompletions), [function calling](https://docs.fireworks.ai/guides/function-calling), [vision](https://docs.fireworks.ai/guides/querying-vision-language-models); [Go `httptest`](https://pkg.go.dev/net/http/httptest); [current run loop](src/agent.ts) and [vision tool](src/media.ts).

### Watch mode

Keep startup execution, prompt reload, content-hash deduplication, quiet period, self-write suppression, deletions as context, draft revalidation before dispatch, one retry, and two-stage signal handling. Base parity on [src/watch.ts](src/watch.ts) and [tests/watch.test.ts](tests/watch.test.ts), not on reproducing the same callback structure.

Use [`fsnotify`](https://github.com/fsnotify/fsnotify) for native notifications if watch-mode parity and low idle filesystem activity matter. It does not watch recursively by itself: add directory watches, handle moves and atomic saves, and reconcile the tree after overflow. Test an initially missing watch root and directories created after startup. Polling is a standard-library option for small trees, but scanning costs idle I/O and can miss changes between scans. Don't choose it just to save a dependency.

## 4. Build in slices; release once

1. **Freeze compatibility:** Capture TS fixtures and a memory baseline before changing implementations. Agree on read-only Bash, `--deny` removal, glob coverage, and Linux prerequisites.
2. **Prove the risky seams first:** On the target host, run real `rg` pipelines against a filtered view. Verify no unapproved files, no inherited secrets or network, no live writes, and no leftover children after cancellation. Prove no-symlink behavior and trigger masking independently. Use disposable vaults.
3. **Build a vertical slice:** CLI → Fireworks request → typed note tool → JSONL → final summary. Drive it through `httptest` with no key. Add Bash, vision, and watch after the one-shot slice is complete. Keep the old CLI runnable until cutover.
4. **Release gates:** Port meaningful tests from all existing suites; replace emulator-specific tests with actual Bubblewrap integration tests. Run Go tests, race checks, vet, and targeted fuzzing. Test clean and failed runs, actual sandbox preflight, audit write failures, event overflow and signal cancellation. Test on Linux amd64 and arm64 if both are release targets; a cross-build alone doesn't prove runtime behavior. Use a disposable vault for a final opt-in Fireworks acceptance run.
5. **Measure and cut over:** Compare idle watch RSS/PSS, peak memory of the **whole job** (Go plus Bash/`rg` and staging effects), and time/bytes per staged search against the current CLI. Use the same corpus and workload: no-tool run, repeated searches, and a large image. On Linux, a fresh cgroup's `memory.peak` can capture what process-only RSS misses. Set the budget from the real deployment and baseline; don't promise a savings number before measuring. Ship the binary and document external packages (`bwrap`, Bash, `rg`/`grep`, and any selected utilities), namespace prerequisites, and the CLI migration.

Use standard Go cross-compilation with `CGO_ENABLED=0`, `GOOS=linux`, and the supported `GOARCH` values; this covers Bobik's binary, not the external shell runtime. Sources: [Go build environment](https://go.dev/doc/install/source#environment), [Go fuzzing](https://go.dev/doc/security/fuzz/), [Linux cgroup memory accounting](https://docs.kernel.org/admin-guide/cgroup-v2.html#memory).

**Not yet verified:** No Go implementation, target-host sandbox smoke test, memory benchmark, or live Fireworks call was run for this research. Those are release gates, not assumed outcomes.
