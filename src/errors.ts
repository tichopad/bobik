/** A problem with how the CLI was invoked or configured. Exits with code 2. */
export class UsageError extends Error {
  override name = "UsageError";
}

/** Run statistics attached to a runtime failure so the CLI can summarize it. */
export interface RunFailureSummary {
  steps: number;
  filesWritten: number;
  usage: {
    inputTokens: number | undefined;
    outputTokens: number | undefined;
    totalTokens: number | undefined;
  };
  durationMs: number;
  logPath: string;
  logRelativePath: string;
  modelId: string;
}

/** A problem while running the agent. Exits with code 1. */
export class RunError extends Error {
  override name = "RunError";
  summary: RunFailureSummary | undefined;

  constructor(
    message: string,
    options?: ErrorOptions & { summary?: RunFailureSummary },
  ) {
    const { summary, ...errorOptions } = options ?? {};
    super(message, errorOptions);
    this.summary = summary;
  }
}
