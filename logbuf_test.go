package main

import (
	"fmt"
	"testing"
)

// fill pushes n numbered lines and returns the buffer.
func fill(b *LogBuffer, n int) {
	for i := 1; i <= n; i++ {
		b.Append(StreamStdout, fmt.Sprintf("line %d", i))
	}
}

// count is how many lines the buffer would hand to a page starting fresh.
func count(b *LogBuffer) int {
	lines, _, _ := b.Since(0)
	return len(lines)
}

func TestLogBufferKeepsItsMax(t *testing.T) {
	b := NewLogBuffer(10)
	fill(b, 25)
	if got := count(b); got != 10 {
		t.Fatalf("kept %d lines, want 10", got)
	}
	lines, last, _ := b.Since(0)
	if lines[0].Text != "line 16" || lines[9].Text != "line 25" {
		t.Errorf("kept the wrong window: %q..%q", lines[0].Text, lines[9].Text)
	}
	if last != 25 {
		t.Errorf("last seq = %d, want 25", last)
	}
}

// log_lines is presented as live-editable, so a shrink has to take effect on
// the lines already buffered rather than at the next restart.
func TestLogBufferShrinksAtOnce(t *testing.T) {
	b := NewLogBuffer(4000)
	fill(b, 500)

	b.SetMax(100)
	if got := count(b); got != 100 {
		t.Fatalf("after shrinking to 100 the buffer holds %d lines", got)
	}
	lines, _, _ := b.Since(0)
	if lines[0].Text != "line 401" {
		t.Errorf("the newest 100 lines were not the ones kept: starts at %q", lines[0].Text)
	}
	// And it stays at the new size as more arrive.
	fill(b, 50)
	if got := count(b); got != 100 {
		t.Errorf("the new limit did not hold: %d lines", got)
	}
}

func TestLogBufferGrowsAtOnce(t *testing.T) {
	b := NewLogBuffer(10)
	fill(b, 25)

	b.SetMax(100)
	if got := count(b); got != 10 {
		t.Fatalf("growing invented lines: %d, want the 10 still held", got)
	}
	fill(b, 200)
	if got := count(b); got != 100 {
		t.Errorf("the buffer did not grow to 100: %d lines", got)
	}
}

// A shrink must not leave a gap for a page that is up to date: the lines it
// has already seen are gone, but it has them on screen.
func TestLogBufferShrinkKeepsSequenceNumbers(t *testing.T) {
	b := NewLogBuffer(4000)
	fill(b, 500)
	b.SetMax(100)

	if _, _, gap := b.Since(500); gap {
		t.Error("a caller holding the newest line was told it fell behind")
	}
	if _, _, gap := b.Since(5); !gap {
		t.Error("a caller that really fell behind was not told so")
	}
	if seq := b.Append(StreamSystem, "next"); seq != 501 {
		t.Errorf("sequence restarted after a resize: %d, want 501", seq)
	}
}

func TestLogBufferSetMaxZeroFallsBackToTheDefault(t *testing.T) {
	b := NewLogBuffer(10)
	b.SetMax(0)
	fill(b, DefaultLogLines+50)
	if got := count(b); got != DefaultLogLines {
		t.Errorf("kept %d lines, want the default %d", got, DefaultLogLines)
	}
}
