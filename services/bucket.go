package services

import (
	"code.cloudfoundry.org/bytefmt"
	"github.com/pkg/errors"

	"github.com/dgrijalva/jwt-go"
	"github.com/juju/ratelimit"
)

type Bucket struct {
}

func NewBucket() *Bucket {
	return &Bucket{}
}

func (s *Bucket) Get(mc jwt.MapClaims) (*ratelimit.Bucket, error) {
	rate, ok := mc["rate"].(string)
	if !ok {
		return nil, nil
	}
	r, err := bytefmt.ToBytes(rate)
	if err != nil {
		return nil, errors.Errorf("failed to parse rate %v", rate)
	}
	return ratelimit.NewBucketWithRate(float64(r)/8, int64(r)), nil
}
