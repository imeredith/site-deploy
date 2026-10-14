package caddy_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"site-deploy/internal/caddy"
	"site-deploy/internal/model"
)

// publicSites routes each domain without access control.
func publicSites(domains ...string) []model.Site {
	sites := make([]model.Site, 0, len(domains))
	for _, domain := range domains {
		sites = append(sites, model.Site{Domain: domain})
	}
	return sites
}

type config struct {
	Admin struct {
		Listen string `json:"listen"`
	} `json:"admin"`
	Apps struct {
		HTTP struct {
			Servers map[string]struct {
				Listen         []string `json:"listen"`
				AutomaticHTTPS *struct {
					Disable bool `json:"disable"`
				} `json:"automatic_https"`
				Routes []struct {
					Match []struct {
						Host []string `json:"host"`
					} `json:"match"`
					Handle []struct {
						Handler          string `json:"handler"`
						StatusCode       int    `json:"status_code"`
						StreamCloseDelay int64  `json:"stream_close_delay"`
						Upstreams        []struct {
							Dial string `json:"dial"`
						} `json:"upstreams"`
					} `json:"handle"`
				} `json:"routes"`
			} `json:"servers"`
		} `json:"http"`
	} `json:"apps"`
}

func TestConfigRestrictsDomainsAndEnablesHTTPS(t *testing.T) {
	domains := []string{"z.example", "A.example.", "a.example"}
	data, err := caddy.Config(publicSites(domains...), "127.0.0.1:9080", false)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	server := cfg.Apps.HTTP.Servers["site-deploy"]
	if cfg.Admin.Listen != "127.0.0.1:2019" || !reflect.DeepEqual(server.Listen, []string{":443"}) || server.AutomaticHTTPS != nil {
		t.Fatalf("unexpected listeners or HTTPS settings: %s", data)
	}
	if len(server.Routes) != 2 || len(server.Routes[0].Match) != 1 || !reflect.DeepEqual(server.Routes[0].Match[0].Host, []string{"a.example", "z.example"}) {
		t.Fatalf("unexpected routes: %s", data)
	}
	proxy := server.Routes[0].Handle[0]
	if proxy.Handler != "reverse_proxy" || len(proxy.Upstreams) != 1 || proxy.Upstreams[0].Dial != "127.0.0.1:9080" || proxy.StreamCloseDelay != int64(5*time.Minute) || server.Routes[1].Handle[0].StatusCode != 404 {
		t.Fatalf("unexpected proxy/default route: %s", data)
	}
	if !reflect.DeepEqual(domains, []string{"z.example", "A.example.", "a.example"}) {
		t.Fatal("input domains mutated")
	}
	second, err := caddy.Config(publicSites("a.example", "z.example"), "127.0.0.1:9080", false)
	if err != nil || string(second) != string(data) {
		t.Fatal("equivalent configurations are not deterministic")
	}
}

func TestEmptyConfigCannotForwardCatchall(t *testing.T) {
	for _, httpOnly := range []bool{false, true} {
		data, err := caddy.Config(nil, "[::1]:9080", httpOnly)
		if err != nil {
			t.Fatal(err)
		}
		var cfg config
		if err := json.Unmarshal(data, &cfg); err != nil {
			t.Fatal(err)
		}
		server := cfg.Apps.HTTP.Servers["site-deploy"]
		if len(server.Routes) != 1 || len(server.Routes[0].Match) != 0 || server.Routes[0].Handle[0].Handler != "static_response" || server.Routes[0].Handle[0].StatusCode != 404 || strings.Contains(string(data), "reverse_proxy") {
			t.Fatalf("empty config can forward a request: %s", data)
		}
		if httpOnly && (!reflect.DeepEqual(server.Listen, []string{":80"}) || server.AutomaticHTTPS == nil || !server.AutomaticHTTPS.Disable) {
			t.Fatalf("HTTP development mode is not explicit: %s", data)
		}
	}
}

func TestConfigRejectsUnsafeInputs(t *testing.T) {
	for _, address := range []string{"", "localhost:9080", "0.0.0.0:9080", "192.0.2.3:9080", "127.0.0.1", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:+80", "http://127.0.0.1:9080", "unix//tmp/gateway.sock", "[::]:9080"} {
		if _, err := caddy.Config(publicSites("app.example"), address, false); err == nil {
			t.Errorf("accepted unsafe gateway address %q", address)
		}
	}
	for _, domain := range []string{"", "*.example", "a.example:80", "https://a.example", "a.example/path", "a..example", "-a.example", "a_.example", "a.example ", "127.0.0.1"} {
		if _, err := caddy.Config(publicSites(domain), "127.0.0.1:9080", false); err == nil {
			t.Errorf("accepted invalid application domain %q", domain)
		}
	}
}

const testHash = "$2a$14$O4UeNfvX7xUhP9mj6tjWIOkvvB/sTqKipfaXtgWqmJ7MZjMg/l9me"

