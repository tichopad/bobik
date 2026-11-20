# Bobik

Bobik is a single-purpose AI CLI for Markdown notes. It takes instructions from a
prompt file, reads and changes only files matched by your allowlist, and writes a
JSONL audit log. The final answer goes to **stdout**; status, errors, and run
statistics go to **stderr**.

```sh
bobik --prompt examples/daily-notes.md --allow-path './daily-notes/**/*.md'
```

## Install

Bobik runs on Linux amd64 and arm64. The filesystem policy requires a kernel
with `openat2` (Linux 5.6+); there is no fallback to unsafe path opens.

Build with Go 1.26 or later:

```sh
CGO_ENABLED=0 go build -o bobik ./cmd/bobik
install -m 755 bobik ~/.local/bin/bobik
```

Bash searches also require an unprivileged, non-setuid Bubblewrap with
`--json-status-fd` and `--disable-userns`, util-linux (`prlimit`), Bash, ripgrep,
grep, coreutils and sed. Bobik uses distro-packaged tools from `/usr/bin` or
`/bin`, not your `PATH`. On Debian/Ubuntu:

```sh
sudo apt install bubblewrap util-linux bash ripgrep grep coreutils sed
bobik check-sandbox
```

Run that check as your deployment user under its actual security policy,
including for cron or systemd. User, PID and network namespaces and loopback
configuration inside the new network namespace must be permitted. If setup
fails, Bash tool calls fail closed; typed file tools do not require Bubblewrap.
Use a trusted, disk-backed `TMPDIR` with sufficient space. For hard job-wide
memory and process ceilings, run Bobik in a cgroup with `memory.max` and
`pids.max` (for example, systemd `MemoryMax=` and `TasksMax=`); the built-in
sandbox limits are per process.

## API key and flags

```sh
bobik set-api-key fw-your-key-here
```

This replaces `$XDG_CONFIG_HOME/bobik/config.json` with a mode-`0600` file (the
default location is `~/.config/bobik/config.json`). A nonblank
`FIREWORKS_API_KEY` takes precedence. Bobik does not load `.env` files.

| Flag | Meaning |
| --- | --- |
| `--prompt <path>` | Required Markdown instructions. The host can read this file from outside the note directory. |
| `--allow-path <glob>` | Required allowlist, within the current directory. Quote it to prevent shell expansion. |
| `--model <id>` | Fireworks model. Default: `accounts/fireworks/models/deepseek-v4p1-flash-1017`. |
| `--vision-model <id>` | Image inspection model. Defaults to the exact `--model`; no automatic fallback. |
| `--max-steps <n>` | Positive step limit per run, default 25. Each run also has a five-minute deadline. |
| `--watch` | Start once and re-run on note submissions. |
| `--trigger <string>` | Default `#bobik`; other strings are literal and case-sensitive. |
| `--watch-delay <seconds>` | Quiet period, default 60 seconds. Fractions are accepted; `0` disables it. |

Exit codes: `0` success, `1` runtime failure, `2` usage/configuration error,
and `130` for a second interrupt in watch mode. Tool errors are recoverable:
the model can try another operation and should report unresolved failures.

### Migration from the Node CLI

- Remove `--deny` from scripts and cron entries. Bobik rejects it with a
  migration message. A filtered, read-only Bash view replaces the deny list.
- Use `writeFile`, `editFile`, or `deleteFile` for changes. `editFile` replaces
  literal text, with an explicit option to replace all occurrences. `deleteFile`
  removes one note; there are no recursive mutations or dedicated moves.
- Typed file paths are relative to where you started Bobik. Bash starts in the
  staged copy of the allowlist's leading literal directory (`/notes` inside
  the sandbox). For `./daily-notes/**/*.md`, `grep -r TODO .` searches staged
  daily notes; use `daily-notes/...` when passing paths to typed tools.
- The shell includes `bash`, `rg`, `grep`, `cat`, `head`, `tail`, `sort`, `uniq`,
  `wc`, `cut`, `tr`, and `sed`. Pipelines and command substitutions work;
  process substitution requiring `/dev/fd` does not. Use the host date/time
  included with the run rather than relying on a shell `date` command.
- Globs support literal paths, escapes, `*`, `?`, classes, `**`, and
  within-segment brace alternatives such as `**/*.{md,png}`. Negation, extglobs,
  brace ranges, and braces spanning path separators are not supported. Escape
  literal parentheses. Wildcards do not match leading dots; target each hidden
  path component explicitly.

