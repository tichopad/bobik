import { createHash } from "node:crypto";
import { appendFile, readFile } from "node:fs/promises";
import { join, relative, sep } from "node:path";
import { watch, type FSWatcher } from "chokidar";
import type { LanguageModel } from "ai";
import {
  formatSummary,
  runNotesAgent,
  type ChangedFile,
  type RunNotesAgentOptions,
  type RunSummary,
} from "./agent.ts";
import { compileAllowlist } from "./allowlist.ts";
import { RunError } from "./errors.ts";
import { analyzeNote, DEFAULT_TRIGGER } from "./trigger.ts";

export { DEFAULT_TRIGGER } from "./trigger.ts";
export const SETTLE_MS = 1_000;
export const RETRY_DELAY_MS = 60_000;
export const AWAIT_WRITE_FINISH_MS = 500;

export interface WatchModeOptions {
  /** Host working directory and sandbox root. */
  cwd: string;
  /** Absolute prompt file path; re-read from disk before every run. */
  promptPath: string;
  /** Path shown in the audit log. */
  promptDisplayPath: string;
  /** Allow-path glob, resolved relative to cwd. */
  allowPath: string;
  deny?: readonly string[];
  model: LanguageModel;
  visionModel?: LanguageModel;
  /** Changed/added files must contain this string to trigger a run. */
  trigger?: string;
  /** Agent loop step limit per run; defaults to MAX_STEPS. */
  maxSteps?: number;
  /** Run implementation; defaults to runNotesAgent. Tests may wrap it. */
  runAgent?: (options: RunNotesAgentOptions) => Promise<RunSummary>;
  /** Suppression window for agent-written files after a run ends. */
  settleMs?: number;
  /** Delay before the single retry of a failed run. */
  retryDelayMs?: number;
  /** chokidar awaitWriteFinish stability threshold (the event debounce). */
  awaitWriteFinishMs?: number;
  stdout?: (text: string) => void;
  stderr?: (text: string) => void;
  /** Signal source for shutdown; defaults to process. */
  signals?: Pick<NodeJS.EventEmitter, "on" | "off">;
}

interface PendingRun {
  initial: boolean;
  /** Relative paths whose content matched the trigger. */
  triggered: Set<string>;
  /** Relative paths deleted since the last run; never trigger on their own. */
  deleted: Set<string>;
}

/**
 * Watch mode: run the agent once at startup, then re-run it whenever an
 * allowed file changes and contains the trigger string.
 *
 * Guards: agent-written paths are suppressed while a run is in flight plus a
 * short settle window, triggered content is hash-deduplicated, runs are
 * serialized with at most one coalesced follow-up, and a failed run gets
 * exactly one retry before bobik gives up on those files.
 *
 * Resolves with the session exit code: 0 on a clean shutdown, 130 when a
 * second signal aborted an in-flight run, 1 on a fatal session error such as
 * an unreadable prompt file.
 */
