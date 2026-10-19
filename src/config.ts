import { chmod, mkdir, readFile, writeFile } from "node:fs/promises";
import { homedir } from "node:os";
import { dirname, join } from "node:path";
import { UsageError } from "./errors.ts";

/** The environment variables that influence API key resolution. */
export interface ConfigEnvironment {
  FIREWORKS_API_KEY?: string | undefined;
  XDG_CONFIG_HOME?: string | undefined;
  HOME?: string | undefined;
}

/**
 * Where the global config file lives.
 *
 * `$XDG_CONFIG_HOME/bobik/config.json`, or `~/.config/bobik/config.json` when
 * XDG_CONFIG_HOME is unset or empty.
 */
export function configFilePath(env: ConfigEnvironment = process.env): string {
  const xdgHome = env.XDG_CONFIG_HOME?.trim();
  if (xdgHome) {
    return join(xdgHome, "bobik", "config.json");
  }

  const home = env.HOME?.trim() || homedir();
  return join(home, ".config", "bobik", "config.json");
}

/**
 * Resolve the Fireworks API key. The environment variable wins over the
 * config file. Returns undefined when neither source is configured.
 *
 * Throws {@link UsageError} when the config file exists but is unreadable or
 * malformed.
 */
export async function readApiKey(
  env: ConfigEnvironment = process.env,
): Promise<string | undefined> {
  const fromEnvironment = env.FIREWORKS_API_KEY?.trim();
  if (fromEnvironment) {
    return fromEnvironment;
  }

  const path = configFilePath(env);
  let raw: string;
  try {
    raw = await readFile(path, "utf8");
  } catch (error) {
    if (isNotFound(error)) {
      return undefined;
    }
    throw new UsageError(
      `Could not read bobik config at ${path}: ${errorMessage(error)}`,
    );
  }

  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch (error) {
    throw new UsageError(
      `Invalid bobik config at ${path}: ${errorMessage(error)}`,
    );
  }

  if (
    typeof parsed !== "object" ||
    parsed === null ||
    !("apiKey" in parsed) ||
    typeof parsed.apiKey !== "string" ||
    parsed.apiKey.trim() === ""
  ) {
    throw new UsageError(
      `Invalid bobik config at ${path}: expected { "apiKey": "<key>" }`,
    );
  }

  return parsed.apiKey.trim();
}

/**
 * Resolve the API key or explain how to configure one.
 */
export async function requireApiKey(
  env: ConfigEnvironment = process.env,
): Promise<string> {
  const apiKey = await readApiKey(env);
  if (!apiKey) {
    throw new UsageError(
      "No Fireworks API key found. Set FIREWORKS_API_KEY or run: bobik set-api-key <key>",
    );
  }
  return apiKey;
}

/**
 * Persist the API key to the global config file with mode 0o600.
 * Returns the path that was written.
 */
export async function writeApiKey(
  apiKey: string,
  env: ConfigEnvironment = process.env,
): Promise<string> {
  const key = apiKey.trim();
  if (!key) {
    throw new UsageError("The API key must not be empty.");
  }

  const path = configFilePath(env);
  await mkdir(dirname(path), { recursive: true, mode: 0o700 });
  await writeFile(path, `${JSON.stringify({ apiKey: key }, null, 2)}\n`, {
    encoding: "utf8",
    mode: 0o600,
  });
  // writeFile only applies mode when creating the file; make an existing file
  // private too.
  await chmod(path, 0o600);
  return path;
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
