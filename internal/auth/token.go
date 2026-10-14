// Package auth shares the accepted Bearer token format between server and client.
package auth

import (
	"errors"
	"strings"
)

const MaxTokenBytes = 4096

func ValidateToken(token string) error {
	if len(token) < 32 || len(token) > MaxTokenBytes {
		return errors.New("API token must contain between 32 and 4096 characters")
	}
	padding := false
	for _, c := range token {
		if c == '=' {
			padding = true
			continue
		}
		if padding || !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.ContainsRune("-._~+/", c)) {
			return errors.New("API token must be a single Bearer token without whitespace or control characters")
		}
	}
	return nil
}
