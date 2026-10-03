package services

import (
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

// Every connection the web port accepts carries the not-yet-sent cap; without it the
// kernel default lets a stalled reader's queue grow to tcp_wmem max.
func TestLowatListenerCapsUnsent(t *testing.T) {
	s := &Web{host: "127.0.0.1", port: 0}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	defer s.ln.Close()
	addr := s.ln.Addr().String()

	go func() {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			defer c.Close()
			buf := make([]byte, 1)
			_, _ = c.Read(buf)
		}
	}()
	c, err := s.ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	rc, err := c.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var got int
	var gerr error
	if err := rc.Control(func(fd uintptr) {
		got, gerr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT)
	}); err != nil || gerr != nil {
		t.Fatal(err, gerr)
	}
	if got != notsentLowat {
		t.Fatalf("TCP_NOTSENT_LOWAT = %d, want %d", got, notsentLowat)
	}
}
