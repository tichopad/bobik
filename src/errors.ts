/** A problem with how the CLI was invoked or configured. Exits with code 2. */
export class UsageError extends Error {
  override name = "UsageError";
}

/** A problem while running the agent. Exits with code 1. */
export class RunError extends Error {
  override name = "RunError";
}
