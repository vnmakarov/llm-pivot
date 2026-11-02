package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
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

// ------------------------------------------------------------- pump

// pumpInto runs the pump over what write produces and returns the lines it
// captured, plus any error the writing side hit. A writer that gets EPIPE is
// the symptom that matters: it means the pump stopped reading.
func pumpInto(t *testing.T, max int, write func(w io.Writer) error) ([]LogLine, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	b := NewLogBuffer(max)
	done := make(chan struct{})
	go func() { b.pump(StreamStdout, r); close(done) }()

	werr := write(w)
	w.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the pump did not finish")
	}
	lines, _, _ := b.Since(0)
	return lines, werr
}

func TestPumpKeepsReadingPastAnEnormousLine(t *testing.T) {
	lines, werr := pumpInto(t, 100, func(w io.Writer) error {
		if _, err := io.WriteString(w, "before\n"); err != nil {
			return err
		}
		// One line of about 2 MB, as \r progress output accumulates.
		if _, err := io.WriteString(w, strings.Repeat("progress\r", 250000)+"\n"); err != nil {
			return err
		}
		_, err := io.WriteString(w, "after\n")
		return err
	})
	// The write side failing at all means the pump closed the pipe early,
	// which is what killed the engine with SIGPIPE.
	if werr != nil {
		t.Fatalf("the engine's writes failed: %v", werr)
	}
	if len(lines) != 3 {
		t.Fatalf("captured %d lines, want 3", len(lines))
	}
	if lines[0].Text != "before" || lines[2].Text != "after" {
		t.Errorf("the lines around the huge one were lost: %q / %q", lines[0].Text, lines[2].Text)
	}
	if !strings.Contains(lines[1].Text, "[cut:") {
		t.Error("the huge line was not marked as cut")
	}
	if len(lines[1].Text) > maxLogLine+200 {
		t.Errorf("the cut line is %d bytes, which is not cut", len(lines[1].Text))
	}
}

// A line that exactly fills the cap is not over it, so it must be stored
// whole and not be marked.
func TestPumpKeepsALineExactlyAtTheCap(t *testing.T) {
	lines, werr := pumpInto(t, 10, func(w io.Writer) error {
		_, err := io.WriteString(w, strings.Repeat("x", maxLogLine)+"\n")
		return err
	})
	if werr != nil {
		t.Fatal(werr)
	}
	if len(lines) != 1 {
		t.Fatalf("captured %d lines, want 1", len(lines))
	}
	if len(lines[0].Text) != maxLogLine || strings.Contains(lines[0].Text, "[cut:") {
		t.Errorf("a line of exactly %d bytes was cut: len %d", maxLogLine, len(lines[0].Text))
	}
}

// Ordinary output has to come through exactly as before: line endings
// stripped, blank lines kept, a last line without a newline still captured.
func TestPumpOrdinaryOutput(t *testing.T) {
	lines, werr := pumpInto(t, 100, func(w io.Writer) error {
		_, err := io.WriteString(w, "one\ntwo\r\n\nthree without a newline")
		return err
	})
	if werr != nil {
		t.Fatal(werr)
	}
	var got []string
	for _, l := range lines {
		got = append(got, l.Text)
	}
	want := []string{"one", "two", "", "three without a newline"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Several long lines in a row must not make the pump hold on to one buffer
// per line, and each must still be cut independently.
func TestPumpRecoversBetweenLongLines(t *testing.T) {
	lines, werr := pumpInto(t, 100, func(w io.Writer) error {
		for i := 0; i < 3; i++ {
			if _, err := io.WriteString(w, strings.Repeat("y", maxLogLine+500)+"\n"); err != nil {
				return err
			}
			if _, err := io.WriteString(w, "short\n"); err != nil {
				return err
			}
		}
		return nil
	})
	if werr != nil {
		t.Fatal(werr)
	}
	if len(lines) != 6 {
		t.Fatalf("captured %d lines, want 6", len(lines))
	}
	for i, l := range lines {
		if i%2 == 0 {
			if !strings.Contains(l.Text, "[cut:") {
				t.Errorf("line %d should have been cut", i)
			}
		} else if l.Text != "short" {
			t.Errorf("line %d = %q, want \"short\"", i, l.Text)
		}
	}
}
