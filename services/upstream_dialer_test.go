package services

import (
	"bytes"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"

	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/urfave/cli"
)

// testHTTPProxy is NewHTTPProxy on its flags' defaults, kv overriding them.
func testHTTPProxy(t *testing.T, kv ...string) *HTTPProxy {
	t.Helper()
	set := flag.NewFlagSet("thp-test", flag.ContinueOnError)
	for _, f := range RegisterHTTPProxyFlags(nil) {
		f.Apply(set)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if err := set.Set(kv[i], kv[i+1]); err != nil {
			t.Fatal(err)
		}
	}
	return NewHTTPProxy(cli.NewContext(cli.NewApp(), set, nil), nil, 0, nil)
}

// A custom dialer turns net/http's automatic HTTP/2 off; redirect targets
// over TLS keep it as they had it with the default dialer.
func TestExternalTransportKeepsHTTP2(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	tr := testHTTPProxy(t).externalTransport.Clone()
	tr.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("external transport spoke %s to an HTTP/2 server, want HTTP/2", resp.Proto)
	}
}

// slowFailReader returns its data, then after delay a connection error.
type slowFailReader struct {
	data  *bytes.Reader
	delay time.Duration
}

func (r *slowFailReader) Read(p []byte) (int, error) {
	if r.data.Len() > 0 {
		return r.data.Read(p)
	}
	time.Sleep(r.delay)
	return 0, syscall.ECONNRESET
}

func (r *slowFailReader) Close() error { return nil }

// The retry's line says how long the upstream had been silent: from the
// last read that returned bytes, not from when the response arrived.
func TestRetryLogIdleSinceLastRead(t *testing.T) {
	hook := logtest.NewGlobal()
	defer hook.Reset()
	r := newTestRRC(&slowFailReader{data: bytes.NewReader(make([]byte, 10)), delay: 300 * time.Millisecond}, -1,
		func(int64) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(nil)), nil })
	r.lastRead = time.Now().Add(-time.Hour) // the response arrived long ago
	if _, err := io.ReadAll(r); err != nil {
		t.Fatal(err)
	}
	for _, e := range hook.AllEntries() {
		if e.Message != "upstream connection lost, retrying" {
			continue
		}
		idle, ok := e.Data["idle"].(float64)
		if !ok || idle < 0.3 || idle > 2 {
			t.Fatalf("idle = %v, want ~0.3 s (since the last read with bytes)", e.Data["idle"])
		}
		return
	}
	t.Fatal("no retry line")
}
