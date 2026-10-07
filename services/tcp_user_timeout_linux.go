//go:build linux

package services

import (
	"math"
	"syscall"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

const tcpUserTimeoutSupported = true

func setTCPUserTimeout(rc syscall.RawConn, d time.Duration) error {
	ms := d.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	if ms > math.MaxInt32 {
		ms = math.MaxInt32
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, int(ms))
	}); err != nil {
		return errors.Wrap(err, "failed to access socket to set TCP_USER_TIMEOUT")
	}
	return errors.Wrap(serr, "failed to set TCP_USER_TIMEOUT")
}
