package server

import (
	"errors"
	"io"
	"slices"
	"testing"
)

type sizedReader struct {
	remaining int
	sizes     []int
}

func (r *sizedReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	r.remaining--
	r.sizes = append(r.sizes, len(p))
	return len(p), nil
}

func TestCopyBufferLogGrowsAfterSustainedFullReads(t *testing.T) {
	src := &sizedReader{remaining: 8}
	if err := copyBufferLog(io.Discard, src, func(uint64) bool { return true }); err != nil {
		t.Fatal(err)
	}
	want := []int{4 << 10, 4 << 10, 8 << 10, 8 << 10, 16 << 10, 16 << 10, 32 << 10, 32 << 10}
	if !slices.Equal(src.sizes, want) {
		t.Fatalf("read buffer progression = %v, want %v", src.sizes, want)
	}
}

type oneByteReader struct{ done bool }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	p[0] = 1
	return 1, nil
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestCopyBufferLogRejectsShortWrite(t *testing.T) {
	err := copyBufferLog(writerFunc(func(p []byte) (int, error) {
		return len(p) - 1, nil
	}), &oneByteReader{}, func(uint64) bool { return true })
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("error = %v, want io.ErrShortWrite", err)
	}
}