```cron
# Every day at 07:15, carry unfinished TODOs into today's note.
15 7 * * * cd /home/me/notes && /home/me/.local/bin/bobik --prompt /home/me/.config/bobik/daily-notes.md --allow-path './daily-notes/**/*.md' >> /home/me/.local/state/bobik-cron.log 2>&1
```

## File and shell boundaries

Treat the prompt as trusted and the model output as untrusted. The Go note
store checks the glob and rejects symlinks, multiply linked files, special
files, and mount crossings. `.agent-bobik` and Bobik's saved API key are not
accessible through tools. All live mutations happen in Go. Writes use
same-directory temporary files with atomic publication, and each mutation
rechecks draft protection. This check is not a distributed lock against an
external editor or note sync.

Each Bash call gets a fresh, read-only copy of the allowed files, not a mount
of the live vault. Bash gets a clean environment, no host home, no host
sockets or network, and no writable scratch directory. Search does not expose
empty directories. An excluded `.gitignore` can change ripgrep results; search
filters are not authorization. The copy reflects completed Bobik mutations but
is not an atomic snapshot of other writers' activity. Exit status is separate
from stdout and stderr; ripgrep's exit `1` means no match.

| Operation | Limit |
| --- | --- |
| Prompt file | 1 MiB |
| `readFile` | 128 KiB |
| Watch classification / mutation safety check | 16 MiB / 32 MiB per note |
| `viewMedia` input | 10 MiB |
| Tool argument JSON / result JSON | 1 MiB / 256 KiB |
| Fireworks response / conversation history | 4 MiB / 8 MiB |
| Bash command / retained output | 32 KiB / 64 KiB per stream |
| Bash staging / call deadline | 10,000 files, 10,000 directories, 256 MiB / 30 seconds including staging |

Bash reports truncated output. It refuses an over-budget staging request
rather than searching an incomplete copy. If the conversation exceeds its
budget, the run fails rather than silently dropping history.

## Image inspection

Include images in the allowlist, for example
`--allow-path './notes/**/*.{md,png,jpg,jpeg,webp,gif}'`. Markdown links do not
grant access. `viewMedia` checks local PNG, JPEG, WebP, and GIF by file contents.
It sends the original bytes in one tool-free request with an optional inspection
prompt, not the outer conversation. The response is text only; image-embedded
instructions are data and animation coverage may vary. Empty, refused, and
failed responses are tool errors; incomplete descriptions carry a warning.
The provider may reject a request below the local file limit. Vision shares the
run deadline and reports token usage separately in the audit.

## Watch mode and drafts

Watch mode runs once immediately, then watches the allowlist's leading literal
directory recursively. A changed or created file triggers only if its content
changed and has a ready trigger. Untagged edits do not reset the quiet period.
Deleted paths are included in the next triggered run but do not trigger their
own run. The model gets the change set and run time separately from the prompt
instructions, which are reloaded from disk before each run.

Draft a request inside an Obsidian comment, then uncomment to submit:

```markdown
%%
#bobik: Reorganize this section.
Keep all links and unfinished tasks.
%%
```

`#bobik` must be a standalone, case-sensitive tag followed by a colon,
whitespace, or end-of-file. Triggers and comment delimiters inside Markdown
code do not count. An unclosed `%%` comment extends to end-of-file. A commented
trigger protects the **entire note** from mutation and overrides any ready
trigger elsewhere in that note.

Runs are serialized. Bobik suppresses its own writes during the run and for
about a second afterward. Failed runs retry once after 60 seconds; after a
second failure Bobik logs that it gave up and waits for another edit. If the
prompt becomes unreadable, watch mode stops. The first
SIGINT/SIGTERM stops accepting changes and waits for the current run, and a
second cancels it. Filesystem notifications are not durable across suspension
or restart.

## Audit and development

Each run streams events to `.agent-bobik/<local-timestamp>.jsonl`: `run-start`,
`tool-call`, `tool-result`, `assistant-text`, `step-finish`, `vision-inspection`,
and `run-end`. Watch may append `watch-gave-up`. Logs can contain note contents
and tool arguments; treat them as private. Bobik does not log the API key
configuration, raw provider error bodies, or image payloads. `run-end` records
outer, vision, and combined token usage; do not sum the aggregate again.

```sh
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/bobik
```

Tests use temporary note directories and local fake model responses. Run
`bobik check-sandbox` as well: a passing unit suite on a restricted host is not
evidence that the sandbox can start there. See
[rewrite validation](docs/rewrite-validation.md) for release checks.
