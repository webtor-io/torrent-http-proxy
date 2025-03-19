package services

import (
	"github.com/webtor-io/lazymap"
	"time"

	"github.com/pkg/errors"

	"code.cloudfoundry.org/bytefmt"

	"github.com/dgrijalva/jwt-go"
	"github.com/juju/ratelimit"
)

type Bucket struct {
	lazymap.LazyMap[*ratelimit.Bucket]
}

func NewBucket() *Bucket {
	return &Bucket{
		LazyMap: lazymap.New[*ratelimit.Bucket](&lazymap.Config{
			Expire: 5 * 60 * time.Second,
		}),
	}
}

func (s *Bucket) Get(mc jwt.MapClaims) (*ratelimit.Bucket, error) {
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
	return s.LazyMap.Get(key, func() (*ratelimit.Bucket, error) {
		return ratelimit.NewBucketWithRate(float64(r)/8, int64(r)), nil
	})
}
