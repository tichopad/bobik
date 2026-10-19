#!/usr/bin/env node

/**
 * Published-install launcher.
 *
 * Node refuses to strip TypeScript types for files under `node_modules`, so a
 * package whose bin points directly at `src/cli.ts` cannot run after a normal
 * install. This launcher enables type stripping for Bobik's own source files,
 * then imports the real CLI entry point. There is still no build step.
 */
import { readFileSync } from "node:fs";
import { registerHooks, stripTypeScriptTypes } from "node:module";
import { fileURLToPath } from "node:url";

const sourceRoot = new URL("../src/", import.meta.url);

// stripTypeScriptTypes is still experimental; suppress only its warning so the
// CLI's stderr contract stays clean.
const emitWarning = process.emitWarning;
process.emitWarning = function (warning, ...args) {
  const type = typeof args[0] === "string" ? args[0] : args[0]?.type;
  if (
    type === "ExperimentalWarning" &&
    String(warning).includes("stripTypeScriptTypes")
  ) {
    return;
  }
  return emitWarning.call(process, warning, ...args);
};

registerHooks({
  load(url, context, nextLoad) {
    if (url.startsWith(sourceRoot.href) && url.endsWith(".ts")) {
      const source = readFileSync(fileURLToPath(url), "utf8").replace(
        /^#!.*\n/,
        "\n",
      );
      return {
        format: "module",
        source: stripTypeScriptTypes(source),
        shortCircuit: true,
      };
    }
    return nextLoad(url, context);
  },
});

const { main } = await import("../src/cli.ts");
process.exitCode = await main(process.argv.slice(2));
