import assert from "node:assert/strict";
import { describe, it } from "node:test";
import {
  buildSystemPrompt,
  formatLocalDateTime,
  resolveTimeZone,
} from "../src/agent.ts";

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

  it("falls back to host-local time for an invalid zone", () => {
    assert.match(
      formatLocalDateTime(new Date("2026-10-19T17:55:00.123Z"), "Etc/Unknown"),
      /^Monday, October 19, 2026 at \d{2}:\d{2}:\d{2} /,
    );
  });
});

describe("resolveTimeZone", () => {
  const date = new Date("2026-10-19T23:30:00Z");

  it("uses a valid IANA TZ value", () => {
    withTimeZone("America/New_York", () => {
      assert.equal(resolveTimeZone(date), "America/New_York");
    });
  });

  it("converts simple POSIX offsets to a fixed IANA zone", () => {
    withTimeZone("UTC-5", () => {
      assert.equal(resolveTimeZone(date), "Etc/GMT-5");
      assert.equal(
        formatLocalDateTime(date, resolveTimeZone(date)),
        "Tuesday, October 20, 2026 at 04:30:00 GMT+5",
      );
    });
  });

  it("treats an empty TZ as UTC", () => {
    withTimeZone("", () => {
      assert.equal(resolveTimeZone(date), "Etc/UTC");
    });
  });
});

function withTimeZone(value: string, run: () => void): void {
  const previous = process.env.TZ;
  process.env.TZ = value;
  try {
    run();
  } finally {
    if (previous === undefined) {
      delete process.env.TZ;
    } else {
      process.env.TZ = previous;
    }
  }
}

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
    assert.match(prompt, /Sandbox root: \//);
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
