package services

import (
	"github.com/dgrijalva/jwt-go"
	"github.com/pkg/errors"
	"github.com/urfave/cli"
)

const (
	JWT_SECRET_FLAG     = "jwt-secret"
	JWT_TOKEN_GET_PARAM = "token"
)

func RegisterClaimsFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   JWT_SECRET_FLAG,
		Usage:  "JWT Secret",
		Value:  "",
		EnvVar: "SECRET",
	})
}

type Claims struct {
	JWTSecret string
}

func NewClaims(c *cli.Context) *Claims {
	return &Claims{JWTSecret: c.String(JWT_SECRET_FLAG)}
}

func (s *Claims) Get(tokenString string) (jwt.MapClaims, error) {

	if s.JWTSecret == "" {
		return jwt.MapClaims{}, nil
	}

	if tokenString == "" {
		return nil, errors.Errorf("Failed to get token")
	}

	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		// Don't forget to validate the alg is what you expect:
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.Errorf("Unexpected signing method=%v", token.Header["alg"])
		}
		// hmacSampleSecret is a []byte containing your secret, e.g. []byte("my_secret_key")
		return []byte(s.JWTSecret), nil
	})
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to parse token")
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return nil, errors.Wrapf(err, "Failed to validate token")
	}
	return claims, nil
}
