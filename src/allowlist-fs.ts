import { posix } from "node:path";
import { unsafeBytesFromLatin1 } from "just-bash";
import type {
  BufferEncoding,
  ByteString,
  CpOptions,
  CreateExclusiveOptions,
  FileContent,
  FsStat,
  IFileSystem,
  MkdirOptions,
  RmOptions,
} from "just-bash";
import {
  normalizeVirtualPath,
  type Allowlist,
  PathEscapeError,
} from "./allowlist.ts";

export interface AllowlistFsOptions {
  allowlist: Allowlist;
}

interface DirentEntry {
  name: string;
  isFile: boolean;
  isDirectory: boolean;
  isSymbolicLink: boolean;
}

type ReadFileOptions = {
  encoding?: BufferEncoding | null;
};
type WriteFileOptions = {
  encoding?: BufferEncoding;
};

/**
 * Virtual device files that bash expects to exist. They never touch the host
 * filesystem and are safe to allow regardless of the note glob.
 */
const DEVICE_PATHS: ReadonlySet<string> = new Set([
  "/dev/null",
  "/dev/zero",
  "/dev/stdin",
  "/dev/stdout",
  "/dev/stderr",
]);

/**
 * A filesystem wrapper that enforces an allowlist glob.
 *
 * This is the single enforcement point for all three tools: bash commands,
 * readFile, and writeFile. File contents may only be read or written when the
 * path matches the glob. Directories that could contain matches remain
 * traversable so the agent can navigate and list them; listings are filtered
 * to hide non-matching entries.
 */
export class AllowlistFs implements IFileSystem {
  private readonly inner: IFileSystem;
  private readonly allowlist: Allowlist;

  constructor(inner: IFileSystem, options: AllowlistFsOptions) {
    this.inner = inner;
    this.allowlist = options.allowlist;
  }

  async readFile(
    path: string,
    options?: ReadFileOptions | BufferEncoding,
  ): Promise<string> {
    if (this.device(path)) {
      return "";
    }
    return this.inner.readFile(this.requireFile(path, "open"), options);
  }

  async readFileBytes(path: string): Promise<ByteString> {
    if (this.device(path)) {
      return unsafeBytesFromLatin1("");
    }
    const target = this.requireFile(path, "open");
    if (!this.inner.readFileBytes) {
      throw denied(path, "open");
    }
    return this.inner.readFileBytes(target);
  }

  async readFileBuffer(path: string): Promise<Uint8Array> {
    if (this.device(path)) {
      return new Uint8Array(0);
    }
    return this.inner.readFileBuffer(this.requireFile(path, "open"));
  }

  async writeFile(
    path: string,
    content: FileContent,
    options?: WriteFileOptions | BufferEncoding,
  ): Promise<void> {
    if (this.device(path)) {
      return;
    }
    return this.inner.writeFile(
      this.requireFile(path, "write"),
      content,
      options,
    );
  }

  async appendFile(
    path: string,
    content: FileContent,
    options?: WriteFileOptions | BufferEncoding,
  ): Promise<void> {
    if (this.device(path)) {
      return;
    }
    return this.inner.appendFile(
      this.requireFile(path, "append"),
      content,
      options,
    );
  }

  async exists(path: string): Promise<boolean> {
    const target = this.tryNormalize(path);
    if (!target) {
      return false;
    }
    if (DEVICE_PATHS.has(target)) {
      return true;
    }

    if (this.allowlist.matches(target)) {
      return this.inner.exists(target);
    }
    if (!this.allowlist.isTraversable(target)) {
      return false;
    }

    try {
      return (await this.inner.stat(target)).isDirectory;
    } catch {
      return false;
    }
  }

  async stat(path: string): Promise<FsStat> {
    const device = this.device(path);
    if (device) {
      return deviceStat();
    }
    return this.policyStat(path, "stat");
  }

  async lstat(path: string): Promise<FsStat> {
    const device = this.device(path);
    if (device) {
      return deviceStat();
    }
    return this.policyLstat(path, "lstat");
  }

  async mkdir(path: string, options?: MkdirOptions): Promise<void> {
    return this.inner.mkdir(this.requireTraversable(path, "mkdir"), options);
  }

  async createExclusive(
    path: string,
    options: CreateExclusiveOptions,
  ): Promise<void> {
    const target = options.directory
      ? this.requireTraversable(path, "mkdir")
      : this.requireFile(path, "open");
    if (!this.inner.createExclusive) {
      throw denied(path, "create");
    }
    return this.inner.createExclusive(target, options);
  }

