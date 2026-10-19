# Bobik — Implementation Spec

Bobik is a single-purpose CLI AI agent. You run it inside a directory of Markdown notes;
it follows instructions from a user-supplied prompt file and reads/creates/edits/deletes
notes **only** within an allowed glob path. Every action is logged to a JSONL audit file.
Designed for on-demand or CRON execution.

## Tech stack

- **Runtime**: Node.js ≥ 22.18 (native TypeScript type stripping — **no build step**).
- **Language**: TypeScript, ESM (`"type": "module"`), erasable syntax only.
- **Package manager**: pnpm.
- **CLI parsing**: [citty](https://github.com/unjs/citty).
- **Agent framework**: [AI SDK 6](https://ai-sdk.dev/docs/ai-sdk-core/overview) —
  `ToolLoopAgent` + `isStepCount`.
- **Provider**: `@ai-sdk/fireworks` (Fireworks.ai).
- **Agent tools**: [bash-tool](https://github.com/vercel-labs/bash-tool)
  (`createBashTool`) over [just-bash](https://github.com/vercel-labs/just-bash).
- **Glob matching**: picomatch.
- **Tests**: Node.js built-in test runner (`node:test`) + AI SDK `MockLanguageModel`.
- **Lint/format**: ESLint flat config + Prettier (details below).

## CLI interface

Package name and executable: **`bobik`**. `bin` points directly at the TypeScript
entry: `bin: { "bobik": "./src/cli.ts" }` with a `#!/usr/bin/env node` shebang.
`package.json` must declare `engines: { "node": ">=22.18" }`.

### Default command: run the agent

```
bobik --prompt <path> --allow-path <glob> [--deny "rm,wget"] [--model <id>]
```

| Flag | Required | Description |
| --- | --- | --- |
| `--prompt <path>` | yes | Path to a Markdown file with the user's task instructions. Read by the **host process** at startup (it is NOT subject to sandbox restrictions and may live anywhere). |
| `--allow-path <glob>` | yes | Glob (resolved relative to cwd) defining the only files the agent may read/write. E.g. `"./daily-notes/**/*.md"`. |
| `--deny <list>` | no | Comma-separated bash command names to block, e.g. `"rm,wget"`. Entries are literal command names matched against AST-extracted commands. |
| `--model <id>` | no | Fireworks model ID. Default: `accounts/fireworks/models/deepseek-v4p1-flash-1017`. |

### Subcommand: set API key

```
bobik set-api-key <key>
```

Writes `{ "apiKey": "<key>" }` to the global config file (see below).

### Exit codes

- `0` — success, **including** "agent decided there was nothing to do".
- `1` — runtime error (API failure, timeout, step limit reached, tool failure).
- `2` — usage error (missing/invalid flags, glob escapes cwd, prompt file missing, no API key).

### Output

- **stdout**: the agent's final text message.
- **stderr**: one-line run summary — steps used, files written, token usage, log file path.
- No live streaming (CRON-friendly).

## API key resolution

1. `FIREWORKS_API_KEY` environment variable (wins if set).
2. Global config file: `$XDG_CONFIG_HOME/bobik/config.json`, falling back to
   `~/.config/bobik/config.json` when `XDG_CONFIG_HOME` is unset.
   - `set-api-key` creates parent dirs and writes the file with mode `0o600`.
3. If neither exists: clear error message pointing at `bobik set-api-key`, exit 2.

There is **no `.env` support** — the config file / env var are the only sources.

## Sandbox & security model

### Filesystem

- Create a `just-bash` `Bash` instance with `ReadWriteFs` rooted at **cwd** (the notes
  directory), with `allowSymlinks: false`.
- Wrap that FS in a custom **allowlist wrapper FS** class: every path operation
  (read, write, append, delete, mkdir, rename, stat, etc.) resolves the path relative
  to the FS root and checks it against the `--allow-path` glob via picomatch
  (`dot: false`, so dotfiles/dot-dirs like `.agent-bobik/` never match unless the glob
  explicitly targets them). Non-matching paths fail with `EACCES`. Directory listing of
  the root is allowed (the agent needs to navigate), but reading/writing non-matching
  entries is blocked. This single wrapper is the enforcement point for all three tools
  (bash, readFile, writeFile).
- Startup validation: reject `--allow-path` patterns that escape cwd (`../**`, absolute
  paths outside cwd) with exit code 2.
- Pass the wrapped FS to `createBashTool` (via a custom just-bash `Bash` instance —
  see bash-tool's "Use a custom just-bash instance" docs). Expose **all three** tools
  to the agent: `bash`, `readFile`, `writeFile`.
- **Network stays disabled** (just-bash default: `curl`/`wget` don't exist).

### Deny list

- Enforced in bash-tool's `onBeforeBashCall` hook.
- Parse the command with just-bash's AST tooling (`CommandCollectorPlugin` /
  `BashTransformPipeline`) to extract every command name in the script (pipelines,
  subshells, `xargs` arguments). If any extracted command is in the deny set, block it:
  return a result like `{ command: "echo 'Blocked: <name> is denied' >&2; exit 126" }`
  or equivalent so the agent sees a clear refusal.
- Documented limitation: `find -delete`, `sed -i`, and redirection tricks are not
  caught; the wrapper FS is the real write-boundary.

### Guardrails (hardcoded defaults)

- Agent loop: `stopWhen: isStepCount(25)`.
- Per-bash-command: just-bash default `maxExecutionTimeMs` (30s).
- Whole run: `AbortSignal.timeout(5 * 60 * 1000)`.

## Agent construction

```ts
import { ToolLoopAgent, isStepCount } from "ai";
import { fireworks } from "@ai-sdk/fireworks";

const agent = new ToolLoopAgent({
  model: fireworks(modelId), // from --model flag
  tools,                     // { bash, readFile, writeFile } from createBashTool
  stopWhen: isStepCount(25),
});
const result = await agent.generate({ prompt: userPromptText });
```

### Built-in system prompt

The user prompt file content is the task; it is applied on top of a built-in system
prompt containing:

- Current **local** date/time and timezone name (so "yesterday's daily note" is
  unambiguous); also pass the host `TZ` env var into the sandbox so `date` inside bash
  agrees.
- The cwd path and the active allowlist glob (tell the agent it may only touch files
  matching it; attempts otherwise will fail with EACCES).
- The active deny list (if any).
- Tool-usage guidance: prefer `readFile`/`writeFile` for file edits, `bash` for
  search/exploration.
- Instruction to finish and stop when the task is done (it's a one-shot run, not a
  conversation).

## Audit logging

- Directory: `.agent-bobik/` inside cwd, created on each run.
- One file per run: `.agent-bobik/<local-ISO-timestamp-with-ms>.jsonl`, with `:`
  replaced by `-`, e.g. `2026-10-19T19-55-00.123.jsonl`. Milliseconds make CRON runs
  collision-proof; files sort chronologically.
- **Full-fidelity JSONL events** (one JSON object per line, each with `ts` and `type`):
  - `run-start` — flags (prompt path, allow-path, deny, model), cwd, resolved date/TZ.
  - `tool-call` — tool name + full arguments (for `writeFile`: full content).
  - `tool-result` — full output (stdout/stderr/exitCode; full written content).
  - `assistant-text` — model text/reasoning messages.
  - `step-finish` — token usage per step.
  - `run-end` — status (success/error), error message if any, total duration, total
    token usage.
- No truncation: notes are small Markdown files; keep perfect replay capability.
- The log file is written by the **host** process; the sandboxed agent cannot see
  `.agent-bobik/` (glob `dot: false`).

## Project layout (suggested)

```
src/
  cli.ts            # citty entry, shebang, default command + set-api-key subcommand
  config.ts         # XDG config resolution, API key read/write (0o600)
  agent.ts          # ToolLoopAgent construction, system prompt assembly, guardrails
  sandbox.ts        # just-bash Bash + ReadWriteFs + allowlist wrapper FS wiring
  allowlist-fs.ts   # the wrapper FS enforcing the glob
  deny.ts           # AST command extraction + deny matching
  logger.ts         # JSONL audit log writer
tests/
  allowlist-fs.test.ts
  deny.test.ts
  smoke.test.ts     # MockLanguageModel-driven integration test
examples/
  daily-notes.md    # example prompt: TODO carryover use case
```

## Toolchain configuration

### TypeScript (`tsconfig.json`)

- `noEmit: true`, `erasableSyntaxOnly: true`, `verbatimModuleSyntax: true`,
  `strict: true`, `module: "nodenext"`, `target: "esnext"`, `skipLibCheck: true`.
- `@types/node` as devDependency.
- No enums, no parameter properties, `import type` for type-only imports (enforced by
  `erasableSyntaxOnly`).

### ESLint (`eslint.config.ts` — flat config)

- `@eslint/js` recommended + `typescript-eslint` recommended (**non**-type-checked)
  + `eslint-config-prettier` last to disable conflicting rules.

### Prettier

- Stock defaults, no config overrides.

### package.json scripts

```json
{
  "lint": "eslint .",
  "format": "prettier --write .",
  "format:check": "prettier --check .",
  "typecheck": "tsc --noEmit",
  "test": "node --test tests/"
}
```

(Adjust the test glob to match the actual layout, e.g. `node --test "tests/**/*.test.ts"` —
Node ≥22.18 runs `.ts` tests natively.)

## Testing

Use `node:test` + `node:assert/strict` (no vitest).

1. **`allowlist-fs` unit tests** — glob edge cases: nested paths, `**/*.md` matching,
   dotfiles never matching, `../` traversal rejected, absolute paths outside root
   rejected, EACCES on non-matching read/write/delete.
2. **`deny` unit tests** — command extraction: plain `rm`, pipes (`cat x | rm`),
   `xargs rm`, chained commands (`&&`, `;`), subshells, quoting (`r"m"` — document
   behavior either way), non-denied commands pass through.
3. **Smoke integration test** — use AI SDK's
   [testing utilities](https://ai-sdk.dev/docs/ai-sdk-core/testing)
   (`MockLanguageModel` from `ai/test`) to drive `ToolLoopAgent` against a fixture
   notes dir in a temp folder: mock model emits a `writeFile` tool call, assert the
   file landed on disk, the allowlist blocked an out-of-glob write, and the JSONL log
   contains the expected event sequence. No real API key needed.

## Repo scaffolding

- `examples/daily-notes.md` — the first real-world prompt: *"Take yesterday's daily
  note, copy unfinished TODO backlog items into today's note; items carried over more
  than 3 days get an emoji + day-count marker."* (Used with
  `--allow-path "./daily-notes/**/*.md"`.)
- `README.md` — what it is, install (`pnpm install -g .` / `pnpx bobik`), `set-api-key`,
  flag reference, CRON example, security model summary, log format.
- `.gitignore` — `node_modules/`, `.agent-bobik/`.

## Dependencies (expected)

- **dependencies**: `ai`, `@ai-sdk/fireworks`, `bash-tool`, `just-bash`, `citty`,
  `picomatch`
- **devDependencies**: `typescript`, `@types/node`, `eslint`, `@eslint/js`,
  `typescript-eslint`, `eslint-config-prettier`, `prettier`

## Explicit non-goals

- No `.env` / dotenv support (global config file + env var only).
- No network access inside the sandbox.
- No build step / no esbuild.
- No interactive mode, no conversation history, no streaming output.
- No `--max-steps` / `--timeout` CLI flags (hardcoded defaults for now).
