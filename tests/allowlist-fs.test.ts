import assert from "node:assert/strict";
import {
  mkdir,
  mkdtemp,
  readFile,
  rm,
  stat,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, describe, it } from "node:test";
import { ReadWriteFs, type IFileSystem } from "just-bash";
import { AllowlistFs } from "../src/allowlist-fs.ts";
import { compileAllowlist } from "../src/allowlist.ts";
import { UsageError } from "../src/errors.ts";

interface Fixture {
  root: string;
  inner: IFileSystem;
}

async function createFixture(): Promise<Fixture> {
  const root = await mkdtemp(join(tmpdir(), "bobik-allowlist-"));
  await mkdir(join(root, "daily-notes", "sub"), { recursive: true });
  await mkdir(join(root, "other"), { recursive: true });
  await mkdir(join(root, ".agent-bobik"), { recursive: true });
  await writeFile(join(root, "daily-notes", "2026-10-18.md"), "yesterday");
  await writeFile(join(root, "daily-notes", "sub", "2026-10-19.md"), "today");
  await writeFile(join(root, "daily-notes", "notes.txt"), "not a note");
  await writeFile(join(root, "daily-notes", ".draft.md"), "draft");
  await writeFile(join(root, "other", "secret.md"), "secret");
  await writeFile(join(root, ".hidden.md"), "hidden");
  await writeFile(join(root, ".agent-bobik", "run.jsonl"), "{}\n");
  return { root, inner: new ReadWriteFs({ root, allowSymlinks: false }) };
}

function rejectedWithEacces(promise: Promise<unknown>): Promise<void> {
  return assert.rejects(promise, (error: unknown) => {
    assert.ok(error instanceof Error);
    assert.equal((error as NodeJS.ErrnoException).code, "EACCES");
    assert.match(error.message, /EACCES/);
    return true;
  });
}

describe("compileAllowlist", () => {
  it("accepts globs relative to cwd and normalizes them to virtual paths", () => {
    const allowlist = compileAllowlist("/notes", "./daily-notes/**/*.md");

    assert.equal(allowlist.pattern, "/daily-notes/**/*.md");
    assert.equal(allowlist.matches("/daily-notes/2026-10-18.md"), true);
    assert.equal(allowlist.matches("/daily-notes/sub/2026-10-19.md"), true);
    assert.equal(allowlist.matches("/other/secret.md"), false);
  });

  it("accepts absolute globs inside cwd", () => {
    const allowlist = compileAllowlist("/notes", "/notes/daily-notes/**/*.md");

    assert.equal(allowlist.pattern, "/daily-notes/**/*.md");
    assert.equal(allowlist.matches("/daily-notes/a.md"), true);
  });

  it("rejects relative globs that escape cwd", () => {
    assert.throws(() => compileAllowlist("/notes", "../**/*.md"), UsageError);
    assert.throws(
      () => compileAllowlist("/notes", "daily-notes/../../**/*.md"),
      UsageError,
    );
  });

  it("rejects absolute globs outside cwd", () => {
    assert.throws(() => compileAllowlist("/notes", "/tmp/**/*.md"), UsageError);
  });

  it("rejects negated globs", () => {
    assert.throws(() => compileAllowlist("/notes", "!**/*.md"), UsageError);
  });

  it("treats dotfiles as hidden unless explicitly targeted", () => {
    const wildcard = compileAllowlist("/notes", "**/*.md");
    assert.equal(wildcard.matches("/.hidden.md"), false);
    assert.equal(wildcard.matches("/daily-notes/.draft.md"), false);

    const explicit = compileAllowlist("/notes", ".notes/**/*.md");
    assert.equal(explicit.matches("/.notes/a.md"), true);
  });
});

