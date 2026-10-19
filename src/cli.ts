#!/usr/bin/env node

import { realpathSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { resolve } from "node:path";
import { createFireworks } from "@ai-sdk/fireworks";
import { defineCommand, runCommand } from "citty";
import { MAX_STEPS, RUN_TIMEOUT_MS, runNotesAgent } from "./agent.ts";
import { requireApiKey, writeApiKey } from "./config.ts";
import { RunError, UsageError } from "./errors.ts";

export const DEFAULT_MODEL =
  "accounts/fireworks/models/deepseek-v4p1-flash-1017";

const VERSION = "0.1.0";

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
  const cwd = process.cwd();
  const promptPath = resolve(cwd, args.prompt);
  const prompt = await readPrompt(promptPath);
  const apiKey = await requireApiKey();
  const deny = parseDenyList(args.deny);
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
  if (rawArgs.includes("--help") || rawArgs.includes("-h")) {
    process.stdout.write(HELP);
    return 0;
  }
  if (rawArgs.length === 1 && rawArgs[0] === "--version") {
    process.stdout.write(`${VERSION}\n`);
    return 0;
  }

  try {
    await runCommand(mainCommand, { rawArgs });
    return 0;
  } catch (error) {
    const { code, message } = classifyError(error);
    process.stderr.write(`bobik: ${message}\n`);
    return code;
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
  return error instanceof Error ? error.message : String(error);
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
