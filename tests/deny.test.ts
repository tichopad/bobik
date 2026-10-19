import assert from "node:assert/strict";
import { describe, it } from "node:test";
import {
  createDenyPolicy,
  extractCommandNames,
  findDeniedCommand,
} from "../src/deny.ts";

function commands(script: string): string[] {
  return extractCommandNames(script);
}

describe("extractCommandNames", () => {
  it("extracts a plain command", () => {
    assert.deepEqual(commands("rm notes.md"), ["rm"]);
  });

  it("extracts commands from pipelines", () => {
    assert.deepEqual(commands("cat notes.md | grep TODO | rm"), [
      "cat",
      "grep",
      "rm",
    ]);
  });

  it("extracts commands from &&, || and ; chains", () => {
    assert.deepEqual(commands("echo start && rm a.md; echo done || rm b.md"), [
      "echo",
      "rm",
      "echo",
      "rm",
    ]);
  });

  it("extracts commands from subshells and groups", () => {
    assert.deepEqual(
      commands("(cd notes && rm a.md) && { echo x; rm b.md; }"),
      ["cd", "rm", "echo", "rm"],
    );
  });

  it("extracts commands from command substitutions", () => {
    assert.deepEqual(commands('echo "$(rm a.md)"'), ["echo", "rm"]);
    assert.deepEqual(commands("echo `rm a.md`"), ["echo", "rm"]);
  });

  it("extracts commands from control flow bodies", () => {
    assert.deepEqual(
      commands(
        'if test -f a.md; then rm a.md; else echo missing; fi; for f in *.md; do rm "$f"; done; while true; do rm x.md; break; done',
      ),
      ["test", "rm", "echo", "rm", "true", "rm", "break"],
    );
  });

  it("extracts the command invoked by xargs", () => {
    assert.deepEqual(commands("printf '%s\\n' a.md | xargs rm -f"), [
      "printf",
      "xargs",
      "rm",
    ]);
  });

  it("handles xargs options and their values", () => {
    assert.deepEqual(commands("xargs -I{} rm {}"), ["xargs", "rm"]);
    assert.deepEqual(commands("xargs -n 1 rm"), ["xargs", "rm"]);
    assert.deepEqual(commands("xargs --max-args=1 rm"), ["xargs", "rm"]);
    assert.deepEqual(commands("xargs -- rm"), ["xargs", "rm"]);
  });

  it("does not treat xargs' own arguments as commands", () => {
    assert.deepEqual(commands("xargs echo rm"), ["xargs", "echo"]);
  });

  it("unwraps command wrappers", () => {
    assert.deepEqual(commands("command rm a.md"), ["command", "rm"]);
    assert.deepEqual(commands("env FOO=1 rm a.md"), ["env", "rm"]);
    assert.deepEqual(commands("sudo -u root rm a.md"), ["sudo", "rm"]);
    assert.deepEqual(commands("timeout 10 rm a.md"), ["timeout", "rm"]);
    assert.deepEqual(commands("nice -n 5 rm a.md"), ["nice", "rm"]);
    assert.deepEqual(commands("busybox rm a.md"), ["busybox", "rm"]);
  });

  it("reconstructs command names from quoted and escaped words", () => {
    assert.deepEqual(commands('r"m" a.md'), ["rm"]);
    assert.deepEqual(commands("r\\m a.md"), ["rm"]);
    assert.deepEqual(commands("'r''m' a.md"), ["rm"]);
  });

  it("does not guess dynamically constructed command names", () => {
    assert.deepEqual(commands("$CMD a.md"), []);
    assert.deepEqual(commands('"$CMD" a.md'), []);
  });

  it("returns an empty list for empty or unparseable scripts", () => {
    assert.deepEqual(commands(""), []);
    assert.deepEqual(commands("if then"), []);
  });

  it("documents commands the AST cannot see through", () => {
    assert.deepEqual(commands("find . -name '*.md' -delete"), ["find"]);
    assert.deepEqual(commands("sed -i s/a/b/ notes.md"), ["sed"]);
  });
});

describe("findDeniedCommand", () => {
  const deny = new Set(["rm", "wget"]);

  it("matches exact command names", () => {
    assert.equal(findDeniedCommand("rm a.md", deny), "rm");
    assert.equal(findDeniedCommand("echo ok", deny), undefined);
  });

  it("matches commands invoked through wrappers and xargs", () => {
    assert.equal(findDeniedCommand("command rm a.md", deny), "rm");
    assert.equal(findDeniedCommand("xargs -n1 rm", deny), "rm");
  });

  it("matches absolute command paths by basename", () => {
    assert.equal(findDeniedCommand("/bin/rm a.md", deny), "rm");
    assert.equal(findDeniedCommand("./wget example.com", deny), "wget");
  });
});

describe("createDenyPolicy", () => {
  it("normalizes, trims and de-duplicates the deny list", () => {
    const policy = createDenyPolicy([" rm ", "wget", "", "rm"]);

    assert.deepEqual([...policy.commands], ["rm", "wget"]);
  });

  it("ignores an empty deny list", () => {
    const policy = createDenyPolicy(undefined);

    assert.equal(policy.check("rm a.md"), undefined);
  });

  it("reports the first denied command", () => {
    const policy = createDenyPolicy(["rm", "wget"]);

    assert.equal(policy.check("echo hi | wget x && rm y"), "wget");
  });
});
