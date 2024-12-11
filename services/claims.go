package services

import (
	"github.com/dgrijalva/jwt-go"
	"github.com/pkg/errors"
	"github.com/urfave/cli"
)

const (
	apiKeyFlag    = "api-key"
	apiSecretFlag = "api-secret"
)

func RegisterAPIFlags(flags []cli.Flag) []cli.Flag {
	return append(flags,
		cli.StringFlag{
			Name:   apiKeyFlag,
			Usage:  "API key for authentication",
			EnvVar: "API_KEY",
		},
		cli.StringFlag{
			Name:   apiSecretFlag,
			Usage:  "API secret for authentication",
			EnvVar: "API_SECRET",
		},
	)
}

type Claims struct {
	apiKey    string
	apiSecret string
}
type StandardClaims struct {
	Grace  int    `json:"grace"`
	Preset string `json:"preset"`
	Rate   string `json:"rate"`
	Role   string `json:"role"`
	jwt.StandardClaims
}

func NewClaims(c *cli.Context) *Claims {
	return &Claims{
		apiKey:    c.String(apiKeyFlag),
		apiSecret: c.String(apiSecretFlag),
	}
}

func (s *Claims) Set(apiKey string, claims jwt.Claims) (string, error) {

	if s.apiKey == "" && s.apiSecret == "" {
		return "", nil
	}

	if s.apiKey != apiKey {
		return "", errors.New("wrong api key")
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString([]byte(s.apiSecret))

	if err != nil {
		return "", errors.Wrapf(err, "auth token generation failed")
	}
	return tokenString, nil
}

func (s *Claims) Get(tokenString string, apiKey string) (jwt.MapClaims, error) {

	if s.apiKey == "" && s.apiSecret == "" {
		return jwt.MapClaims{}, nil
	}

	if tokenString == "" {
		return nil, errors.Errorf("failed to get token")
	}

	if s.apiKey != apiKey {
		return nil, errors.New("wrong api key")
	}

	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		// Don't forget to validate the alg is what you expect:
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.Errorf("Unexpected signing method=%v", token.Header["alg"])
		}
		// hmacSampleSecret is a []byte containing your secret, e.g. []byte("my_secret_key")
		return []byte(s.apiSecret), nil
	})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse token")
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return nil, errors.Wrapf(err, "failed to validate token")
	}
	return claims, nil
}
