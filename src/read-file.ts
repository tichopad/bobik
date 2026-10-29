import { posix } from "node:path";
import { jsonSchema, tool } from "ai";
import type { IFileSystem } from "just-bash";

/** How many same-name candidates a missing-file hint may list. */
const MAX_HINTS = 5;

interface ReadFileOptions {
  fs: Pick<IFileSystem, "readFile" | "getAllPaths">;
}

/**
 * Read a file, annotating a missing-path error with other allowed files that
 * share its name.
 *
 * The agent often sees a wiki-link such as `[[Memories]]` and guesses a
 * root-level path. A bare ENOENT then reads as "the file does not exist", so
 * it may create a duplicate instead of searching for the real note. Listing
 * same-name candidates turns that dead end into a usable path.
 */
export async function readFileWithHint(
  fs: Pick<IFileSystem, "readFile" | "getAllPaths">,
  path: string,
): Promise<string> {
  try {
    return await fs.readFile(path, "utf8");
  } catch (error) {
    throw withMissingFileHint(fs, path, error);
  }
}

/** Return `error`, or an ENOENT copy naming allowed files with the same name. */
export function withMissingFileHint(
  fs: Pick<IFileSystem, "getAllPaths">,
  path: string,
  error: unknown,
): unknown {
  if (!isNotFound(error)) {
    return error;
  }

  const name = posix.basename(path);
  if (!name) {
    return error;
  }

  let matches: string[];
  try {
    matches = fs
      .getAllPaths()
      .map((candidate) => candidate.replace(/^\/+/, ""))
      .filter((candidate) => posix.basename(candidate) === name)
      .sort();
  } catch {
    // A hint is best-effort; never hide the original filesystem error.
    return error;
  }

  if (matches.length === 0) {
    return error;
  }

  const shown = matches.slice(0, MAX_HINTS);
  const remaining = matches.length - shown.length;
  const list = shown.map((match) => `"${match}"`).join(", ");
  const files = matches.length === 1 ? "a file" : "files";
  const verb = matches.length === 1 ? "exists" : "exist";
  const where = remaining > 0 ? `${list}, and ${remaining} more` : list;

  const hinted = new Error(
    `${errorMessage(error)} — ${files} named "${name}" ${verb} at ${where}; ` +
      "use that path instead of creating a new file.",
  );
  (hinted as NodeJS.ErrnoException).code = "ENOENT";
  return hinted;
}

export function createReadFileTool(options: ReadFileOptions) {
  return tool({
    description: "Read the contents of a file from the sandbox.",
    inputSchema: jsonSchema<{ path: string }>({
      type: "object",
      properties: {
        path: { type: "string", minLength: 1 },
      },
      required: ["path"],
      additionalProperties: false,
    }),
    execute: async ({ path }) => {
      return { content: await readFileWithHint(options.fs, path) };
    },
  });
}

function isNotFound(error: unknown): boolean {
  if (typeof error !== "object" || error === null) {
    return false;
  }
  if ("code" in error && error.code === "ENOENT") {
    return true;
  }
  // just-bash reports filesystem errors as plain Error objects without a
  // `code`, so fall back to the Node-style message prefix.
  return error instanceof Error && error.message.startsWith("ENOENT:");
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