describe("AllowlistFs", () => {
  let fixture: Fixture;
  let fs: AllowlistFs;

  beforeEach(async () => {
    fixture = await createFixture();
    fs = new AllowlistFs(fixture.inner, {
      allowlist: compileAllowlist(fixture.root, "./daily-notes/**/*.md"),
    });
  });

  afterEach(async () => {
    await rm(fixture.root, { recursive: true, force: true });
  });

  it("reads matching files at any depth", async () => {
    assert.equal(await fs.readFile("/daily-notes/2026-10-18.md"), "yesterday");
    assert.equal(await fs.readFile("/daily-notes/sub/2026-10-19.md"), "today");
  });

  it("rejects reads outside the allowlist", async () => {
    await rejectedWithEacces(fs.readFile("/other/secret.md"));
    await rejectedWithEacces(
      fs.readFile(join(fixture.root, "other", "secret.md")),
    );
  });

  it("rejects dotfiles and dot-directories", async () => {
    await rejectedWithEacces(fs.readFile("/.hidden.md"));
    await rejectedWithEacces(fs.readFile("/daily-notes/.draft.md"));
    await rejectedWithEacces(fs.readFile("/.agent-bobik/run.jsonl"));
  });

  it("rejects traversal above the filesystem root", async () => {
    await rejectedWithEacces(fs.readFile("/daily-notes/../../other/secret.md"));
  });

  it("rejects writes, appends and deletes outside the allowlist", async () => {
    await rejectedWithEacces(fs.writeFile("/other/new.md", "nope"));
    await rejectedWithEacces(fs.appendFile("/other/secret.md", "nope"));
    await rejectedWithEacces(fs.rm("/other/secret.md"));
    await rejectedWithEacces(fs.mkdir("/other/new-dir"));

    assert.equal(
      await readFile(join(fixture.root, "other", "secret.md"), "utf8"),
      "secret",
    );
  });

  it("allows writes, appends and deletes for matching files", async () => {
    await fs.writeFile("/daily-notes/new.md", "new");
    await fs.appendFile("/daily-notes/new.md", " note");
    await fs.mkdir("/daily-notes/archive");

    assert.equal(
      await readFile(join(fixture.root, "daily-notes", "new.md"), "utf8"),
      "new note",
    );

    await fs.rm("/daily-notes/new.md");
    await assert.rejects(stat(join(fixture.root, "daily-notes", "new.md")));
  });

  it("lists the root but hides unreadable entries", async () => {
    assert.deepEqual(await fs.readdir("/"), ["daily-notes"]);

    const entries = await fs.readdirWithFileTypes("/");
    assert.deepEqual(
      entries.map((entry) => [entry.name, entry.isDirectory]),
      [["daily-notes", true]],
    );
  });

  it("lists matching files while hiding non-matching and dot entries", async () => {
    assert.deepEqual(await fs.readdir("/daily-notes"), [
      "2026-10-18.md",
      "sub",
    ]);
  });

  it("rejects directory listings outside the traversal roots", async () => {
    await rejectedWithEacces(fs.readdir("/other"));
    await rejectedWithEacces(fs.readdir("/.agent-bobik"));
  });

  it("allows stat for matching files and traversable directories", async () => {
    assert.equal((await fs.stat("/daily-notes")).isDirectory, true);
    assert.equal((await fs.stat("/daily-notes/2026-10-18.md")).isFile, true);
  });

  it("rejects stat for non-matching paths", async () => {
    await rejectedWithEacces(fs.stat("/other/secret.md"));
    await rejectedWithEacces(fs.stat("/other"));
  });

  it("allows renaming and copying matching files", async () => {
    await fs.mv("/daily-notes/2026-10-18.md", "/daily-notes/2026-10-17.md");
    assert.equal(
      await readFile(
        join(fixture.root, "daily-notes", "2026-10-17.md"),
        "utf8",
      ),
      "yesterday",
    );

    await fs.cp("/daily-notes/2026-10-17.md", "/daily-notes/copy.md");
    assert.equal(
      await readFile(join(fixture.root, "daily-notes", "copy.md"), "utf8"),
      "yesterday",
    );
  });

  it("rejects renaming and copying across the allowlist boundary", async () => {
    await rejectedWithEacces(
      fs.mv("/daily-notes/2026-10-18.md", "/other/stolen.md"),
    );
    await rejectedWithEacces(
      fs.cp("/other/secret.md", "/daily-notes/secret.md"),
    );
    await rejectedWithEacces(
      fs.cp("/daily-notes/2026-10-18.md", "/other/copy.md"),
    );
  });

  it("rejects recursively deleting a directory with non-matching files", async () => {
    await rejectedWithEacces(fs.rm("/daily-notes", { recursive: true }));

    assert.equal(
      await readFile(join(fixture.root, "daily-notes", "notes.txt"), "utf8"),
      "not a note",
    );
  });

  it("allows recursively deleting a directory containing only matching files", async () => {
    await fs.mkdir("/daily-notes/archive/2026", { recursive: true });
    await fs.writeFile("/daily-notes/archive/2026/old.md", "old");

    await fs.rm("/daily-notes/archive", { recursive: true });

    await assert.rejects(stat(join(fixture.root, "daily-notes", "archive")));
  });

  it("filters getAllPaths to allowed files", () => {
    const paths = fs.getAllPaths();

    assert.ok(paths.includes("/daily-notes/2026-10-18.md"));
    assert.ok(paths.includes("/daily-notes/sub/2026-10-19.md"));
    assert.ok(!paths.includes("/other/secret.md"));
    assert.ok(!paths.includes("/daily-notes/notes.txt"));
    assert.ok(!paths.includes("/.agent-bobik/run.jsonl"));
  });

  it("allows explicit dot-directory globs", async () => {
    await mkdir(join(fixture.root, ".notes"));
    await writeFile(join(fixture.root, ".notes", "a.md"), "dot note");

    const dotFs = new AllowlistFs(fixture.inner, {
      allowlist: compileAllowlist(fixture.root, ".notes/**/*.md"),
    });

    assert.equal(await dotFs.readFile("/.notes/a.md"), "dot note");
    assert.deepEqual(await dotFs.readdir("/"), [".notes"]);
  });

  it("rejects symlink creation", async () => {
    await rejectedWithEacces(
      fs.symlink("/daily-notes/2026-10-18.md", "/daily-notes/link.md"),
    );
  });

  it("provides the standard devices bash expects", async () => {
    await fs.writeFile("/dev/null", "discarded");
    assert.equal(await fs.readFile("/dev/null"), "");
    assert.equal(await fs.exists("/dev/null"), true);
    assert.equal((await fs.stat("/dev/null")).isFile, true);
  });

  it("always hides the audit directory, even when the glob targets it", async () => {
    const logFs = new AllowlistFs(fixture.inner, {
      allowlist: compileAllowlist(fixture.root, ".agent-bobik/**/*.jsonl"),
    });

    await rejectedWithEacces(logFs.readFile("/.agent-bobik/run.jsonl"));
    await rejectedWithEacces(logFs.readdir("/.agent-bobik"));
    assert.deepEqual(await logFs.readdir("/"), []);
    assert.equal(
      logFs.getAllPaths().includes("/.agent-bobik/run.jsonl"),
      false,
    );
  });
});
