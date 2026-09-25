package services

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// sleepThrottler blocks every Wait for a fixed duration, standing in for a
// token bucket that has run dry.
type sleepThrottler struct{ d time.Duration }

func (s sleepThrottler) Wait(int64) { time.Sleep(s.d) }

// slowClientWriter blocks every Write, standing in for a client whose TCP
// window is full.
type slowClientWriter struct {
	http.ResponseWriter
	d time.Duration
}

func (w slowClientWriter) Write(p []byte) (int, error) {
	time.Sleep(w.d)
	return w.ResponseWriter.Write(p)
}

func TestThrottledResponseWriterAccumulatesWait(t *testing.T) {
	const n = 5
	const d = 20 * time.Millisecond
	rec := httptest.NewRecorder()
	w := NewThrottledRequestWrtier(rec, sleepThrottler{d: d})
	if got := w.Waited(); got != 0 {
		t.Fatalf("fresh writer reports %v waited", got)
	}
	var want bytes.Buffer
	for i := 0; i < n; i++ {
		p := []byte(fmt.Sprintf("chunk-%d;", i))
		got, err := w.Write(p)
		if err != nil || got != len(p) {
			t.Fatalf("Write(%q) = %d, %v", p, got, err)
		}
		want.Write(p)
	}
	if rec.Body.String() != want.String() {
		t.Fatalf("body = %q, want %q", rec.Body.String(), want.String())
	}
	// Sleep never returns early, so n*d is a hard floor; the ceiling only
	// has to tell n waits from 2n.
	if got := w.Waited(); got < n*d || got > n*d+n*d/2 {
		t.Fatalf("Waited() = %v after %d writes blocked %v each, want about %v", got, n, d, n*d)
	}
}

// Time the client takes to accept the bytes is the client's, not the
// tier's: a slow client must not read as a tier-bound request.
func TestThrottledResponseWriterExcludesDownstreamWrite(t *testing.T) {
	const n = 5
	const wait = 10 * time.Millisecond
	const client = 40 * time.Millisecond
	w := NewThrottledRequestWrtier(slowClientWriter{httptest.NewRecorder(), client}, sleepThrottler{d: wait})
	for i := 0; i < n; i++ {
		if _, err := w.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if got := w.Waited(); got < n*wait || got > n*wait+n*client/2 {
		t.Fatalf("Waited() = %v, want about %v (limiter only, not the %v the client took)", got, n*wait, n*client)
	}
}
