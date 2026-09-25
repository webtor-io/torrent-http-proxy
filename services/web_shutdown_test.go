package services

import (
	"flag"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dgrijalva/jwt-go"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
)

// servedWeb is a Web built by NewWeb, as run() builds it, around the
// throttle harness's proxy to an upstream the test supplies, listening on a
// free local port and served.
type servedWeb struct {
	web    *Web
	addr   string
	secret string
	served chan error // Serve's result
}

// newNewWeb builds a Web with NewWeb from the web and shutdown flags, on
// 127.0.0.1 and a free port, proxying to upstream. Not listening yet.
func newNewWeb(t *testing.T, upstream http.HandlerFunc, shutdownTimeout time.Duration) (*Web, string) {
	t.Helper()
	h := newThrottleHarness(t, upstream)
	set := flag.NewFlagSet("thp-test", flag.ContinueOnError)
	for _, f := range cs.RegisterShutdownFlags(RegisterWebFlags(nil)) {
		f.Apply(set)
	}
	for k, v := range map[string]string{"host": "127.0.0.1", "port": "0", "shutdown-timeout": shutdownTimeout.String()} {
		if err := set.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	web := NewWeb(cli.NewContext(cli.NewApp(), set, nil), h.web.parser, h.web.r, h.web.pr, h.web.claims, h.web.bucket, nil, nil, nil)
	return web, h.secret
}

// newServedWeb is newNewWeb listening and served. Close is the test's; a
// cleanup closes it again (Close is idempotent) and checks what Serve
// returned.
func newServedWeb(t *testing.T, upstream http.HandlerFunc, shutdownTimeout time.Duration) *servedWeb {
	t.Helper()
	web, secret := newNewWeb(t, upstream, shutdownTimeout)
	if err := web.Listen(); err != nil {
		t.Fatal(err)
	}
	s := &servedWeb{web: web, addr: web.ln.Addr().String(), secret: secret, served: make(chan error, 1)}
	go func() { s.served <- web.Serve() }()
	t.Cleanup(func() {
		web.Close()
		select {
		case err := <-s.served:
			if err != nil {
				t.Errorf("Serve returned %v after Close, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("Serve still running 5 s after Close")
		}
	})
	return s
}

// startRequest GETs the harness torrent from s with claims and query (""
// or "key=value&"), as the ingress would (X-Forwarded-For), and checks for
// a 200.
func (s *servedWeb) startRequest(t *testing.T, claims jwt.MapClaims, query string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+s.addr+"/"+harnessHash+"/Sintel/Sintel.mkv?"+query+"token="+signToken(t, s.secret, claims)+"&api-key=k", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	return resp
}

// startDownload is startRequest for content, reading the first n bytes.
func (s *servedWeb) startDownload(t *testing.T, claims jwt.MapClaims, n int) *http.Response {
	t.Helper()
	resp := s.startRequest(t, claims, "")
	if _, err := io.ReadFull(resp.Body, make([]byte, n)); err != nil {
		t.Fatalf("first %d bytes: %v", n, err)
	}
	return resp
}

// seederEvents answers as the seeder does ?stats and ?warmup: an event
// stream, one event now and one every 50 ms, until the client goes or, when
// end is closed, with a last event.
func seederEvents(end <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for {
			_, _ = w.Write([]byte("data: {}\n\n"))
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-end:
				_, _ = w.Write([]byte("data: {\"done\":true}\n\n"))
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
}

// firstEvent reads the stream up to its first event's blank line.
func firstEvent(t *testing.T, body io.Reader) {
	t.Helper()
	if _, err := io.ReadFull(body, make([]byte, len("data: {}\n\n"))); err != nil {
		t.Fatalf("first event: %v", err)
	}
}

// inFlight is webtor_http_proxy_request_current for role: proxyHTTP's
// deferred closure takes it down after the ClickHouse Add, so at 0 that
// work is done.
func inFlight(t *testing.T, role string) float64 {
	t.Helper()
	return gaugeValue(t, promHTTPProxyRequestCurrent.WithLabelValues(string(External), role, throttleTestSvc))
}

// closeAsync runs Close on another goroutine; the channel closes when it
// returns.
func closeAsync(web *Web) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		web.Close()
		close(done)
	}()
	return done
}

func refused(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return true
	}
	_ = c.Close()
	return false
}

// SIGTERM mid-download: the listener closes, the download runs to its end,
// and Close returns only once proxyHTTP is done with it, so the deferred
// ClickHouse Add and metrics run before run() closes their clients.
func TestWebCloseDrainsInFlightDownload(t *testing.T) {
	const role, session = "t-drain", "s-drain"
	release := make(chan struct{})
	s := newServedWeb(t, heldBody(64<<10, release), 30*time.Second)
	releaseBody := releaser(t, release)
	resp := s.startDownload(t, tierClaims(role, session, "2M"), 64<<10)
	if n := inFlight(t, role); n != 1 {
		t.Fatalf("%v requests in flight, want 1", n)
	}
	closed := closeAsync(s.web)
	eventually(t, "the listener to refuse new connections", func() bool { return refused(s.addr) })
	select {
	case <-closed:
		t.Fatal("Close returned while a download was in flight")
	case <-time.After(200 * time.Millisecond):
	}
	releaseBody()
	if rest, err := io.ReadAll(resp.Body); err != nil || len(rest) != 0 {
		t.Errorf("end of the download: %d more bytes, error %v; want a clean end", len(rest), err)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close still running 5 s after the download ended")
	}
	if n := inFlight(t, role); n != 0 {
		t.Errorf("%v requests in flight when Close returned, want 0: the handler's deferred work had not run", n)
	}
	if e := s.web.stats.lookup(statsKey(session, harnessHash)); e == nil || e.conns.Load() != 0 {
		t.Errorf("session stats entry %v when Close returned, want one with 0 conns", e)
	}
}

// A download longer than the shutdown timeout is cut at it (the flag
// reaches the drain), and Close still waits for its handler to unwind.
func TestWebCloseCutsAtShutdownTimeout(t *testing.T) {
	const role = "t-cut"
	const timeout = 300 * time.Millisecond
	release := make(chan struct{})
	s := newServedWeb(t, heldBody(64<<10, release), timeout)
	releaser(t, release)
	resp := s.startDownload(t, tierClaims(role, "s-cut", "2M"), 64<<10)
	start := time.Now()
	closed := closeAsync(s.web)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close still running 5 s past a 300 ms shutdown timeout")
	}
	if took := time.Since(start); took < timeout {
		t.Errorf("Close returned after %v, before the %v shutdown timeout", took, timeout)
	}
	if n := inFlight(t, role); n != 0 {
		t.Errorf("%v requests in flight when Close returned, want 0", n)
	}
	read := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(resp.Body)
		read <- err
	}()
	select {
	case err := <-read:
		if err == nil {
			t.Errorf("the download ended cleanly, want it cut")
		}
	case <-time.After(5 * time.Second):
		// Unblocks the reader: the upstream holds the response open.
		_ = resp.Body.Close()
		t.Errorf("the download still open 5 s after Close, want it cut")
	}
}

