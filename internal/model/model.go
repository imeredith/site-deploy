// Package model defines the persisted deployment format shared by the daemon and CLI.
package model

import (
	"fmt"
	"regexp"
	"time"
)

type Spec struct {
	Domain       string `json:"domain" yaml:"domain"`
	Service      string `json:"service" yaml:"service"`
	Port         int    `json:"port" yaml:"port"`
	IdleTimeout  string `json:"idle_timeout" yaml:"idle_timeout"`
	StartTimeout string `json:"start_timeout" yaml:"start_timeout"`
	SuspendMode  string `json:"suspend_mode" yaml:"suspend_mode"`
	// BasicAuth, when set, makes Caddy require one of these accounts before a
	// request reaches the gateway, so unauthenticated requests never wake the app.
	BasicAuth []BasicAuthUser `json:"basic_auth,omitempty" yaml:"basic_auth,omitempty"`
}

// BasicAuthUser is one HTTP basic authentication account. Hash is a bcrypt
// hash; plaintext passwords are never accepted or stored.
type BasicAuthUser struct {
	User string `json:"user" yaml:"user"`
	Hash string `json:"hash,omitempty" yaml:"hash"`
}

// MaxBasicAuthUsers bounds the accounts per application.
const MaxBasicAuthUsers = 64

var (
	basicAuthUser = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@+-]{0,63}$`)
	bcryptHash    = regexp.MustCompile(`^\$2[aby]\$(0[4-9]|[12][0-9]|3[01])\$[./A-Za-z0-9]{53}$`)
)

// Validate checks an account strictly. Caddy rejects a whole configuration
// containing one malformed account, which would stall routing for every app.
func (u BasicAuthUser) Validate() error {
	if !basicAuthUser.MatchString(u.User) {
		return fmt.Errorf("basic auth user %q must start with a letter or digit and contain at most 64 letters, digits, or . _ @ + -", u.User)
	}
	if !bcryptHash.MatchString(u.Hash) {
		return fmt.Errorf("basic auth hash for %q must be a bcrypt hash such as $2a$14$..., written without Compose $$ escaping; generate one with caddy hash-password", u.User)
	}
	return nil
}

// Site is one routed hostname and the access control Caddy applies to it.
type Site struct {
	Domain    string
	BasicAuth []BasicAuthUser
}

func (s Spec) IdleDuration() time.Duration {
	d, _ := time.ParseDuration(s.IdleTimeout)
	return d
}

func (s Spec) StartDuration() time.Duration {
	d, err := time.ParseDuration(s.StartTimeout)
	if err != nil || d <= 0 {
		return 60 * time.Second
	}
	return d
}

type State string

const (
	StateRunning   State = "running"
	StateSuspended State = "suspended"
	StateStarting  State = "starting"
	StateStopping  State = "stopping"
	StateDeleting  State = "deleting"
	StateError     State = "error"
)

type App struct {
	Name         string    `json:"name"`
	Spec         Spec      `json:"spec"`
	Project      string    `json:"project"`
	Directory    string    `json:"directory"`
	ComposeFile  string    `json:"compose_file"`
	State        State     `json:"state"`
	Upstream     string    `json:"upstream,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	LastActivity time.Time `json:"last_activity"`
	Error        string    `json:"error,omitempty"`
}
