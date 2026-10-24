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

/**
 * Lines kept newest last, each counted with the newline that joins it,
 * dropped from the front by moving a start index (no shift() per line).
 */
class LineWindow {
  private items: string[] = [];
  private start = 0;
  private size = 0;

  get count(): number {
    return this.items.length - this.start;
  }

  push(line: string): void {
    this.items.push(line);
    this.size += line.length + 1;
  }

  /** Drops the oldest lines while over `max`, always keeping the newest. */
  keepAtMost(max: number): void {
    while (this.size > max && this.count > 1) this.drop();
  }

  /** Drops the oldest lines only while what is left still holds `min`. */
  keepAtLeast(min: number): void {
    while (this.count > 1 && this.size - (this.items[this.start]!.length + 1) > min) this.drop();
  }

  join(): string {
    return this.items.slice(this.start).join("\n");
  }

  private drop(): void {
    this.size -= this.items[this.start]!.length + 1;
    this.start += 1;
    // Compact now and then, so the array does not grow with every line logged.
    if (this.start >= 1024 && this.start * 2 >= this.items.length) {
      this.items = this.items.slice(this.start);
      this.start = 0;
    }
  }
}

export function createRecorder(run: Run): RunRecorder {
  // One byte past redactAndCap's look-behind ensures a cut inside a secret is discarded there.
  const window = OUTPUT_CAP + REDACT_EDGE + 2;
  const copy = (text: string): string => JSON.parse(JSON.stringify(text)) as string;
  const lines = new LineWindow();
  const head: string[] = [];
  let headSize = 0;
  let whole: string | null = "";
  // The raw lines behind expectText()'s tail, joined only when it is read.
  const raw = new LineWindow();
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
      raw.push(copy(line.slice(-OUTPUT_CAP)));
      raw.keepAtLeast(OUTPUT_CAP);
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
      // Drop from the front once well past the cap; redactAndCap trims exactly at the end.
      lines.keepAtMost(window);
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
    output: () => (lines.count === 0 ? null : lines.join()),
    expectText: () => {
      if (!logged) return null;
      return whole ?? head.join("\n").slice(0, OUTPUT_CAP) + "\n" + raw.join().slice(-OUTPUT_CAP);
    },
    metrics: () => ({ ...metrics }),
    abort: () => controller.abort(),
  };
}
