import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { buildSystemPrompt, formatLocalDateTime } from "../src/agent.ts";

describe("formatLocalDateTime", () => {
  it("formats in the requested timezone", () => {
    const date = new Date("2026-10-19T17:55:00.123Z");

    assert.equal(
      formatLocalDateTime(date, "Europe/Prague"),
      "Monday, October 19, 2026 at 19:55:00 GMT+2",
    );
    assert.equal(
      formatLocalDateTime(date, "UTC"),
      "Monday, October 19, 2026 at 17:55:00 UTC",
    );
  });
});

describe("buildSystemPrompt", () => {
  const date = new Date("2026-10-19T17:55:00.123Z");

  it("includes the resolved date, timezone, cwd and allowlist", () => {
    const prompt = buildSystemPrompt({
      cwd: "/notes",
      allowPath: "./daily-notes/**/*.md",
      deny: [],
      now: date,
      timeZone: "Europe/Prague",
    });

    assert.match(prompt, /Monday, October 19, 2026 at 19:55:00 GMT\+2/);
    assert.match(prompt, /Europe\/Prague/);
    assert.match(prompt, /Working directory: \/notes/);
    assert.match(prompt, /\.\/daily-notes\/\*\*\/\*\.md/);
    assert.match(prompt, /EACCES/);
  });

  it("includes the deny list when configured", () => {
    const prompt = buildSystemPrompt({
      cwd: "/notes",
      allowPath: "./**/*.md",
      deny: ["rm", "wget"],
      now: date,
      timeZone: "UTC",
    });

    assert.match(prompt, /Denied bash commands: rm, wget/);
  });

  it("omits the deny list when empty", () => {
    const prompt = buildSystemPrompt({
      cwd: "/notes",
      allowPath: "./**/*.md",
      deny: [],
      now: date,
      timeZone: "UTC",
    });

    assert.doesNotMatch(prompt, /Denied bash commands/);
  });

  it("tells the agent to use the tools and finish in one shot", () => {
    const prompt = buildSystemPrompt({
      cwd: "/notes",
      allowPath: "./**/*.md",
      deny: [],
      now: date,
      timeZone: "UTC",
    });

    assert.match(prompt, /readFile/);
    assert.match(prompt, /writeFile/);
    assert.match(prompt, /bash/);
    assert.match(prompt, /one-shot/i);
    assert.match(prompt, /do not ask questions/i);
  });
});
