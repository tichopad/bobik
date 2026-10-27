import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { analyzeNote, DEFAULT_TRIGGER } from "../src/trigger.ts";

describe("note triggers", () => {
  it("uses the Obsidian-native default", () => {
    assert.equal(DEFAULT_TRIGGER, "#bobik");
  });

  for (const text of [
    "#bobik",
    "#bobik: do this",
    "a note\n#bobik request",
    "(#bobik: request)",
  ]) {
    it(`recognizes a ready tag: ${JSON.stringify(text)}`, () => {
      assert.deepEqual(analyzeNote(text), { ready: true, draft: false });
    });
  }

  for (const text of [
    "@bobik: old",
    "#Bobik",
    "#bobik-draft",
    "#bobik/foo",
    "#bobik123",
    "word#bobik",
    "##bobik",
    "#bobik_other",
  ]) {
    it(`rejects a non-tag: ${JSON.stringify(text)}`, () => {
      assert.deepEqual(analyzeNote(text), { ready: false, draft: false });
    });
  }

  for (const text of [
    "%%#bobik: drafting%%",
    "%%\n#bobik: drafting\n%%",
    "%%#bobik: incomplete",
    "#bobik: ready\n%% #bobik: draft %%",
    "%% other %%\n%% #bobik: draft %%",
  ]) {
    it(`protects the whole note: ${JSON.stringify(text)}`, () => {
      assert.deepEqual(analyzeNote(text), { ready: false, draft: true });
    });
  }

  for (const text of [
    "`#bobik`",
    "``#bobik: ` example``",
    "```md\n#bobik: example\n```",
    "~~~\n#bobik: example\n~~~",
    "```\n#bobik: unfinished fence",
    "%% `#bobik` %%",
  ]) {
    it(`ignores code: ${JSON.stringify(text)}`, () => {
      assert.deepEqual(analyzeNote(text), { ready: false, draft: false });
    });
  }

  it("does not interpret comment delimiters in code", () => {
    assert.deepEqual(analyzeNote("`%%` #bobik: ready"), {
      ready: true,
      draft: false,
    });
    assert.deepEqual(analyzeNote("```\n%%\n```\n#bobik: ready"), {
      ready: true,
      draft: false,
    });
    assert.deepEqual(analyzeNote("%% #bobik: draft `%%`"), {
      ready: false,
      draft: true,
    });
  });

  it("recognizes code nested in quotes and lists, and indented code", () => {
    for (const text of [
      "> ```md\n> #bobik: example\n> ```",
      "- ```md\n  #bobik: example\n  ```",
      "    #bobik: indented example",
      "> ```\n> %%\n> ```",
    ]) {
      assert.deepEqual(analyzeNote(text), { ready: false, draft: false });
    }
    assert.equal(analyzeNote("> ```\n> %%\n> ```\n#bobik: ready").ready, true);
  });

  it("resumes prose after closed comments and code", () => {
    assert.equal(analyzeNote("%% unrelated %% #bobik: ready").ready, true);
    assert.equal(analyzeNote("`example` #bobik: ready").ready, true);
    assert.equal(analyzeNote("````\n```\n#bobik\n````").ready, false);
    assert.equal(analyzeNote("` unmatched #bobik: ready").ready, true);
  });

  it("preserves literal custom matching with comment and code exclusions", () => {
    assert.deepEqual(analyzeNote("prefix@custom-suffix", "@custom"), {
      ready: true,
      draft: false,
    });
    assert.deepEqual(analyzeNote("%% @custom drafting %%", "@custom"), {
      ready: false,
      draft: true,
    });
    assert.deepEqual(analyzeNote("`@custom`", "@custom"), {
      ready: false,
      draft: false,
    });
  });
});
