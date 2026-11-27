package uploads

import (
	"strings"
	"testing"
)

// TestPredicates: the vectors every copy of this package shares.
func TestPredicates(t *testing.T) {
	const (
		kind0 = "9c5db5752b7c0123456789abcdef0123456789ab"
		kind1 = "9c5db5752b7c1123456789abcdef0123456789ab"
		kindF = "9c5db5752b7cf123456789abcdef0123456789ab"
	)
	for _, c := range []struct {
		id          string
		home, owned bool
	}{
		{kind0, true, true},
		{kind1, true, false},
		{kindF, true, false},
		{strings.ToUpper(kind0), false, false},
		{strings.ToUpper(kind1), false, false},
		{strings.ToUpper(kindF), false, false},
		{"9c5db5752b7cA123456789abcdef0123456789ab", false, false},
		{"9c5db5752b7c/../x", false, false},
		{"9c5db5752b7c0123456789abcdef0123456789a", false, false},
		{"0123456789abcdef0123456789abcdef01234567", false, false},
	} {
		if home, owned := HomeHosted(c.id), Owned(c.id); home != c.home || owned != c.owned {
			t.Errorf("%q: HomeHosted %v, Owned %v; want %v, %v", c.id, home, owned, c.home, c.owned)
		}
	}
}
