import { appendFile, mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";

export interface AuditEvent {
  ts: string;
  type: string;
  [key: string]: unknown;
}

/**
 * Append-only JSONL audit log for one run.
 *
 * Writes are serialized through an internal queue so concurrent callbacks
 * cannot interleave partial lines. Each event is written in full; nothing is
 * truncated.
 */
export class AuditLog {
  readonly path: string;
  readonly relativePath: string;
  readonly filename: string;

  private queue: Promise<void> = Promise.resolve();

  private constructor(path: string, relativePath: string, filename: string) {
    this.path = path;
    this.relativePath = relativePath;
    this.filename = filename;
  }

  /**
   * Create the `.agent-bobik` directory and a fresh log file for this run.
   * If two runs start in the same millisecond, a numeric suffix is added.
   */
  static async create(cwd: string, now: Date = new Date()): Promise<AuditLog> {
    const directoryName = ".agent-bobik";
    const directory = join(cwd, directoryName);
    await mkdir(directory, { recursive: true });

    const base = auditLogFilename(now).replace(/\.jsonl$/, "");
    for (let attempt = 0; attempt < 100; attempt += 1) {
      const filename = `${base}${attempt === 0 ? "" : `-${attempt}`}.jsonl`;
      const path = join(directory, filename);
      try {
        await writeFile(path, "", {
          encoding: "utf8",
          mode: 0o600,
          flag: "wx",
        });
        return new AuditLog(path, join(directoryName, filename), filename);
      } catch (error) {
        if (!isAlreadyExists(error)) {
          throw error;
        }
      }
    }

    throw new Error(
      `Could not create an audit log in ${directory}: too many collisions`,
    );
  }

  /** Append one event. Events are written in call order. */
  log(type: string, data: Record<string, unknown> = {}): Promise<void> {
    const event: AuditEvent = {
      ...data,
      ts: new Date().toISOString(),
      type,
    };
    const line = `${JSON.stringify(event)}\n`;

    this.queue = this.queue.then(() => appendFile(this.path, line, "utf8"));
    return this.queue;
  }

  /** Wait for every queued event to reach disk. */
  flush(): Promise<void> {
    return this.queue;
  }
}

/** Local timestamp used in filenames, e.g. `2026-10-19T19-55-00.123.jsonl`. */
export function auditLogFilename(now: Date): string {
  const year = pad(now.getFullYear(), 4);
  const month = pad(now.getMonth() + 1);
  const day = pad(now.getDate());
  const hours = pad(now.getHours());
  const minutes = pad(now.getMinutes());
  const seconds = pad(now.getSeconds());
  const milliseconds = pad(now.getMilliseconds(), 3);

  return `${year}-${month}-${day}T${hours}-${minutes}-${seconds}.${milliseconds}.jsonl`;
}

function pad(value: number, length = 2): string {
  return String(value).padStart(length, "0");
}

function isAlreadyExists(error: unknown): boolean {
  return (
    typeof error === "object" &&
    error !== null &&
    "code" in error &&
    error.code === "EEXIST"
  );
}
