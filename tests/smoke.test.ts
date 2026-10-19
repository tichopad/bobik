import assert from "node:assert/strict";
import {
  mkdtemp,
  mkdir,
  readdir,
  readFile,
  rm,
  stat,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, it } from "node:test";
import { MockLanguageModelV3 } from "ai/test";
import { runNotesAgent } from "../src/agent.ts";

function usage(tokens = 10) {
  return {
    inputTokens: {
      total: tokens,
      noCache: tokens,
      cacheRead: 0,
      cacheWrite: 0,
    },
    outputTokens: {
      total: tokens,
      text: tokens,
      reasoning: 0,
    },
    raw: {},
  };
}

function finish(unified: "tool-calls" | "stop") {
  return { unified, raw: unified };
}

describe("smoke: agent against a notes directory", () => {
  let root: string | undefined;

  afterEach(async () => {
    if (root) {
      await rm(root, { recursive: true, force: true });
      root = undefined;
    }
  });

  it("writes allowed files, blocks escapes, and records a full audit log", async () => {
    root = await mkdtemp(join(tmpdir(), "bobik-smoke-"));
    await mkdir(join(root, "daily-notes"), { recursive: true });
    await writeFile(
      join(root, "daily-notes", "2026-10-18.md"),
      "# Yesterday\n- [ ] carry me over\n",
    );

    const model = new MockLanguageModelV3({
      modelId: "mock-model",
      doGenerate: [
        {
          content: [
            {
              type: "text",
              text: "Copying yesterday's unfinished items.",
            },
            {
              type: "tool-call",
              toolCallId: "call-write",
              toolName: "writeFile",
              input: JSON.stringify({
                path: "daily-notes/2026-10-19.md",
                content: "# Today\n- [ ] carry me over\n",
              }),
            },
          ],
          finishReason: finish("tool-calls"),
          usage: usage(),
          warnings: [],
        },
        {
          content: [
            {
              type: "tool-call",
              toolCallId: "call-escape",
              toolName: "writeFile",
              input: JSON.stringify({
                path: "outside.md",
                content: "should not exist",
              }),
            },
          ],
          finishReason: finish("tool-calls"),
          usage: usage(),
          warnings: [],
        },
        {
          content: [
            {
              type: "tool-call",
              toolCallId: "call-deny",
              toolName: "bash",
              input: JSON.stringify({
                command: "rm daily-notes/2026-10-18.md",
              }),
            },
          ],
          finishReason: finish("tool-calls"),
          usage: usage(),
          warnings: [],
        },
        {
          content: [{ type: "text", text: "Done. Copied the backlog item." }],
          finishReason: finish("stop"),
          usage: usage(),
          warnings: [],
        },
      ],
    });

    const summary = await runNotesAgent({
      cwd: root,
      promptPath: "prompt.md",
      prompt: "Carry over unfinished TODOs.",
      allowPath: "./daily-notes/**/*.md",
      deny: ["rm"],
      model,
      now: new Date("2026-10-19T17:55:00.123Z"),
      timeZone: "Europe/Prague",
      timeoutMs: 30_000,
    });

    assert.equal(summary.text, "Done. Copied the backlog item.");
    assert.equal(summary.steps, 4);
    assert.equal(summary.filesWritten, 1);
    assert.equal(
      await readFile(join(root, "daily-notes", "2026-10-19.md"), "utf8"),
      "# Today\n- [ ] carry me over\n",
    );
    await assert.rejects(stat(join(root, "outside.md")));
    assert.match(
      await readFile(join(root, "daily-notes", "2026-10-18.md"), "utf8"),
      /carry me over/,
    );

    const events = (await readFile(summary.logPath, "utf8"))
      .trimEnd()
      .split("\n")
      .map((line) => JSON.parse(line) as Record<string, unknown>);

    assert.equal(events[0]?.type, "run-start");
    assert.deepEqual(events[0]?.flags, {
      prompt: "prompt.md",
      allowPath: "./daily-notes/**/*.md",
      deny: ["rm"],
      model: "mock-model",
    });
    assert.equal(events[0]?.timeZone, "Europe/Prague");
    assert.match(String(events[0]?.localDateTime), /19:55:00/);

    const last = events.at(-1);
    assert.equal(last?.type, "run-end");
    assert.equal(last?.status, "success");
    assert.equal(last?.durationMs !== undefined, true);

    const toolCalls = events.filter((event) => event.type === "tool-call");
    assert.deepEqual(
      toolCalls.map((event) => event.toolName),
      ["writeFile", "writeFile", "bash"],
    );
    assert.equal(
      (toolCalls[0]?.input as { content: string }).content,
      "# Today\n- [ ] carry me over\n",
    );

    const toolResults = events.filter((event) => event.type === "tool-result");
    assert.equal(toolResults.length, 3);
    const deniedResult = toolResults[2]?.output as {
      stdout: string;
      stderr: string;
      exitCode: number;
    };
    assert.equal(deniedResult.exitCode, 126);
    assert.match(deniedResult.stderr, /Blocked: rm is denied/);
    assert.equal(toolResults[1]?.error !== undefined, true);

    assert.equal(
      events.filter((event) => event.type === "step-finish").length,
      4,
    );
    assert.ok(
      events.some(
        (event) =>
          event.type === "assistant-text" &&
          event.text === "Done. Copied the backlog item.",
      ),
    );

    assert.equal(summary.logRelativePath.startsWith(".agent-bobik/"), true);
  });

  it("fails when the step limit is reached", async () => {
    root = await mkdtemp(join(tmpdir(), "bobik-smoke-"));
    await mkdir(join(root, "daily-notes"), { recursive: true });

    const model = new MockLanguageModelV3({
      modelId: "mock-model",
      doGenerate: Array.from({ length: 25 }, (_, index) => ({
        content: [
          {
            type: "tool-call" as const,
            toolCallId: `call-${index}`,
            toolName: "writeFile",
            input: JSON.stringify({
              path: `daily-notes/note-${index}.md`,
              content: `note ${index}`,
            }),
          },
        ],
        finishReason: finish("tool-calls"),
        usage: usage(),
        warnings: [],
      })),
    });

    await assert.rejects(
      runNotesAgent({
        cwd: root,
        promptPath: "prompt.md",
        prompt: "Keep writing notes forever.",
        allowPath: "./daily-notes/**/*.md",
        model,
        timeoutMs: 30_000,
      }),
      /Step limit reached \(25 steps\)/,
    );

    const entries = await readdir(join(root, ".agent-bobik"));
    const events = (
      await readFile(join(root, ".agent-bobik", entries[0] ?? ""), "utf8")
    )
      .trimEnd()
      .split("\n")
      .map((line) => JSON.parse(line) as Record<string, unknown>);
    const last = events.at(-1);
    assert.equal(last?.type, "run-end");
    assert.equal(last?.status, "error");
    assert.match(String(last?.error), /Step limit reached/);
    assert.equal(
      events.filter((event) => event.type === "step-finish").length,
      25,
    );
  });

  it("aborts a stalled run at the timeout", async () => {
    root = await mkdtemp(join(tmpdir(), "bobik-smoke-"));
    await mkdir(join(root, "daily-notes"), { recursive: true });

    const model = new MockLanguageModelV3({
      modelId: "mock-model",
      doGenerate: () => new Promise(() => {}),
    });

    await assert.rejects(
      runNotesAgent({
        cwd: root,
        promptPath: "prompt.md",
        prompt: "Never finish.",
        allowPath: "./daily-notes/**/*.md",
        model,
        timeoutMs: 20,
      }),
      /Run timed out after 20ms/,
    );
  });
});
