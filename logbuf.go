package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const (
	// logReadChunk is how much of the engine's output is read at a time.
	logReadChunk = 64 * 1024
	// maxLogLine caps one stored line. A longer one is cut rather than
	// dropped, and never stops the line after it from being read.
	maxLogLine = 1 << 20
)

// Stream tags tell the monitor page where a line came from.
const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
	StreamSystem = "system" // llm-pivot's own notes (start, exit, errors)
)

// LogLine is one captured line of engine output.
type LogLine struct {
	Seq    uint64 `json:"seq"`
	Time   string `json:"time"`
	Stream string `json:"stream"`
	Text   string `json:"text"`
}

// LogBuffer is a bounded, sequence-numbered ring of engine output. The
// monitor page polls it with the highest sequence number it has already seen.
type LogBuffer struct {
	mu    sync.Mutex
	lines []LogLine
	max   int
	seq   uint64
}

func NewLogBuffer(max int) *LogBuffer {
	if max <= 0 {
		max = DefaultLogLines
	}
	return &LogBuffer{max: max, lines: make([]LogLine, 0, 256)}
}

// SetMax resizes the ring. log_lines is editable on the settings page, which
// promises that a save takes effect at once, so this has to work on a buffer
// that is already filling up: shrinking drops the oldest lines right away
// rather than waiting for the next one to arrive.
func (b *LogBuffer) SetMax(max int) {
	if max <= 0 {
		max = DefaultLogLines
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.max = max
	if len(b.lines) > b.max {
		// A fresh array, not a copy in place: the old one would go on
		// holding the text of every dropped line.
		b.lines = append(make([]LogLine, 0, b.max), b.lines[len(b.lines)-b.max:]...)
	}
}

// Append records one line and returns its sequence number.
func (b *LogBuffer) Append(stream, text string) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	b.lines = append(b.lines, LogLine{
		Seq:    b.seq,
		Time:   time.Now().Format("15:04:05"),
		Stream: stream,
		Text:   strings.TrimRight(text, "\r\n"),
	})
	if len(b.lines) > b.max {
		b.lines = append(b.lines[:0], b.lines[len(b.lines)-b.max:]...)
	}
	return b.seq
}

// Appendf records a formatted system note.
func (b *LogBuffer) System(text string) { b.Append(StreamSystem, text) }

// Since returns the lines newer than seq, plus the latest sequence number.
// The second return value is true when lines were dropped from the ring
// because the caller fell behind.
func (b *LogBuffer) Since(seq uint64) (lines []LogLine, last uint64, gap bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	last = b.seq
	if len(b.lines) == 0 {
		return nil, last, false
	}
	oldest := b.lines[0].Seq
	if seq != 0 && seq+1 < oldest {
		gap = true
	}
	for _, l := range b.lines {
		if l.Seq > seq {
			lines = append(lines, l)
		}
	}
	return lines, last, gap
}

// Clear drops all buffered lines but keeps the sequence counter monotonic, so
// clients polling with an old cursor are not confused.
func (b *LogBuffer) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = b.lines[:0]
}

// pump copies a process pipe into the buffer line by line.
//
// It has to drain the pipe until EOF whatever the engine writes down it. A
// pump that stops reading early makes the engine's next write fail with
// EPIPE, and the SIGPIPE that comes with it kills the very process llm-pivot
// is supposed to be running - reported, confusingly, as the engine having
// exited by itself. So an unreadably long line is cut and marked rather than
// allowed to end the loop, and a read error is logged rather than swallowed.
func (b *LogBuffer) pump(stream string, r io.ReadCloser) {
	defer r.Close()
	br := bufio.NewReaderSize(r, logReadChunk)
	var (
		cur []byte // the line being assembled
		cut bool   // it passed maxLogLine; the rest of it is dropped
	)
	for {
		// chunk points into br's own buffer and stays valid only until the
		// next read, so whatever is kept has to be copied out of it.
		chunk, err := br.ReadSlice('\n')
		whole := err == nil
		if whole {
			chunk = bytes.TrimRight(chunk, "\r\n")
		}
		if !cut {
			if room := maxLogLine - len(cur); len(chunk) > room {
				cur, cut = append(cur, chunk[:room]...), true
			} else {
				cur = append(cur, chunk...)
			}
		}

		switch {
		case whole:
			b.Append(stream, lineText(cur, cut))
			if cap(cur) > logReadChunk {
				cur = nil // hand the outsized array back
			} else {
				cur = cur[:0]
			}
			cut = false
		case errors.Is(err, bufio.ErrBufferFull):
			// Only part of the line so far; the rest is still coming.
		default:
			// EOF or a broken pipe: whatever is held is the final line.
			if len(cur) > 0 {
				b.Append(stream, lineText(cur, cut))
			}
			if !errors.Is(err, io.EOF) {
				b.System(fmt.Sprintf("✗ cannot read the engine's %s: %v", stream, err))
			}
			return
		}
	}
}

// lineText renders one captured line, saying so when it was cut short.
func lineText(line []byte, cut bool) string {
	s := strings.TrimRight(string(line), "\r\n")
	if cut {
		s += fmt.Sprintf(" … [cut: this line is longer than %d bytes]", maxLogLine)
	}
	return s
}
