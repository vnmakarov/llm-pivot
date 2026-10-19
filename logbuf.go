package main

import (
	"bufio"
	"io"
	"strings"
	"sync"
	"time"
)

// Stream tags tell the monitor page where a line came from.
const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
	StreamSystem = "system" // engine-router's own notes (start, exit, errors)
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
func (b *LogBuffer) pump(stream string, r io.ReadCloser) {
	defer r.Close()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		b.Append(stream, sc.Text())
	}
}
