package services

import (
	"sync"

	"github.com/bsm/redislock"
)

type Locker struct {
	value  *redislock.Client
	r      *RedisClient
	inited bool
	mux    sync.Mutex
}

func NewLocker(r *RedisClient) *Locker {
	return &Locker{r: r, inited: false}
}

func (s *Locker) get() *redislock.Client {
	l := redislock.New(s.r.Get())
	return l
}

func (s *Locker) Get() *redislock.Client {
	s.mux.Lock()
	defer s.mux.Unlock()
	if s.inited {
		return s.value
	}
	s.value = s.get()
	s.inited = true
	return s.value
}
