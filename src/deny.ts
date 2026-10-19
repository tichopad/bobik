import { basename } from "node:path";
import {
  parse,
  type CommandNode,
  type PipelineNode,
  type ScriptNode,
  type SimpleCommandNode,
  type StatementNode,
  type WordNode,
} from "just-bash";

/**
 * The deny policy for bash commands. The interface is deliberately small: a
 * caller hands in the configured command names and asks whether a script uses
 * one of them.
 */
export interface DenyPolicy {
  /** Normalized deny list. */
  readonly commands: ReadonlySet<string>;
  /** Return the first denied command in the script, or undefined. */
  check(script: string): string | undefined;
}

/** Build a deny policy from the CLI's deny list. */
export function createDenyPolicy(
  denyList: readonly string[] | undefined,
): DenyPolicy {
  const commands = new Set(
    (denyList ?? []).map(normalizeCommandName).filter(Boolean),
  );

  return {
    commands,
    check(script: string): string | undefined {
      return findDeniedCommand(script, commands);
    },
  };
}

/**
 * Return the first command in `script` that appears in `deny`, or undefined.
 *
 * Matching is by command name (basename), so `/bin/rm` matches `rm`.
 */
export function findDeniedCommand(
  script: string,
  deny: ReadonlySet<string>,
): string | undefined {
  for (const name of extractCommandNames(script)) {
    const normalized = normalizeCommandName(name);
    if (deny.has(name)) {
      return name;
    }
    if (deny.has(normalized)) {
      return normalized;
    }
  }
  return undefined;
}

/**
 * Extract every command name mentioned in a bash script.
 *
 * The walk follows pipelines, subshells, control-flow bodies, command
 * substitutions, xargs arguments, and common command wrappers (`command`,
 * `env`, `sudo`, `timeout`, ...). Dynamic names such as `$CMD` and commands
 * hidden in `find -exec` or `sed -i` scripts are not resolved; the filesystem
 * allowlist remains the write boundary.
 */
export function extractCommandNames(script: string): string[] {
  let ast: ScriptNode;
  try {
    ast = parse(script);
  } catch {
    return [];
  }

  const names: string[] = [];
  walkScript(ast, names);
  return names;
}

function walkScript(script: ScriptNode, names: string[]): void {
  for (const statement of script.statements) {
    walkStatement(statement, names);
  }
}

function walkStatement(statement: StatementNode, names: string[]): void {
  for (const pipeline of statement.pipelines) {
    walkPipeline(pipeline, names);
  }
}

function walkPipeline(pipeline: PipelineNode, names: string[]): void {
  for (const command of pipeline.commands) {
    walkCommand(command, names);
  }
}

function walkCommand(command: CommandNode, names: string[]): void {
  switch (command.type) {
    case "SimpleCommand":
      walkSimpleCommand(command, names);
      return;
    case "If":
      for (const clause of command.clauses) {
        walkStatements(clause.condition, names);
        walkStatements(clause.body, names);
      }
      walkStatements(command.elseBody ?? [], names);
      return;
    case "For":
      for (const word of command.words ?? []) {
        walkWord(word, names);
      }
      walkStatements(command.body, names);
      return;
    case "CStyleFor":
      walkStatements(command.body, names);
      return;
    case "While":
    case "Until":
      walkStatements(command.condition, names);
      walkStatements(command.body, names);
      return;
    case "Case":
      walkWord(command.word, names);
      for (const item of command.items) {
        walkStatements(item.body, names);
      }
      return;
    case "Subshell":
    case "Group":
      walkStatements(command.body, names);
      return;
    case "FunctionDef":
      walkCommand(command.body, names);
      return;
    case "ConditionalCommand":
      walkConditional(command.expression, names);
      return;
    case "ArithmeticCommand":
      return;
  }
}

function walkStatements(statements: StatementNode[], names: string[]): void {
  for (const statement of statements) {
    walkStatement(statement, names);
  }
}

function walkSimpleCommand(command: SimpleCommandNode, names: string[]): void {
  const name = literalWord(command.name);
  if (name) {
    recordInvocation(name, command.args, names, 0);
  }

  for (const word of command.args) {
    walkWord(word, names);
  }
  for (const assignment of command.assignments) {
    if (assignment.value) {
      walkWord(assignment.value, names);
    }
    for (const word of assignment.array ?? []) {
      walkWord(word, names);
    }
  }
  for (const redirection of command.redirections) {
    if (redirection.target.type === "HereDoc") {
      walkWord(redirection.target.content, names);
    } else {
      walkWord(redirection.target, names);
    }
  }
}

function walkWord(word: WordNode, names: string[]): void {
  walkWordParts(word.parts, names);
}

function walkWordParts(parts: WordNode["parts"], names: string[]): void {
  for (const part of parts) {
    switch (part.type) {
      case "CommandSubstitution":
      case "ProcessSubstitution":
        walkScript(part.body, names);
        break;
      case "DoubleQuoted":
        walkWordParts(part.parts, names);
        break;
      case "BraceExpansion":
        for (const item of part.items) {
          if (item.type === "Word") {
            walkWord(item.word, names);
          }
        }
        break;
    }
  }
}

