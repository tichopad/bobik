import assert from "node:assert/strict";
import { mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { afterEach, beforeEach, describe, it } from "node:test";
import { createEditFileTool } from "../src/edit.ts";
import { createNotesSandbox } from "../src/sandbox.ts";

describe("editFile", () => {
  let root: string;
  beforeEach(async () => {
    root = await mkdtemp("/tmp/opencode/bobik-edit-");
    await mkdir(join(root, "notes"));
    await writeFile(
      join(root, "notes", "links.md"),
      "# Links\n\n- [old](https://old.example)\n- dup\n- dup\n",
    );
  });
  afterEach(async () => {
    await rm(root, { recursive: true, force: true });
  });

  async function editing(allowPath = "./notes/**/*.md") {
    const sandbox = await createNotesSandbox({
      cwd: root,
      allowPath,
      timeZone: "UTC",
    });
    const tool = createEditFileTool({ fs: sandbox.fs });
    const execute = (input: {
      path: string;
      oldString: string;
      newString: string;
      replaceAll?: boolean;
    }) => tool.execute!(input, { toolCallId: "edit", messages: [] });
    return { execute, sandbox };
  }

  it("replaces a unique occurrence and reports one replacement", async () => {
    const { execute } = await editing();

    const result = await execute({
      path: "notes/links.md",
      oldString: "- [old](https://old.example)",
      newString: "- [new](https://new.example)",
    });

    assert.deepEqual(result, { success: true, replacements: 1 });
    assert.match(
      await readFile(join(root, "notes", "links.md"), "utf8"),
      /- \[new\]\(https:\/\/new\.example\)/,
    );
  });

  it("rejects ambiguous matches unless replaceAll is set", async () => {
    const { execute } = await editing();

    await assert.rejects(
      async () =>
        execute({ path: "notes/links.md", oldString: "- dup", newString: "x" }),
      /occurs 2 times/,
    );

    const result = await execute({
      path: "notes/links.md",
      oldString: "- dup",
      newString: "- done",
      replaceAll: true,
    });
    assert.deepEqual(result, { success: true, replacements: 2 });
    assert.equal(
      await readFile(join(root, "notes", "links.md"), "utf8"),
      "# Links\n\n- [old](https://old.example)\n- done\n- done\n",
    );
  });

  it("inserts replacement text verbatim, including $ patterns", async () => {
    const { execute } = await editing();

    await execute({
      path: "notes/links.md",
      oldString: "# Links",
      newString: "# Links $& $1",
    });

    assert.match(
      await readFile(join(root, "notes", "links.md"), "utf8"),
      /^# Links \$& \$1$/m,
    );
  });

  it("fails when oldString is missing or unchanged", async () => {
    const { execute } = await editing();

    await assert.rejects(
      async () =>
        execute({ path: "notes/links.md", oldString: "nope", newString: "x" }),
      /not found/,
    );
    await assert.rejects(
      async () =>
        execute({
          path: "notes/links.md",
          oldString: "- dup",
          newString: "- dup",
        }),
      /must differ/,
    );
  });

  it("enforces the allowlist and surfaces missing files", async () => {
    const { execute } = await editing();

    await assert.rejects(
      async () =>
        execute({ path: "outside.md", oldString: "a", newString: "b" }),
      /EACCES/,
    );
    await assert.rejects(
      async () =>
        execute({ path: "notes/missing.md", oldString: "a", newString: "b" }),
      /ENOENT/,
    );
  });
});
