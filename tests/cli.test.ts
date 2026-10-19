import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import {
  mkdtemp,
  readdir,
  readFile,
  rm,
  stat,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, describe, it } from "node:test";
import { parseDenyList } from "../src/cli.ts";

const cliPath = fileURLToPath(new URL("../src/cli.ts", import.meta.url));
const launcherPath = fileURLToPath(
  new URL("../bin/bobik.mjs", import.meta.url),
);

interface ProcessResult {
  code: number;
  stdout: string;
  stderr: string;
}

function runCli(
  args: string[],
  options: { cwd?: string; env?: NodeJS.ProcessEnv } = {},
): Promise<ProcessResult> {
  return runNode(cliPath, args, options);
}

function runNode(
  entry: string,
  args: string[],
  options: { cwd?: string; env?: NodeJS.ProcessEnv } = {},
): Promise<ProcessResult> {
  return new Promise((resolvePromise) => {
    execFile(
      process.execPath,
      [entry, ...args],
      {
        cwd: options.cwd,
        env: options.env ?? process.env,
      },
      (error, stdout, stderr) => {
        const code =
          error && typeof error === "object" && "code" in error
            ? Number(error.code)
            : 0;
        resolvePromise({ code, stdout, stderr });
      },
    );
  });
}

describe("parseDenyList", () => {
  it("splits, trims and drops empty entries", () => {
    assert.deepEqual(parseDenyList(" rm, wget,,curl "), ["rm", "wget", "curl"]);
  });

  it("returns an empty list when the flag is absent", () => {
    assert.deepEqual(parseDenyList(undefined), []);
  });
});

describe("cli", () => {
  let root: string | undefined;

  afterEach(async () => {
    if (root) {
      await rm(root, { recursive: true, force: true });
      root = undefined;
    }
  });

  it("prints help and exits 0", async () => {
    const result = await runCli(["--help"]);

    assert.equal(result.code, 0);
    assert.match(result.stdout, /Usage:/);
    assert.match(result.stdout, /--allow-path/);
    assert.match(result.stdout, /set-api-key/);
  });

  it("prints the version and exits 0", async () => {
    const result = await runCli(["--version"]);

    assert.equal(result.code, 0);
    assert.match(result.stdout, /^\d+\.\d+\.\d+/);
  });

  it("runs through the published launcher without warnings", async () => {
    const result = await runNode(launcherPath, ["--version"]);

    assert.equal(result.code, 0);
    assert.match(result.stdout, /^\d+\.\d+\.\d+/);
    assert.equal(result.stderr, "");
  });

  it("shows subcommand help", async () => {
    const result = await runCli(["set-api-key", "--help"]);

    assert.equal(result.code, 0);
    assert.match(result.stdout, /set-api-key/);
    assert.match(result.stdout, /KEY/);
  });

  it("exits 2 when a flag is missing its value", async () => {
    const model = await runCli([
      "--prompt",
      "prompt.md",
      "--allow-path",
      "./notes/**/*.md",
      "--model",
    ]);
    assert.equal(model.code, 2);
    assert.match(model.stderr, /--model requires a value/);

    const deny = await runCli([
      "--prompt",
      "prompt.md",
      "--allow-path",
      "./notes/**/*.md",
      "--deny",
    ]);
    assert.equal(deny.code, 2);
    assert.match(deny.stderr, /--deny requires a value/);
  });

  it("rejects extra set-api-key arguments", async () => {
    root = await mkdtemp(join(tmpdir(), "bobik-cli-"));

    const result = await runCli(["set-api-key", "fw-secret", "extra"], {
      env: { ...process.env, XDG_CONFIG_HOME: root },
    });

    assert.equal(result.code, 2);
    assert.match(result.stderr, /Unexpected argument: extra/);
    await assert.rejects(readFile(join(root, "bobik", "config.json")));
  });

  it("exits 2 when required flags are missing", async () => {
    const result = await runCli([]);

    assert.equal(result.code, 2);
    assert.match(result.stderr, /Missing required argument/);
  });

  it("exits 2 when the prompt file does not exist", async () => {
    root = await mkdtemp(join(tmpdir(), "bobik-cli-"));
    const result = await runCli(
      ["--prompt", "missing.md", "--allow-path", "./notes/**/*.md"],
      {
        cwd: root,
        env: { ...process.env, FIREWORKS_API_KEY: "test-key" },
      },
    );

    assert.equal(result.code, 2);
    assert.match(result.stderr, /Prompt file not found/);
  });

  it("exits 2 and points at set-api-key when no key is configured", async () => {
    root = await mkdtemp(join(tmpdir(), "bobik-cli-"));
    await writeFile(join(root, "prompt.md"), "Do the task.");
    const env: NodeJS.ProcessEnv = { ...process.env, XDG_CONFIG_HOME: root };
    delete env.FIREWORKS_API_KEY;
    delete env.HOME;

    const result = await runCli(
      ["--prompt", "prompt.md", "--allow-path", "./notes/**/*.md"],
      { cwd: root, env },
    );

    assert.equal(result.code, 2);
    assert.match(result.stderr, /bobik set-api-key/);
  });

  it("exits 2 when the allow-path escapes the working directory", async () => {
    root = await mkdtemp(join(tmpdir(), "bobik-cli-"));
    await writeFile(join(root, "prompt.md"), "Do the task.");

    const result = await runCli(
      ["--prompt", "prompt.md", "--allow-path", "../**/*.md"],
      {
        cwd: root,
        env: { ...process.env, FIREWORKS_API_KEY: "test-key" },
      },
    );

    assert.equal(result.code, 2);
    assert.match(result.stderr, /must stay inside/);

    const logEvent = JSON.parse(
      (await readFile(await findOnlyLog(root), "utf8"))
        .trimEnd()
        .split("\n")[0] ?? "",
    ) as { type: string };
    assert.equal(logEvent.type, "run-start");
  });

  it("saves the API key to the XDG config with mode 0o600", async () => {
    root = await mkdtemp(join(tmpdir(), "bobik-cli-"));

    const result = await runCli(["set-api-key", "fw-secret"], {
      env: { ...process.env, XDG_CONFIG_HOME: root },
    });

    assert.equal(result.code, 0);
    const path = join(root, "bobik", "config.json");
    assert.deepEqual(JSON.parse(await readFile(path, "utf8")), {
      apiKey: "fw-secret",
    });
    assert.equal((await stat(path)).mode & 0o777, 0o600);
  });
});

async function findOnlyLog(root: string): Promise<string> {
  const entries = await readdir(join(root, ".agent-bobik"));
  assert.equal(entries.length, 1);
  return join(root, ".agent-bobik", entries[0] ?? "");
}
