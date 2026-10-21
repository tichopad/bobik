import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import {
  mkdtemp,
  mkdir,
  readFile,
  readdir,
  rm,
  symlink,
  writeFile,
} from "node:fs/promises";
import { join } from "node:path";
import { afterEach, beforeEach, describe, it } from "node:test";
import { setTimeout } from "node:timers/promises";
import type { LanguageModelUsage } from "ai";
import { MockLanguageModelV3 } from "ai/test";
import { runNotesAgent } from "../src/agent.ts";
import { RunError } from "../src/errors.ts";
import {
  createViewMediaTool,
  DEFAULT_MEDIA_PROMPT,
  MAX_IMAGE_BYTES,
} from "../src/media.ts";
import { createNotesSandbox } from "../src/sandbox.ts";

const png = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=",
  "base64",
);
const gif = Buffer.from(
  "R0lGODlhAQABAIAAAAAAAP///ywAAAAAAQABAAACAUwAOw==",
  "base64",
);
const webp = Buffer.from(
  "UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA",
  "base64",
);
// A complete 1x1 JPEG, including frame and scan headers.
const jpeg = Buffer.from(
  "/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAP//////////////////////////////////////////////////////////////////////////////////////2wBDAf//////////////////////////////////////////////////////////////////////////////////////wAARCAABAAEDASIAAhEBAxEB/8QAFQABAQAAAAAAAAAAAAAAAAAAAAf/xAAUEAEAAAAAAAAAAAAAAAAAAAAA/9oADAMBAAIQAxAAAAF//8QAFBABAAAAAAAAAAAAAAAAAAAAAP/aAAgBAQABBQJ//8QAFBEBAAAAAAAAAAAAAAAAAAAAAP/aAAgBAwEBPwF//8QAFBEBAAAAAAAAAAAAAAAAAAAAAP/aAAgBAgEBPwF//8QAFBABAAAAAAAAAAAAAAAAAAAAAP/aAAgBAQAGPwJ//8QAFBABAAAAAAAAAAAAAAAAAAAAAP/aAAgBAQABPyF//9oADAMBAAIAAwAAABD/xAAUEQEAAAAAAAAAAAAAAAAAAAAA/9oACAEDAQE/EH//xAAUEQEAAAAAAAAAAAAAAAAAAAAA/9oACAECAQE/EH//xAAUEAEAAAAAAAAAAAAAAAAAAAAA/9oACAEBAAE/EH//2Q==",
  "base64",
);

function response(text = "A tiny image.", tokens = 3, reason = "stop") {
  return {
    content: text ? [{ type: "text" as const, text }] : [],
    finishReason: {
      unified: reason as
        "stop" | "tool-calls" | "length" | "content-filter" | "error",
      raw: reason,
    },
    usage: {
      inputTokens: {
        total: tokens,
        noCache: tokens,
        cacheRead: 0,
        cacheWrite: 0,
      },
      outputTokens: { total: tokens, text: tokens, reasoning: 0 },
    },
    warnings: [],
  };
}

function calls(...inputs: { path: string; prompt?: string }[]) {
  return {
    ...response("", 10, "tool-calls"),
    content: inputs.map((input, index) => ({
      type: "tool-call" as const,
      toolName: "viewMedia",
      toolCallId: `image-${index}`,
      input: JSON.stringify(input),
    })),
  };
}

