package services

import (
	"github.com/dgrijalva/jwt-go"
	"github.com/pkg/errors"
)

type Claims struct {
	cs *Clients
}

func NewClaims(cs *Clients) *Claims {
	return &Claims{cs: cs}
}

func (s *Claims) Get(tokenString string, apiKey string) (jwt.MapClaims, *Client, error) {

	if s.cs.Empty() {
		return jwt.MapClaims{}, nil, nil
	}

	if tokenString == "" {
		return nil, nil, errors.Errorf("Failed to get token")
	}

	cl := s.cs.Get(apiKey)

	if cl == nil {
		return nil, nil, errors.Errorf("Failed to find secret by API key %v", apiKey)
	}

	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		// Don't forget to validate the alg is what you expect:
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.Errorf("Unexpected signing method=%v", token.Header["alg"])
		}
		// hmacSampleSecret is a []byte containing your secret, e.g. []byte("my_secret_key")
		return []byte(cl.Secret), nil
	})
	if err != nil {
		return nil, nil, errors.Wrapf(err, "Failed to parse token")
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return nil, nil, errors.Wrapf(err, "Failed to validate token")
	}
	return claims, cl, nil
}
