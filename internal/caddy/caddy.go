// Package caddy builds and loads configuration for a dedicated Caddy instance.
package caddy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"site-deploy/internal/model"
)

// Mode selects how Caddy serves application domains.
type Mode int

const (
	// HTTPS serves port 443 with publicly trusted certificates from Caddy's
	// automatic HTTPS, which needs the domains reachable for ACME challenges.
	HTTPS Mode = iota
	// InternalHTTPS serves port 443 with certificates from Caddy's local CA.
	// Browsers do not trust that CA, but it needs no public ACME challenge,
	// which suits hosts that are reachable only privately.
	InternalHTTPS
	// HTTP serves plain HTTP on port 80.
	HTTP
)

// Config returns a complete Caddy configuration, replacing any existing sites
// when loaded. Caddy terminates TLS and forwards only registered application
// domains to the host-local gateway, which wakes the corresponding application.
// Sites with basic auth get their own route that checks credentials first, so
// unauthenticated requests are answered by Caddy and never wake an application.
//
// Caddy also loads every .pem file in certFolders, each holding a certificate
// chain and its private key, and obtains no certificate for the names those
// cover. The folders are paths as Caddy sees them, so a containerized Caddy
// reads them from its own mounts.
func Config(sites []model.Site, gateway string, mode Mode, certFolders ...string) ([]byte, error) {
	if !loopbackSocket(gateway) {
		return nil, errors.New("gateway must be a literal loopback IP address and port, such as 127.0.0.1:9080")
	}
	public := make([]string, 0, len(sites))
	protected := make([]model.Site, 0)
	// seen records whether each domain was protected. Repeated public entries
	// collapse; a repeated protected domain could carry conflicting accounts.
	seen := make(map[string]bool, len(sites))
	for _, site := range sites {
		domain := strings.TrimSuffix(strings.ToLower(site.Domain), ".")
		if !validDomain(domain) {
			return nil, fmt.Errorf("invalid application domain %q", domain)
		}
		wasProtected, repeated := seen[domain]
		if repeated && (wasProtected || len(site.BasicAuth) > 0) {
			return nil, fmt.Errorf("application domain %q is listed more than once with basic auth", domain)
		}
		seen[domain] = len(site.BasicAuth) > 0
		if repeated {
			continue
		}
		if len(site.BasicAuth) == 0 {
			public = append(public, domain)
			continue
		}
		for _, account := range site.BasicAuth {
			if err := account.Validate(); err != nil {
				return nil, fmt.Errorf("%s: %w", domain, err)
			}
		}
		protected = append(protected, model.Site{Domain: domain, BasicAuth: site.BasicAuth})
	}
	sort.Strings(public)
	sort.Slice(protected, func(i, j int) bool { return protected[i].Domain < protected[j].Domain })
	proxy := map[string]any{
		"handler":            "reverse_proxy",
		"upstreams":          []any{map[string]any{"dial": gateway}},
		"stream_close_delay": int64(5 * time.Minute),
	}
	routes := make([]any, 0, len(protected)+2)
	for _, site := range protected {
		accounts := make([]any, 0, len(site.BasicAuth))
		for _, account := range site.BasicAuth {
			accounts = append(accounts, map[string]any{"username": account.User, "password": account.Hash})
		}
		routes = append(routes, map[string]any{
			"match": []any{map[string]any{"host": []string{site.Domain}}},
			"handle": []any{
				map[string]any{"handler": "authentication", "providers": map[string]any{"http_basic": map[string]any{
					"accounts": accounts,
					"hash":     map[string]any{"algorithm": "bcrypt"},
					// bcrypt is deliberately slow; cache verified credentials so a
					// page's many asset requests do not each pay the full cost.
					"hash_cache": map[string]any{},
				}}},
				proxy,
			},
			"terminal": true,
		})
	}
	if len(public) > 0 {
		routes = append(routes, map[string]any{
			"match":    []any{map[string]any{"host": public}},
			"handle":   []any{proxy},
			"terminal": true,
		})
	}
	// An empty host matcher would match every request. Keep the default response
	// explicit so an empty deployment set can never become a forwarding catchall.
	routes = append(routes, map[string]any{
		"handle": []any{map[string]any{"handler": "static_response", "status_code": 404}},
	})
	server := map[string]any{"listen": []string{":443"}, "routes": routes}
	apps := map[string]any{"http": map[string]any{
		"servers": map[string]any{"site-deploy": server},
	}}
	switch mode {
	case HTTPS:
	case InternalHTTPS:
		apps["tls"] = map[string]any{"automation": map[string]any{
			"policies": []any{map[string]any{"issuers": []any{map[string]any{"module": "internal"}}}},
		}}
		// Caddy otherwise tries to add its root to the system trust store,
		// which fails in a container and trusts nothing for remote browsers.
		apps["pki"] = map[string]any{"certificate_authorities": map[string]any{
			"local": map[string]any{"install_trust": false},
		}}
	case HTTP:
		server["listen"] = []string{":80"}
		server["automatic_https"] = map[string]any{"disable": true}
	default:
		return nil, fmt.Errorf("unknown Caddy mode %d", mode)
	}
	if len(certFolders) > 0 {
		if mode == HTTP {
			return nil, errors.New("certificate folders need HTTPS")
		}
		for _, folder := range certFolders {
			if !path.IsAbs(folder) || path.Clean(folder) != folder {
				return nil, fmt.Errorf("certificate folder %q must be a clean absolute path", folder)
			}
		}
		tls, _ := apps["tls"].(map[string]any)
		if tls == nil {
			tls = map[string]any{}
			apps["tls"] = tls
		}
		tls["certificates"] = map[string]any{"load_folders": certFolders}
	}
	config := map[string]any{
		"admin": map[string]any{"listen": "127.0.0.1:2019"},
		"apps":  apps,
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// Sync replaces the complete configuration through Caddy's POST /load API.
// The caller must explicitly opt in to managing a dedicated Caddy instance.
// The admin listener is kept at adminURL's address so future syncs reach the
// same endpoint, including when Caddy uses a non-default loopback port.
func Sync(ctx context.Context, adminURL string, config []byte) error {
	u, err := url.Parse(adminURL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || !loopbackSocket(u.Host) || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("Caddy admin URL must be an HTTP(S) loopback IP address and port without credentials, a path, query, or fragment")
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(config, &document); err != nil || document == nil {
		return errors.New("Caddy configuration must be a JSON object")
	}
	admin := map[string]json.RawMessage{}
	if raw, ok := document["admin"]; ok {
		if err := json.Unmarshal(raw, &admin); err != nil {
			return errors.New("Caddy admin configuration must be a JSON object")
		}
	}
	if admin == nil {
		admin = map[string]json.RawMessage{}
	}
	admin["listen"], _ = json.Marshal(u.Host)
	document["admin"], _ = json.Marshal(admin)
	config, err = json.Marshal(document)
	if err != nil {
		return fmt.Errorf("encode Caddy configuration: %w", err)
	}
	u.Path = "/load"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(config))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("load Caddy configuration: %w", err)
	}
	defer response.Body.Close()
	const maxResponse = 8192
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil {
		return fmt.Errorf("read Caddy response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := strings.TrimSpace(string(body))
		if len(body) > maxResponse {
			message = strings.TrimSpace(string(body[:maxResponse])) + " (truncated)"
		}
		return fmt.Errorf("Caddy rejected configuration (%s): %s", response.Status, message)
	}
	return nil
}

func loopbackSocket(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() || port == "" {
		return false
	}
	for _, ch := range port {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n <= 65535
}

func validDomain(domain string) bool {
	if domain == "" || len(domain) > 253 || net.ParseIP(domain) != nil {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z') && !(ch >= '0' && ch <= '9') && ch != '-' {
				return false
			}
		}
	}
	return true
}
