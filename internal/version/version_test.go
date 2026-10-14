package version

import (
	"strings"
	"testing"
)

func TestCompare(t *testing.T) {
	for _, test := range []struct {
		a, b string
		want int
		ok   bool
	}{
		{"0.1.0", "0.1.0", 0, true},
		{"v0.1.0", "0.1.0", 0, true},
		{"0.1.0+abc1234", "0.1.0+def5678.dirty", 0, true},
		{"0.1.0", "0.2.0", -1, true},
		{"0.10.0", "0.9.9", 1, true},
		{"1.0.0", "0.99.99", 1, true},
		{"0.2.0-rc1", "0.2.0", -1, true},
		{"0.2.0", "0.2.0-rc1", 1, true},
		{"0.2.0-rc1", "0.2.0-rc2", -1, true},
		{"dev", "0.1.0", 0, false},
		{"0.1", "0.1.0", 0, false},
		{"01.0.0", "1.0.0", 0, false},
		{"", "0.1.0", 0, false},
	} {
		got, ok := Compare(test.a, test.b)
		if got != test.want || ok != test.ok {
			t.Errorf("Compare(%q, %q) = %d, %v; want %d, %v", test.a, test.b, got, ok, test.want, test.ok)
		}
	}
}

func TestClientSupported(t *testing.T) {
	for _, test := range []struct {
		client, daemon, problem string
	}{
		{"0.1.0", "0.1.0", ""},
		{"0.2.0+abc", "0.1.0+def", ""},
		{"dev", "dev", ""},
		{"", "dev", ""},
		{"0.1.0", "dev", ""},
		{"0.1.0", "0.2.0", "older than daemon"},
		{"dev", "0.1.0", "not a release build"},
		{"", "0.1.0", "did not report its version"},
	} {
		err := ClientSupported(test.client, test.daemon)
		if test.problem == "" && err != nil {
			t.Errorf("ClientSupported(%q, %q) = %v, want nil", test.client, test.daemon, err)
		}
		if test.problem != "" && (err == nil || !strings.Contains(err.Error(), test.problem)) {
			t.Errorf("ClientSupported(%q, %q) = %v, want %q", test.client, test.daemon, err, test.problem)
		}
	}
}
