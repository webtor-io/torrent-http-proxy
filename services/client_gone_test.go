package services

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// closingMessages are the messages of proxyHTTP's one closing log line.
var closingMessages = map[string]bool{
	"request served successfully": true,
	"bad request":                 true,
	"failed to serve request":     true,
	"client closed request":       true,
}

// statusSnap is webtor_http_proxy_request_total and the request duration
// count for one role, by status class. The vectors are process-global and
// survive -count=N: cases compare snapshots, never absolute values.
type statusSnap map[string]float64

func snapStatus(t *testing.T, role string) statusSnap {
	t.Helper()
	s := statusSnap{}
	for _, class := range []string{"200", "400", "500"} {
		s["total/"+class] = counterValue(t, promHTTPProxyRequestTotal.WithLabelValues(string(External), role, throttleTestSvc, class))
		s["duration/"+class] = float64(histogramCount(t, promHTTPProxyRequestDuration.WithLabelValues(string(External), role, throttleTestSvc, class)))
	}
	return s
}

func histogramCount(t *testing.T, o prometheus.Observer) uint64 {
	t.Helper()
	m, ok := o.(prometheus.Metric)
	if !ok {
		t.Fatalf("%T is not a metric", o)
	}
	d := &dto.Metric{}
	if err := m.Write(d); err != nil {
		t.Fatal(err)
	}
	return d.GetHistogram().GetSampleCount()
}

// expectOnly checks that exactly one request was recorded, in class, in both
// the counter and the duration histogram.
func expectOnly(t *testing.T, before, after statusSnap, class string) {
	t.Helper()
	for k, v := range after {
		want := 0.0
		if strings.HasSuffix(k, "/"+class) {
			want = 1
		}
		if got := v - before[k]; got != want {
			t.Errorf("%s{status=%q} went up by %v, want %v", strings.Split(k, "/")[0], strings.Split(k, "/")[1], got, want)
		}
	}
}

// serveEntryAsync runs r through proxyHTTP on another goroutine and delivers
// its closing log entry (nil when there is none) once it returns.
func (h *throttleHarness) serveEntryAsync(t *testing.T, w http.ResponseWriter, r *http.Request) <-chan *logrus.Entry {
	t.Helper()
	src, err := h.web.parser.Parse(r.URL)
	if err != nil {
		t.Fatal(err)
	}
	r.URL.Path = src.Path
	done := make(chan *logrus.Entry, 1)
	go func() {
		logger, hook := logtest.NewNullLogger()
		h.web.proxyHTTP(w, r, src, logrus.NewEntry(logger))
		for _, e := range hook.AllEntries() {
			if closingMessages[e.Message] {
				done <- e
				return
			}
		}
		done <- nil
	}()
	return done
}

func recvEntry(t *testing.T, ch <-chan *logrus.Entry) *logrus.Entry {
	t.Helper()
	select {
	case e := <-ch:
		if e == nil {
			t.Fatal("proxyHTTP returned without its closing log line")
		}
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("proxyHTTP still running after 5 s")
		return nil
	}
}

func recvSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// expectLine checks the closing line's level, message, status field and
// error field ("" = no error field at all).
func expectLine(t *testing.T, e *logrus.Entry, level logrus.Level, msg, status, errText string) {
	t.Helper()
	if e.Level != level || e.Message != msg {
		t.Errorf("closing line %s %q, want %s %q", e.Level, e.Message, level, msg)
	}
	if got := e.Data["status"]; got != status {
		t.Errorf("status field %v, want %s", got, status)
	}
	got, has := e.Data[logrus.ErrorKey]
	switch {
	case errText == "" && has:
		t.Errorf("error field %v, want none", got)
	case errText != "" && !has:
		t.Errorf("no error field, want one containing %q", errText)
	case errText != "" && !strings.Contains(fmt.Sprint(got), errText):
		t.Errorf("error field %v, want it to contain %q", got, errText)
	}
}

// arrivedThenHold is an upstream that signals each request's arrival and
// sends no headers until the request is canceled: a seeder still fetching
// the torrent's metadata, a transcoder not yet at the first segment.
func arrivedThenHold(arrived chan<- struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-r.Context().Done()
	}
}

// A client that leaves while the upstream has not answered yet is the
// client's doing: recorded 499 (class "400"), logged below Warn with the
// cancellation, never counted as a 5xx.
func TestProxyClientGoneBeforeHeadersIs499(t *testing.T) {
	const role = "t-gone-harness"
	arrived := make(chan struct{}, 1)
	h := newThrottleHarness(t, arrivedThenHold(arrived))
	before := snapStatus(t, role)

	ctx, leave := context.WithCancel(context.Background())
	defer leave()
	rec := httptest.NewRecorder()
	done := h.serveEntryAsync(t, rec, h.request(t, tierClaims(role, "s-gone", "2M"), true, "").WithContext(ctx))
	recvSignal(t, arrived, "the request to reach the upstream")
	leave()
	e := recvEntry(t, done)

	// The wire is unchanged: 499 is only what thp records.
	if rec.Code != http.StatusBadGateway {
		t.Errorf("wrote %d, want the 502 it always wrote", rec.Code)
	}
	expectLine(t, e, logrus.InfoLevel, "client closed request", "499", context.Canceled.Error())
	expectOnly(t, before, snapStatus(t, role), "400")
}

