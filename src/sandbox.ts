import { Bash, ReadWriteFs } from "just-bash";
import { createBashTool, type BashToolkit } from "bash-tool";
import { compileAllowlist, type Allowlist } from "./allowlist.ts";
import { AllowlistFs } from "./allowlist-fs.ts";
import { createDenyPolicy, type DenyPolicy } from "./deny.ts";

export interface NotesSandboxOptions {
  /** Host working directory; also the sandbox root. */
  cwd: string;
  /** User-supplied allow-path glob, relative to cwd. */
  allowPath: string;
  /** Command names blocked in bash. */
  deny?: readonly string[];
  /** Timezone name handed to the shell so `date` agrees with the prompt. */
  timeZone: string;
}

export interface NotesSandbox {
  tools: BashToolkit["tools"];
  bash: Bash;
  fs: AllowlistFs;
  allowlist: Allowlist;
  denyPolicy: DenyPolicy;
}

/**
 * Build the full sandbox stack: a real-FS backend rooted at cwd, the
 * allowlist wrapper, a just-bash shell, and the three AI SDK tools.
 *
 * The wrapper is the only enforcement point; the shell has no network access
 * and all three tools read and write through it.
 */
export async function createNotesSandbox(
  options: NotesSandboxOptions,
): Promise<NotesSandbox> {
  const allowlist = compileAllowlist(options.cwd, options.allowPath);
  const denyPolicy = createDenyPolicy(options.deny);

  const inner = new ReadWriteFs({
    root: options.cwd,
    allowSymlinks: false,
  });
  const fs = new AllowlistFs(inner, { allowlist });

  const bash = new Bash({
    fs,
    cwd: "/",
    env: { TZ: options.timeZone },
    executionLimits: {
      maxExecutionTimeMs: 30_000,
    },
  });

  const { tools } = await createBashTool({
    sandbox: bash,
    destination: "/",
    onBeforeBashCall: ({ command }) => {
      const denied = denyPolicy.check(command);
      if (!denied) {
        return undefined;
      }
      return {
        command: `echo ${shellQuote(`Blocked: ${denied} is denied`)} >&2; exit 126`,
      };
    },
  });

  return { tools, bash, fs, allowlist, denyPolicy };
}

function shellQuote(value: string): string {
  return `'${value.replaceAll("'", "'\\''")}'`;
}
