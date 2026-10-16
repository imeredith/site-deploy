package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCLIVolumeCommands(t *testing.T) {
	cliTestEnvironment(t)
	daemon := "0.10.0"
	var request string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/version" {
			fmt.Fprintf(w, `{"version":%q}`, daemon)
			return
		}
		request = r.Method + " " + r.URL.Path
		switch request {
		case "GET /v1/volumes":
			io.WriteString(w, `[{"name":"corpus","path":"/srv/volumes/corpus/data","snapshots":[{"app":"pr1","path":"/srv/volumes/corpus/snapshots/pr1"}],"used_by":[{"app":"main","snapshot":false},{"app":"pr1","snapshot":true}]},{"name":"empty","path":"/srv/volumes/empty/data","snapshots":[],"used_by":[]}]`)
		case "POST /v1/volumes/corpus":
			io.WriteString(w, `{"name":"corpus","path":"/srv/volumes/corpus/data","snapshots":[],"used_by":[]}`)
		case "DELETE /v1/volumes/corpus":
			io.WriteString(w, `{"deleted":"corpus"}`)
		case "DELETE /v1/volumes/corpus/snapshots/pr1":
			io.WriteString(w, `{"volume":"corpus","snapshot":"pr1"}`)
		default:
			t.Errorf("request %s", request)
		}
	}))
	defer server.Close()
	run := func(args ...string) (int, string, string) {
		return cliTestRun(append(args, "--token", cliTestToken, "--server", server.URL)...)
	}
	for _, test := range []struct {
		args          []string
		request, want string
	}{
		{[]string{"volume", "list"}, "GET /v1/volumes", "corpus  /srv/volumes/corpus/data  pr1        main, pr1 (snapshot)\nempty   /srv/volumes/empty/data   -          -"},
		{[]string{"volume", "create", "corpus"}, "POST /v1/volumes/corpus", "Created volume corpus at /srv/volumes/corpus/data\nMount it from a bundle with:\n  x-site-deploy:\n    volumes:\n      corpus:\n        target: /data\n"},
		{[]string{"volume", "delete", "corpus"}, "DELETE /v1/volumes/corpus", "Deleted volume corpus\n"},
		{[]string{"volume", "delete-snapshot", "corpus", "pr1"}, "DELETE /v1/volumes/corpus/snapshots/pr1", "Deleted pr1's snapshot of volume corpus\n"},
	} {
		request = ""
		code, stdout, stderr := run(test.args...)
		if code != 0 || request != test.request || !strings.Contains(stdout, test.want) {
			t.Errorf("%q: code=%d request=%q stdout=%q stderr=%q", test.args, code, request, stdout, stderr)
		}
	}
	if code, stdout, _ := run("volume", "create", "corpus", "--json"); code != 0 || !strings.Contains(stdout, `"path": "/srv/volumes/corpus/data"`) {
		t.Errorf("create --json: code=%d stdout=%q", code, stdout)
	}

	// Older daemons don't have the endpoints, or, like 0.9.0, need keys, so the CLI explains instead.
	for _, daemon = range []string{"0.8.1", "0.9.0"} {
		request = ""
		if code, _, stderr := run("volume", "list"); code != 1 || request != "" || !strings.Contains(stderr, "site-deploy volumes needs daemon 0.10.0 or newer") {
			t.Errorf("daemon %s: code=%d request=%q stderr=%q", daemon, code, request, stderr)
		}
	}

	for _, args := range [][]string{
		{"volume"},
		{"volume", "create"},
		{"volume", "create", "Corpus"},
		{"volume", "list", "corpus"},
		{"volume", "delete-snapshot", "corpus"},
		{"volume", "delete-snapshot", "corpus", "Pr1"},
		{"volume", "format", "corpus"},
		{"volume", "rekey", "corpus"},
	} {
		if code, _, _ := cliTestRun(args...); code != 2 {
			t.Errorf("%q: code=%d, want 2", args, code)
		}
	}
}
