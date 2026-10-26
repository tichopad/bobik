# AGENTS.md

Bobik is a single-purpose CLI AI agent for a directory of Markdown notes: it reads, creates, edits, and deletes only files matching a user-supplied glob, and writes a JSONL audit log of each run.

- Use pnpm (never npm/yarn).
- There is no build step: Node 22.18+ runs the TypeScript source directly via native type stripping. Do not add a build step or emit settings.
- Verify changes with `pnpm typecheck`, `pnpm lint`, and `pnpm test`.
