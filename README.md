# Bobik

Bobik is a single-purpose CLI AI agent for a directory of Markdown notes. You
run it with a prompt file and a glob of allowed notes; it reads, creates, edits,
and deletes only files matching that glob, and writes a full JSONL audit log of
the run. It is designed for on-demand or CRON execution: no streaming, no
conversation history, one final answer.

```console
$ bobik --prompt examples/daily-notes.md --allow-path "./daily-notes/**/*.md" --deny "rm,wget"
```

The final assistant message goes to **stdout**. A one-line summary (steps,
files written through any tool, token usage, log path) goes to **stderr**.

## Requirements

- Node.js **22.18 or newer** (Bobik runs TypeScript directly through Node's
  native type stripping; there is no build step).
- pnpm for installation and development.

## Install

```console
# From this repository (install dependencies first, then link the executable):
pnpm install
pnpm install -g .

# Or run the published package without installing:
pnpx bobik --prompt ./prompt.md --allow-path "./notes/**/*.md"
```

The package exposes a `bobik` executable through a small launcher that enables
Node's native type stripping for the TypeScript source. There is still no build
step.

## API key

Bobik uses [Fireworks.ai](https://fireworks.ai/). Save a key once:

```console
bobik set-api-key fw-your-key-here
```

The key is written to `$XDG_CONFIG_HOME/bobik/config.json` (or
`~/.config/bobik/config.json` when `XDG_CONFIG_HOME` is unset) with mode
`0600`. `FIREWORKS_API_KEY` takes precedence over the config file. There is no
`.env` support.

If neither source is configured, Bobik exits with code `2` and prints how to
run `bobik set-api-key`.

## Usage

```console
bobik --prompt <path> --allow-path <glob> [--deny "rm,wget"] [--model <id>] [--vision-model <id>]
bobik set-api-key <key>
```

| Flag                  | Required | Description                                                                                                                                                |
| --------------------- | -------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--prompt <path>`     | yes      | Markdown file with task instructions. Read by the host process, so it may live anywhere.                                                                   |
| `--allow-path <glob>` | yes      | Only files matching this glob (resolved relative to the current directory) may be read or written. Example: `"./daily-notes/**/*.md"`.                     |
| `--deny <list>`       | no       | Comma-separated bash command names to block, e.g. `"rm,wget"`.                                                                                             |
| `--model <id>`        | no       | Fireworks model ID. Default: `accounts/fireworks/models/deepseek-v4p1-flash-1017`.                                                                         |
| `--vision-model <id>` | no       | Fireworks model ID for image inspection, using the same API key. Defaults to the exact `--model` selection; no automatic fallback or capability preflight. |

Example:

```console
cd ~/notes
bobik \
  --prompt ~/.config/bobik/daily-notes.md \
  --allow-path "./daily-notes/**/*.md" \
  --deny "rm,wget"
```

Exit codes:

- `0` — success, including when the agent decides there is nothing to do.
- `1` — runtime error (API failure, timeout, step limit, uncaught tool failure).
- `2` — usage error (missing/invalid flags, allow-path escapes cwd, prompt file
  missing, no API key).

Tool errors are recoverable: an unsuccessful inspection alone does not force a
nonzero exit. The agent must report unresolved or incomplete inspections in its
final answer.

## Image inspection

The always-available `viewMedia({path, prompt?})` tool returns **plain text only**
to the notes agent. It accepts local PNG, JPEG, WebP, and GIF files up to **10 MiB**,
checks the format from the bytes rather than the extension, and sends the original
bytes without resizing or cropping. URLs, SVG, PDF, audio, and video are not supported.
Animation frame support varies by provider; a description does not guarantee that
every frame was analyzed.

Include images in the allow-path explicitly; a link in a Markdown note grants no
access. For example (replace the model placeholder with a Fireworks image-capable ID):

```console
bobik --prompt ./prompt.md \
  --allow-path './notes/**/*.{md,png,jpg,jpeg,webp,gif}' \
  --vision-model '<Fireworks-image-capable-model-id>'
```

Each inspection is a single, tool-free model request containing only the image,
the supplied prompt (default: describe the image and transcribe visible text), and
fixed faithful-analysis/uncertainty instructions. Outer task instructions and history
are not included. Image-embedded instructions and returned media-derived instructions
are data, not commands. Model/image failures are tool errors with a hint to set
`--vision-model`; Bobik never silently chooses another model. Empty/refused responses
are failures; output-limit responses carry a prominent incomplete warning. There is
no Bobik-specific output cap.

Vision requests share the run's five-minute timeout and caller cancellation; the
25-step limit counts only the outer loop. Their token usage is included in the run
summary and audit totals (including known usage on failures). An aborted/failed
provider request may not report usage. Programmatic callers can inject an optional
AI SDK `LanguageModel` as `visionModel`; omitting it reuses the exact `model` instance.

## CRON example

```cron
# Every day at 07:15, carry yesterday's unfinished TODOs into today's note.
15 7 * * * cd /home/me/notes && "$(command -v bobik)" --prompt /home/me/.config/bobik/daily-notes.md --allow-path "./daily-notes/**/*.md" --deny "rm,wget" >> /home/me/.local/state/bobik-cron.log 2>&1
```

Use absolute paths in CRON. `command -v bobik` must resolve in cron's `PATH`;
otherwise replace it with the absolute path reported by `command -v bobik` in
your shell (often `$(pnpm bin -g)/bobik`). The run summary and any errors land
in the log file; the agent's final answer is also appended to stdout.

## Security model

Bobik assumes the prompt is trusted and the model's output is not.

- **Allowlist filesystem.** A `just-bash` `ReadWriteFs` is rooted at the
  current working directory with `allowSymlinks: false`. It is wrapped by an
  allowlist filesystem that checks every path operation — read, write, append,
  delete, mkdir, rename, stat, copy, and directory listing. File contents may
  only be read, written, or deleted when the path matches `--allow-path` using
  picomatch with `dot: false`. Directories on the way to a match, including the
  root, remain traversable so the agent can navigate; listings hide
  non-matching entries. Non-matching file operations fail with `EACCES`.
  Dotfiles and dot-directories never match unless the glob explicitly targets
  them. Inside the sandbox, `/` is the working directory, so shell paths are
  virtual and cannot reach the host filesystem outside cwd.
- **One enforcement point.** The same wrapped filesystem backs all four tools:
  `bash`, `readFile`, `writeFile`, and `viewMedia`. Image reads inherit the same
  virtual paths, allowlist, hidden-file rules, audit-directory protection, and
  symlink safeguards; they never open agent-selected host paths directly.
- **No network.** The sandbox does not configure networking, so `curl` and
  `wget` do not exist.
- **Deny list.** In `onBeforeBashCall`, the command is parsed with just-bash's
  AST, and every command name is checked, including pipeline stages, subshells,
  command substitutions, parameter-expansion fallbacks, `xargs` targets,
  `find -exec`, nested `bash`/`sh -c`, `eval`/`trap` payloads, heredocs, and
  common wrappers such as `command`, `env`, `sudo`, and `timeout`. A denied command is
  replaced with an error that exits `126`, which the agent sees. Shell scripts
  the analyzer cannot see into (`source file.sh`, a script file argument, or a
  shell reading a pipeline) fail closed while a deny list is configured.
- **Audit directory.** Logs live in `.agent-bobik/`. The wrapper hard-denies
  that directory (and everything under it) even if the allow-path glob names it,
  so the sandboxed agent cannot read or list the logs.
- **Guardrails.** The agent loop stops after 25 steps, each bash command is
  limited to 30 seconds, and the whole run is limited to 5 minutes.

The deny list is defense in depth, not the write boundary. It intentionally
does not catch commands hidden inside `find -delete`, `sed -i`, dynamically
constructed command names such as `$CMD`, or shell redirection tricks. The
filesystem wrapper is what actually prevents writes outside `--allow-path`.

## Audit log

Each run creates `.agent-bobik/<local-timestamp>.jsonl`, for example:

```text
.agent-bobik/2026-10-19T19-55-00.123.jsonl
```

One JSON object is written per line. Events are append-only, serialized, and
never truncated:

```jsonl
{"ts":"2026-10-19T17:55:00.123Z","type":"run-start","flags":{"prompt":"examples/daily-notes.md","allowPath":"./daily-notes/**/*.md","deny":["rm"],"model":"accounts/fireworks/models/deepseek-v4p1-flash-1017"},"cwd":"/home/me/notes","dateTime":"2026-10-19T17:55:00.123Z","localDateTime":"Monday, October 19, 2026 at 19:55:00 GMT+2","timeZone":"Europe/Prague"}
{"ts":"2026-10-19T17:55:02.001Z","type":"tool-call","step":1,"toolName":"writeFile","toolCallId":"call_1","input":{"path":"daily-notes/2026-10-19.md","content":"# Today\n"}}
{"ts":"2026-10-19T17:55:02.002Z","type":"tool-result","step":1,"toolName":"writeFile","toolCallId":"call_1","output":{"success":true}}
{"ts":"2026-10-19T17:55:02.003Z","type":"step-finish","step":1,"usage":{"inputTokens":1200,"outputTokens":34,"totalTokens":1234}}
{"ts":"2026-10-19T17:55:03.000Z","type":"run-end","status":"success","durationMs":2877,"steps":2,"filesWritten":["daily-notes/2026-10-19.md"],"totalUsage":{"inputTokens":2400,"outputTokens":68,"totalTokens":2468}}
```

Event types: `run-start`, `tool-call`, `tool-result`, `assistant-text`,
`step-finish`, `vision-inspection`, and `run-end`. Tool arguments and outputs,
including full note contents, are recorded as-is. `run-start` records both model
identities. `vision-inspection` records the outer tool-call ID, path, detected MIME,
byte count, SHA256, effective prompt, selected model, full returned text, status,
and known token usage (metadata unavailable before a validation failure is omitted).
Raw/base64 image bytes and raw provider errors are **not** logged. Image replay
therefore requires the original files matching the recorded SHA256, not just the log.
`step-finish` usage is outer-only; vision events contain nested usage. `run-end`
records separate `outerUsage` and `visionUsage` plus their combined `totalUsage`;
do not add the aggregate to its components.

## Development

```console
pnpm install
pnpm test          # node:test, including a MockLanguageModelV3 smoke test
pnpm lint
pnpm typecheck
pnpm format:check
```

The integration test drives the real `ToolLoopAgent`, sandbox, filesystem
wrapper, and audit logger against a temporary notes directory. It needs no API
key or network access.

## Non-goals

- `.env` / dotenv loading.
- Network access inside the sandbox.
- A build step or bundling.
- Interactive mode, conversation history, or streaming output.
- `--max-steps` / `--timeout` flags (the guardrails are fixed for now).
