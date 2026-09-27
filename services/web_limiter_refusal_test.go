package services

import (
	"context"
	"net/http"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
)

func rejectedTotal(t *testing.T, reason string) float64 {
	t.Helper()
	var m dto.Metric
	if err := promSessionLimiterRejected.WithLabelValues(reason).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

// refusalHarness is a session whose one slot the viewer's own request
// already holds, so the next external request is refused with "total".
func refusalHarness(t *testing.T, sid string, delay time.Duration) *throttleHarness {
	t.Helper()
	h := newThrottleHarness(t, fixedBody(http.StatusOK, 1024))
	h.web.sl = &SessionLimiter{maxTotal: 1, rejectDelay: delay, sessions: map[string]*sessionState{}}
	release, reason := h.web.sl.Acquire(sid, harnessHash, "/Sintel/Sintel.mkv", "203.0.113.7")
	if release == nil {
		t.Fatalf("the viewer's own request was refused (%s): the case proves nothing", reason)
	}
	t.Cleanup(release)
	return h
}

// A refusal is answered 429 with Retry-After, counted by its cap, and only
// after the limiter's delay: the hold is what slows a client that retries at
// once, whatever it makes of the header.
func TestWebLimiterRefusalIsHeldAndCarriesRetryAfter(t *testing.T) {
	const delay = 200 * time.Millisecond
	h := refusalHarness(t, "s-refused", delay)
	before := rejectedTotal(t, "total")

	start := time.Now()
	rec, _, _ := h.serveRaw(t, h.request(t, tierClaims("t-refused", "s-refused", "2M"), true, ""))
	took := time.Since(start)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("at the session's cap: %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != limiterRetryAfter {
		t.Errorf("Retry-After = %q, want %q", got, limiterRetryAfter)
	}
	if took < delay {
		t.Errorf("answered after %v, want the %v hold first", took, delay)
	}
	if n := rejectedTotal(t, "total") - before; n != 1 {
		t.Errorf("rejected_total{reason=total} grew by %v, want 1", n)
	}
}

// The hold is never the one thing keeping a request alive: a client that
// leaves, or thp shutting down, ends it at once.
func TestWebLimiterRefusalHoldEnds(t *testing.T) {
	const delay = time.Minute
	cases := []struct {
		name string
		end  func(h *throttleHarness, cancel context.CancelFunc)
	}{
		{"client left", func(_ *throttleHarness, cancel context.CancelFunc) { cancel() }},
		{"thp closing", func(h *throttleHarness, _ context.CancelFunc) {
			h.web.closeOnce.Do(func() { close(h.web.closing) })
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := refusalHarness(t, "s-held", delay)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := h.request(t, tierClaims("t-held", "s-held", "2M"), true, "").WithContext(ctx)
			time.AfterFunc(50*time.Millisecond, func() { c.end(h, cancel) })

			start := time.Now()
			rec, _, _ := h.serveRaw(t, r)
			if took := time.Since(start); took > 10*time.Second {
				t.Fatalf("held %v after the hold should have ended", took)
			}
			if rec.Code != http.StatusTooManyRequests {
				t.Errorf("%d, want 429", rec.Code)
			}
		})
	}
}
