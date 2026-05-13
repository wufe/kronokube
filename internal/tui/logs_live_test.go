package tui

import (
	"bytes"
	"strings"
	"testing"
)

func TestLogRing_DropsOldestLines(t *testing.T) {
	r := newLogRing(3)
	r.write([]byte("a\nb\nc\n"))
	if got := string(r.snapshot()); got != "a\nb\nc\n" {
		t.Fatalf("snapshot after 3 writes = %q, want %q", got, "a\nb\nc\n")
	}
	r.write([]byte("d\n"))
	if got := string(r.snapshot()); got != "b\nc\nd\n" {
		t.Fatalf("snapshot after overflow = %q, want %q", got, "b\nc\nd\n")
	}
}

func TestLogRing_HandlesPartialLines(t *testing.T) {
	r := newLogRing(10)
	r.write([]byte("hello "))
	r.write([]byte("world\nnext"))
	got := string(r.snapshot())
	if got != "hello world\nnext" {
		t.Fatalf("snapshot = %q, want %q", got, "hello world\nnext")
	}
}

func TestLogRing_BulkOverflow(t *testing.T) {
	r := newLogRing(5)
	var buf bytes.Buffer
	for i := 0; i < 20; i++ {
		buf.WriteString("line\n")
	}
	r.write(buf.Bytes())
	got := string(r.snapshot())
	if strings.Count(got, "\n") != 5 {
		t.Fatalf("snapshot kept %d newlines, want 5: %q", strings.Count(got, "\n"), got)
	}
}
