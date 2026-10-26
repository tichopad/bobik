import assert from "node:assert/strict";
import { describe, it } from "node:test";
import {
  buildSystemPrompt,
  buildUserMessageSuffix,
  formatLocalDateTime,
  resolveTimeZone,
} from "../src/agent.ts";

describe("formatLocalDateTime", () => {
  it("formats in the requested timezone", () => {
    const date = new Date("2026-10-19T17:55:00.123Z");

    assert.equal(
      formatLocalDateTime(date, "Europe/Prague"),
      "Monday, October 19, 2026 at 19:55 GMT+2",
    );
    assert.equal(
      formatLocalDateTime(date, "UTC"),
      "Monday, October 19, 2026 at 17:55 UTC",
    );
  });

  it("falls back to host-local time for an invalid zone", () => {
    assert.match(
      formatLocalDateTime(new Date("2026-10-19T17:55:00.123Z"), "Etc/Unknown"),
      /^Monday, October 19, 2026 at \d{2}:\d{2} /,
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
        "Tuesday, October 20, 2026 at 04:30 GMT+5",
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
  it("includes the timezone, cwd and allowlist but no date/time line", () => {
    const prompt = buildSystemPrompt({
      cwd: "/notes",
      allowPath: "./daily-notes/**/*.md",
      deny: [],
      timeZone: "Europe/Prague",
    });

    // The system prompt stays byte-static for prefix caching; the per-run
    // date/time lives in the user-message suffix instead.
    assert.doesNotMatch(prompt, /Current local date\/time/);
    assert.match(prompt, /Timezone: Europe\/Prague/);
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
      timeZone: "UTC",
    });

    assert.match(prompt, /Denied bash commands: rm, wget/);
  });

  it("omits the deny list when empty", () => {
    const prompt = buildSystemPrompt({
      cwd: "/notes",
      allowPath: "./**/*.md",
      deny: [],
      timeZone: "UTC",
    });

    assert.doesNotMatch(prompt, /Denied bash commands/);
  });

  it("tells the agent to use the tools and finish in one shot", () => {
    const prompt = buildSystemPrompt({
      cwd: "/notes",
      allowPath: "./**/*.md",
      deny: [],
      timeZone: "UTC",
    });

    assert.match(prompt, /readFile/);
    assert.match(prompt, /writeFile/);
    assert.match(prompt, /bash/);
    assert.match(prompt, /one-shot/i);
    assert.match(prompt, /do not ask questions/i);
  });
});

describe("buildUserMessageSuffix", () => {
  const date = new Date("2026-10-19T17:55:00.123Z");

  it("carries only the date/time line for one-shot runs", () => {
    const suffix = buildUserMessageSuffix({
      now: date,
      timeZone: "Europe/Prague",
    });

    assert.match(
      suffix,
      /Current local date\/time: Monday, October 19, 2026 at 19:55 GMT\+2/,
    );
    assert.doesNotMatch(suffix, /Trigger:/);
  });

  it("states the initial run has no change set", () => {
    const suffix = buildUserMessageSuffix({
      now: date,
      timeZone: "UTC",
      initialRun: true,
    });

    assert.match(suffix, /Trigger: initial run, no change set\./);
  });

  it("names the trigger and lists changed files, marking deletions", () => {
    const suffix = buildUserMessageSuffix({
      now: date,
      timeZone: "UTC",
      trigger: "@bobik",
      changedFiles: [
        { path: "daily-notes/a.md" },
        { path: "daily-notes/b.md", deleted: true },
      ],
    });

    assert.match(suffix, /Trigger: changes to 2 files matching "@bobik":/);
    assert.match(suffix, /daily-notes\/a\.md\n/);
    assert.match(suffix, /daily-notes\/b\.md \(deleted\)/);
  });

  it("uses singular wording for a single changed file", () => {
    const suffix = buildUserMessageSuffix({
      now: date,
      timeZone: "UTC",
      trigger: "@bobik",
      changedFiles: [{ path: "a.md" }],
    });

    assert.match(suffix, /Trigger: changes to 1 file matching "@bobik":/);
  });
});
