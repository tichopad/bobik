import { isAbsolute, relative, resolve, sep } from "node:path";
import picomatch from "picomatch";
import { UsageError } from "./errors.ts";

/** Thrown when a path tries to climb above the filesystem root. */
export class PathEscapeError extends Error {
  override name = "PathEscapeError";
}

/**
 * The allow-path policy, expressed in the sandbox's virtual path space.
 *
 * `matches` is the read/write boundary for files. `isTraversable` additionally
 * allows directory metadata and listings so the agent can navigate, without
 * granting access to non-matching file contents.
 */
export interface Allowlist {
  /** Normalized virtual glob, rooted at /. */
  readonly pattern: string;
  /** True when the virtual path matches the allow glob. */
  matches(path: string): boolean;
  /** True when a directory may be listed or navigated. */
  isTraversable(path: string): boolean;
}

const MAGIC_SEGMENT = /[*?[\]{}()!+@]/;

/**
 * Normalize any path into the sandbox's virtual absolute path space.
 *
 * Relative paths are rooted at `/`. Traversal above `/` is rejected instead of
 * being silently clamped, so attempts to escape are visible as EACCES.
 */
export function normalizeVirtualPath(path: string): string {
  if (path.includes("\0")) {
    throw new PathEscapeError("Path contains a NUL byte");
  }

  const parts = path.split("/");
  const stack: string[] = [];

  for (const part of parts) {
    if (part === "" || part === ".") {
      continue;
    }
    if (part === "..") {
      if (stack.length === 0) {
        throw new PathEscapeError(`Path escapes the sandbox root: ${path}`);
      }
      stack.pop();
      continue;
    }
    stack.push(part);
  }

  return `/${stack.join("/")}`;
}

/**
 * Compile the user-supplied --allow-path glob into a policy.
 *
 * The glob is resolved relative to `cwd`. Patterns that escape cwd are a usage
 * error, because the sandbox root is cwd itself.
 */
export function compileAllowlist(cwd: string, allowGlob: string): Allowlist {
  const glob = allowGlob.trim();
  if (!glob) {
    throw new UsageError("--allow-path must not be empty.");
  }
  if (glob.startsWith("!")) {
    throw new UsageError(
      "--allow-path does not support negated globs. Pass the files you want to allow.",
    );
  }
  if (glob.includes("\0")) {
    throw new UsageError("--allow-path contains an invalid NUL byte.");
  }

  const absoluteCwd = resolve(cwd);
  const absolutePattern = isAbsolute(glob)
    ? resolve(glob)
    : resolve(absoluteCwd, glob);
  const relativePattern = relative(absoluteCwd, absolutePattern);

  if (
    relativePattern === ".." ||
    relativePattern.startsWith(`..${sep}`) ||
    isAbsolute(relativePattern)
  ) {
    throw new UsageError(
      `--allow-path must stay inside the working directory (${absoluteCwd}): ${glob}`,
    );
  }

  const pattern = virtualPatternFromRelative(relativePattern);
  const matcher = picomatch(pattern, { dot: false });
  const segments = pattern.split("/").filter(Boolean);

  let baseSegmentCount = 0;
  while (
    baseSegmentCount < segments.length &&
    !MAGIC_SEGMENT.test(segments[baseSegmentCount] ?? "")
  ) {
    baseSegmentCount += 1;
  }
  const base =
    baseSegmentCount === 0
      ? "/"
      : `/${segments.slice(0, baseSegmentCount).join("/")}`;
  const explicitDotSegments = new Set(
    segments.filter(
      (segment) => segment.startsWith(".") && !MAGIC_SEGMENT.test(segment),
    ),
  );

  return {
    pattern,
    matches(path: string): boolean {
      try {
        return matcher(normalizeVirtualPath(path));
      } catch {
        return false;
      }
    },
    isTraversable(path: string): boolean {
      let normalized: string;
      try {
        normalized = normalizeVirtualPath(path);
      } catch {
        return false;
      }

      if (normalized === "/") {
        return true;
      }

      const directorySegments = normalized.split("/").filter(Boolean);
      if (
        directorySegments.some(
          (segment) =>
            segment.startsWith(".") && !explicitDotSegments.has(segment),
        )
      ) {
        return false;
      }

      const isWithinBase =
        base === "/" ||
        normalized === base ||
        normalized.startsWith(`${base}/`);
      const isAncestorOfBase =
        normalized === "/" || base.startsWith(`${normalized}/`);

      return isWithinBase || isAncestorOfBase;
    },
  };
}

function virtualPatternFromRelative(relativePattern: string): string {
  const slashPattern = relativePattern.split(sep).join("/");
  const trimmed = slashPattern.replace(/^\/+/, "");
  return trimmed ? `/${trimmed}` : "/";
}
