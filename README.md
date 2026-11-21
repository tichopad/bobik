# Bobik

Bobik is an AI CLI that reads a Markdown prompt and works in a vault you choose.
It prints the final answer to stdout and sends status and errors to stderr.

## Quick start

Requires Linux 5.6+ (amd64 or arm64) and Go 1.26+ to build.

```sh
CGO_ENABLED=0 go build -o bobik ./cmd/bobik
install -D -m 755 bobik ~/.local/bin/bobik
bobik set-api-key fw-your-key-here
bobik --prompt examples/daily-notes.md --vault ~/Obsidian/MyVault \
  --hide .obsidian --read-only Archive
```

Make sure `~/.local/bin` is on your `PATH` and the vault and exception
directories exist. A nonblank `FIREWORKS_API_KEY` overrides the saved key
(`~/.config/bobik/config.json` by default). Bobik doesn't load `.env` files.

### Bash setup

The Bash tool needs Bash, `prlimit` (util-linux), and a compatible non-setuid
Bubblewrap. On Debian or Ubuntu, start with:

```sh
sudo apt install bubblewrap util-linux bash
bobik check-sandbox
```

Run the check as the user that will run Bobik, under the same cron or systemd
restrictions if applicable. See [sandbox setup](internal/sandbox/README.md) for
Bubblewrap and host requirements. If isolation fails, Bash won't run. The
built-in file tools don't need Bubblewrap.

## Vault access

- **Edits are live.** Bash starts in `/notes`, a writable mount of the vault.
  Failed or timed-out runs can leave changes. Keep backups; Bobik doesn't roll
  back edits or lock out Obsidian Sync.
- **Use directory exceptions.** Repeat `--hide` to conceal contents or
  `--read-only` to allow reads but prevent edits. Both take existing
  vault-relative directories, not globs, and apply to Bash and file tools.
  Dotfiles are visible unless hidden; `.agent-bobik` is always hidden.
- **Keep secrets out of the vault.** Bash has no network access and doesn't
  mount your home directory. Keep keys outside the vault and the sandbox's
  system paths. A prompt in a hidden directory still loads and its text is
  sent to the model, but the tools can't read its source file. In-vault prompt
  paths can't use symlinks; outside prompts must be outside sandbox system paths.
- **Don't change mounts or move protected directories while Bobik is running.**
  Links, submounts, and special files can prevent Bash from starting. Don't
  execute code from the writable vault on the host.

`viewMedia` sends a readable local PNG, JPEG, WebP, or GIF (up to 10 MiB) to
Fireworks for inspection. It returns text and doesn't send the rest of the
conversation. Each run has a five-minute deadline; each Bash call has 30
seconds and reports when its output is truncated.

## Watch mode

Add `--watch` to run once at startup, then rerun when a changed Markdown note
contains a ready `#bobik` tag. Bobik reloads the prompt before each run.
Untagged edits don't reset the quiet period. Deletions are included in the
next run but don't trigger one.

Draft a request inside an Obsidian comment, then remove the `%%` lines to submit:

```markdown
%%
#bobik: Reorganize this section. Keep links and unfinished tasks.
%%
```

Use `#bobik` as a standalone, case-sensitive tag followed by a colon,
whitespace, or end of file. Tags inside Markdown code don't count. A commented
tag makes the whole note a draft, even if it also has a ready tag. Built-in file
tools won't edit drafts; Bash is told to leave them alone but can't enforce that.

Runs don't overlap. Bobik suppresses known file-tool writes, but Bash edits can
trigger another run. Remove the tag when handling a submission to avoid
repeats. Failed runs retry once after 60 seconds. The first interrupt waits
for the current run; the second cancels it.

## Options

| Flag | Default or behavior |
| --- | --- |
| `--prompt <path>` | Required Markdown instructions, inside or outside the vault. |
| `--vault <dir>` | Required existing vault directory. |
| `--hide <dir>` / `--read-only <dir>` | Repeatable directory exceptions. |
| `--model <id>` | `accounts/fireworks/models/deepseek-v4p1-flash-1017` |
| `--vision-model <id>` | Uses `--model` unless set; no automatic fallback. |
| `--max-steps <n>` | 25 steps per run. |
| `--watch` | Enable watch mode. |
| `--trigger <text>` | `#bobik`; custom triggers match literal, case-sensitive text. |
| `--watch-delay <seconds>` | 60-second quiet period. Fractions work; `0` disables the delay. |

Exit codes: `0` success, `1` runtime failure, `2` usage or configuration error,
`130` second interrupt in watch mode.

## Audit and development

Each run writes a JSONL log under `.agent-bobik/` with tool calls, results, and
token usage. Keep logs private: they can contain note contents and tool
arguments. Bash logs aren't a complete record of which files changed.

```sh
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/bobik
```

Run `bobik check-sandbox` on the deployment host too; passing unit tests don't
prove Bubblewrap can start there.
