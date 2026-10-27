import assert from "node:assert/strict";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { describe, it } from "node:test";
import { MockLanguageModelV3 } from "ai/test";
import { runNotesAgent } from "../src/agent.ts";

describe("agent draft protection", () => {
  for (const trigger of ["#bobik", "@custom"]) {
    it(`guards write and shell tools during a startup run (${trigger})`, async () => {
      const root = await mkdtemp("/tmp/opencode/bobik-draft-");
      const draft = `Original text\n%% ${trigger}: still drafting`;
      await writeFile(join(root, "note.md"), "Original text");
      let calls = 0;
      const usage = {
        inputTokens: { total: 1, noCache: 1, cacheRead: 0, cacheWrite: 0 },
        outputTokens: { total: 1, text: 1, reasoning: 0 },
      };
      const model = new MockLanguageModelV3({
        doGenerate: async () => {
          calls += 1;
          if (calls === 1) {
            // The sandbox already exists: protection must not be a startup snapshot.
            await writeFile(join(root, "note.md"), draft);
            return {
              content: [
                {
                  toolName: "writeFile",
                  input: { path: "note.md", content: "overwritten" },
                },
                {
                  toolName: "bash",
                  input: {
                    command:
                      "echo overwritten > note.md; rm note.md; mv note.md moved.md",
                  },
                },
              ].map(({ toolName, input }, index) => ({
                type: "tool-call" as const,
                toolCallId: `draft-${index}`,
                toolName,
                input: JSON.stringify(input),
              })),
              finishReason: {
                unified: "tool-calls" as const,
                raw: "tool-calls",
              },
              usage,
              warnings: [],
            };
          }
          return {
            content: [{ type: "text" as const, text: "Left draft alone." }],
            finishReason: { unified: "stop" as const, raw: "stop" },
            usage,
            warnings: [],
          };
        },
      });
      try {
        const summary = await runNotesAgent({
          cwd: root,
          allowPath: "*.md",
          promptPath: "prompt.md",
          prompt: "Maintain notes.",
          model,
          initialRun: true,
          trigger,
        });
        assert.equal(await readFile(join(root, "note.md"), "utf8"), draft);
        assert.equal(summary.filesWritten, 0);
        const log = await readFile(summary.logPath, "utf8");
        assert.match(log, /modify protected draft/);
        await assert.rejects(readFile(join(root, "moved.md")));
      } finally {
        await rm(root, { recursive: true, force: true });
      }
    });
  }
});
