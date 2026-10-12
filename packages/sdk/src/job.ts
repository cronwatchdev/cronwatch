import { OUTPUT_CAP, REDACT_EDGE, stripNul } from "./output.js";
import type { Run } from "./types.js";

/** What a job function receives. */
export interface JobContext {
  readonly name: string;
  readonly runId: string;
  readonly startedAt: number;
  /** Aborts when the job's timeout elapses. Honour it if the work can stop. */
  readonly signal: AbortSignal;
  /** Append a line of output. Kept with the run, capped at 16 KB, shown in alerts and the dashboard. */
  log(...parts: unknown[]): void;
  /** Report a number for this run: tokens, cost, rows, anything. Watched against budgets and baselines. */
  metric(name: string, value: number): void;
  metrics(values: Record<string, number>): void;
}

export interface RunRecorder {
  context: JobContext;
  /**
   * The lines still held in a bounded redaction window, joined and not
   * yet capped: the client redacts them first, then caps them (redactAndCap).
   */
  output(): string | null;
  /**
   * What an expect rule is checked against: everything logged, or when that
   * ran long, the first 16 KB and the last 16 KB. The stored output keeps only
   * the tail, so a "done" line printed early would otherwise be lost.
   */
  expectText(): string | null;
  metrics(): Record<string, number>;
  abort(): void;
}

function stringify(part: unknown): string {
  if (typeof part === "string") return part;
  if (part instanceof Error) return `${part.name}: ${part.message}`;
  try {
    return JSON.stringify(part);
  } catch {
    return String(part);
  }
}

export function createRecorder(run: Run): RunRecorder {
  // One byte past redactAndCap's look-behind ensures a cut inside a secret is discarded there.
  const window = OUTPUT_CAP + REDACT_EDGE + 2;
  const copy = (text: string): string => JSON.parse(JSON.stringify(text)) as string;
  const lines: string[] = [];
  let size = 0;
  const head: string[] = [];
  let headSize = 0;
  let whole: string | null = "";
  let tail = "";
  let logged = false;
  const metrics: Record<string, number> = {};
  const controller = new AbortController();

  const context: JobContext = {
    name: run.job,
    runId: run.id,
    startedAt: run.startedAt,
    signal: controller.signal,
    log(...parts) {
      let line = parts.map(stringify).join(" ");
      const separator = logged ? "\n" : "";
      logged = true;
      if (whole !== null) whole = whole.length + separator.length + line.length <= 2 * OUTPUT_CAP ? whole + separator + line : null;
      tail = copy((tail + separator + copy(line.slice(-OUTPUT_CAP))).slice(-OUTPUT_CAP));
      if (headSize < OUTPUT_CAP) {
        head.push(copy(line.slice(0, OUTPUT_CAP - headSize)));
        headSize += line.length + 1;
      }
      // Expectations keep the raw prefix and tail; storage removes NUL before cutting so it cannot shrink the redaction margin.
      line = stripNul(line);
      if (line.length >= window) {
        line = copy(line.slice(-(window - 1)));
      }
      lines.push(line);
      size += line.length + 1;
      // Drop from the front once well past the cap; redactAndCap trims exactly at the end.
      while (size > window && lines.length > 1) {
        size -= lines.shift()!.length + 1;
      }
    },
    metric(name, value) {
      if (typeof value !== "number" || !Number.isFinite(value)) {
        throw new Error(`metric "${name}" must be a finite number`);
      }
      metrics[name] = value;
    },
    metrics(values) {
      for (const [k, v] of Object.entries(values)) context.metric(k, v);
    },
  };

  return {
    context,
    output: () => (lines.length === 0 ? null : lines.join("\n")),
    expectText: () => {
      if (!logged) return null;
      return whole ?? head.join("\n").slice(0, OUTPUT_CAP) + "\n" + tail;
    },
    metrics: () => ({ ...metrics }),
    abort: () => controller.abort(),
  };
}
