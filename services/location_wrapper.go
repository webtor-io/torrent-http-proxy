package services

import (
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type LocationWrapper struct {
	src *Source
	r   *Resolver
	loc *Location
}

func NewLocationWrapper(src *Source, r *Resolver, loc *Location) *LocationWrapper {
	return &LocationWrapper{src: src, r: r, loc: loc}
}
func (s *LocationWrapper) Location() *Location {
	return s.loc
}

func (s *LocationWrapper) Refresh(logger *logrus.Entry) (*Location, error) {
	loc, err := s.r.Resolve(s.src, logger, true)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to refresh location")
	}
	s.loc = loc
	return loc, nil
}
