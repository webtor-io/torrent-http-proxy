package services

import (
	"context"
	"net/http"

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
// Rule describes an optional policy attached to a primary token. The grace
// rule is the first kind: it carries a separate signed token that THP swaps
// in on segment URLs while movie-time falls within DurationSec.
type Rule struct {
	Kind        string `json:"kind"`
	Scope       string `json:"scope"`
	DurationSec int    `json:"duration_sec"`
	Token       string `json:"token"`
}

type StandardClaims struct {
	Grace  int    `json:"grace"`
	Preset string `json:"preset"`
	Rate   string `json:"rate"`
	Role   string `json:"role"`
	Rules  []Rule `json:"rules,omitempty"`
	jwt.StandardClaims
}

type rulesCtxKey struct{}

// RulesContext is the per-request bundle that downstream rule-driven hooks
// (manifest rewriting, future kinds) read from the request context. It carries
// the validated claims plus the request inputs they need to apply rules
// without re-parsing — primary token (for swaps) and infohash (for binding).
type RulesContext struct {
	Claims       jwt.MapClaims
	PrimaryToken string
	InfoHash     string
}

func WithRulesContext(r *http.Request, rc *RulesContext) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), rulesCtxKey{}, rc))
}

func GetRulesContext(r *http.Request) *RulesContext {
	rc, _ := r.Context().Value(rulesCtxKey{}).(*RulesContext)
	return rc
}

// ExtractRules pulls the optional "rules" claim out of a MapClaims and
// returns it as typed []Rule. Returns nil if absent or malformed.
func ExtractRules(claims jwt.MapClaims) []Rule {
	raw, ok := claims["rules"]
	if !ok {
		return nil
	}
	arr, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	out := make([]Rule, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		r := Rule{}
		if v, ok := m["kind"].(string); ok {
			r.Kind = v
		}
		if v, ok := m["scope"].(string); ok {
			r.Scope = v
		}
		if v, ok := m["duration_sec"].(float64); ok {
			r.DurationSec = int(v)
		}
		if v, ok := m["token"].(string); ok {
			r.Token = v
		}
		out = append(out, r)
	}
	return out
}

func NewClaims(c *cli.Context) *Claims {
	return &Claims{
		apiKey:    c.String(apiKeyFlag),
		apiSecret: c.String(apiSecretFlag),
	}
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
