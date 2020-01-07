package services

import (
	"sync"
	"time"
)

type AccessLock struct {
	C      chan error
	closed bool
	mux    sync.Mutex
	timer  *time.Timer
	d      time.Duration
}

func NewAccessLock(d time.Duration) *AccessLock {
	timer := time.NewTimer(d)
	al := &AccessLock{C: make(chan error), timer: timer, d: d}
	go func() {
		<-timer.C
		al.Unlock()
	}()
	return al
}
func (al *AccessLock) Reset() bool {
	return al.timer.Reset(al.d)
}
func (al *AccessLock) Unlocked() chan error {
	return al.C
}
func (al *AccessLock) Unlock() {
	al.mux.Lock()
	defer al.mux.Unlock()
	if !al.closed {
		close(al.C)
		al.closed = true
	}
}
