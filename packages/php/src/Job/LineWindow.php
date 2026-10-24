<?php

declare(strict_types=1);

namespace Cronwatch\Job;

/**
 * Lines kept newest last, each counted in UTF-16 units with the newline that
 * joins it, dropped from the front by moving a start index rather than
 * shifting the array (job.ts's LineWindow).
 *
 * @internal
 */
final class LineWindow
{
    /** @var list<string> */
    private array $items = [];
    /** @var list<int> each item's length in UTF-16 units */
    private array $lengths = [];
    private int $start = 0;
    private int $size = 0;

    public function count(): int
    {
        return count($this->items) - $this->start;
    }

    public function push(string $line, int $length): void
    {
        $this->items[] = $line;
        $this->lengths[] = $length;
        $this->size += $length + 1;
    }

    /** Drops the oldest lines while over `$max`, always keeping the newest. */
    public function keepAtMost(int $max): void
    {
        while ($this->size > $max && $this->count() > 1) {
            $this->drop();
        }
    }

    /** Drops the oldest lines only while what is left still holds `$min`. */
    public function keepAtLeast(int $min): void
    {
        while ($this->count() > 1 && $this->size - ($this->lengths[$this->start] + 1) > $min) {
            $this->drop();
        }
    }

    public function join(): string
    {
        return implode("\n", $this->start === 0 ? $this->items : array_slice($this->items, $this->start));
    }

    private function drop(): void
    {
        $this->size -= $this->lengths[$this->start] + 1;
        $this->start++;
        // Compact now and then, so the arrays do not grow with every line logged.
        if ($this->start >= 1024 && $this->start * 2 >= count($this->items)) {
            $this->items = array_slice($this->items, $this->start);
            $this->lengths = array_slice($this->lengths, $this->start);
            $this->start = 0;
        }
    }
}
