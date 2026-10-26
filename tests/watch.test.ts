import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { existsSync } from "node:fs";
import {
  mkdtemp,
  mkdir,
  readdir,
  readFile,
  rm,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, it } from "node:test";
import { MockLanguageModelV3 } from "ai/test";
import {
  runNotesAgent,
  type RunNotesAgentOptions,
  type RunSummary,
} from "../src/agent.ts";
import { watchMode, type WatchModeOptions } from "../src/watch.ts";

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

function stopResponse(text = "done") {
  return {
    content: [{ type: "text" as const, text }],
    finishReason: { unified: "stop" as const, raw: "stop" },
    usage: usage(),
    warnings: [],
  };
}

type MockResponse =
  | ReturnType<typeof stopResponse>
  | {
      content: Array<{
        type: "tool-call";
        toolCallId: string;
        toolName: string;
        input: string;
      }>;
      finishReason: { unified: "tool-calls"; raw: string };
      usage: ReturnType<typeof usage>;
      warnings: never[];
    };

interface Harness {
  root: string;
  signals: EventEmitter;
  runs: RunNotesAgentOptions[];
  stderr: string[];
  session: Promise<number>;
}

interface HarnessOptions {
  /** Called for every model generation; may throw to fail the run. */
  onGenerate?: (
    call: number,
    runIndex: number,
  ) => MockResponse | undefined | Promise<MockResponse | undefined>;
  watch?: Partial<WatchModeOptions>;
}

async function startWatch(
  options: HarnessOptions = {},
  allowPath = "./daily-notes/**/*.md",
): Promise<Harness> {
  const root = await mkdtemp(join(tmpdir(), "bobik-watch-"));
  await mkdir(join(root, "daily-notes"), { recursive: true });
  await writeFile(join(root, "prompt.md"), "Maintain the notes.");

  let generations = 0;
  const model = new MockLanguageModelV3({
    modelId: "mock-model",
    doGenerate: async () => {
      generations += 1;
      // runs.push happens when the run starts, so the in-flight run is last.
      const runIndex = harness.runs.length - 1;
      const custom = await options.onGenerate?.(generations, runIndex);
      return custom ?? stopResponse();
    },
  });

  const signals = new EventEmitter();
  const runs: RunNotesAgentOptions[] = [];
  const stderr: string[] = [];
  const runAgent = (runOptions: RunNotesAgentOptions): Promise<RunSummary> => {
    runs.push(runOptions);
    return runNotesAgent(runOptions);
  };

  const harness: Harness = {
    root,
    signals,
    runs,
    stderr,
    session: Promise.resolve(0),
  };
  harness.session = watchMode({
    cwd: root,
    promptPath: join(root, "prompt.md"),
    promptDisplayPath: "prompt.md",
    allowPath,
    model,
    runAgent,
    signals,
    settleMs: 100,
    retryDelayMs: 100,
    awaitWriteFinishMs: 30,
    stdout: () => {},
    stderr: (text) => stderr.push(text),
    ...options.watch,
  });
  return harness;
}

async function stopWatch(harness: Harness): Promise<number> {
  harness.signals.emit("SIGINT");
  const code = await harness.session;
  await rm(harness.root, { recursive: true, force: true });
  return code;
}

async function waitFor(
  condition: () => boolean,
  description: string,
  timeoutMs = 10_000,
): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (!condition()) {
    if (Date.now() > deadline) {
      throw new Error(`Timed out waiting for: ${description}`);
    }
    await new Promise((resolve) => setTimeout(resolve, 20));
  }
}

/** Wait long enough for any (incorrectly) triggered run to have started. */
async function settle(ms = 600): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, ms));
}