// Session-stats streams never end on their own: Close ends them before the
// drain, or each would hold it for the whole shutdown timeout.
func TestWebCloseEndsSessionStatsStreamsFirst(t *testing.T) {
	s := newServedWeb(t, fixedBody(http.StatusOK, 1), 30*time.Second)
	st := openStats(t, statsURL("http://"+s.addr, harnessHash, statsToken(t, s.secret, "s1")))
	if st.resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", st.resp.StatusCode)
	}
	st.next(t)
	closed := closeAsync(s.web)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close still running 5 s in with a session-stats stream open")
	}
	st.expectEnd(t)
}

// web-ui's status page holds the seeder's ?stats stream through thp for as
// long as it is open, and the seeder never ends it: drained, it would hold
// every rollout for the whole timeout and then be cut all the same. Close
// ends it as it begins.
func TestWebCloseEndsSeederStatsStreams(t *testing.T) {
	const role = "t-close-stats"
	s := newServedWeb(t, seederEvents(nil), 10*time.Second)
	resp := s.startRequest(t, tierClaims(role, "s-close-stats", "2M"), "stats=true&")
	firstEvent(t, resp.Body)
	closed := closeAsync(s.web)
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close still running 3 s in with a ?stats stream open: drained, not ended")
	}
	if n := inFlight(t, role); n != 0 {
		t.Errorf("%v requests in flight when Close returned, want 0", n)
	}
	read := make(chan struct{})
	go func() {
		_, _ = io.ReadAll(resp.Body)
		close(read)
	}()
	select {
	case <-read:
	case <-time.After(5 * time.Second):
		_ = resp.Body.Close()
		t.Error("the ?stats stream still open 5 s after Close")
	}
}

