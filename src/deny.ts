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
 * Marker for a nested shell script the analyzer cannot inspect (for example
 * `sh notes.sh` or `sh` reading a pipeline). When a deny list is active these
 * forms fail closed instead of guessing what the script contains.
 */
export const UNINSPECTABLE_COMMAND = "<nested-script>";

const UNINSPECTABLE_LABEL = "nested shell script";

/** Shell interpreters that can execute another script. */
const SHELL_COMMANDS: ReadonlySet<string> = new Set([
  "bash",
  "sh",
  "dash",
  "zsh",
  "ksh",
]);

/** `find` primaries that run a command for each match. */
const FIND_EXEC_OPTIONS: ReadonlySet<string> = new Set([
  "-exec",
  "-execdir",
  "-ok",
  "-okdir",
]);

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
  if (deny.size === 0) {
    return undefined;
  }

  for (const name of extractCommandNames(script)) {
    if (name === UNINSPECTABLE_COMMAND) {
      return UNINSPECTABLE_LABEL;
    }
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
  if (command.name) {
    walkWord(command.name, names);
  }

  const name = literalWord(command.name);
  if (name) {
    recordInvocation(name, command.args, names, 0, command.redirections);
  } else if (command.name) {
    recordNameCandidates(command.name, names);
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
      case "ParameterExpansion":
        if (part.operation) {
          walkParameterOperation(part.operation, names);
        }
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

type ParameterExpansionPart = Extract<
  WordNode["parts"][number],
  { type: "ParameterExpansion" }
>;
type ParameterOperation = NonNullable<ParameterExpansionPart["operation"]>;

function walkParameterOperation(
  operation: ParameterOperation,
  names: string[],
): void {
  switch (operation.type) {
    case "DefaultValue":
    case "AssignDefault":
    case "UseAlternative":
      walkWord(operation.word, names);
      return;
    case "ErrorIfUnset":
      if (operation.word) {
        walkWord(operation.word, names);
      }
      return;
    case "PatternRemoval":
      walkWord(operation.pattern, names);
      return;
    case "PatternReplacement":
      walkWord(operation.pattern, names);
      if (operation.replacement) {
        walkWord(operation.replacement, names);
      }
      return;
    case "CaseModification":
      if (operation.pattern) {
        walkWord(operation.pattern, names);
      }
      return;
    case "Indirection":
      if (operation.innerOp) {
        walkParameterOperation(operation.innerOp, names);
      }
      return;
  }
}

/**
 * A command name built from parameter expansion, such as `${CMD:-rm}`, has
 * several possible values. Collect literal defaults as conservative candidates
 * so a denied fallback command is still caught.
 */
function recordNameCandidates(word: WordNode, names: string[]): void {
  for (const part of word.parts) {
    if (part.type !== "ParameterExpansion" || !part.operation) {
      continue;
    }
    for (const candidate of operationWords(part.operation)) {
      const literal = literalWord(candidate);
      if (literal === undefined) {
        continue;
      }
      for (const token of literal.trim().split(/\s+/)) {
        if (token) {
          names.push(token);
        }
      }
    }
  }
}

function operationWords(operation: ParameterOperation): WordNode[] {
  switch (operation.type) {
    case "DefaultValue":
    case "AssignDefault":
    case "UseAlternative":
      return [operation.word];
    case "ErrorIfUnset":
      return operation.word ? [operation.word] : [];
    case "PatternRemoval":
      return [operation.pattern];
    case "PatternReplacement":
      return operation.replacement
        ? [operation.pattern, operation.replacement]
        : [operation.pattern];
    case "CaseModification":
      return operation.pattern ? [operation.pattern] : [];
    case "Indirection":
      return operation.innerOp ? operationWords(operation.innerOp) : [];
    default:
      return [];
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
  redirections: SimpleCommandNode["redirections"] = [],
): void {
  if (depth > 8) {
    return;
  }
  names.push(name);

  const command = basename(name);

  if (command === "eval") {
    recordEval(args, names);
    return;
  }

  if (command === "trap") {
    recordTrap(args, names);
    return;
  }

  if (command === "source" || command === ".") {
    // The target script may live on disk; inspecting it synchronously is not
    // possible here, so a configured deny list fails closed.
    names.push(UNINSPECTABLE_COMMAND);
    return;
  }

  if (SHELL_COMMANDS.has(command)) {
    recordShell(args, redirections, names);
    return;
  }

  if (command === "find") {
    recordFindExec(args, names, depth);
    return;
  }

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

/**
 * Nested scripts are parsed recursively. The counter is module-level because
 * extraction is synchronous; it prevents adversarial nesting from exhausting
 * the stack.
 */
let nestedScriptDepth = 0;

function walkNestedScript(script: string, names: string[]): void {
  if (nestedScriptDepth >= 8) {
    names.push(UNINSPECTABLE_COMMAND);
    return;
  }

  nestedScriptDepth += 1;
  try {
    let ast: ScriptNode;
    try {
      ast = parse(script);
    } catch {
      names.push(UNINSPECTABLE_COMMAND);
      return;
    }
    walkScript(ast, names);
  } finally {
    nestedScriptDepth -= 1;
  }
}

function recordEval(args: WordNode[], names: string[]): void {
  if (args.length === 0) {
    return;
  }

  const parts = args.map((arg) => literalWord(arg));
  if (parts.some((part) => part === undefined)) {
    names.push(UNINSPECTABLE_COMMAND);
    return;
  }
  walkNestedScript(parts.join(" "), names);
}

function recordTrap(args: WordNode[], names: string[]): void {
  for (const arg of args) {
    const value = literalWord(arg);
    if (value === undefined) {
      names.push(UNINSPECTABLE_COMMAND);
      return;
    }
    if (value === "--") {
      continue;
    }
    if (value.startsWith("-")) {
      // -p / -l list traps; they do not execute anything.
      return;
    }
    if (value === "" || value === "-") {
      return;
    }
    walkNestedScript(value, names);
    return;
  }
}

function recordShell(
  args: WordNode[],
  redirections: SimpleCommandNode["redirections"],
  names: string[],
): void {
  for (let index = 0; index < args.length; index += 1) {
    const value = literalWord(args[index]);
    if (value === undefined || !value.startsWith("-")) {
      continue;
    }
    if (value.startsWith("--")) {
      continue;
    }

    const commandFlag = value.indexOf("c");
    if (commandFlag === -1) {
      continue;
    }

    const attached = value.slice(commandFlag + 1);
    if (attached) {
      walkNestedScript(attached, names);
      return;
    }

    const scriptWord = args[index + 1];
    if (!scriptWord) {
      names.push(UNINSPECTABLE_COMMAND);
      return;
    }
    const script = literalWord(scriptWord);
    if (script === undefined) {
      names.push(UNINSPECTABLE_COMMAND);
      return;
    }
    walkNestedScript(script, names);
    return;
  }

  // Without -c, a shell runs a file or reads stdin. A heredoc is inspectable;
  // everything else fails closed because the script content is not in the AST.
  const hasUninspectableArgument = args.some((arg) => {
    const value = literalWord(arg);
    return value === undefined || (!value.startsWith("-") && value !== "--");
  });
  if (hasUninspectableArgument) {
    names.push(UNINSPECTABLE_COMMAND);
    return;
  }

  let inspected = false;
  for (const redirection of redirections) {
    if (
      redirection.operator !== "<<" &&
      redirection.operator !== "<<-" &&
      redirection.operator !== "<<<"
    ) {
      continue;
    }
    inspected = true;
    const content =
      redirection.target.type === "HereDoc"
        ? literalWord(redirection.target.content)
        : literalWord(redirection.target);
    if (content === undefined) {
      names.push(UNINSPECTABLE_COMMAND);
      return;
    }
    walkNestedScript(content, names);
  }

  if (!inspected) {
    names.push(UNINSPECTABLE_COMMAND);
  }
}

function recordFindExec(
  args: WordNode[],
  names: string[],
  depth: number,
): void {
  for (let index = 0; index < args.length; index += 1) {
    const value = literalWord(args[index]);
    if (value === undefined || !FIND_EXEC_OPTIONS.has(value)) {
      continue;
    }

    const commandArgs: WordNode[] = [];
    let cursor = index + 1;
    for (; cursor < args.length; cursor += 1) {
      const terminator = literalWord(args[cursor]);
      if (terminator === ";" || terminator === "+") {
        break;
      }
      const argument = args[cursor];
      if (argument) {
        commandArgs.push(argument);
      }
    }

    const inner = firstOrdinaryArgument(commandArgs, undefined);
    if (inner) {
      recordInvocation(inner.name, inner.rest, names, depth + 1);
    }
    index = cursor;
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
