# Bobik

Bobik is a single-purpose AI CLI for Markdown notes. It takes instructions from a
prompt file, works in a designated live vault, and writes a
JSONL audit log. The final answer goes to **stdout**; status, errors, and run
statistics go to **stderr**.

```sh
bobik --prompt examples/daily-notes.md --vault ~/Obsidian/MyVault \
  --hide .obsidian --read-only Archive --pin Projects
```

## Install

Bobik runs on Linux amd64 and arm64. The filesystem policy requires a kernel
with `openat2` (Linux 5.6+); there is no fallback to unsafe path opens.

Build with Go 1.26 or later:

```sh
CGO_ENABLED=0 go build -o bobik ./cmd/bobik
install -m 755 bobik ~/.local/bin/bobik
```

The shell sandbox requires Bash, util-linux (`prlimit`), and an unprivileged,
non-setuid Bubblewrap with `--bind-fd`, `--ro-bind-fd`, `--block-fd`,
`--json-status-fd`, and `--disable-userns`. On Debian/Ubuntu:

```sh
sudo apt install bubblewrap util-linux bash
bobik check-sandbox
```

Run that check as your deployment user under its actual security policy,
including for cron or systemd. User, PID and network namespaces and loopback
configuration inside the new network namespace must be permitted. If setup
fails, Bash tool calls fail closed; typed file tools do not require Bubblewrap.
See [sandbox deployment](internal/sandbox/README.md) for the host policy and
filesystem requirements.
For hard job-wide memory and process ceilings, run Bobik in a cgroup with
`memory.max` and `pids.max` (for example, systemd `MemoryMax=` and `TasksMax=`);
the built-in sandbox limits are per process. Use a filesystem quota if you
also need a hard cap on total vault growth.

## API key and flags

```sh
bobik set-api-key fw-your-key-here
```

This replaces `$XDG_CONFIG_HOME/bobik/config.json` with a mode-`0600` file (the
default location is `~/.config/bobik/config.json`). A nonblank
`FIREWORKS_API_KEY` takes precedence. Bobik does not load `.env` files.
Keep the saved key outside the vault and out of system directories exposed to
Bash. The prompt can live inside the vault, where normal vault rules apply.
To keep its source file out of reach of the agent's tools, put it in a directory
such as `Agent/Preferences` and pass `--hide Agent/Preferences`. Bobik still
loads the prompt and sends its text to the model. In-vault prompt paths cannot
use symlinks. An outside prompt must stay outside the vault and Bash's system
directories.

| Flag | Meaning |
| --- | --- |
| `--prompt <path>` | Required Markdown instructions inside or outside the vault. |
| `--vault <dir>` | Required existing directory. Choose only a tree you trust the agent to edit. |
| `--hide <dir>` | Hide a vault-relative directory's contents. Repeatable. |
| `--read-only <dir>` | Allow reads but prevent changes under a vault-relative directory. Repeatable. |
| `--pin <dir>` | Prevent renaming or removing a vault-relative directory itself, not its contents. Repeatable. |
| `--model <id>` | Fireworks model. Default: `accounts/fireworks/models/deepseek-v4p1-flash-1017`. |
| `--vision-model <id>` | Image inspection model. Defaults to the exact `--model`; no automatic fallback. |
| `--max-steps <n>` | Positive step limit per run, default 25. Each run also has a five-minute deadline. |
| `--watch` | Start once and re-run on note submissions. |
| `--trigger <string>` | Default `#bobik`; other strings are literal and case-sensitive. |
| `--watch-delay <seconds>` | Quiet period, default 60 seconds. Fractions are accepted; `0` disables it. |

Exit codes: `0` success, `1` runtime failure, `2` usage/configuration error,
and `130` for a second interrupt in watch mode. Tool errors are recoverable:
the model can try another operation and should report unresolved failures.

### Migration

Replace `--allow-path` with `--vault`, and `--deny` with explicit directory
exceptions. `--vault` exposes the entire selected tree, including dotfiles
unless hidden. If your old glob targeted only a small part of the vault,
point `--vault` at that subdirectory instead of the full vault.

## File and shell boundaries

