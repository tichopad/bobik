import { jsonSchema, tool } from "ai";
import type { IFileSystem } from "just-bash";

interface EditFileOptions {
  fs: Pick<IFileSystem, "readFile" | "writeFile">;
}

interface EditFileInput {
  path: string;
  oldString: string;
  newString: string;
  replaceAll?: boolean;
}

/**
 * Targeted find-and-replace edits on a single note. All file access stays
 * behind the sandbox seam, so the allowlist is enforced exactly as for
 * readFile/writeFile.
 */
export function createEditFileTool(options: EditFileOptions) {
  return tool({
    description:
      "Replace exact text in a file inside the allowed filesystem. By default oldString must occur exactly once; set replaceAll to replace every occurrence. Prefer this over shell scripting (sed, heredocs) for edits.",
    inputSchema: jsonSchema<EditFileInput>({
      type: "object",
      properties: {
        path: { type: "string", minLength: 1 },
        oldString: { type: "string", minLength: 1 },
        newString: { type: "string" },
        replaceAll: { type: "boolean" },
      },
      required: ["path", "oldString", "newString"],
      additionalProperties: false,
    }),
    execute: async ({ path, oldString, newString, replaceAll }) => {
      if (oldString === newString) {
        throw new Error("oldString and newString must differ.");
      }
      const content = await options.fs.readFile(path, "utf8");
      const count = content.split(oldString).length - 1;
      if (count === 0) {
        throw new Error(`oldString not found in ${path}.`);
      }
      if (count > 1 && !replaceAll) {
        throw new Error(
          `oldString occurs ${count} times in ${path}; include more surrounding context to make it unique, or set replaceAll to true.`,
        );
      }

      // Split/join, not String.replace: newString must be inserted verbatim,
      // even when it contains `$` patterns. Past the guards, !replaceAll
      // implies count === 1.
      const updated = content.split(oldString).join(newString);
      await options.fs.writeFile(path, updated, "utf8");
      return { success: true, replacements: replaceAll ? count : 1 };
    },
  });
}
