// Package gateway wakes applications and proxies each request while retaining
// a lease that prevents the application from being suspended mid-response.
package gateway

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"site-deploy/internal/orchestrator"
)

type manager interface {
	Acquire(context.Context, string) (*orchestrator.Lease, error)
}

// resumeHeader marks the resuming page's status checks. The gateway answers
// them itself, so applications never receive this header.
const resumeHeader = "X-Site-Deploy-Resume"

var (
	// resumePageDelay is how long a browser page load waits for a sleeping
	// application before it gets the resuming page instead, so quick wakes
	// never flash it. Tests shorten it.
	resumePageDelay = time.Second
	// resumeCheckTimeout bounds each status check from the resuming page. A
	// check answers as soon as the application is ready.
	resumeCheckTimeout = 10 * time.Second

	//go:embed resume.html
	resumeHTML     string
	resumeTemplate = template.Must(template.New("resume").Parse(resumeHTML))
)

// New returns the data-plane handler. It has no administrative endpoints;
// application selection always uses the incoming HTTP Host.
func New(m manager) http.Handler {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	// A connection can outlive its Docker port allocation. New connections also
	// prevent net/http from automatically replaying a request on a stale socket.
	transport.DisableKeepAlives = true
	transport.ResponseHeaderTimeout = 0
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, check := r.Header[resumeHeader]; check {
			resumeStatus(w, r, m)
			return
		}
		// A browser loading a page gets the resuming page if the application is
		// slow to wake. Other requests, such as API calls and form posts, wait.
		ctx := r.Context()
		page := r.Method == http.MethodGet && r.Header.Get("Sec-Fetch-Mode") == "navigate"
		if page {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, resumePageDelay)
			defer cancel()
		}
		// Startup belongs to the daemon, so it continues after this wait ends.
		lease, err := m.Acquire(ctx, r.Host)
		if err != nil {
			if errors.Is(err, orchestrator.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			if page && r.Context().Err() == nil {
				resumePage(w, r.Host, !errors.Is(err, context.DeadlineExceeded))
				return
			}
			unavailable(w)
			return
		}
		if lease == nil {
			unavailable(w)
			return
		}
		if lease.Release != nil {
			defer lease.Release()
		}
		if !loopbackSocket(lease.Upstream) {
			unavailable(w)
			return
		}

		target := &url.URL{Scheme: "http", Host: lease.Upstream}
		proxy := &httputil.ReverseProxy{
			Transport:     transport,
			FlushInterval: -1,
			Rewrite: func(p *httputil.ProxyRequest) {
				p.SetURL(target)
				p.Out.Host = p.In.Host
				// This gateway does not interpret queries. Preserve their exact wire
				// representation, including semicolons and repeated signed fields.
				p.Out.URL.RawQuery = p.In.URL.RawQuery
				// ReverseProxy removes standard forwarding headers before Rewrite,
				// but remove any other X-Forwarded-* fields as well. Only metadata
				// Caddy owns is accepted from the immediate loopback peer.
				for key := range p.Out.Header {
					if strings.HasPrefix(strings.ToLower(key), "x-forwarded-") {
						p.Out.Header.Del(key)
					}
				}
				p.SetXForwarded()
				if loopbackPeer(p.In.RemoteAddr) {
					for _, key := range []string{"X-Forwarded-For", "X-Forwarded-Host"} {
						if values := p.In.Header.Values(key); len(values) > 0 {
							p.Out.Header[key] = append([]string(nil), values...)
						}
					}
					if proto := p.In.Header.Get("X-Forwarded-Proto"); proto == "http" || proto == "https" {
						p.Out.Header.Set("X-Forwarded-Proto", proto)
					}
				}
			},
			ErrorHandler: func(w http.ResponseWriter, request *http.Request, err error) {
				var connectionError *net.OpError
				connectionFailure := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || (errors.As(err, &connectionError) && (connectionError.Op == "dial" || connectionError.Op == "read" || connectionError.Op == "write"))
				if request.Context().Err() == nil && connectionFailure && lease.ReportFailure != nil {
					lease.ReportFailure(err)
				}
				unavailable(w)
			},
		}
		// The daemon breaks the connection when the lease ends early, for example
		// on deletion: cancelling the outbound request aborts a response or
		// stream, and ReverseProxy closes an upgraded WebSocket connection.
		if lease.Context != nil {
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			stop := context.AfterFunc(lease.Context, cancel)
			defer stop()
			r = r.WithContext(ctx)
		}
		// ServeHTTP blocks until the response, stream, or WebSocket has finished.
		// The body is neither read before startup nor buffered for later replay.
		proxy.ServeHTTP(w, r)
	})
}

func unavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "2")
	http.Error(w, "application unavailable", http.StatusServiceUnavailable)
}

// resumePage shows a spinner that reloads once the application is ready, or
// a generic failure without internal details.
func resumePage(w http.ResponseWriter, host string, failed bool) {
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	header := w.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	header.Set("Retry-After", "2")
	w.WriteHeader(http.StatusServiceUnavailable)
	resumeTemplate.Execute(w, struct {
		Host   string
		Failed bool
	}{host, failed})
}

// resumeStatus waits briefly for the application and reports its state to
// the resuming page: running, starting, or failed. A check also wakes the
// application, in case the page outlived an earlier start.
func resumeStatus(w http.ResponseWriter, r *http.Request, m manager) {
	ctx, cancel := context.WithTimeout(r.Context(), resumeCheckTimeout)
	defer cancel()
	lease, err := m.Acquire(ctx, r.Host)
	state, code := "running", http.StatusOK
	switch {
	case err == nil && lease != nil:
		if lease.Release != nil {
			lease.Release()
		}
	case errors.Is(err, orchestrator.ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, context.DeadlineExceeded):
		state, code = "starting", http.StatusAccepted
	default:
		state, code = "failed", http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	fmt.Fprintf(w, "{\"state\":%q}\n", state)
}

func loopbackPeer(address string) bool {
	host, _, err := net.SplitHostPort(address)
	ip := net.ParseIP(host)
	return err == nil && ip != nil && ip.IsLoopback()
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
