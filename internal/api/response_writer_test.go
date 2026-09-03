package api

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

type minimalResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *minimalResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *minimalResponseWriter) WriteHeader(status int) { w.status = status }
func (w *minimalResponseWriter) Write(p []byte) (int, error) {
	return w.body.Write(p)
}

type flusherResponseWriter struct {
	*minimalResponseWriter
	flushed bool
}

func (w *flusherResponseWriter) Flush() { w.flushed = true }

type fullResponseWriter struct {
	*minimalResponseWriter
	flushed bool
	pushed  bool
	read    bool
	server  net.Conn
	client  net.Conn
}

func (w *fullResponseWriter) Flush() { w.flushed = true }

func (w *fullResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.server, w.client = net.Pipe()
	return w.server, bufio.NewReadWriter(bufio.NewReader(w.server), bufio.NewWriter(w.server)), nil
}

func (w *fullResponseWriter) Push(string, *http.PushOptions) error {
	w.pushed = true
	return nil
}

func (w *fullResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	w.read = true
	return io.Copy(&w.body, r)
}

func TestStatusResponseWriter_PreservesAllCapabilities(t *testing.T) {
	underlying := &fullResponseWriter{minimalResponseWriter: &minimalResponseWriter{}}
	wrapped, observer := NewStatusResponseWriter(underlying)

	flusher, ok := wrapped.(http.Flusher)
	if !ok {
		t.Fatal("wrapped writer lost http.Flusher")
	}
	hijacker, ok := wrapped.(http.Hijacker)
	if !ok {
		t.Fatal("wrapped writer lost http.Hijacker")
	}
	pusher, ok := wrapped.(http.Pusher)
	if !ok {
		t.Fatal("wrapped writer lost http.Pusher")
	}
	readerFrom, ok := wrapped.(io.ReaderFrom)
	if !ok {
		t.Fatal("wrapped writer lost io.ReaderFrom")
	}
	unwrapper, ok := wrapped.(interface{ Unwrap() http.ResponseWriter })
	if !ok || unwrapper.Unwrap() != underlying {
		t.Fatal("wrapped writer did not preserve Unwrap semantics")
	}

	flusher.Flush()
	if !underlying.flushed {
		t.Fatal("Flush was not forwarded")
	}
	conn, _, err := hijacker.Hijack()
	if err != nil || conn == nil {
		t.Fatalf("Hijack() conn=%v err=%v", conn, err)
	}
	t.Cleanup(func() {
		_ = underlying.server.Close()
		_ = underlying.client.Close()
	})
	if err := pusher.Push("/asset.js", nil); err != nil || !underlying.pushed {
		t.Fatalf("Push() forwarded=%t err=%v", underlying.pushed, err)
	}
	if n, err := readerFrom.ReadFrom(strings.NewReader("payload")); err != nil || n != 7 || !underlying.read {
		t.Fatalf("ReadFrom() n=%d forwarded=%t err=%v", n, underlying.read, err)
	}
	if observer.Status() != http.StatusOK || observer.BytesWritten() != 7 {
		t.Fatalf("observer status/bytes = %d/%d", observer.Status(), observer.BytesWritten())
	}
}

func TestStatusResponseWriter_DoesNotInventCapabilities(t *testing.T) {
	minimal := &minimalResponseWriter{}
	wrapped, _ := NewStatusResponseWriter(minimal)
	if _, ok := wrapped.(http.Flusher); ok {
		t.Fatal("minimal writer unexpectedly implements Flusher")
	}
	if _, ok := wrapped.(http.Hijacker); ok {
		t.Fatal("minimal writer unexpectedly implements Hijacker")
	}
	if _, ok := wrapped.(http.Pusher); ok {
		t.Fatal("minimal writer unexpectedly implements Pusher")
	}
	if _, ok := wrapped.(io.ReaderFrom); ok {
		t.Fatal("minimal writer unexpectedly implements ReaderFrom")
	}

	flusher := &flusherResponseWriter{minimalResponseWriter: &minimalResponseWriter{}}
	wrapped, _ = NewStatusResponseWriter(flusher)
	if _, ok := wrapped.(http.Flusher); !ok {
		t.Fatal("partial writer lost Flusher")
	}
	if _, ok := wrapped.(http.Hijacker); ok {
		t.Fatal("partial writer unexpectedly implements Hijacker")
	}
}