// A client that half-closes (FIN on its write side) and keeps reading has a
// done context in net/http, exactly like one that left, yet it still reads
// the answer: it must get the 502 it always got, while thp records 499.
func TestWebClientHalfClosedStillGets502(t *testing.T) {
	const role = "t-half-closed"
	hook := logtest.NewGlobal()
	t.Cleanup(func() { logrus.StandardLogger().ReplaceHooks(make(logrus.LevelHooks)) })
	arrived := make(chan struct{}, 1)
	s := newServedWeb(t, arrivedThenHold(arrived), 30*time.Second)
	before := snapStatus(t, role)

	c, err := net.Dial("tcp", s.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	path := "/" + harnessHash + "/Sintel/Sintel.mkv?token=" + signToken(t, s.secret, tierClaims(role, "s-half-closed", "2M")) + "&api-key=k"
	if _, err := fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: thp\r\nX-Forwarded-For: 203.0.113.7\r\n\r\n", path); err != nil {
		t.Fatal(err)
	}
	recvSignal(t, arrived, "the request to reach the upstream")
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("half-closed client read no response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("half-closed client received %d, want the 502 it always got", resp.StatusCode)
	}

	var e *logrus.Entry
	eventually(t, "the closing log line", func() bool {
		for _, x := range hook.AllEntries() {
			if closingMessages[x.Message] && x.Data["role"] == role {
				e = x
				return true
			}
		}
		return false
	})
	expectLine(t, e, logrus.InfoLevel, "client closed request", "499", context.Canceled.Error())
	expectOnly(t, before, snapStatus(t, role), "400")
}

// The same over a real server and a real disconnect, which is how it
// happens in prod: the client's connection closes, net/http cancels the
// request's context, the transport returns context.Canceled.
func TestWebClientDisconnectBeforeHeadersIs499(t *testing.T) {
	const role = "t-gone-served"
	hook := logtest.NewGlobal()
	t.Cleanup(func() { logrus.StandardLogger().ReplaceHooks(make(logrus.LevelHooks)) })
	arrived := make(chan struct{}, 1)
	s := newServedWeb(t, arrivedThenHold(arrived), 30*time.Second)
	before := snapStatus(t, role)

	ctx, leave := context.WithCancel(context.Background())
	defer leave()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.addr+"/"+harnessHash+"/Sintel/Sintel.mkv?token="+signToken(t, s.secret, tierClaims(role, "s-gone-served", "2M"))+"&api-key=k", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	got := make(chan error, 1)
	go func() {
		resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
		if err == nil {
			_ = resp.Body.Close()
			err = fmt.Errorf("got a %d response, want none", resp.StatusCode)
		}
		got <- err
	}()
	recvSignal(t, arrived, "the request to reach the upstream")
	leave()
	if err := <-got; !errors.Is(err, context.Canceled) {
		t.Fatalf("client side: %v, want its own cancellation", err)
	}

	var e *logrus.Entry
	eventually(t, "the closing log line", func() bool {
		for _, x := range hook.AllEntries() {
			if closingMessages[x.Message] && x.Data["role"] == role {
				e = x
				return true
			}
		}
		return false
	})
	expectLine(t, e, logrus.InfoLevel, "client closed request", "499", context.Canceled.Error())
	expectOnly(t, before, snapStatus(t, role), "400")
}

// closedPort is a local port nothing listens on: a dial is refused.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close()
	return port
}

// An upstream that refuses the connection is a real failure: the connected
// client gets the 502 it always got, recorded and logged as one, with the
// cause in the closing line.
func TestProxyUpstreamRefusedIs502(t *testing.T) {
	const role = "t-refused"
	h := newThrottleHarness(t, fixedBody(http.StatusOK, 1))
	// The location is resolved on the first request: point it at nothing.
	t.Setenv("THP_THROTTLE_TEST_SERVICE_PORT", closedPort(t))
	before := snapStatus(t, role)

	rec := httptest.NewRecorder()
	e := recvEntry(t, h.serveEntryAsync(t, rec, h.request(t, tierClaims(role, "s-refused", "2M"), true, "")))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("wrote %d, want 502", rec.Code)
	}
	expectLine(t, e, logrus.ErrorLevel, "failed to serve request", "502", "connection refused")
	expectOnly(t, before, snapStatus(t, role), "500")
}

