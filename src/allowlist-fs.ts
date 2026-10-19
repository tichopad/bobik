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
  /** Called after a file is created or modified through this filesystem. */
  onWrite?: (path: string) => void;
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

/** The host-written audit directory is never visible inside the sandbox. */
const PROTECTED_DIRECTORY = "/.agent-bobik";

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
  private readonly onWrite: ((path: string) => void) | undefined;

  constructor(inner: IFileSystem, options: AllowlistFsOptions) {
    this.inner = inner;
    this.allowlist = options.allowlist;
    this.onWrite = options.onWrite;
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
    const target = this.requireFile(path, "write");
    await this.inner.writeFile(target, content, options);
    this.notifyWrite(target);
  }

  async appendFile(
    path: string,
    content: FileContent,
    options?: WriteFileOptions | BufferEncoding,
  ): Promise<void> {
    if (this.device(path)) {
      return;
    }
    const target = this.requireFile(path, "append");
    await this.inner.appendFile(target, content, options);
    this.notifyWrite(target);
  }

  async exists(path: string): Promise<boolean> {
    const target = this.tryNormalize(path);
    if (!target) {
      return false;
    }
    if (DEVICE_PATHS.has(target)) {
      return true;
    }
    if (this.isProtected(target)) {
      return false;
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
    await this.inner.createExclusive(target, options);
    if (!options.directory) {
      this.notifyWrite(target);
    }
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
    if (this.isProtected(target)) {
      throw denied(path, "remove");
    }
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
      await this.inner.cp(source, target, options);
      await this.notifyTreeWrites(source, target);
      return;
    }

    this.requireFile(source, "cp");
    this.requireFile(target, "cp");
    await this.inner.cp(source, target, options);
    this.notifyWrite(target);
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
      const files = await this.collectTreeFiles(source);
      await this.inner.mv(source, target);
      for (const file of files) {
        this.notifyWrite(posix.join(target, posix.relative(source, file)));
      }
      return;
    }

    this.requireFile(source, "mv");
    this.requireFile(target, "mv");
    await this.inner.mv(source, target);
    this.notifyWrite(target);
  }

  resolvePath(base: string, path: string): string {
    return this.inner.resolvePath(base, path);
  }

  getAllPaths(): string[] {
    const allowed = this.inner.getAllPaths().filter((path) => {
      const target = this.tryNormalize(path);
      return (
        target !== undefined &&
        !this.isProtected(target) &&
        this.allowlist.matches(target)
      );
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
      this.isProtected(target) ||
      (!this.allowlist.matches(target) && !this.allowlist.isTraversable(target))
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
      if (this.isProtected(child)) {
        return false;
      }
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

  private async notifyTreeWrites(
    source: string,
    target: string,
  ): Promise<void> {
    for (const file of await this.collectTreeFiles(source)) {
      this.notifyWrite(posix.join(target, posix.relative(source, file)));
    }
  }

  private async collectTreeFiles(directory: string): Promise<string[]> {
    const entries = await this.listWithTypes(directory);
    const files: string[] = [];
    for (const entry of entries) {
      const child = posix.join(directory, entry.name);
      if (entry.isDirectory) {
        files.push(...(await this.collectTreeFiles(child)));
      } else {
        files.push(child);
      }
    }
    return files;
  }

  private notifyWrite(path: string): void {
    this.onWrite?.(path.startsWith("/") ? path.slice(1) : path);
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
      this.isProtected(target) ||
      (!this.allowlist.matches(target) && !this.allowlist.isTraversable(target))
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
    if (this.isProtected(directory)) {
      throw denied(directory, "remove");
    }
    const entries = await this.listWithTypes(directory);
    for (const entry of entries) {
      const child = posix.join(directory, entry.name);
      if (this.isProtected(child)) {
        throw denied(child, "remove");
      }
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
    if (this.isProtected(source) || this.isProtected(target)) {
      throw denied(this.isProtected(source) ? source : target, "cp");
    }
    const entries = await this.listWithTypes(source);
    for (const entry of entries) {
      const childSource = posix.join(source, entry.name);
      const childTarget = posix.join(target, entry.name);
      if (this.isProtected(childSource) || this.isProtected(childTarget)) {
        throw denied(
          this.isProtected(childSource) ? childSource : childTarget,
          "cp",
        );
      }
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
    if (this.isProtected(target) || !this.allowlist.matches(target)) {
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
    if (this.isProtected(target) || !this.allowlist.isTraversable(target)) {
      throw denied(path, operation);
    }
    return target;
  }

  private isProtected(path: string): boolean {
    return (
      path === PROTECTED_DIRECTORY || path.startsWith(`${PROTECTED_DIRECTORY}/`)
    );
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
