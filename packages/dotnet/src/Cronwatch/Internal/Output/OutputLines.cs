using System.Collections.Generic;
using System.Text;
using System.Threading;

namespace Cronwatch.Internal;

/// <summary>
/// The lines half of <c>createRecorder</c> in the SDK's <c>job.ts</c>: what a run logs, what it
/// stores as its output, and what its expect rule is checked against. Safe to share, since a job
/// may log from threads of its own.
/// </summary>
internal sealed class OutputLines
{
    /// <summary>
    /// How much logged text is held, in code units, before lines are dropped from the front. The
    /// cap trims exactly at the end, so this only bounds memory: well past the cap, so the kept
    /// tail is whole.
    /// </summary>
    public const int Window = OutputText.OutputCap + OutputText.RedactEdge + 2;

    private readonly Lock _lock = new();
    private readonly LinkedList<string> _lines = new();
    private long _size;
    private readonly List<string> _head = [];
    private long _headSize;
    // Appended in place, so a run of short lines does not copy what it holds on each one.
    private StringBuilder? _whole = new();
    // The raw lines behind ExpectText's tail, joined only when it is read: each line's last 16 KB,
    // the oldest dropped once what is left still holds 16 KB.
    private readonly LinkedList<string> _raw = new();
    private long _rawSize;
    private bool _logged;

    /// <summary>Whether anything was logged.</summary>
    public bool IsEmpty
    {
        get
        {
            lock (_lock)
            {
                return _lines.Count == 0;
            }
        }
    }

    /// <summary>Appends a line.</summary>
    public void Log(string line)
    {
        lock (_lock)
        {
            string separator = _logged ? "\n" : "";
            _logged = true;
            if (_whole != null)
            {
                _whole = _whole.Length + separator.Length + line.Length <= 2 * OutputText.OutputCap ? _whole.Append(separator).Append(line) : null;
            }
            string last = line[^System.Math.Min(line.Length, OutputText.OutputCap)..];
            _raw.AddLast(last);
            _rawSize += last.Length + 1;
            while (_raw.Count > 1 && _rawSize - (_raw.First!.Value.Length + 1) > OutputText.OutputCap)
            {
                _rawSize -= _raw.First.Value.Length + 1;
                _raw.RemoveFirst();
            }
            if (_headSize < OutputText.OutputCap)
            {
                _head.Add(line[..System.Math.Min(line.Length, (int)(OutputText.OutputCap - _headSize))]);
                _headSize += line.Length + 1;
            }
            line = NulText.StripNul(line);
            if (line.Length >= Window)
            {
                line = line[^(Window - 1)..];
            }
            _lines.AddLast(line);
            _size += line.Length + 1;
            // Drop from the front once well past the cap; RedactAndCap trims exactly at the end.
            while (_size > Window && _lines.Count > 1)
            {
                _size -= _lines.First!.Value.Length + 1;
                _lines.RemoveFirst();
            }
        }
    }

    /// <summary>
    /// The lines still held (past the window the oldest are let go), joined and not yet capped:
    /// the client redacts them first, then caps them (<see cref="OutputText.RedactAndCap"/>).
    /// Null when nothing was logged.
    /// </summary>
    public string? Output()
    {
        lock (_lock)
        {
            return _lines.Count == 0 ? null : string.Join('\n', _lines);
        }
    }

    /// <summary>
    /// What an expect rule is checked against: everything logged, or when that ran long, the
    /// first 16 KB and the last 16 KB. The stored output keeps only the tail, so a "done" line
    /// printed early would otherwise be lost. Null when nothing was logged.
    /// </summary>
    public string? ExpectText()
    {
        lock (_lock)
        {
            if (_lines.Count == 0)
            {
                return null;
            }
            if (_whole != null)
            {
                return _whole.ToString();
            }
            string tail = string.Join('\n', _raw);
            return Js.Head(string.Join('\n', _head), OutputText.OutputCap) + "\n" + tail[^System.Math.Min(tail.Length, OutputText.OutputCap)..];
        }
    }
}
