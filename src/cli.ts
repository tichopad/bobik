#!/usr/bin/env node

import { existsSync, realpathSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { join, resolve } from "node:path";
import { createFireworks } from "@ai-sdk/fireworks";
import { defineCommand, runCommand, showUsage, type CommandDef } from "citty";
import { MAX_STEPS, RUN_TIMEOUT_MS, runNotesAgent } from "./agent.ts";
import { compileAllowlist } from "./allowlist.ts";
import { requireApiKey, writeApiKey } from "./config.ts";
import { RunError, UsageError } from "./errors.ts";

export const DEFAULT_MODEL =
  "accounts/fireworks/models/deepseek-v4p1-flash-1017";

const VERSION = "0.1.0";

const ANSI_ESCAPE = new RegExp(`${String.fromCharCode(27)}\\[[0-9;]*m`, "g");

const HELP = `bobik — a single-purpose AI agent for Markdown notes

Usage:
  bobik --prompt <path> --allow-path <glob> [--deny "rm,wget"] [--model <id>]
  bobik set-api-key <key>
  bobik --help

Options:
  --prompt <path>       Markdown file with task instructions (required)
  --allow-path <glob>   Only files matching this glob may be read or written (required)
  --deny <list>         Comma-separated bash command names to block
  --model <id>          Fireworks model ID (default: ${DEFAULT_MODEL})

Environment:
  FIREWORKS_API_KEY     Overrides the key saved by set-api-key

The API key is read from FIREWORKS_API_KEY or $XDG_CONFIG_HOME/bobik/config.json
(~/.config/bobik/config.json when XDG_CONFIG_HOME is unset).

Runs are limited to ${MAX_STEPS} steps and ${RUN_TIMEOUT_MS / 60_000} minutes.
`;

const runArgs = {
  prompt: {
    type: "string",
    required: true,
    description: "Markdown file with task instructions",
  },
  "allow-path": {
    type: "string",
    required: true,
    description: "Glob of files the agent may read or write",
  },
  deny: {
    type: "string",
    description: 'Comma-separated command names to block, e.g. "rm,wget"',
  },
  model: {
    type: "string",
    default: DEFAULT_MODEL,
    description: "Fireworks model ID",
  },
} as const;

const topLevelArgs = {
  prompt: { type: "string" },
  "allow-path": { type: "string" },
  deny: { type: "string" },
  model: { type: "string" },
} as const;

export const runCommandDefinition = defineCommand({
  meta: {
    name: "run",
    description: "Run the notes agent (default command)",
    hidden: true,
  },
  args: runArgs,
  async run({ args }) {
    if (args._.length > 0) {
      throw new UsageError(`Unexpected argument: ${String(args._[0])}`);
    }
    await runDefault({
      prompt: args.prompt,
      allowPath: args["allow-path"],
      deny: args.deny,
      model: args.model,
    });
  },
});

export const setApiKeyCommand = defineCommand({
  meta: {
    name: "set-api-key",
    description: "Save the Fireworks API key to the global config file",
  },
  args: {
    key: {
      type: "positional",
      required: true,
      description: "Fireworks API key",
    },
  },
  async run({ args }) {
    if (args._.length > 1) {
      throw new UsageError(`Unexpected argument: ${String(args._[1])}`);
    }
    const path = await writeApiKey(args.key);
    process.stdout.write(`API key saved to ${path}\n`);
  },
});

export const mainCommand = defineCommand({
  meta: {
    name: "bobik",
    version: VERSION,
    description: "A single-purpose AI agent for Markdown notes",
  },
  args: topLevelArgs,
  default: "run",
  subCommands: {
    run: runCommandDefinition,
    "set-api-key": setApiKeyCommand,
  },
});

interface DefaultRunArgs {
  prompt: string;
  allowPath: string;
  deny?: string;
  model: string;
}

async function runDefault(args: DefaultRunArgs): Promise<void> {
  if (args.model.trim() === "") {
    throw new UsageError("--model requires a value.");
  }
  if (args.deny !== undefined && args.deny.trim() === "") {
    throw new UsageError("--deny requires a non-empty value.");
  }

  const cwd = process.cwd();
  const promptPath = resolve(cwd, args.prompt);
  const prompt = await readPrompt(promptPath);
  const apiKey = await requireApiKey();
  const deny = parseDenyList(args.deny);
  const warning = allowPathWarning(cwd, args.allowPath);
  if (warning) {
    process.stderr.write(warning);
  }
  const provider = createFireworks({ apiKey });

  const summary = await runNotesAgent({
    cwd,
    promptPath: args.prompt,
    prompt,
    allowPath: args.allowPath,
    deny,
    model: provider(args.model),
  });

  const text = summary.text.endsWith("\n") ? summary.text : `${summary.text}\n`;
  process.stdout.write(text);
  process.stderr.write(`${formatSummary(summary)}\n`);
}

async function readPrompt(path: string): Promise<string> {
  try {
    return await readFile(path, "utf8");
  } catch (error) {
    if (isNotFound(error)) {
      throw new UsageError(`Prompt file not found: ${path}`);
    }
    throw new UsageError(
      `Could not read prompt file ${path}: ${errorMessage(error)}`,
    );
  }
}

/** Parse the CLI deny flag into normalized command names. */
export function parseDenyList(value: string | undefined): string[] {
  return (value ?? "")
    .split(",")
    .map((entry) => entry.trim())
    .filter(Boolean);
}

/**
 * Detect the shell-quoting pitfall in --allow-path and return a warning, or
 * undefined when the glob looks fine.
 *
 * Inside shell quotes a backslash is passed through verbatim, so a glob like
 * "./Daily\ Notes" reaches bobik with a literal backslash before the space.
 * Escapes are resolved for matching and traversal, but a backslashed segment
 * that names no real directory is almost always a quoting mistake or a typo,
 * so it is reported before the run starts.
 */
export function allowPathWarning(
  cwd: string,
  allowPath: string,
): string | undefined {
  let escapedDirectories: readonly string[];
  try {
    escapedDirectories = compileAllowlist(
      cwd,
      allowPath,
    ).escapedBaseDirectories;
  } catch {
    // Invalid globs surface as usage errors when the run starts.
    return undefined;
  }

  const missing = escapedDirectories
    .map((directory) => join(cwd, directory))
    .find((directory) => !existsSync(directory));
  if (!missing) {
    return undefined;
  }

  return [
    `bobik: warning: --allow-path names a directory that does not exist: ${missing}`,
    `bobik: if the backslash was meant for the shell, quote the glob without escapes: --allow-path "./Daily Notes/**/*.md"`,
    "",
  ].join("\n");
}

function formatSummary(summary: {
  steps: number;
  filesWritten: number;
  usage: {
    inputTokens: number | undefined;
    outputTokens: number | undefined;
    totalTokens: number | undefined;
  };
  logRelativePath: string;
}): string {
  const steps = `${summary.steps} ${summary.steps === 1 ? "step" : "steps"}`;
  const files = `${summary.filesWritten} ${
    summary.filesWritten === 1 ? "file" : "files"
  } written`;
  const input = summary.usage.inputTokens ?? 0;
  const output = summary.usage.outputTokens ?? 0;
  const total = summary.usage.totalTokens ?? input + output;

  return `bobik: ${steps}, ${files}, ${total} tokens (in ${input}, out ${output}), log ${summary.logRelativePath}`;
}

/**
 * Run the CLI and return its exit code. Exported for tests; the executable
 * entry point applies the code to process.exitCode.
 */
export async function main(rawArgs: string[]): Promise<number> {
  if (rawArgs[0] === "set-api-key" && hasHelpFlag(rawArgs)) {
    await showUsage(setApiKeyCommand as CommandDef, mainCommand as CommandDef);
    return 0;
  }
  if (rawArgs[0] === "run" && hasHelpFlag(rawArgs)) {
    await showUsage(
      runCommandDefinition as CommandDef,
      mainCommand as CommandDef,
    );
    return 0;
  }
  if (hasHelpFlag(rawArgs)) {
    process.stdout.write(HELP);
    return 0;
  }
  if (rawArgs.length === 1 && rawArgs[0] === "--version") {
    process.stdout.write(`${VERSION}\n`);
    return 0;
  }

  try {
    assertFlagValues(rawArgs);
    await runCommand(mainCommand, { rawArgs });
    return 0;
  } catch (error) {
    const { code, message } = classifyError(error);
    process.stderr.write(`bobik: ${message}\n`);
    if (error instanceof RunError && error.summary) {
      process.stderr.write(`${formatSummary(error.summary)}\n`);
    }
    return code;
  }
}

function hasHelpFlag(rawArgs: string[]): boolean {
  return rawArgs.includes("--help") || rawArgs.includes("-h");
}

const FLAGS_WITH_VALUES: ReadonlySet<string> = new Set([
  "--prompt",
  "--allow-path",
  "--deny",
  "--model",
]);

function assertFlagValues(rawArgs: string[]): void {
  for (let index = 0; index < rawArgs.length; index += 1) {
    const flag = rawArgs[index];
    if (!flag || !FLAGS_WITH_VALUES.has(flag)) {
      continue;
    }
    const value = rawArgs[index + 1];
    if (value === undefined || (value.startsWith("-") && value !== "-")) {
      throw new UsageError(`${flag} requires a value.`);
    }
  }
}

function classifyError(error: unknown): { code: number; message: string } {
  if (error instanceof UsageError) {
    return { code: 2, message: error.message };
  }
  if (error instanceof RunError) {
    return { code: 1, message: error.message };
  }
  if (isCittyUsageError(error)) {
    return { code: 2, message: errorMessage(error) };
  }
  return { code: 1, message: errorMessage(error) };
}

function isCittyUsageError(error: unknown): boolean {
  if (typeof error !== "object" || error === null) {
    return false;
  }
  const code = "code" in error ? error.code : undefined;
  return code === "EARG" || code === "E_UNKNOWN_COMMAND";
}

function isNotFound(error: unknown): boolean {
  return (
    typeof error === "object" &&
    error !== null &&
    "code" in error &&
    error.code === "ENOENT"
  );
}

function errorMessage(error: unknown): string {
  const message = error instanceof Error ? error.message : String(error);
  return message.replace(ANSI_ESCAPE, "");
}

function isEntrypoint(): boolean {
  const entry = process.argv[1];
  if (!entry) {
    return false;
  }
  try {
    return realpathSync(entry) === realpathSync(fileURLToPath(import.meta.url));
  } catch {
    return false;
  }
}

if (isEntrypoint()) {
  process.exitCode = await main(process.argv.slice(2));
}
