import { fromMarkdown } from "mdast-util-from-markdown";

export const DEFAULT_TRIGGER = "#bobik";

export interface NoteTrigger {
  ready: boolean;
  draft: boolean;
}

/** Shared by the watcher and filesystem guard; code is never executable prose. */
export function analyzeNote(
  content: string,
  trigger = DEFAULT_TRIGGER,
): NoteTrigger {
  if (!content.includes(trigger)) return { ready: false, draft: false };
  const prose = withoutCode(content);
  let commented = false;
  let ready = false;
  let draft = false;
  for (const part of prose.split("%%")) {
    const matches =
      trigger === DEFAULT_TRIGGER
        ? /(?<![\p{L}\p{N}_/#-])#bobik(?=[:\s]|$)/u.test(part)
        : part.includes(trigger);
    if (matches) {
      if (commented) draft = true;
      else ready = true;
    }
    commented = !commented;
  }
  return { ready: ready && !draft, draft };
}

interface MarkdownNode {
  type: string;
  children?: readonly MarkdownNode[];
  position?: { start: { offset?: number }; end: { offset?: number } };
}

/** Use source ranges, not rendered text: code in lists/quotes is code too. */
function withoutCode(content: string): string {
  const parts: string[] = [];
  let start = 0;

  function visit(node: MarkdownNode): void {
    if (node.type === "code" || node.type === "inlineCode") {
      const from = node.position?.start.offset;
      const to = node.position?.end.offset;
      if (from !== undefined && to !== undefined) {
        // Mask rather than delete so tags cannot join across code boundaries.
        parts.push(content.slice(start, from), " ");
        start = to;
      }
      return;
    }
    for (const child of node.children ?? []) visit(child);
  }

  visit(fromMarkdown(content));
  parts.push(content.slice(start));
  return parts.join("");
}