function walkConditional(
  expression: Extract<
    CommandNode,
    { type: "ConditionalCommand" }
  >["expression"],
  names: string[],
): void {
  switch (expression.type) {
    case "CondBinary":
      walkWord(expression.left, names);
      walkWord(expression.right, names);
      return;
    case "CondUnary":
      walkWord(expression.operand, names);
      return;
    case "CondNot":
      walkConditional(expression.operand, names);
      return;
    case "CondAnd":
    case "CondOr":
      walkConditional(expression.left, names);
      walkConditional(expression.right, names);
      return;
    case "CondGroup":
      walkConditional(expression.expression, names);
      return;
    case "CondWord":
      walkWord(expression.word, names);
      return;
  }
}

interface WrapperSpec {
  optionsWithValues?: ReadonlySet<string>;
  skipAssignments?: boolean;
}

const NO_OPTIONS: WrapperSpec = {};

const WRAPPERS: Readonly<Record<string, WrapperSpec>> = {
  command: NO_OPTIONS,
  builtin: NO_OPTIONS,
  exec: { optionsWithValues: new Set(["-a"]) },
  nohup: NO_OPTIONS,
  busybox: NO_OPTIONS,
  sudo: {
    optionsWithValues: new Set([
      "-u",
      "-g",
      "-h",
      "-p",
      "-C",
      "-T",
      "-U",
      "-r",
      "-t",
      "--user",
      "--group",
      "--host",
      "--prompt",
      "--close-from",
      "--command-timeout",
      "--chdir",
      "--other-user",
      "--role",
      "--type",
    ]),
  },
  env: {
    optionsWithValues: new Set([
      "-u",
      "--unset",
      "-C",
      "--chdir",
      "-S",
      "--split-string",
    ]),
    skipAssignments: true,
  },
  nice: {
    optionsWithValues: new Set(["-n", "--adjustment"]),
  },
  stdbuf: {
    optionsWithValues: new Set([
      "-i",
      "-o",
      "-e",
      "--input",
      "--output",
      "--error",
    ]),
  },
};

const XARGS_OPTIONS_WITH_VALUES: ReadonlySet<string> = new Set([
  "-I",
  "-n",
  "-L",
  "-P",
  "-s",
  "-d",
  "-E",
  "-e",
  "-a",
  "--replace",
  "--max-args",
  "--max-lines",
  "--max-procs",
  "--max-chars",
  "--delimiter",
  "--eof",
  "--arg-file",
]);

const TIMEOUT_OPTIONS_WITH_VALUES: ReadonlySet<string> = new Set([
  "-s",
  "-k",
  "--signal",
  "--kill-after",
]);

function recordInvocation(
  name: string,
  args: WordNode[],
  names: string[],
  depth: number,
): void {
  if (depth > 8) {
    return;
  }
  names.push(name);

  const command = basename(name);

  if (command === "timeout") {
    const duration = firstOrdinaryArgument(args, TIMEOUT_OPTIONS_WITH_VALUES);
    if (duration) {
      const inner = firstOrdinaryArgument(
        duration.rest,
        TIMEOUT_OPTIONS_WITH_VALUES,
      );
      if (inner) {
        recordInvocation(inner.name, inner.rest, names, depth + 1);
      }
    }
    return;
  }

  if (command === "xargs") {
    const inner = firstOrdinaryArgument(args, XARGS_OPTIONS_WITH_VALUES);
    if (inner) {
      recordInvocation(inner.name, inner.rest, names, depth + 1);
    }
    return;
  }

  const wrapper = WRAPPERS[command];
  if (wrapper) {
    const inner = firstOrdinaryArgument(
      args,
      wrapper.optionsWithValues,
      wrapper.skipAssignments,
    );
    if (inner) {
      recordInvocation(inner.name, inner.rest, names, depth + 1);
    }
  }
}

function firstOrdinaryArgument(
  args: WordNode[],
  optionsWithValues: ReadonlySet<string> | undefined,
  skipAssignments = false,
): { name: string; rest: WordNode[] } | undefined {
  for (let index = 0; index < args.length; index += 1) {
    const argument = args[index];
    if (!argument) {
      return undefined;
    }
    const value = literalWord(argument);
    if (value === undefined) {
      return undefined;
    }
    if (value === "--") {
      continue;
    }
    if (value.startsWith("-") && value !== "-") {
      if (optionsWithValues?.has(value)) {
        index += 1;
      }
      continue;
    }
    if (skipAssignments && /^[A-Za-z_][A-Za-z0-9_]*=/.test(value)) {
      continue;
    }
    return { name: value, rest: args.slice(index + 1) };
  }
  return undefined;
}

function literalWord(word: WordNode | null): string | undefined {
  if (!word) {
    return undefined;
  }
  return literalParts(word.parts);
}

function literalParts(parts: WordNode["parts"]): string | undefined {
  let value = "";
  for (const part of parts) {
    switch (part.type) {
      case "Literal":
      case "SingleQuoted":
      case "Escaped":
        value += part.value;
        break;
      case "DoubleQuoted": {
        const inner = literalParts(part.parts);
        if (inner === undefined) {
          return undefined;
        }
        value += inner;
        break;
      }
      default:
        return undefined;
    }
  }
  return value;
}

function normalizeCommandName(name: string): string {
  const trimmed = name.trim();
  return trimmed ? basename(trimmed) : "";
}
