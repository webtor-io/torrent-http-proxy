package services

import (
	"net/http/httptest"
	"testing"
	"time"
)

// blockingDownstream stands in for a reader that is slow to take the bytes
// (a full ingress buffer, a client's full TCP window): every Write blocks
// for write, every Flush for flush.
type blockingDownstream struct {
	*httptest.ResponseRecorder
	write, flush time.Duration
}

func (d blockingDownstream) Write(p []byte) (int, error) {
	time.Sleep(d.write)
	return d.ResponseRecorder.Write(p)
}

func (d blockingDownstream) Flush() {
	time.Sleep(d.flush)
	d.ResponseRecorder.Flush()
}

// ReverseProxy flushes after every write (FlushInterval -1), so back-pressure
// shows up in Write and in Flush alike; both count.
func TestResponseWriterInterceptorCountsDownstreamBlocking(t *testing.T) {
	const n = 4
	const write = 10 * time.Millisecond
	const flush = 25 * time.Millisecond
	d := blockingDownstream{httptest.NewRecorder(), write, flush}
	w := NewResponseWrtierInterceptor(d)
	if got := w.Blocked(); got != 0 {
		t.Fatalf("fresh interceptor reports %v blocked", got)
	}
	for i := 0; i < n; i++ {
		if _, err := w.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		w.Flush()
	}
	if d.Body.String() != "xxxx" || !d.Flushed {
		t.Fatalf("body = %q, flushed = %v; bytes or flushes did not pass through", d.Body.String(), d.Flushed)
	}
	// Sleep never returns early, so the sum is a hard floor; the ceiling only
	// has to tell writes+flushes from either alone or from double counting.
	want := n * (write + flush)
	if got := w.Blocked(); got < want || got > want+want/2 {
		t.Fatalf("Blocked() = %v after %d writes of %v and flushes of %v, want about %v", got, n, write, flush, want)
	}
}
