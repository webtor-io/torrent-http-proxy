//go:build linux

package services

import (
	"context"
	"net"
	"net/http"
	"testing"

	"golang.org/x/sys/unix"
)

// Both upstream transports dial with the keepalive and TCP_USER_TIMEOUT
// their flags set; both flags 0 is Go's default dialer (15 s, 15 s, 9, none).
// What they do to a vanished peer: upstream_blackhole_linux_test.go.
func TestUpstreamDialerSockopts(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	type opts struct{ keepAlive, idle, intvl, cnt, userTimeoutMS int }
	read := func(t *testing.T, tr *http.Transport) opts {
		c, err := tr.DialContext(context.Background(), "tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		rc, err := c.(*net.TCPConn).SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		var o opts
		var errs []error
		get := func(fd, level, opt int) int {
			v, err := unix.GetsockoptInt(fd, level, opt)
			if err != nil {
				errs = append(errs, err)
			}
			return v
		}
		if err := rc.Control(func(fd uintptr) {
			f := int(fd)
			o = opts{
				get(f, unix.SOL_SOCKET, unix.SO_KEEPALIVE),
				get(f, unix.IPPROTO_TCP, unix.TCP_KEEPIDLE),
				get(f, unix.IPPROTO_TCP, unix.TCP_KEEPINTVL),
				get(f, unix.IPPROTO_TCP, unix.TCP_KEEPCNT),
				get(f, unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT),
			}
		}); err != nil || len(errs) > 0 {
			t.Fatal(err, errs)
		}
		return o
	}
	for _, c := range []struct {
		name string
		kv   []string
		want opts
	}{
		{"defaults", nil, opts{1, 5, 5, 3, 20000}},
		{"both 0", []string{upstreamKeepAliveFlag, "0s", upstreamTCPUserTimeoutFlag, "0s"}, opts{1, 15, 15, 9, 0}},
	} {
		p := testHTTPProxy(t, c.kv...)
		for name, tr := range map[string]*http.Transport{"transport": p.transport, "externalTransport": p.externalTransport} {
			if got := read(t, tr); got != c.want {
				t.Errorf("%s, %s: got %+v, want %+v", c.name, name, got, c.want)
			}
		}
	}
}