describe("watch mode", () => {
  let harness: Harness | undefined;

  afterEach(async () => {
    if (harness) {
      harness.signals.emit("SIGINT");
      harness.signals.emit("SIGINT");
      await harness.session.catch(() => {});
      await rm(harness.root, { recursive: true, force: true });
      harness = undefined;
    }
  });

  it("runs once at startup with the initial-run context", async () => {
    harness = await startWatch();

    await waitFor(() => harness!.runs.length === 1, "initial run");
    assert.equal(harness.runs[0]?.initialRun, true);
    assert.equal(harness.runs[0]?.changedFiles, undefined);

    assert.equal(await stopWatch(harness), 0);
  });

  it("deduplicates by content hash: same content does not re-trigger", async () => {
    harness = await startWatch();
    await waitFor(() => harness!.runs.length === 1, "initial run");

    const note = join(harness.root, "daily-notes", "a.md");
    await writeFile(note, "task @bobik v1\n");
    await waitFor(() => harness!.runs.length === 2, "run for v1");
    assert.deepEqual(
      harness.runs[1]?.changedFiles?.map((file) => file.path),
      ["daily-notes/a.md"],
    );

    // Same bytes again: the event fires but the hash matches.
    await writeFile(note, "task @bobik v1\n");
    await settle();
    assert.equal(harness.runs.length, 2);

    await writeFile(note, "task @bobik v2\n");
    await waitFor(() => harness!.runs.length === 3, "run for v2");

    assert.equal(await stopWatch(harness), 0);
  });

  it("ignores changed files without the trigger string", async () => {
    harness = await startWatch();
    await waitFor(() => harness!.runs.length === 1, "initial run");

    await writeFile(join(harness.root, "daily-notes", "plain.md"), "no tag\n");
    await settle();
    assert.equal(harness.runs.length, 1);

    await writeFile(join(harness.root, "daily-notes", "plain.md"), "@bobik\n");
    await waitFor(() => harness!.runs.length === 2, "tagged run");

    assert.equal(await stopWatch(harness), 0);
  });

  it("suppresses files written by the agent itself", async () => {
    let toolCallPending = true;
    harness = await startWatch({
      onGenerate: () => {
        if (!toolCallPending) {
          return undefined;
        }
        toolCallPending = false;
        return {
          content: [
            {
              type: "tool-call" as const,
              toolCallId: "call-write",
              toolName: "writeFile",
              input: JSON.stringify({
                path: "daily-notes/agent.md",
                content: "written by the agent @bobik\n",
              }),
            },
          ],
          finishReason: { unified: "tool-calls" as const, raw: "tool-calls" },
          usage: usage(),
          warnings: [],
        };
      },
    });

    await waitFor(() => harness!.runs.length === 1, "initial run");
    await waitFor(
      () => existsSync(join(harness!.root, "daily-notes", "agent.md")),
      "agent write",
    );
    await settle();
    // The agent's own tagged write must not trigger a follow-up run.
    assert.equal(harness.runs.length, 1);

    assert.equal(await stopWatch(harness), 0);
  });

  it("coalesces events during a run into a single follow-up run", async () => {
    let firstRunStarted: () => void = () => {};
    const firstRunGate = new Promise<void>((resolve) => {
      firstRunStarted = resolve;
    });
    let releaseFirstRun: () => void = () => {};
    const firstRunRelease = new Promise<void>((resolve) => {
      releaseFirstRun = resolve;
    });

    harness = await startWatch({
      onGenerate: async (_call, runIndex) => {
        if (runIndex !== 0) {
          return undefined;
        }
        firstRunStarted();
        await firstRunRelease;
        return undefined;
      },
    });

    await waitFor(() => harness!.runs.length === 1, "initial run started");
    await firstRunGate;

    // Two tagged writes while the initial run is still in flight.
    await writeFile(join(harness.root, "daily-notes", "a.md"), "@bobik a\n");
    await writeFile(join(harness.root, "daily-notes", "b.md"), "@bobik b\n");
    await settle(300);
    releaseFirstRun();

    await waitFor(() => harness!.runs.length === 2, "coalesced follow-up");
    assert.deepEqual(
      harness.runs[1]?.changedFiles?.map((file) => file.path).sort(),
      ["daily-notes/a.md", "daily-notes/b.md"],
    );
    await settle();
    assert.equal(harness.runs.length, 2);

    assert.equal(await stopWatch(harness), 0);
  });

  it("retries a failed run once, then succeeds", async () => {
    let failed = false;
    harness = await startWatch({
      onGenerate: (_call, runIndex) => {
        if (runIndex === 1 && !failed) {
          failed = true;
          throw new Error("boom");
        }
        return undefined;
      },
    });
    await waitFor(() => harness!.runs.length === 1, "initial run");

    await writeFile(join(harness.root, "daily-notes", "a.md"), "@bobik\n");
    await waitFor(() => harness!.runs.length === 3, "retry");
    assert.ok(
      harness.stderr.some((line) => /retrying/.test(line)),
      "expected a retry notice on stderr",
    );
    assert.ok(
      !harness.stderr.some((line) => /gave up/.test(line)),
      "did not expect a give-up",
    );

    assert.equal(await stopWatch(harness), 0);
  });

  it("gives up after one failed retry and logs it", async () => {
    harness = await startWatch({
      onGenerate: (_call, runIndex) => {
        if (runIndex >= 1) {
          throw new Error("always fails");
        }
        return undefined;
      },
    });
    await waitFor(() => harness!.runs.length === 1, "initial run");

    await writeFile(join(harness.root, "daily-notes", "a.md"), "@bobik\n");
    await waitFor(() => harness!.runs.length === 3, "failed retry");
    await waitFor(
      () =>
        harness!.stderr.some((line) =>
          /gave up on daily-notes\/a\.md/.test(line),
        ),
      "give-up notice",
    );
    await settle();
    assert.equal(harness.runs.length, 3);

    const logs = await readdir(join(harness.root, ".agent-bobik"));
    const gaveUp = await Promise.all(
      logs.map(async (name) =>
        (
          await readFile(join(harness!.root, ".agent-bobik", name), "utf8")
        ).includes('"watch-gave-up"'),
      ),
    );
    assert.ok(gaveUp.some(Boolean), "expected a watch-gave-up audit event");

    assert.equal(await stopWatch(harness), 0);
  });

  it("records deletions in the next triggered run without triggering one", async () => {
    harness = await startWatch();
    await waitFor(() => harness!.runs.length === 1, "initial run");

    const doomed = join(harness.root, "daily-notes", "doomed.md");
    await writeFile(doomed, "@bobik delete me\n");
    await waitFor(() => harness!.runs.length === 2, "run for the new file");

    // Deleting a tagged file must not trigger a run on its own.
    await rm(doomed);
    await settle();
    assert.equal(harness.runs.length, 2);

    await writeFile(join(harness.root, "daily-notes", "other.md"), "@bobik\n");
    await waitFor(() => harness!.runs.length === 3, "run after deletion");
    assert.deepEqual(harness.runs[2]?.changedFiles, [
      { path: "daily-notes/other.md" },
      { path: "daily-notes/doomed.md", deleted: true },
    ]);

    assert.equal(await stopWatch(harness), 0);
  });

  it("stops the session with exit 1 when the prompt becomes unreadable", async () => {
    harness = await startWatch();
    await waitFor(() => harness!.runs.length === 1, "initial run");

    await rm(join(harness.root, "prompt.md"));
    await writeFile(join(harness.root, "daily-notes", "a.md"), "@bobik\n");

    const code = await harness.session;
    assert.equal(code, 1);
    assert.ok(
      harness.stderr.some((line) => /could not read prompt file/.test(line)),
      "expected a prompt-file error on stderr",
    );
    await rm(harness.root, { recursive: true, force: true });
    harness = undefined;
  });
});