describe("viewMedia", () => {
  let root: string;
  beforeEach(async () => {
    root = await mkdtemp("/tmp/opencode/bobik-media-");
    await mkdir(join(root, "notes"));
    await writeFile(join(root, "notes", "image.png"), png);
  });
  afterEach(async () => {
    await rm(root, { recursive: true, force: true });
  });

  async function inspection(
    model = new MockLanguageModelV3({ doGenerate: response() }),
    signal = new AbortController().signal,
    allowPath = "./notes/**/*",
  ) {
    const sandbox = await createNotesSandbox({
      cwd: root,
      allowPath,
      timeZone: "UTC",
    });
    const events: Record<string, unknown>[] = [];
    const usages: LanguageModelUsage[] = [];
    const tool = createViewMediaTool({
      fs: sandbox.fs,
      model,
      signal,
      onUsage: (usage) => {
        usages.push(usage);
      },
      audit: async (data) => {
        events.push(data);
      },
    });
    const execute = (path: string, prompt?: string) =>
      tool.execute!(
        { path, ...(prompt !== undefined ? { prompt } : {}) },
        { toolCallId: "inspection", messages: [] },
      );
    return { execute, events, usages, model, sandbox };
  }

  it("returns only text and sends exact bytes, detected MIME, and isolated default/custom prompts", async () => {
    for (const [name, bytes, mime] of [
      ["disguised.txt", png, "image/png"],
      ["image.gif", gif, "image/gif"],
      ["image.webp", webp, "image/webp"],
      ["image.jpg", jpeg, "image/jpeg"],
    ] as const) {
      await writeFile(join(root, "notes", name), bytes);
      const completion = response();
      const { execute, model, events, usages } = await inspection(
        new MockLanguageModelV3({
          doGenerate: {
            ...completion,
            usage: {
              ...completion.usage,
              raw: { providerRequest: bytes.toString("base64") },
            },
          },
        }),
      );
      assert.equal(await execute(`notes/${name}`), "A tiny image.");
      const request = model.doGenerateCalls[0]!;
      assert.equal(request.tools, undefined);
      assert.equal(request.prompt.length, 2);
      assert.equal(request.prompt[0]?.role, "system");
      assert.match(String(request.prompt[0]?.content), /embedded.*data/);
      const user = request.prompt[1]!;
      assert.equal(user.role, "user");
      if (user.role !== "user") throw new Error("Expected user message");
      assert.equal(user.content[0]?.type, "text");
      assert.equal(
        user.content[0]?.type === "text" ? user.content[0].text : undefined,
        DEFAULT_MEDIA_PROMPT,
      );
      const image = user.content[1]!;
      assert.equal(image.type, "file");
      if (image.type !== "file") throw new Error("Expected image file part");
      assert.equal(image.mediaType, mime);
      assert.deepEqual(Buffer.from(image.data as Uint8Array), bytes);
      assert.equal(
        events[0]?.sha256,
        createHash("sha256").update(bytes).digest("hex"),
      );
      assert.equal(events[0]?.byteCount, bytes.length);
      assert.equal(events[0]?.mediaType, mime);
      assert.equal(events[0]?.toolCallId, "inspection");
      assert.equal(events[0]?.text, "A tiny image.");
      assert.equal(usages[0]?.totalTokens, 6);
      assert.ok(!JSON.stringify(events).includes(bytes.toString("base64")));
    }
    const { execute, model, events } = await inspection();
    await execute("/notes/image.png", "Read only the label.");
    assert.equal(events[0]?.prompt, "Read only the label.");
    assert.ok(
      JSON.stringify(model.doGenerateCalls[0]?.prompt).includes(
        "Read only the label.",
      ),
    );
  });

  it("denies escapes, hidden/protected files, symlinks, host paths and URLs before invoking a model", async () => {
    await writeFile(join(root, "outside.png"), png);
    await writeFile(join(root, "notes", ".hidden.png"), png);
    await mkdir(join(root, ".agent-bobik"));
    await writeFile(join(root, ".agent-bobik", "secret.png"), png);
    await symlink(join(root, "outside.png"), join(root, "notes", "link.png"));
    await symlink(join(root, "notes"), join(root, "notes", "linkdir"));
    const { execute, model } = await inspection();
    for (const path of [
      "outside.png",
      "../outside.png",
      "notes/../../outside.png",
      "notes/.hidden.png",
      ".agent-bobik/secret.png",
      "notes/link.png",
      "notes/linkdir/image.png",
      join(root, "notes", "image.png"),
      "https://example.com/image.png",
      "file:///etc/passwd",
    ]) {
      await assert.rejects(async () => execute(path));
    }
    const protectedTool = await inspection(
      undefined,
      undefined,
      "./.agent-bobik/**",
    );
    await assert.rejects(
      async () => protectedTool.execute(".agent-bobik/secret.png"),
      /EACCES/,
    );
    assert.equal(model.doGenerateCalls.length, 0);
    assert.equal(protectedTool.model.doGenerateCalls.length, 0);
    const hiddenTool = await inspection(
      undefined,
      undefined,
      "./notes/.hidden.png",
    );
    assert.equal(
      await hiddenTool.execute("notes/.hidden.png"),
      "A tiny image.",
    );
  });

  it("rejects directories, unsupported content, malformed headers, and oversized files", async () => {
    const { execute, model, sandbox } = await inspection();
    for (const [name, bytes] of [
      ["fake.png", Buffer.from("not an image")],
      ["vector.svg", Buffer.from('<svg xmlns="http://www.w3.org/2000/svg"/>')],
      ["paper.pdf", Buffer.from("%PDF-1.7")],
      ["sound.wav", Buffer.from("RIFF....WAVE")],
      ["truncated.png", png.subarray(0, 24)],
      ["truncated.gif", gif.subarray(0, 13)],
      ["truncated.jpg", jpeg.subarray(0, 20)],
      ["truncated.webp", webp.subarray(0, 15)],
      [
        "bad-header.png",
        Buffer.concat([
          png.subarray(0, 24),
          Buffer.from([0, 99]),
          png.subarray(26),
        ]),
      ],
      [
        "bad-header.gif",
        Buffer.concat([gif.subarray(0, 19), Buffer.from("arbitrary content;")]),
      ],
    ] as const) {
      await writeFile(join(root, "notes", name), bytes);
      await assert.rejects(
        async () => execute(`notes/${name}`),
        /Unsupported or invalid/,
      );
    }
    await assert.rejects(async () => execute("notes"), /regular image file/);
    await writeFile(
      join(root, "notes", "huge.png"),
      Buffer.alloc(MAX_IMAGE_BYTES + 1),
    );
    let reads = 0;
    const read = sandbox.fs.readFileBuffer.bind(sandbox.fs);
    sandbox.fs.readFileBuffer = (path) => {
      reads++;
      return read(path);
    };
    await assert.rejects(async () => execute("notes/huge.png"), /10 MiB/);
    assert.equal(reads, 0);
    const stat = sandbox.fs.stat.bind(sandbox.fs);
    sandbox.fs.stat = async (path) => ({ ...(await stat(path)), size: 1 });
    sandbox.fs.readFileBuffer = async () => {
      reads++;
      return Buffer.alloc(MAX_IMAGE_BYTES + 1);
    };
    await assert.rejects(async () => execute("notes/huge.png"), /10 MiB/);
    assert.equal(reads, 1);
    assert.equal(model.doGenerateCalls.length, 0);
  });

  it("marks output-limit text incomplete without adding an output cap", async () => {
    const { execute, events, model } = await inspection(
      new MockLanguageModelV3({ doGenerate: response("Partial", 4, "length") }),
    );
    assert.match(
      String(await execute("notes/image.png")),
      /^INCOMPLETE IMAGE INSPECTION.*output limit.*\n\nPartial/s,
    );
    assert.equal(events[0]?.text, "Partial");
    assert.equal(model.doGenerateCalls[0]?.maxOutputTokens, undefined);
  });

  it("rejects empty/refused/error completions while retaining their known usage and full text", async () => {
    for (const [text, reason] of [
      ["", "stop"],
      ["Refused", "content-filter"],
      ["Error text", "error"],
    ]) {
      const { execute, events, usages } = await inspection(
        new MockLanguageModelV3({ doGenerate: response(text, 4, reason) }),
      );
      await assert.rejects(
        async () => execute("notes/image.png"),
        /--vision-model/,
      );
      assert.equal(events[0]?.status, "error");
      assert.equal(events[0]?.text, text);
      assert.equal(usages[0]?.totalTokens, 8);
    }
  });

  it("sanitizes provider errors so neither tool errors nor audit expose request bytes", async () => {
    const sensitive = `request=${png.toString("base64")}`;
    const { execute, events } = await inspection(
      new MockLanguageModelV3({
        doGenerate: async () => {
          throw new Error(sensitive);
        },
      }),
    );
    await assert.rejects(
      async () => execute("notes/image.png"),
      (error) => {
        assert.match(String(error), /--vision-model/);
        assert.ok(!String(error).includes(sensitive));
        return true;
      },
    );
    assert.ok(!JSON.stringify(events).includes(sensitive));
    assert.equal(events[0]?.status, "error");
  });

  it("passes caller/timeout cancellation into nested generation and stops even an uncooperative provider", async () => {
    for (const mode of ["caller", "timeout"]) {
      const controller = new AbortController();
      const signal =
        mode === "caller" ? controller.signal : AbortSignal.timeout(30);
      const model = new MockLanguageModelV3({
        doGenerate: async ({ abortSignal }) => {
          assert.equal(abortSignal, signal);
          if (mode === "caller") controller.abort();
          return new Promise(() => {});
        },
      });
      const { execute, events } = await inspection(model, signal);
      await assert.rejects(
        async () => execute("notes/image.png"),
        /cancelled|timed out/,
      );
      assert.equal(events[0]?.status, "error");
    }
  });

  it("aggregates parallel vision usage separately from outer steps and logs ordinary text tool results", async () => {
    const outer = new MockLanguageModelV3({
      modelId: "outer",
      doGenerate: [
        calls(
          { path: "notes/image.png" },
          { path: "notes/image.png", prompt: "Read labels" },
        ),
        response("Done", 10),
      ],
    });
    let active = 0;
    let concurrent = 0;
    const vision = new MockLanguageModelV3({
      modelId: "vision",
      doGenerate: async () => {
        active++;
        concurrent = Math.max(concurrent, active);
        await setTimeout(10);
        active--;
        return response("Image text", 4);
      },
    });
    const summary = await runNotesAgent({
      cwd: root,
      promptPath: "secret-task.md",
      prompt: "SECRET OUTER TASK",
      allowPath: "./notes/**",
      model: outer,
      visionModel: vision,
    });
    assert.equal(summary.steps, 2);
    assert.equal(summary.usage.totalTokens, 56);
    assert.equal(concurrent, 2);
    const raw = await readFile(summary.logPath, "utf8");
    const events = raw
      .trim()
      .split("\n")
      .map((line) => JSON.parse(line) as Record<string, unknown>);
    assert.equal(
      (events[0]?.flags as Record<string, unknown>).visionModel,
      "vision",
    );
    assert.equal(
      (events.at(-1)?.outerUsage as LanguageModelUsage).totalTokens,
      40,
    );
    assert.equal(
      (events.at(-1)?.visionUsage as LanguageModelUsage).totalTokens,
      16,
    );
    assert.equal(
      (events.at(-1)?.totalUsage as LanguageModelUsage).totalTokens,
      56,
    );
    assert.equal(
      events.filter((e) => e.type === "vision-inspection").length,
      2,
    );
    assert.equal(
      events.filter((e) => e.type === "tool-call" && e.toolName === "viewMedia")
        .length,
      2,
    );
    assert.deepEqual(
      events.filter((e) => e.type === "tool-result").map((e) => e.output),
      ["Image text", "Image text"],
    );
    assert.ok(!raw.includes(png.toString("base64")));
    assert.ok(
      !JSON.stringify(vision.doGenerateCalls).includes("SECRET OUTER TASK"),
    );
    assert.ok(
      !JSON.stringify(vision.doGenerateCalls).includes("secret-task.md"),
    );
  });

  it("defaults to the exact agent model and exposes viewMedia even with a notes-only glob", async () => {
    const model = new MockLanguageModelV3({
      modelId: "shared",
      doGenerate: [
        calls({ path: "notes/image.png" }),
        response("Analysis", 4),
        response("Done", 10),
      ],
    });
    const summary = await runNotesAgent({
      cwd: root,
      promptPath: "prompt.md",
      prompt: "Inspect",
      allowPath: "./notes/**",
      model,
    });
    assert.equal(model.doGenerateCalls.length, 3);
    assert.equal(summary.usage.totalTokens, 48);
    const denied = new MockLanguageModelV3({
      doGenerate: [
        calls({ path: "notes/image.png" }),
        response("Unresolved inspection", 10),
      ],
    });
    const result = await runNotesAgent({
      cwd: root,
      promptPath: "prompt.md",
      prompt: "Inspect",
      allowPath: "./notes/**/*.md",
      model: denied,
    });
    assert.equal(result.text, "Unresolved inspection");
    assert.equal(denied.doGenerateCalls.length, 2);
    assert.ok(
      denied.doGenerateCalls[0]?.tools?.some((t) => t.name === "viewMedia"),
    );
  });

  it("keeps inspection failure recoverable and includes known failed-call usage", async () => {
    const model = new MockLanguageModelV3({
      doGenerate: [
        calls({ path: "notes/image.png" }),
        response("Unresolved", 10),
      ],
    });
    const vision = new MockLanguageModelV3({
      doGenerate: response("Refused", 5, "content-filter"),
    });
    const result = await runNotesAgent({
      cwd: root,
      promptPath: "prompt.md",
      prompt: "Inspect",
      allowPath: "./notes/**",
      model,
      visionModel: vision,
    });
    assert.equal(result.text, "Unresolved");
    assert.equal(result.usage.totalTokens, 50);
    const log = await readFile(result.logPath, "utf8");
    assert.match(log, /--vision-model/);
    assert.match(log, /"status":"success"/);
    assert.match(log, /"tool-result"/);
  });

  it("keeps raw provider requests out of the real audit log and outer tool error", async () => {
    const model = new MockLanguageModelV3({
      doGenerate: [
        calls({ path: "notes/image.png" }),
        response("Inspection unresolved", 10),
      ],
    });
    const vision = new MockLanguageModelV3({
      doGenerate: async () => {
        throw new Error(`raw request image=${png.toString("base64")}`);
      },
    });
    const summary = await runNotesAgent({
      cwd: root,
      promptPath: "prompt.md",
      prompt: "Inspect",
      allowPath: "./notes/**",
      model,
      visionModel: vision,
    });
    const audit = await readFile(summary.logPath, "utf8");
    assert.equal(vision.doGenerateCalls.length, 1);
    assert.ok(!audit.includes(png.toString("base64")));
    assert.ok(!audit.includes("raw request"));
    const outerRequest = JSON.stringify(model.doGenerateCalls[1]?.prompt);
    assert.match(outerRequest, /--vision-model/);
    assert.ok(!outerRequest.includes(png.toString("base64")));
  });

  it("retains vision usage in runtime-error summaries and run-end totals", async () => {
    const model = new MockLanguageModelV3({
      doGenerate: [
        calls({ path: "notes/image.png" }),
        response("", 10, "error"),
      ],
    });
    const vision = new MockLanguageModelV3({
      doGenerate: response("Inspected", 5),
    });
    await assert.rejects(
      runNotesAgent({
        cwd: root,
        promptPath: "prompt.md",
        prompt: "Inspect",
        allowPath: "./notes/**",
        model,
        visionModel: vision,
      }),
      (error) => {
        assert.ok(error instanceof RunError);
        assert.equal(error.summary?.usage.totalTokens, 50);
        return true;
      },
    );
    const logs = await readdir(join(root, ".agent-bobik"));
    const events = (
      await readFile(join(root, ".agent-bobik", logs[0]!), "utf8")
    )
      .trim()
      .split("\n")
      .map((line) => JSON.parse(line));
    assert.equal(events.at(-1).totalUsage.totalTokens, 50);
    assert.equal(events.at(-1).status, "error");
  });

  it("shares the run timeout/caller signal and logs cancellation before run-end", async () => {
    for (const mode of ["caller", "timeout"]) {
      const controller = new AbortController();
      let received: AbortSignal | undefined;
      const model = new MockLanguageModelV3({
        doGenerate: calls({ path: "notes/image.png" }),
      });
      const vision = new MockLanguageModelV3({
        doGenerate: async ({ abortSignal }) => {
          received = abortSignal;
          assert.equal(abortSignal, model.doGenerateCalls[0]?.abortSignal);
          if (mode === "caller") controller.abort(new Error("Caller stopped"));
          return new Promise(() => {});
        },
      });
      await assert.rejects(
        runNotesAgent({
          cwd: root,
          promptPath: "prompt.md",
          prompt: "Inspect",
          allowPath: "./notes/**",
          model,
          visionModel: vision,
          signal: controller.signal,
          timeoutMs: mode === "timeout" ? 100 : 30_000,
        }),
        /Caller stopped|Run timed out/,
      );
      assert.equal(received?.aborted, true);
    }
    for (const filename of await readdir(join(root, ".agent-bobik"))) {
      const events = (
        await readFile(join(root, ".agent-bobik", filename), "utf8")
      )
        .trim()
        .split("\n")
        .map((line) => JSON.parse(line));
      assert.equal(events.at(-1).type, "run-end");
      assert.ok(
        events.some(
          (e) => e.type === "vision-inspection" && e.status === "error",
        ),
      );
    }
  });
});
