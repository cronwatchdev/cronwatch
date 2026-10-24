<?php

declare(strict_types=1);

namespace Cronwatch\Job;

use Cronwatch\Js;
use Cronwatch\Output;
use Cronwatch\Run;

/**
 * Collects a run's output and metrics while its function runs (job.ts).
 *
 * @internal
 */
final class RunRecorder
{
    /** Lines are dropped from the front once the output is well past the cap; redactAndCap trims it exactly at the end. */
    public const KEEP = Output::OUTPUT_CAP + Output::REDACT_EDGE + 2;

    /** The lines kept for the output. */
    private LineWindow $lines;
    /** @var list<string> The first lines logged, up to the cap. */
    private array $head = [];
    private int $headSize = 0;
    /** Raw expectation text while it fits twice the cap; otherwise the prefix and tail are kept. */
    private ?string $whole = '';
    private int $wholeLength = 0;
    /** The raw lines behind expectText()'s tail, joined only when it is read. */
    private LineWindow $raw;
    private bool $logged = false;
    /** @var array<string, int|float> */
    private array $metrics = [];
    public readonly AbortSignal $signal;
    public readonly JobContext $context;

    public function __construct(Run $run, int|float|null $timeoutMs)
    {
        $this->lines = new LineWindow();
        $this->raw = new LineWindow();
        $this->signal = new AbortSignal($timeoutMs);
        $this->context = new JobContext($run, $this->signal, $this);
    }

    public function log(string $line): void
    {
        $length = Js::length16($line);
        $separator = $this->logged ? "\n" : '';
        $this->logged = true;
        if ($this->whole !== null) {
            $this->wholeLength += strlen($separator) + $length;
            if ($this->wholeLength <= 2 * Output::OUTPUT_CAP) {
                $this->whole .= $separator . $line;
            } else {
                $this->whole = null;
            }
        }
        $this->raw->push(Js::tail16($line, Output::OUTPUT_CAP), min($length, Output::OUTPUT_CAP));
        $this->raw->keepAtLeast(Output::OUTPUT_CAP);
        if ($this->headSize < Output::OUTPUT_CAP) {
            $this->head[] = Js::head16($line, Output::OUTPUT_CAP - $this->headSize);
            $this->headSize += $length + 1;
        }
        $line = Output::stripNul($line);
        $length = Js::length16($line);
        if ($length >= self::KEEP) {
            $line = Js::tail16($line, self::KEEP - 1);
            $length = Js::length16($line);
        }
        $this->lines->push($line, $length);
        // Drop from the front once well past the cap; redactAndCap trims exactly at the end.
        $this->lines->keepAtMost(self::KEEP);
    }

    public function metric(string $name, int|float $value): void
    {
        $this->metrics[$name] = $value;
    }

    /**
     * The lines still held (past the bounded redaction window the oldest are let go), joined and not
     * yet capped: the client redacts them first, then caps them (Output::redactAndCap).
     */
    public function output(): ?string
    {
        return $this->lines->count() === 0 ? null : $this->lines->join();
    }

    /**
     * What an expect rule is checked against: everything logged, or when that
     * ran long, the first 16 KB and the last 16 KB. The stored output keeps
     * only the tail, so a "done" line printed early would otherwise be lost.
     */
    public function expectText(): ?string
    {
        if (!$this->logged) {
            return null;
        }
        return $this->whole ?? Js::head16(implode("\n", $this->head), Output::OUTPUT_CAP) . "\n" . Js::tail16($this->raw->join(), Output::OUTPUT_CAP);
    }

    /** @return array<string, int|float> */
    public function metrics(): array
    {
        return $this->metrics;
    }
}
