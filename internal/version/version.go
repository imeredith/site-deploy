// Package version identifies builds and decides whether a CLI may control a daemon.
package version

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is stamped at build time by the Makefile:
//
//	-ldflags '-X site-deploy/internal/version.Version=0.1.0+abc1234'
//
// Unstamped builds report "dev".
var Version = "dev"

const (
	// Header carries the daemon's version on every control API response.
	Header = "X-Site-Deploy-Version"
	// ClientHeader carries the CLI's version on every control API request.
	ClientHeader = "X-Site-Deploy-Client-Version"
)

type semver struct {
	core       [3]int
	prerelease string
}

// parse accepts MAJOR.MINOR.PATCH with an optional "v" prefix, a "-prerelease"
// suffix, and "+metadata". Build metadata does not affect precedence.
func parse(value string) (semver, bool) {
	value = strings.TrimPrefix(value, "v")
	value, _, _ = strings.Cut(value, "+")
	value, prerelease, _ := strings.Cut(value, "-")
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	var v semver
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || (len(part) > 1 && part[0] == '0') {
			return semver{}, false
		}
		v.core[i] = n
	}
	v.prerelease = prerelease
	return v, true
}

// Compare returns -1, 0, or 1 as a is older than, equal to, or newer than b.
// ok is false if either value is not a release version, such as "dev".
func Compare(a, b string) (result int, ok bool) {
	x, okA := parse(a)
	y, okB := parse(b)
	if !okA || !okB {
		return 0, false
	}
	for i := range x.core {
		if x.core[i] != y.core[i] {
			if x.core[i] < y.core[i] {
				return -1, true
			}
			return 1, true
		}
	}
	switch {
	case x.prerelease == y.prerelease:
		return 0, true
	case x.prerelease == "":
		return 1, true
	case y.prerelease == "":
		return -1, true
	case x.prerelease < y.prerelease:
		return -1, true
	default:
		return 1, true
	}
}

// ClientSupported reports whether a CLI build may control a daemon build. A
// released daemon requires a released CLI at least as new as itself, so the CLI
// is always upgraded first. A dev daemon accepts any CLI for local development.
func ClientSupported(client, daemon string) error {
	if _, ok := parse(daemon); !ok {
		return nil
	}
	if client == "" {
		return fmt.Errorf("site-deploy CLI did not report its version; daemon %s requires CLI %s or newer", daemon, daemon)
	}
	order, ok := Compare(client, daemon)
	if !ok {
		return fmt.Errorf("site-deploy CLI %s is not a release build; daemon %s requires CLI %s or newer (build with make)", client, daemon, daemon)
	}
	if order < 0 {
		return fmt.Errorf("site-deploy CLI %s is older than daemon %s; upgrade the CLI first", client, daemon)
	}
	return nil
}