func TestProtectedSitesAuthenticateBeforeProxying(t *testing.T) {
	sites := []model.Site{
		{Domain: "public.example"},
		{Domain: "Private.Example.", BasicAuth: []model.BasicAuthUser{{User: "preview", Hash: testHash}, {User: "alice", Hash: testHash}}},
		{Domain: "another.example", BasicAuth: []model.BasicAuthUser{{User: "bob", Hash: testHash}}},
	}
	data, err := caddy.Config(sites, "127.0.0.1:9080", true)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Apps struct {
			HTTP struct {
				Servers map[string]struct {
					Routes []struct {
						Match []struct {
							Host []string `json:"host"`
						} `json:"match"`
						Handle []struct {
							Handler   string `json:"handler"`
							Providers struct {
								HTTPBasic struct {
									Accounts []struct {
										Username string `json:"username"`
										Password string `json:"password"`
									} `json:"accounts"`
									Hash struct {
										Algorithm string `json:"algorithm"`
									} `json:"hash"`
									HashCache *struct{} `json:"hash_cache"`
								} `json:"http_basic"`
							} `json:"providers"`
						} `json:"handle"`
						Terminal bool `json:"terminal"`
					} `json:"routes"`
				} `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	routes := cfg.Apps.HTTP.Servers["site-deploy"].Routes
	// Protected routes come first, one per domain, sorted; then public; then 404.
	if len(routes) != 4 {
		t.Fatalf("routes: %s", data)
	}
	for i, want := range []string{"another.example", "private.example"} {
		route := routes[i]
		if len(route.Match) != 1 || !reflect.DeepEqual(route.Match[0].Host, []string{want}) || !route.Terminal || len(route.Handle) != 2 {
			t.Fatalf("protected route %d: %s", i, data)
		}
		auth := route.Handle[0]
		if auth.Handler != "authentication" || auth.Providers.HTTPBasic.Hash.Algorithm != "bcrypt" || auth.Providers.HTTPBasic.HashCache == nil || route.Handle[1].Handler != "reverse_proxy" {
			t.Fatalf("protected route %d does not authenticate before proxying: %s", i, data)
		}
	}
	accounts := routes[1].Handle[0].Providers.HTTPBasic.Accounts
	if len(accounts) != 2 || accounts[0].Username != "preview" || accounts[1].Username != "alice" || accounts[0].Password != testHash {
		t.Fatalf("accounts not preserved in order: %+v", accounts)
	}
	if !reflect.DeepEqual(routes[2].Match[0].Host, []string{"public.example"}) || len(routes[2].Handle) != 1 || routes[2].Handle[0].Handler != "reverse_proxy" {
		t.Fatalf("public route: %s", data)
	}
	if len(routes[3].Match) != 0 || routes[3].Handle[0].Handler != "static_response" {
		t.Fatalf("default route: %s", data)
	}
	// A malformed account would make Caddy reject the whole configuration.
	for _, account := range []model.BasicAuthUser{
		{User: "preview", Hash: "plaintext-password"},
		{User: "preview", Hash: "$$2a$$14$$O4UeNfvX7xUhP9mj6tjWIOkvvB/sTqKipfaXtgWqmJ7MZjMg/l9me"},
		{User: "preview", Hash: "$2a$03$O4UeNfvX7xUhP9mj6tjWIOkvvB/sTqKipfaXtgWqmJ7MZjMg/l9me"},
		{User: "has:colon", Hash: testHash},
		{User: "", Hash: testHash},
	} {
		if _, err := caddy.Config([]model.Site{{Domain: "x.example", BasicAuth: []model.BasicAuthUser{account}}}, "127.0.0.1:9080", true); err == nil {
			t.Errorf("accepted invalid account %+v", account)
		}
	}
	repeated := []model.Site{{Domain: "x.example"}, {Domain: "X.example", BasicAuth: []model.BasicAuthUser{{User: "a", Hash: testHash}}}}
	if _, err := caddy.Config(repeated, "127.0.0.1:9080", true); err == nil {
		t.Error("accepted a domain that is both public and protected")
	}
}

func TestSyncPostsFullConfiguration(t *testing.T) {
	input := `{"admin":{"listen":"127.0.0.1:2019","enforce_origin":true},"apps":{"http":{}}}`
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/load" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected load request: %s %s %s %q", r.Method, r.URL.Path, r.Header.Get("Content-Type"), body)
		}
		want := `{"admin":{"enforce_origin":true,"listen":"` + r.Host + `"},"apps":{"http":{}}}`
		if string(body) != want {
			t.Errorf("admin endpoint or unrelated config changed: %s; want %s", body, want)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	if err := caddy.Sync(context.Background(), server.URL+"/", []byte(input)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("load requests = %d", calls.Load())
	}
}

func TestSyncRejectsRedirectsAndBoundsErrors(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	if err := caddy.Sync(context.Background(), redirect.URL, []byte(`{}`)); err == nil || targetCalls.Load() != 0 {
		t.Fatalf("redirect followed or accepted: %v, target requests %d", err, targetCalls.Load())
	}
	reject := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, strings.Repeat("x", 16384))
	}))
	defer reject.Close()
	err := caddy.Sync(context.Background(), reject.URL, []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "400 Bad Request") || !strings.Contains(err.Error(), "truncated") || len(err.Error()) > 8400 {
		t.Fatalf("unbounded or missing error: %v", err)
	}
}

func TestSyncValidatesURLAndContext(t *testing.T) {
	for _, address := range []string{"http://localhost:2019", "http://192.0.2.1:2019", "http://127.0.0.1", "file:///tmp/admin", "http://user@127.0.0.1:2019", "http://127.0.0.1:2019/config", "http://127.0.0.1:2019/?q=x", "http://127.0.0.1:2019/#x"} {
		if err := caddy.Sync(context.Background(), address, []byte(`{}`)); err == nil {
			t.Errorf("accepted unsafe admin URL %q", address)
		}
	}
	if err := caddy.Sync(context.Background(), "http://127.0.0.1:2019", []byte(`invalid JSON`)); err == nil {
		t.Error("accepted invalid JSON")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if err := caddy.Sync(ctx, "http://127.0.0.1:2019", []byte(`{}`)); err == nil {
		t.Error("ignored cancellation")
	}
	if time.Since(started) > time.Second {
		t.Error("cancellation was not prompt")
	}
}