// thp cancels the seeder's ?stats request itself when it closes, with
// web-ui still connected: that is a 502 web-ui receives, not a client that
// left. Pins that errorHandler looks at the client's context, not at the
// upstream request's, which proxyHTTP derives.
func TestProxyStatsCanceledByCloseIs502(t *testing.T) {
	const role = "t-stats-close"
	arrived := make(chan struct{}, 1)
	h := newThrottleHarness(t, arrivedThenHold(arrived))
	before := snapStatus(t, role)

	rec := httptest.NewRecorder()
	done := h.serveEntryAsync(t, rec, h.request(t, tierClaims(role, "s-stats-close", "2M"), true, "&stats=true"))
	recvSignal(t, arrived, "the ?stats request to reach the upstream")
	close(h.web.closing)
	e := recvEntry(t, done)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("wrote %d, want 502", rec.Code)
	}
	expectLine(t, e, logrus.ErrorLevel, "failed to serve request", "502", context.Canceled.Error())
	expectOnly(t, before, snapStatus(t, role), "500")
}

// An upstream's own 5xx is passed through as it is, recorded and logged as
// a failure, with no proxy error attached.
func TestProxyUpstream5xxPassesThrough(t *testing.T) {
	const role = "t-upstream-503"
	h := newThrottleHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "busy")
	})
	before := snapStatus(t, role)

	rec := httptest.NewRecorder()
	e := recvEntry(t, h.serveEntryAsync(t, rec, h.request(t, tierClaims(role, "s-503", "2M"), true, "")))

	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "busy" {
		t.Errorf("wrote %d %q, want the upstream's 503 \"busy\"", rec.Code, rec.Body.String())
	}
	expectLine(t, e, logrus.ErrorLevel, "failed to serve request", "503", "")
	expectOnly(t, before, snapStatus(t, role), "500")
}

// An upstream that answered 5xx first stays a 5xx when the client leaves
// afterwards, mid-body: 499 is for a client that left before any answer,
// decided where the proxy gives up, not by whether the client is still
// there when the request ends.
func TestWebUpstream5xxThenClientLeavesStays5xx(t *testing.T) {
	const role = "t-503-then-gone"
	hook := logtest.NewGlobal()
	t.Cleanup(func() { logrus.StandardLogger().ReplaceHooks(make(logrus.LevelHooks)) })
	s := newServedWeb(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "busy")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}, 30*time.Second)
	before := snapStatus(t, role)

	req, err := http.NewRequest(http.MethodGet, "http://"+s.addr+"/"+harnessHash+"/Sintel/Sintel.mkv?token="+signToken(t, s.secret, tierClaims(role, "s-503-then-gone", "2M"))+"&api-key=k", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(resp.Body, head); err != nil || resp.StatusCode != http.StatusServiceUnavailable || string(head) != "busy" {
		t.Fatalf("got %d %q (%v), want the upstream's 503 \"busy\"", resp.StatusCode, head, err)
	}
	// Leave mid-body: closing an unread body closes the connection.
	_ = resp.Body.Close()

	var e *logrus.Entry
	eventually(t, "the closing log line", func() bool {
		for _, x := range hook.AllEntries() {
			if closingMessages[x.Message] && x.Data["role"] == role {
				e = x
				return true
			}
		}
		return false
	})
	expectLine(t, e, logrus.ErrorLevel, "failed to serve request", "503", "")
	expectOnly(t, before, snapStatus(t, role), "500")
}

// realDialCanceled is what a dial canceled by its context returns: a
// *net.OpError around net's own "operation was canceled".
func realDialCanceled(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (&net.Dialer{}).DialContext(ctx, "tcp", "127.0.0.1:"+closedPort(t))
	if err == nil {
		t.Fatal("a dial with a canceled context succeeded")
	}
	return err
}

func TestClientGone(t *testing.T) {
	gone, leave := context.WithCancel(context.Background())
	leave()
	here := context.Background()
	cases := []struct {
		name   string
		client context.Context
		err    error
		want   bool
	}{
		{"client left, transport canceled", gone, context.Canceled, true},
		{"client left, manifest read canceled", gone, errors.Wrap(context.Canceled, "failed to read manifest body"), true},
		{"client left, dial canceled", gone, realDialCanceled(t), true},
		// The upstream failed first; the client left before thp answered.
		{"client left, upstream refused first", gone, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, false},
		{"client left, upstream EOF first", gone, io.EOF, false},
		// thp canceled the upstream request itself (?stats on Close), or an
		// upstream deadline ran out: the client is still there.
		{"client here, thp canceled", here, context.Canceled, false},
		{"client here, upstream deadline", here, context.DeadlineExceeded, false},
		{"client here, upstream refused", here, syscall.ECONNREFUSED, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clientGone(c.client, c.err); got != c.want {
				t.Errorf("clientGone(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
