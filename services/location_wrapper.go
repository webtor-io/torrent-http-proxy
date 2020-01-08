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

func NewLocationWrapper(src *Source, r *Resolver) *LocationWrapper {
	return &LocationWrapper{src: src, r: r}
}
func (s *LocationWrapper) GetLocation(logger *logrus.Entry) (*Location, error) {
	if s.loc == nil {
		loc, err := s.r.Resolve(s.src, logger, false)
		if err != nil {
			return nil, errors.Wrap(err, "Failed to init location")
		}
		s.loc = loc
		return loc, nil
	} else if s.loc.Active == false {
		return s.Refresh(logger)
	}
	return s.loc, nil
}

func (s *LocationWrapper) Refresh(logger *logrus.Entry) (*Location, error) {
	loc, err := s.r.Resolve(s.src, logger, true)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to refresh location")
	}
	s.loc = loc
	return loc, nil
}
