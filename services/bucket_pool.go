package services

import (
	"sync"
	"time"

	"github.com/pkg/errors"

	"code.cloudfoundry.org/bytefmt"

	"github.com/dgrijalva/jwt-go"
	"github.com/juju/ratelimit"
)

const (
	bucketTTL = 30 * 60
)

type BucketPool struct {
	sm     sync.Map
	timers sync.Map
	expire time.Duration
}

func NewBucketPool() *BucketPool {
	return &BucketPool{expire: time.Duration(bucketTTL) * time.Second}
}

func (s *BucketPool) Get(mc jwt.MapClaims) (*ratelimit.Bucket, error) {
	sessionID, ok := mc["sessionID"].(string)
	if !ok {
		return nil, nil
	}
	rate, ok := mc["rate"].(string)
	if !ok {
		return nil, nil
	}
	key := sessionID + rate
	r, err := bytefmt.ToBytes(rate)
	if err != nil {
		return nil, errors.Errorf("failed to parse rate %v", rate)
	}

	v, _ := s.sm.LoadOrStore(key, ratelimit.NewBucketWithRate(float64(r)/8, int64(r)))
	t, tLoaded := s.timers.LoadOrStore(key, time.NewTimer(s.expire))
	timer := t.(*time.Timer)
	if !tLoaded {
		go func() {
			<-timer.C
			s.sm.Delete(key)
			s.timers.Delete(key)
		}()
	} else {
		timer.Reset(s.expire)
	}

	return v.(*ratelimit.Bucket), nil
}
