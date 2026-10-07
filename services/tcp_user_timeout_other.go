//go:build !linux

package services

import (
	"syscall"
	"time"
)

const tcpUserTimeoutSupported = false

func setTCPUserTimeout(syscall.RawConn, time.Duration) error {
	return nil
}
