package auth

import (
	"strings"
	"testing"
)

func TestInvalidTokensCannotMakeAnUnusableDaemon(t *testing.T) {
	for _, token := range []string{"short", strings.Repeat("a", 4097), strings.Repeat("a", 32) + " extra", strings.Repeat("a", 32) + "\n", strings.Repeat("a", 32) + "\x00", strings.Repeat("a", 32) + "=b"} {
		if err := ValidateToken(token); err == nil {
			t.Fatalf("accepted invalid token of length %d", len(token))
		}
	}
	for _, token := range []string{strings.Repeat("a", 32), strings.Repeat("b", 4096), strings.Repeat("a", 30) + "+/=="} {
		if err := ValidateToken(token); err != nil {
			t.Fatal(err)
		}
	}
}
