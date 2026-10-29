import assert from "node:assert/strict";
import { mkdtemp, mkdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, describe, it } from "node:test";
import { createEditFileTool } from "../src/edit.ts";
import { createNotesSandbox } from "../src/sandbox.ts";

describe("readFile missing-path hints", () => {
  let root: string;

  beforeEach(async () => {
    root = await mkdtemp(join(tmpdir(), "bobik-read-"));
    await mkdir(join(root, "Agent"), { recursive: true });
    await writeFile(join(root, "Agent", "Memories.md"), "# Memories\n");
    await writeFile(join(root, "Agent", "Instructions.md"), "# Instructions\n");
    await writeFile(join(root, "note.md"), "# Note\n");
  });

  afterEach(async () => {
    await rm(root, { recursive: true, force: true });
  });

  async function readMemories() {
    const sandbox = await createNotesSandbox({
      cwd: root,
      allowPath: "./**/*.md",
      timeZone: "UTC",
    });
    return sandbox.tools.readFile.execute!(
      { path: "Memories.md" },
      { toolCallId: "read", messages: [] },
    );
  }

  it("reads an existing file", async () => {
    const sandbox = await createNotesSandbox({
      cwd: root,
      allowPath: "./**/*.md",
      timeZone: "UTC",
    });

    assert.deepEqual(
      await sandbox.tools.readFile.execute!(
        { path: "note.md" },
        { toolCallId: "read", messages: [] },
      ),
      { content: "# Note\n" },
    );
  });

  it("names a same-name note elsewhere instead of a bare ENOENT", async () => {
    await assert.rejects(readMemories, (error: unknown) => {
      assert.ok(error instanceof Error);
      assert.equal((error as NodeJS.ErrnoException).code, "ENOENT");
      assert.match(error.message, /ENOENT/);
      assert.match(error.message, /"Agent\/Memories\.md"/);
      assert.match(error.message, /instead of creating a new file/);
      return true;
    });
  });

  it("leaves a genuinely missing file as a bare ENOENT", async () => {
    await assert.rejects(
      async () => {
        const sandbox = await createNotesSandbox({
          cwd: root,
          allowPath: "./**/*.md",
          timeZone: "UTC",
        });
        return sandbox.tools.readFile.execute!(
          { path: "missing.md" },
          { toolCallId: "read", messages: [] },
        );
      },
      (error: unknown) => {
        assert.match((error as Error).message, /^ENOENT:/);
        assert.doesNotMatch((error as Error).message, /exists at/);
        return true;
      },
    );
  });

  it("does not mention files outside the allowlist", async () => {
    await mkdir(join(root, "Notes"), { recursive: true });
    await writeFile(join(root, "secret.md"), "hidden\n");
    await writeFile(join(root, "Agent", "secret.md"), "hidden\n");

    await assert.rejects(
      async () => {
        const sandbox = await createNotesSandbox({
          cwd: root,
          allowPath: "./Notes/*.md",
          timeZone: "UTC",
        });
        return sandbox.tools.readFile.execute!(
          { path: "Notes/secret.md" },
          { toolCallId: "read", messages: [] },
        );
      },
      (error: unknown) => {
        assert.match((error as Error).message, /^ENOENT:/);
        assert.doesNotMatch((error as Error).message, /exists at/);
        return true;
      },
    );
  });

  it("adds the same hint to editFile", async () => {
    const sandbox = await createNotesSandbox({
      cwd: root,
      allowPath: "./**/*.md",
      timeZone: "UTC",
    });
    const editFile = createEditFileTool({ fs: sandbox.fs });

    await assert.rejects(
      async () =>
        editFile.execute!(
          { path: "Memories.md", oldString: "a", newString: "b" },
          { toolCallId: "edit", messages: [] },
        ),
      /Agent\/Memories\.md/,
    );
  });
});