  async readdir(path: string): Promise<string[]> {
    return (await this.filteredDirents(path)).map((entry) => entry.name);
  }

  async readdirWithFileTypes(path: string): Promise<DirentEntry[]> {
    return this.filteredDirents(path);
  }

  async rm(path: string, options?: RmOptions): Promise<void> {
    if (this.device(path)) {
      return;
    }
    const target = this.normalize(path);
    let info: FsStat;
    try {
      info = await this.inner.lstat(target);
    } catch (error) {
      if (!isNotFound(error)) {
        throw error;
      }
      if (
        !this.allowlist.matches(target) &&
        !this.allowlist.isTraversable(target)
      ) {
        throw denied(path, "remove");
      }
      return this.inner.rm(target, options);
    }

    if (info.isDirectory) {
      if (!this.allowlist.isTraversable(target)) {
        throw denied(path, "remove");
      }
      if (options?.recursive) {
        await this.assertTreeMatches(target);
      }
    } else if (!this.allowlist.matches(target)) {
      throw denied(path, "remove");
    }

    return this.inner.rm(target, options);
  }

  async cp(src: string, dest: string, options?: CpOptions): Promise<void> {
    const source = this.normalize(src);
    const target = this.normalize(dest);
    const info = await this.policyLstat(source, "cp");

    if (info.isDirectory) {
      if (!options?.recursive) {
        throw denied(src, "cp");
      }
      if (!this.allowlist.isTraversable(target)) {
        throw denied(dest, "cp");
      }
      await this.assertCopyMapping(source, target);
      return this.inner.cp(source, target, options);
    }

    this.requireFile(source, "cp");
    this.requireFile(target, "cp");
    return this.inner.cp(source, target, options);
  }

  async mv(src: string, dest: string): Promise<void> {
    const source = this.normalize(src);
    const target = this.normalize(dest);
    const info = await this.policyLstat(source, "mv");

    if (info.isDirectory) {
      if (!this.allowlist.isTraversable(target)) {
        throw denied(dest, "mv");
      }
      await this.assertCopyMapping(source, target);
      return this.inner.mv(source, target);
    }

    this.requireFile(source, "mv");
    this.requireFile(target, "mv");
    return this.inner.mv(source, target);
  }

  resolvePath(base: string, path: string): string {
    return this.inner.resolvePath(base, path);
  }

  getAllPaths(): string[] {
    const allowed = this.inner.getAllPaths().filter((path) => {
      const target = this.tryNormalize(path);
      return target !== undefined && this.allowlist.matches(target);
    });
    return [...DEVICE_PATHS, ...allowed];
  }

  async chmod(path: string, mode: number): Promise<void> {
    if (this.device(path)) {
      return;
    }
    return this.inner.chmod(this.requireFile(path, "chmod"), mode);
  }

  async symlink(target: string, linkPath: string): Promise<void> {
    void target;
    void linkPath;
    throw denied(linkPath, "symlink");
  }

  async link(existingPath: string, newPath: string): Promise<void> {
    const existing = this.requireFile(existingPath, "link");
    const created = this.requireFile(newPath, "link");
    return this.inner.link(existing, created);
  }

  async readlink(path: string): Promise<string> {
    return this.inner.readlink(this.requireFile(path, "readlink"));
  }

  async realpath(path: string): Promise<string> {
    const device = this.device(path);
    if (device) {
      return device;
    }
    const target = this.normalize(path);
    if (
      !this.allowlist.matches(target) &&
      !this.allowlist.isTraversable(target)
    ) {
      throw denied(path, "realpath");
    }
    return this.inner.realpath(target);
  }

  async utimes(path: string, atime: Date, mtime: Date): Promise<void> {
    if (this.device(path)) {
      return;
    }
    return this.inner.utimes(this.requireFile(path, "utimes"), atime, mtime);
  }

  private async filteredDirents(path: string): Promise<DirentEntry[]> {
    const directory = this.requireTraversable(path, "scandir");
    const entries = await this.listWithTypes(directory);

    return entries.filter((entry) => {
      const child = posix.join(directory, entry.name);
      if (this.allowlist.matches(child)) {
        return true;
      }
      return entry.isDirectory && this.allowlist.isTraversable(child);
    });
  }

