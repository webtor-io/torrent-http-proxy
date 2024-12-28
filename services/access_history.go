package services

import (
	"crypto/sha1"
	"fmt"
	"sync"
	"time"
)

const (
	accessHistoryLimit  = 5
	accessHistoryExpire = 3 * time.Hour
)

type AccessHistory struct {
	mux    sync.Mutex
	m      map[string][]string
	limit  int
	expire time.Duration
}

func NewAccessHistory() *AccessHistory {
	return &AccessHistory{
		m:      map[string][]string{},
		limit:  accessHistoryLimit,
		expire: accessHistoryExpire,
	}
}

func (s *AccessHistory) Store(oip string, oua string, nip string, nua string) (bool, int) {
	s.mux.Lock()
	defer s.mux.Unlock()
	okey := fmt.Sprintf("%x", sha1.Sum([]byte(oip+oua)))
	nkey := fmt.Sprintf("%x", sha1.Sum([]byte(nip+nua)))
	_, ok := s.m[okey]
	if !ok {
		s.m[okey] = []string{}
		go func(k string) {
			<-time.After(s.expire)
			s.mux.Lock()
			defer s.mux.Unlock()
			delete(s.m, k)
		}(okey)
	}
	for _, v := range s.m[okey] {
		if v == nkey {
			return true, s.limit - len(s.m[okey])
		}
	}
	if len(s.m[okey]) >= s.limit {
		return false, 0
	}
	s.m[okey] = append(s.m[okey], nkey)
	return true, s.limit - len(s.m[okey])
}