Bash starts at `/notes`, a writable mount of the live vault. The vault root
itself cannot be moved or removed. Its parent, your home, your credentials,
and the network are not exposed. Utilities are system-dependent; the shell
does not inherit your `PATH`. Typed tools use paths relative to the vault,
enforce the same hide and read-only rules, and work without Bubblewrap.
`.agent-bobik` is always hidden.

Exceptions name existing directories, not globs. Parent directories of
nested exceptions are automatically pinned so an agent cannot move an
ancestor out from under the protection. Bash cannot list a hidden directory's
contents. A pin can prevent atomic moves across mount points; it cannot prevent
copying or deleting files **inside** a writable directory.

Bobik checks the visible vault's metadata before each shell call rather than
copying note contents. Submounts, links, and special files can prevent the
sandbox from starting. Bash cannot create links or special files.
Don't move protected directories from the host while Bobik is running.
Host mount changes can enter the sandbox after its startup checks, so don't
mount or unmount filesystems under the vault or exposed system paths during a
Bash call.

Obsidian Sync sees changes as ordinary local edits. There is no rollback or
isolation from concurrent Sync writes: even a failed or timed-out command
can leave changes. Keep backups and don't put unrelated files in the vault.
Hide `.obsidian` if the agent should not edit settings or plugins. Do not
execute code from the writable vault on the host.

| Operation | Limit |
| --- | --- |
| Prompt file | 1 MiB |
| `readFile` | 128 KiB |
| Watch classification / mutation safety check | 16 MiB / 32 MiB per note |
| `viewMedia` input | 10 MiB |
| Tool argument JSON / result JSON | 1 MiB / 256 KiB |
| Fireworks response / conversation history | 4 MiB / 8 MiB |
| Bash command / retained output | 32 KiB / 64 KiB per stream |
| Bash call deadline / per-file size limit | 30 seconds / 32 MiB |

Bash reports truncated output. If the conversation exceeds its budget, the run
fails rather than silently dropping history.

## Image inspection

Keep images in the readable vault. Markdown links do not grant access.
`viewMedia` checks local PNG, JPEG, WebP, and GIF by file contents.
It sends the original bytes in one tool-free request with an optional inspection
prompt, not the outer conversation. The response is text only; image-embedded
instructions are data and animation coverage may vary. Empty, refused, and
failed responses are tool errors; incomplete descriptions carry a warning.
The provider may reject a request below the local file limit. Vision shares the
run deadline and reports token usage separately in the audit.

## Watch mode and drafts

Watch mode runs once immediately, then watches the visible vault recursively.
A changed or created Markdown file triggers only if its content
changed and has a ready trigger. Untagged edits do not reset the quiet period.
Deleted paths are included in the next triggered run but do not trigger their
own run. The model gets the change set and run time separately from the prompt
instructions, which are reloaded from disk before each run, including changes
made by the agent. Editing the prompt does not have a special trigger.

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
trigger overrides any ready trigger elsewhere in that note. Typed file tools
refuse changes to drafts. Bash is instructed to leave drafts alone, but
Bubblewrap enforces directory rules, not Markdown content.

Runs are serialized. Bobik suppresses known typed-tool writes during the run
and for about a second afterward. Bash writes can trigger later runs: remove
the trigger when handling a submission to avoid repeating it. Failed runs retry
once after 60 seconds; after a second failure Bobik logs that it gave up and
waits for another edit. If the prompt becomes unreadable, watch mode stops. The first
SIGINT/SIGTERM stops accepting changes and waits for the current run, and a
second cancels it. Filesystem notifications are not durable across suspension
or restart.

## Audit and development

Each run streams events to `.agent-bobik/<local-timestamp>.jsonl`: `run-start`,
`tool-call`, `tool-result`, `assistant-text`, `step-finish`, `vision-inspection`,
and `run-end`. Setup failures may add `sandbox-unavailable`; watch may append
`watch-gave-up`. Logs can contain note contents
and tool arguments; treat them as private. Bobik does not log the API key
configuration, raw provider error bodies, or image payloads. `run-end` records
outer, vision, and combined token usage; do not sum the aggregate again.
Bash commands and their results are logged, not a complete inventory of the
files they changed.

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