  private async listWithTypes(path: string): Promise<DirentEntry[]> {
    if (this.inner.readdirWithFileTypes) {
      return this.inner.readdirWithFileTypes(path);
    }

    const names = await this.inner.readdir(path);
    return Promise.all(
      names.map(async (name) => {
        const stat = await this.inner.lstat(posix.join(path, name));
        return {
          name,
          isFile: stat.isFile,
          isDirectory: stat.isDirectory,
          isSymbolicLink: stat.isSymbolicLink,
        };
      }),
    );
  }

  private async policyStat(path: string, operation: string): Promise<FsStat> {
    const target = this.requirePolicyCandidate(path, operation);
    const stat = await this.inner.stat(target);
    this.assertStatAllowed(target, stat, operation);
    return stat;
  }

  private async policyLstat(path: string, operation: string): Promise<FsStat> {
    const target = this.requirePolicyCandidate(path, operation);
    const stat = await this.inner.lstat(target);
    this.assertStatAllowed(target, stat, operation);
    return stat;
  }

  private requirePolicyCandidate(path: string, operation: string): string {
    const target = this.normalize(path);
    if (
      !this.allowlist.matches(target) &&
      !this.allowlist.isTraversable(target)
    ) {
      throw denied(path, operation);
    }
    return target;
  }

  private assertStatAllowed(
    path: string,
    stat: FsStat,
    operation: string,
  ): void {
    if (this.allowlist.matches(path)) {
      return;
    }
    if (stat.isDirectory && this.allowlist.isTraversable(path)) {
      return;
    }
    throw denied(path, operation);
  }

  private async assertTreeMatches(directory: string): Promise<void> {
    const entries = await this.listWithTypes(directory);
    for (const entry of entries) {
      const child = posix.join(directory, entry.name);
      if (entry.isDirectory) {
        if (!this.allowlist.isTraversable(child)) {
          throw denied(child, "remove");
        }
        await this.assertTreeMatches(child);
        continue;
      }
      if (!this.allowlist.matches(child)) {
        throw denied(child, "remove");
      }
    }
  }

  private async assertCopyMapping(
    source: string,
    target: string,
  ): Promise<void> {
    const entries = await this.listWithTypes(source);
    for (const entry of entries) {
      const childSource = posix.join(source, entry.name);
      const childTarget = posix.join(target, entry.name);
      if (entry.isDirectory) {
        if (!this.allowlist.isTraversable(childTarget)) {
          throw denied(childTarget, "cp");
        }
        await this.assertCopyMapping(childSource, childTarget);
        continue;
      }
      if (
        !this.allowlist.matches(childSource) ||
        !this.allowlist.matches(childTarget)
      ) {
        throw denied(
          this.allowlist.matches(childSource) ? childTarget : childSource,
          "cp",
        );
      }
    }
  }

  private requireFile(path: string, operation: string): string {
    const target = this.normalize(path);
    if (!this.allowlist.matches(target)) {
      throw denied(path, operation);
    }
    return target;
  }

  private device(path: string): string | undefined {
    const target = this.tryNormalize(path);
    return target !== undefined && DEVICE_PATHS.has(target)
      ? target
      : undefined;
  }

  private requireTraversable(path: string, operation: string): string {
    const target = this.normalize(path);
    if (!this.allowlist.isTraversable(target)) {
      throw denied(path, operation);
    }
    return target;
  }

  private normalize(path: string): string {
    try {
      return normalizeVirtualPath(path);
    } catch (error) {
      if (error instanceof PathEscapeError) {
        throw denied(path, "access");
      }
      throw error;
    }
  }

  private tryNormalize(path: string): string | undefined {
    try {
      return normalizeVirtualPath(path);
    } catch {
      return undefined;
    }
  }
}

function denied(path: string, operation: string): Error {
  const error = new Error(`EACCES: permission denied, ${operation} '${path}'`);
  (error as NodeJS.ErrnoException).code = "EACCES";
  return error;
}

function deviceStat(): FsStat {
  return {
    isFile: true,
    isDirectory: false,
    isSymbolicLink: false,
    mode: 0o666,
    size: 0,
    mtime: new Date(0),
  };
}

function isNotFound(error: unknown): boolean {
  return (
    typeof error === "object" &&
    error !== null &&
    "code" in error &&
    error.code === "ENOENT"
  );
}
