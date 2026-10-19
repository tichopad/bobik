import assert from "node:assert/strict";
import { mkdtemp, readFile, rm, stat } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, it } from "node:test";
import { AuditLog, auditLogFilename } from "../src/logger.ts";

describe("auditLogFilename", () => {
  it("uses local time with milliseconds and filesystem-safe separators", () => {
    const date = new Date(2026, 9, 19, 19, 55, 0, 123);

    assert.equal(auditLogFilename(date), "2026-10-19T19-55-00.123.jsonl");
  });
});

describe("AuditLog", () => {
  let root: string | undefined;

  afterEach(async () => {
    if (root) {
      await rm(root, { recursive: true, force: true });
      root = undefined;
    }
  });

  it("creates .agent-bobik and one log file for the run", async () => {
    root = await mkdtemp(join(tmpdir(), "bobik-log-"));
    const log = await AuditLog.create(
      root,
      new Date(2026, 9, 19, 19, 55, 0, 1),
    );

    assert.equal(log.relativePath, join(".agent-bobik", log.filename));
    assert.equal(log.filename, "2026-10-19T19-55-00.001.jsonl");
    assert.equal((await stat(log.path)).isFile(), true);
    assert.equal((await stat(join(root, ".agent-bobik"))).isDirectory(), true);
    assert.equal(await readFile(log.path, "utf8"), "");
  });

  it("writes full-fidelity JSONL events in order", async () => {
    root = await mkdtemp(join(tmpdir(), "bobik-log-"));
    const log = await AuditLog.create(root);

    await log.log("run-start", {
      flags: { prompt: "notes.md", allowPath: "./**/*.md", deny: ["rm"] },
    });
    await log.log("tool-call", {
      toolName: "writeFile",
      input: { path: "a.md", content: "line one\nline two\n" },
    });
    await log.flush();

    const lines = (await readFile(log.path, "utf8")).trimEnd().split("\n");
    assert.equal(lines.length, 2);

    const first = JSON.parse(lines[0] ?? "");
    assert.equal(first.type, "run-start");
    assert.match(first.ts, /^\d{4}-\d{2}-\d{2}T/);
    assert.deepEqual(first.flags, {
      prompt: "notes.md",
      allowPath: "./**/*.md",
      deny: ["rm"],
    });

    const second = JSON.parse(lines[1] ?? "");
    assert.equal(second.type, "tool-call");
    assert.equal(second.toolName, "writeFile");
    assert.equal(second.input.content, "line one\nline two\n");
  });

  it("serializes concurrent log calls without interleaving", async () => {
    root = await mkdtemp(join(tmpdir(), "bobik-log-"));
    const log = await AuditLog.create(root);

    await Promise.all(
      Array.from({ length: 20 }, (_, index) =>
        log.log("assistant-text", { index, text: `message ${index}` }),
      ),
    );
    await log.flush();

    const events = (await readFile(log.path, "utf8"))
      .trimEnd()
      .split("\n")
      .map((line) => JSON.parse(line) as { type: string; index: number });

    assert.deepEqual(
      events.map((event) => event.index),
      Array.from({ length: 20 }, (_, index) => index),
    );
  });

  it("does not overwrite logs created at the same millisecond", async () => {
    root = await mkdtemp(join(tmpdir(), "bobik-log-"));
    const date = new Date(2026, 9, 19, 19, 55, 0, 123);

    const first = await AuditLog.create(root, date);
    const second = await AuditLog.create(root, date);

    assert.notEqual(first.path, second.path);
    assert.equal(first.filename, "2026-10-19T19-55-00.123.jsonl");
    assert.equal(second.filename, "2026-10-19T19-55-00.123-1.jsonl");
  });
});
