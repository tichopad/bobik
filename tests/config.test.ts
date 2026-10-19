import assert from "node:assert/strict";
import { mkdir, mkdtemp, readFile, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, describe, it } from "node:test";
import {
  configFilePath,
  readApiKey,
  requireApiKey,
  writeApiKey,
} from "../src/config.ts";
import { UsageError } from "../src/errors.ts";

describe("config", () => {
  let xdgHome: string;
  let env: { XDG_CONFIG_HOME: string; HOME: string };

  beforeEach(async () => {
    xdgHome = await mkdtemp(join(tmpdir(), "bobik-config-"));
    env = { XDG_CONFIG_HOME: xdgHome, HOME: "/home/ignored" };
  });

  afterEach(() => {
    // Temporary directories are left to the OS; the fixtures are tiny.
  });

  it("resolves the config path under XDG_CONFIG_HOME", () => {
    assert.equal(configFilePath(env), join(xdgHome, "bobik", "config.json"));
  });

  it("falls back to ~/.config when XDG_CONFIG_HOME is unset", () => {
    assert.equal(
      configFilePath({ HOME: "/home/tester" }),
      "/home/tester/.config/bobik/config.json",
    );
  });

  it("writes the API key with mode 0o600 and creates parent directories", async () => {
    const path = await writeApiKey("fw-secret", env);

    assert.equal(path, join(xdgHome, "bobik", "config.json"));
    assert.deepEqual(JSON.parse(await readFile(path, "utf8")), {
      apiKey: "fw-secret",
    });
    assert.equal((await stat(path)).mode & 0o777, 0o600);
  });

  it("reads the API key from the config file", async () => {
    await writeApiKey("fw-file", env);

    assert.equal(await readApiKey(env), "fw-file");
  });

  it("prefers FIREWORKS_API_KEY over the config file", async () => {
    await writeApiKey("fw-file", env);

    assert.equal(
      await readApiKey({ ...env, FIREWORKS_API_KEY: "fw-env" }),
      "fw-env",
    );
  });

  it("returns undefined when no key is configured", async () => {
    assert.equal(await readApiKey(env), undefined);
  });

  it("requireApiKey explains how to configure a missing key", async () => {
    await assert.rejects(requireApiKey(env), (error: unknown) => {
      assert.ok(error instanceof UsageError);
      assert.match(error.message, /bobik set-api-key/);
      return true;
    });
  });

  it("rejects malformed config files as a usage error", async () => {
    const path = configFilePath(env);
    await mkdir(join(xdgHome, "bobik"), { recursive: true });
    await writeFile(path, "not json");

    await assert.rejects(readApiKey(env), (error: unknown) => {
      assert.ok(error instanceof UsageError);
      assert.match(error.message, /config/i);
      return true;
    });
  });

  it("rejects config files without a string apiKey", async () => {
    await writeApiKey("placeholder", env);
    await writeFile(
      configFilePath(env),
      JSON.stringify({ apiKey: 42 }),
      "utf8",
    );

    await assert.rejects(readApiKey(env), UsageError);
  });
});