export async function watchMode(options: WatchModeOptions): Promise<number> {
  const trigger = options.trigger ?? DEFAULT_TRIGGER;
  const settleMs = options.settleMs ?? SETTLE_MS;
  const retryDelayMs = options.retryDelayMs ?? RETRY_DELAY_MS;
  const runAgent = options.runAgent ?? runNotesAgent;
  const stdout = options.stdout ?? ((text) => process.stdout.write(text));
  const stderr = options.stderr ?? ((text) => process.stderr.write(text));
  const signals = options.signals ?? process;

  const allowlist = compileAllowlist(options.cwd, options.allowPath);
  const watchRoot =
    allowlist.baseDirectory === "/"
      ? options.cwd
      : join(options.cwd, allowlist.baseDirectory);

  /** Content hash per triggered path; in-memory only, never persisted. */
  const hashes = new Map<string, string>();
  /** Paths written by the in-flight run. */
  const inFlightWrites = new Set<string>();
  /** Agent-written path → end of its post-run suppression window. */
  const suppressedUntil = new Map<string, number>();
  /** Coalesced change set for the next run. */
  const pending: PendingRun = {
    initial: false,
    triggered: new Set(),
    deleted: new Set(),
  };
  /** Failed run awaiting its single retry. */
  let pendingRetry: PendingRun | undefined;
  /** Paths that already used up their one retry. */
  const retried = new Set<string>();
  let initialRetried = false;
  let retryTimer: NodeJS.Timeout | undefined;
  let running = false;
  let stopping = false;
  let exitCode: number | undefined;
  let signalCount = 0;
  const abortController = new AbortController();

  let resolveSession!: (code: number) => void;
  const session = new Promise<number>((resolve) => {
    resolveSession = resolve;
  });
  let sessionResolved = false;

  const watcher: FSWatcher = watch(watchRoot, {
    ignoreInitial: true,
    awaitWriteFinish: {
      stabilityThreshold: options.awaitWriteFinishMs ?? AWAIT_WRITE_FINISH_MS,
    },
    // The audit log directory is written constantly during runs.
    ignored: (watchedPath: string) => {
      const rel = relative(options.cwd, watchedPath);
      return rel === ".agent-bobik" || rel.startsWith(`.agent-bobik${sep}`);
    },
  });

  function relativePath(absolute: string): string {
    return relative(options.cwd, absolute).split(sep).join("/");
  }

  function isSuppressed(path: string): boolean {
    if (running && inFlightWrites.has(path)) {
      return true;
    }
    const until = suppressedUntil.get(path);
    if (until === undefined) {
      return false;
    }
    if (until > Date.now()) {
      return true;
    }
    suppressedUntil.delete(path);
    return false;
  }

  async function onEvent(
    kind: "add" | "change" | "unlink",
    absolute: string,
  ): Promise<void> {
    if (stopping || exitCode !== undefined) {
      return;
    }
    const path = relativePath(absolute);
    if (!allowlist.matches(path) || isSuppressed(path)) {
      return;
    }

    if (kind === "unlink") {
      // Deletions never trigger a run, but are recorded for the next one.
      pending.triggered.delete(path);
      pending.deleted.add(path);
      hashes.delete(path);
      return;
    }

    let content: string;
    try {
      content = await readFile(absolute, "utf8");
    } catch {
      // The file vanished between the event and the read.
      return;
    }
    if (!analyzeNote(content, trigger).ready) {
      pending.triggered.delete(path);
      pendingRetry?.triggered.delete(path);
      hashes.delete(path);
      return;
    }
    const hash = createHash("sha256").update(content).digest("hex");
    if (hashes.get(path) === hash) {
      return;
    }
    hashes.set(path, hash);
    pending.deleted.delete(path);
    pending.triggered.add(path);
    stderr(`bobik: run triggered by ${path}\n`);
    schedule();
  }

  function schedule(): void {
    if (running || stopping || exitCode !== undefined) {
      return;
    }
    const hasWork =
      pending.initial ||
      pending.triggered.size > 0 ||
      (pendingRetry && retryTimer === undefined);
    if (!hasWork) {
      return;
    }
    void executeRun();
  }

  function takePending(): PendingRun {
    const run: PendingRun = {
      initial: pending.initial || (pendingRetry?.initial ?? false),
      triggered: new Set([
        ...pending.triggered,
        ...(pendingRetry?.triggered ?? []),
      ]),
      deleted: new Set([...pending.deleted, ...(pendingRetry?.deleted ?? [])]),
    };
    pending.initial = false;
    pending.triggered.clear();
    pending.deleted.clear();
    pendingRetry = undefined;
    for (const path of run.triggered) {
      run.deleted.delete(path);
    }
    return run;
  }

  async function executeRun(): Promise<void> {
    running = true;
    const run = takePending();
    inFlightWrites.clear();

    // Queued requests (including retries) may have been re-commented meanwhile.
    for (const path of run.triggered) {
      try {
        const content = await readFile(join(options.cwd, path), "utf8");
        if (analyzeNote(content, trigger).ready) continue;
      } catch {
        // A missing or unreadable note cannot submit a request.
      }
      run.triggered.delete(path);
      hashes.delete(path);
    }
    if (!run.initial && run.triggered.size === 0) {
      for (const path of run.deleted) pending.deleted.add(path);
      running = false;
      if (stopping || exitCode !== undefined) await shutdown(exitCode ?? 0);
      else schedule();
      return;
    }

    let prompt: string;
    try {
      prompt = await readFile(options.promptPath, "utf8");
    } catch (error) {
      stderr(
        `bobik: could not read prompt file ${options.promptPath}: ${errorMessage(error)}; stopping watch mode\n`,
      );
      running = false;
      await shutdown(1);
      return;
    }

    const changedFiles: ChangedFile[] = [
      ...[...run.triggered].sort().map((path) => ({ path })),
      ...[...run.deleted].sort().map((path) => ({ path, deleted: true })),
    ];

    try {
      const summary = await runAgent({
        cwd: options.cwd,
        promptPath: options.promptDisplayPath,
        prompt,
        allowPath: options.allowPath,
        trigger,
        deny: options.deny,
        model: options.model,
        ...(options.visionModel ? { visionModel: options.visionModel } : {}),
        signal: abortController.signal,
        onFileWrite: (path) => inFlightWrites.add(path),
        ...(options.maxSteps !== undefined
          ? { maxSteps: options.maxSteps }
          : {}),
        ...(run.initial ? { initialRun: true } : { changedFiles }),
      });
      const text = summary.text.endsWith("\n")
        ? summary.text
        : `${summary.text}\n`;
      stdout(text);
      stderr(`${formatSummary(summary)}\n`);
      for (const path of run.triggered) {
        retried.delete(path);
      }
      if (run.initial) {
        initialRetried = false;
      }
    } catch (error) {
      await handleFailure(error, run);
    }

    running = false;
    const settleUntil = Date.now() + settleMs;
    for (const path of inFlightWrites) {
      suppressedUntil.set(path, settleUntil);
    }
    inFlightWrites.clear();

    if (exitCode !== undefined || stopping) {
      await shutdown(exitCode ?? 0);
      return;
    }
    schedule();
  }

  async function handleFailure(error: unknown, run: PendingRun): Promise<void> {
    if (stopping || exitCode !== undefined) {
      return;
    }
    const message = errorMessage(error);
    stderr(`bobik: run failed: ${message}\n`);

    const retry: PendingRun = {
      initial: false,
      triggered: new Set(),
      deleted: new Set(),
    };
    const gaveUp: string[] = [];

    if (run.initial) {
      if (initialRetried) {
        gaveUp.push("(initial run)");
      } else {
        initialRetried = true;
        retry.initial = true;
      }
    }
    for (const path of run.triggered) {
      if (retried.has(path)) {
        gaveUp.push(path);
      } else {
        retried.add(path);
        retry.triggered.add(path);
      }
    }
    retry.deleted = run.deleted;

    if (gaveUp.length > 0) {
      stderr(
        `bobik: gave up on ${gaveUp.join(", ")} after one retry; edit the file again to re-trigger\n`,
      );
      const logPath =
        error instanceof RunError ? error.summary?.logPath : undefined;
      if (logPath) {
        try {
          await appendFile(
            logPath,
            `${JSON.stringify({
              ts: new Date().toISOString(),
              type: "watch-gave-up",
              paths: gaveUp,
            })}\n`,
            "utf8",
          );
        } catch {
          // Giving up is already reported on stderr; the log is best-effort.
        }
      }
    }

    if (retry.initial || retry.triggered.size > 0) {
      pendingRetry = retry;
      stderr(`bobik: retrying in ${Math.round(retryDelayMs / 1000)}s\n`);
      retryTimer = setTimeout(() => {
        retryTimer = undefined;
        schedule();
      }, retryDelayMs);
    }
  }

  async function shutdown(code: number): Promise<void> {
    if (sessionResolved) {
      return;
    }
    sessionResolved = true;
    if (retryTimer !== undefined) {
      clearTimeout(retryTimer);
      retryTimer = undefined;
    }
    signals.off("SIGINT", onSignal);
    signals.off("SIGTERM", onSignal);
    await watcher.close().catch(() => {});
    resolveSession(code);
  }

  function onSignal(): void {
    signalCount += 1;
    if (signalCount === 1) {
      stopping = true;
      pendingRetry = undefined;
      if (retryTimer !== undefined) {
        clearTimeout(retryTimer);
        retryTimer = undefined;
      }
      stderr("bobik: shutting down; waiting for the current run to finish\n");
      if (!running) {
        void shutdown(0);
      }
      return;
    }
    // Second signal: abort the in-flight run immediately.
    exitCode = 130;
    abortController.abort();
    if (!running) {
      void shutdown(130);
    }
  }

  signals.on("SIGINT", onSignal);
  signals.on("SIGTERM", onSignal);
  watcher.on("add", (path) => void onEvent("add", path));
  watcher.on("change", (path) => void onEvent("change", path));
  watcher.on("unlink", (path) => void onEvent("unlink", path));
  watcher.on("error", (error) => {
    stderr(`bobik: watcher error: ${errorMessage(error)}\n`);
  });

  stderr(`bobik: watching ${watchRoot} for changes matching "${trigger}"\n`);
  pending.initial = true;
  schedule();

  return session;
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