// ?warmup is an event stream too, but web-ui reads its end as "warmup
// complete": it is drained like a download, never cut at Close's start.
func TestWebCloseDrainsWarmupStream(t *testing.T) {
	const role = "t-close-warmup"
	end := make(chan struct{})
	s := newServedWeb(t, seederEvents(end), 10*time.Second)
	endStream := releaser(t, end)
	resp := s.startRequest(t, tierClaims(role, "s-close-warmup", "2M"), "warmup=true&")
	firstEvent(t, resp.Body)
	closed := closeAsync(s.web)
	select {
	case <-closed:
		t.Fatal("Close returned while a warmup stream was open")
	case <-time.After(300 * time.Millisecond):
	}
	endStream()
	rest, err := io.ReadAll(resp.Body)
	if err != nil || !strings.Contains(string(rest), `"done":true`) {
		t.Errorf("warmup stream ended with %q, error %v; want its last event and a clean end", rest, err)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close still running 5 s after the warmup stream ended")
	}
}

// run() may close the Web before its Serve goroutine got going (another
// servable failed first): Serve then serves nothing and returns nil.
func TestWebCloseBeforeServe(t *testing.T) {
	for _, listened := range []bool{true, false} {
		name := "not listening"
		if listened {
			name = "listening"
		}
		t.Run(name, func(t *testing.T) {
			web, _ := newNewWeb(t, fixedBody(http.StatusOK, 1), 30*time.Second)
			if listened {
				if err := web.Listen(); err != nil {
					t.Fatal(err)
				}
			}
			web.Close()
			served := make(chan error, 1)
			go func() { served <- web.Serve() }()
			select {
			case err := <-served:
				if err != nil {
					t.Errorf("Serve after Close returned %v, want nil", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Serve after Close still serving after 5 s")
			}
			if web.ln != nil && !refused(web.ln.Addr().String()) {
				t.Errorf("the port still takes connections after Serve returned")
			}
		})
	}
}

// Close may run twice (the explicit call in run() and a signal path) and
// concurrently: every call waits for the drain, none panics.
func TestWebCloseTwice(t *testing.T) {
	const role = "t-close-twice"
	release := make(chan struct{})
	s := newServedWeb(t, heldBody(1000, release), 30*time.Second)
	releaseBody := releaser(t, release)
	s.startDownload(t, tierClaims(role, "s-close-twice", "2M"), 1000)
	first, second := closeAsync(s.web), closeAsync(s.web)
	select {
	case <-first:
		t.Fatal("a Close returned while a download was in flight")
	case <-second:
		t.Fatal("a Close returned while a download was in flight")
	case <-time.After(200 * time.Millisecond):
	}
	releaseBody()
	for _, c := range []<-chan struct{}{first, second} {
		select {
		case <-c:
		case <-time.After(5 * time.Second):
			t.Fatal("Close still running 5 s after the download ended")
		}
	}
	if n := inFlight(t, role); n != 0 {
		t.Errorf("%v requests in flight after both Close calls, want 0", n)
	}
	third := closeAsync(s.web)
	select {
	case <-third:
	case <-time.After(time.Second):
		t.Fatal("Close after Close did not return at once")
	}
}

// The probe answers Ready as soon as it listens, and a DaemonSet with
// maxSurge 1 retires the old pod on it. Listen binds the web port before
// Serve, so run() can bind it before any servable starts: a connection made
// then waits in the backlog and is served once Serve runs.
func TestWebListenBindsBeforeServe(t *testing.T) {
	web, _ := newNewWeb(t, fixedBody(http.StatusOK, 1), 30*time.Second)
	if err := web.Listen(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(web.Close)
	got := make(chan int, 1)
	go func() {
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + web.ln.Addr().String() + "/")
		if err != nil {
			t.Error(err)
			got <- 0
			return
		}
		_ = resp.Body.Close()
		got <- resp.StatusCode
	}()
	time.Sleep(50 * time.Millisecond)
	served := make(chan error, 1)
	go func() { served <- web.Serve() }()
	if status := <-got; status != http.StatusOK {
		t.Errorf("request made between Listen and Serve got %d, want 200", status)
	}
	web.Close()
	if err := <-served; err != nil {
		t.Errorf("Serve returned %v after Close, want nil", err)
	}
}

// A taken port fails Listen, not a goroutine later.
func TestWebListenPortTaken(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = busy.Close() })
	web, _ := newNewWeb(t, fixedBody(http.StatusOK, 1), time.Second)
	t.Cleanup(web.Close)
	web.port = busy.Addr().(*net.TCPAddr).Port
	if err := web.Listen(); err == nil {
		t.Errorf("Listen on a taken port succeeded")
	}
}
