package api

import (
	"bufio"
	"io"
	"net"
	"net/http"
)

// StatusResponseWriter records response status and size. Optional interfaces
// are added by NewStatusResponseWriter only when the wrapped writer supports
// them, preserving transparent type-assertion behavior.
type StatusResponseWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

// NewStatusResponseWriter returns a capability-preserving writer and its
// status observer.
func NewStatusResponseWriter(w http.ResponseWriter) (http.ResponseWriter, *StatusResponseWriter) {
	observer := &StatusResponseWriter{ResponseWriter: w, status: http.StatusOK}
	mask := 0
	if _, ok := w.(http.Flusher); ok {
		mask |= 1
	}
	if _, ok := w.(http.Hijacker); ok {
		mask |= 2
	}
	if _, ok := w.(http.Pusher); ok {
		mask |= 4
	}
	if _, ok := w.(io.ReaderFrom); ok {
		mask |= 8
	}
	switch mask {
	case 0:
		return observer, observer
	case 1:
		return &responseWriterF{observer}, observer
	case 2:
		return &responseWriterH{observer}, observer
	case 3:
		return &responseWriterFH{observer}, observer
	case 4:
		return &responseWriterP{observer}, observer
	case 5:
		return &responseWriterFP{observer}, observer
	case 6:
		return &responseWriterHP{observer}, observer
	case 7:
		return &responseWriterFHP{observer}, observer
	case 8:
		return &responseWriterR{observer}, observer
	case 9:
		return &responseWriterFR{observer}, observer
	case 10:
		return &responseWriterHR{observer}, observer
	case 11:
		return &responseWriterFHR{observer}, observer
	case 12:
		return &responseWriterPR{observer}, observer
	case 13:
		return &responseWriterFPR{observer}, observer
	case 14:
		return &responseWriterHPR{observer}, observer
	default:
		return &responseWriterFHPR{observer}, observer
	}
}

// Status returns the first response status written.
func (w *StatusResponseWriter) Status() int {
	return w.status
}

// BytesWritten returns the response body size observed by the wrapper.
func (w *StatusResponseWriter) BytesWritten() int64 {
	return w.bytes
}

func (w *StatusResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *StatusResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (w *StatusResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *StatusResponseWriter) flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *StatusResponseWriter) hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hijacker.Hijack()
}

func (w *StatusResponseWriter) push(target string, opts *http.PushOptions) error {
	pusher, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, opts)
}

func (w *StatusResponseWriter) readFrom(r io.Reader) (int64, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	readerFrom, ok := w.ResponseWriter.(io.ReaderFrom)
	if !ok {
		return io.Copy(w.ResponseWriter, r)
	}
	n, err := readerFrom.ReadFrom(r)
	w.bytes += n
	return n, err
}

type responseWriterF struct{ *StatusResponseWriter }

func (w *responseWriterF) Flush() { w.flush() }

type responseWriterH struct{ *StatusResponseWriter }

func (w *responseWriterH) Hijack() (net.Conn, *bufio.ReadWriter, error) { return w.hijack() }

type responseWriterFH struct{ *StatusResponseWriter }

func (w *responseWriterFH) Flush()                                       { w.flush() }
func (w *responseWriterFH) Hijack() (net.Conn, *bufio.ReadWriter, error) { return w.hijack() }

type responseWriterP struct{ *StatusResponseWriter }

func (w *responseWriterP) Push(target string, opts *http.PushOptions) error {
	return w.push(target, opts)
}

type responseWriterFP struct{ *StatusResponseWriter }

func (w *responseWriterFP) Flush() { w.flush() }
func (w *responseWriterFP) Push(target string, opts *http.PushOptions) error {
	return w.push(target, opts)
}

type responseWriterHP struct{ *StatusResponseWriter }

func (w *responseWriterHP) Hijack() (net.Conn, *bufio.ReadWriter, error) { return w.hijack() }
func (w *responseWriterHP) Push(target string, opts *http.PushOptions) error {
	return w.push(target, opts)
}

type responseWriterFHP struct{ *StatusResponseWriter }

func (w *responseWriterFHP) Flush()                                       { w.flush() }
func (w *responseWriterFHP) Hijack() (net.Conn, *bufio.ReadWriter, error) { return w.hijack() }
func (w *responseWriterFHP) Push(target string, opts *http.PushOptions) error {
	return w.push(target, opts)
}

type responseWriterR struct{ *StatusResponseWriter }

func (w *responseWriterR) ReadFrom(r io.Reader) (int64, error) { return w.readFrom(r) }

type responseWriterFR struct{ *StatusResponseWriter }

func (w *responseWriterFR) Flush()                              { w.flush() }
func (w *responseWriterFR) ReadFrom(r io.Reader) (int64, error) { return w.readFrom(r) }

type responseWriterHR struct{ *StatusResponseWriter }

func (w *responseWriterHR) Hijack() (net.Conn, *bufio.ReadWriter, error) { return w.hijack() }
func (w *responseWriterHR) ReadFrom(r io.Reader) (int64, error)          { return w.readFrom(r) }

type responseWriterFHR struct{ *StatusResponseWriter }

func (w *responseWriterFHR) Flush()                                       { w.flush() }
func (w *responseWriterFHR) Hijack() (net.Conn, *bufio.ReadWriter, error) { return w.hijack() }
func (w *responseWriterFHR) ReadFrom(r io.Reader) (int64, error)          { return w.readFrom(r) }

type responseWriterPR struct{ *StatusResponseWriter }

func (w *responseWriterPR) Push(target string, opts *http.PushOptions) error {
	return w.push(target, opts)
}
func (w *responseWriterPR) ReadFrom(r io.Reader) (int64, error) { return w.readFrom(r) }

type responseWriterFPR struct{ *StatusResponseWriter }

func (w *responseWriterFPR) Flush() { w.flush() }
func (w *responseWriterFPR) Push(target string, opts *http.PushOptions) error {
	return w.push(target, opts)
}
func (w *responseWriterFPR) ReadFrom(r io.Reader) (int64, error) { return w.readFrom(r) }

type responseWriterHPR struct{ *StatusResponseWriter }

func (w *responseWriterHPR) Hijack() (net.Conn, *bufio.ReadWriter, error) { return w.hijack() }
func (w *responseWriterHPR) Push(target string, opts *http.PushOptions) error {
	return w.push(target, opts)
}
func (w *responseWriterHPR) ReadFrom(r io.Reader) (int64, error) { return w.readFrom(r) }

type responseWriterFHPR struct{ *StatusResponseWriter }

func (w *responseWriterFHPR) Flush()                                       { w.flush() }
func (w *responseWriterFHPR) Hijack() (net.Conn, *bufio.ReadWriter, error) { return w.hijack() }
func (w *responseWriterFHPR) Push(target string, opts *http.PushOptions) error {
	return w.push(target, opts)
}
func (w *responseWriterFHPR) ReadFrom(r io.Reader) (int64, error) { return w.readFrom(r) }

// statusWriter is retained as an internal alias for existing middleware tests.
type statusWriter = StatusResponseWriter
