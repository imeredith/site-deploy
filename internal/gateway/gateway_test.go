package gateway_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"site-deploy/internal/gateway"
	"site-deploy/internal/orchestrator"
)

type acquireFunc func(context.Context, string) (*orchestrator.Lease, error)

func (f acquireFunc) Acquire(ctx context.Context, host string) (*orchestrator.Lease, error) {
	return f(ctx, host)
}

type trackingBody struct {
	io.Reader
	reads atomic.Int32
}

func (b *trackingBody) Read(p []byte) (int, error) { b.reads.Add(1); return b.Reader.Read(p) }
func (b *trackingBody) Close() error               { return nil }

func wait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for request progress")
	}
}

func TestColdStartPreservesHostBodyAndLease(t *testing.T) {
	const payload = "a body with \x00 bytes and Unicode: 世界"
	type requestInfo struct{ host, method, uri, body string }
	requests := make(chan requestInfo, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- requestInfo{r.Host, r.Method, r.RequestURI, string(body)}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "created")
	}))
	defer backend.Close()
	starting, ready, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls, releases atomic.Int32
	var selected string
	h := gateway.New(acquireFunc(func(ctx context.Context, host string) (*orchestrator.Lease, error) {
		calls.Add(1)
		selected = host
		close(starting)
		select {
		case <-ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &orchestrator.Lease{Upstream: strings.TrimPrefix(backend.URL, "http://"), Release: func() { releases.Add(1) }}, nil
	}))
	body := &trackingBody{Reader: strings.NewReader(payload)}
	req := httptest.NewRequest(http.MethodPost, "http://app.example:8443/path%2Fsegment?q=one&q=two", body)
	req.Header.Set("X-Forwarded-Host", "wrong.example")
	response := httptest.NewRecorder()
	go func() { defer close(done); h.ServeHTTP(response, req) }()
	wait(t, starting)
	if body.reads.Load() != 0 || releases.Load() != 0 {
		t.Error("request body was consumed or lease released before startup finished")
	}
	close(ready)
	wait(t, done)
	if selected != "app.example:8443" {
		t.Errorf("selected host = %q", selected)
	}
	got := <-requests
	if got != (requestInfo{"app.example:8443", http.MethodPost, "/path%2Fsegment?q=one&q=two", payload}) {
		t.Errorf("upstream request changed: %#v", got)
	}
	if response.Code != http.StatusCreated || response.Body.String() != "created" || calls.Load() != 1 || releases.Load() != 1 {
		t.Fatalf("status/body/calls/releases = %d / %q / %d / %d", response.Code, response.Body.String(), calls.Load(), releases.Load())
	}
}

func TestErrorsAndUnsafeUpstreams(t *testing.T) {
	tests := []struct {
		name, upstream string
		err            error
		status         int
	}{
		{name: "unknown domain", err: fmt.Errorf("lookup: %w", orchestrator.ErrNotFound), status: http.StatusNotFound},
		{name: "startup failure", err: orchestrator.ErrUnavailable, status: http.StatusServiceUnavailable},
		{name: "unexpected failure", err: errors.New("private Docker error"), status: http.StatusServiceUnavailable},
		{name: "external address", upstream: "192.0.2.3:80", status: http.StatusServiceUnavailable},
		{name: "DNS address", upstream: "localhost:80", status: http.StatusServiceUnavailable},
		{name: "URL", upstream: "http://127.0.0.1:80", status: http.StatusServiceUnavailable},
		{name: "port zero", upstream: "127.0.0.1:0", status: http.StatusServiceUnavailable},
		{name: "port overflow", upstream: "127.0.0.1:65536", status: http.StatusServiceUnavailable},
		{name: "signed port", upstream: "127.0.0.1:+80", status: http.StatusServiceUnavailable},
		{name: "missing port", upstream: "127.0.0.1", status: http.StatusServiceUnavailable},
		{name: "service port", upstream: "127.0.0.1:http", status: http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var releases int
			h := gateway.New(acquireFunc(func(context.Context, string) (*orchestrator.Lease, error) {
				if test.err != nil {
					return nil, test.err
				}
				return &orchestrator.Lease{Upstream: test.upstream, Release: func() { releases++ }}, nil
			}))
			response := httptest.NewRecorder()
			h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://app.example/healthz", nil))
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d", response.Code, test.status)
			}
			if test.status == http.StatusServiceUnavailable && response.Header().Get("Retry-After") != "2" {
				t.Error("missing Retry-After")
			}
			if strings.Contains(response.Body.String(), "private Docker") {
				t.Error("internal error leaked in public response")
			}
			if test.err == nil && releases != 1 {
				t.Errorf("unsafe upstream lease released %d times", releases)
			}
		})
	}
}

func TestForwardingHeaderTrust(t *testing.T) {
	for _, test := range []struct {
		peer, wantFor, wantProto, wantHost string
	}{
		{"127.0.0.1:42000", "203.0.113.8", "https", "public.example"},
		{"[::1]:42000", "203.0.113.8", "https", "public.example"},
		{"192.0.2.4:42000", "192.0.2.4", "http", "app.example"},
	} {
		t.Run(test.peer, func(t *testing.T) {
			headers := make(chan http.Header, 1)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				headers <- r.Header.Clone()
				w.WriteHeader(http.StatusNoContent)
			}))
			defer backend.Close()
			h := gateway.New(acquireFunc(func(_ context.Context, host string) (*orchestrator.Lease, error) {
				if host != "app.example" {
					t.Errorf("routing used forwarded host: %q", host)
				}
				return &orchestrator.Lease{Upstream: strings.TrimPrefix(backend.URL, "http://")}, nil
			}))
			req := httptest.NewRequest(http.MethodGet, "http://app.example/", nil)
			req.RemoteAddr = test.peer
			req.Header.Set("Forwarded", "for=attacker;host=attacker.example;proto=https")
			req.Header.Set("X-Forwarded-For", "203.0.113.8")
			req.Header.Set("X-Forwarded-Host", "public.example")
			req.Header.Set("X-Forwarded-Proto", "https")
			req.Header.Set("X-Forwarded-Prefix", "/attacker")
			response := httptest.NewRecorder()
			h.ServeHTTP(response, req)
			if response.Code != http.StatusNoContent {
				t.Fatalf("proxy status = %d", response.Code)
			}
			got := <-headers
			for name, want := range map[string]string{"X-Forwarded-For": test.wantFor, "X-Forwarded-Proto": test.wantProto, "X-Forwarded-Host": test.wantHost, "Forwarded": "", "X-Forwarded-Prefix": ""} {
				if got.Get(name) != want {
					t.Errorf("%s = %q, want %q", name, got.Get(name), want)
				}
			}
		})
	}
}

func TestLeaseCoversStreamingResponse(t *testing.T) {
	finish, released := make(chan struct{}), make(chan struct{})
	var finishOnce sync.Once
	defer finishOnce.Do(func() { close(finish) })
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		select {
		case <-finish:
			io.WriteString(w, "last\n")
		case <-r.Context().Done():
		}
	}))
	defer backend.Close()
	front := httptest.NewServer(gateway.New(acquireFunc(func(context.Context, string) (*orchestrator.Lease, error) {
		return &orchestrator.Lease{Upstream: strings.TrimPrefix(backend.URL, "http://"), Release: func() { close(released) }}, nil
	})))
	defer front.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(front.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "first\n" {
		t.Fatalf("first chunk = %q, %v", line, err)
	}
	select {
	case <-released:
		t.Fatal("lease released before stream completed")
	default:
	}
	finishOnce.Do(func() { close(finish) })
	if rest, err := io.ReadAll(reader); err != nil || string(rest) != "last\n" {
		t.Fatalf("remaining stream = %q, %v", rest, err)
	}
	wait(t, released)
}

func TestLeaseCoversWebSocketConnection(t *testing.T) {
	released := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		rw.Flush()
		// After the handshake, proxying is protocol-agnostic. Echo bytes to prove
		// the upgraded duplex connection remains open under the same lease.
		io.Copy(conn, rw)
	}))
	defer backend.Close()
	front := httptest.NewServer(gateway.New(acquireFunc(func(context.Context, string) (*orchestrator.Lease, error) {
		return &orchestrator.Lease{Upstream: strings.TrimPrefix(backend.URL, "http://"), Release: func() { close(released) }}, nil
	})))
	defer front.Close()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(front.URL, "http://"), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprint(conn, "GET /socket HTTP/1.1\r\nHost: app.example\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil || response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade response = %v, %v", response, err)
	}
	fmt.Fprint(conn, "echo through upgraded stream\n")
	if got, err := reader.ReadString('\n'); err != nil || got != "echo through upgraded stream\n" {
		t.Fatalf("upgrade echo = %q, %v", got, err)
	}
	select {
	case <-released:
		t.Fatal("lease released while upgraded stream was active")
	default:
	}
	conn.Close()
	wait(t, released)
}

func TestFailedRequestIsNotReplayed(t *testing.T) {
	var calls, releases atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	defer backend.Close()
	h := gateway.New(acquireFunc(func(context.Context, string) (*orchestrator.Lease, error) {
		return &orchestrator.Lease{Upstream: strings.TrimPrefix(backend.URL, "http://"), Release: func() { releases.Add(1) }}, nil
	}))
	req := httptest.NewRequest(http.MethodPost, "http://app.example/write", strings.NewReader("side effect"))
	req.Header.Set("Idempotency-Key", "must-still-not-replay")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, req)
	if response.Code != http.StatusServiceUnavailable || calls.Load() != 1 || releases.Load() != 1 {
		t.Fatalf("status/calls/releases = %d/%d/%d", response.Code, calls.Load(), releases.Load())
	}
}

func TestBrokenLeaseClosesStreamsAndWebSockets(t *testing.T) {
	backendDone := make(chan string, 2)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/socket" {
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
			rw.Flush()
			io.Copy(conn, rw)
			backendDone <- "socket"
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			backendDone <- "stream"
		case <-time.After(5 * time.Second):
			io.WriteString(w, "never broken\n")
		}
	}))
	defer backend.Close()
	var failures atomic.Int32
	type held struct {
		breakConnection context.CancelFunc
		released        chan struct{}
	}
	leases := make(chan held, 2)
	front := httptest.NewServer(gateway.New(acquireFunc(func(context.Context, string) (*orchestrator.Lease, error) {
		ctx, cancel := context.WithCancel(context.Background())
		released := make(chan struct{})
		leases <- held{cancel, released}
		return &orchestrator.Lease{
			Upstream: strings.TrimPrefix(backend.URL, "http://"), Context: ctx,
			Release:       func() { close(released) },
			ReportFailure: func(error) { failures.Add(1) },
		}, nil
	})))
	defer front.Close()

	// An upgraded WebSocket closes on both sides.
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(front.URL, "http://"), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprint(conn, "GET /socket HTTP/1.1\r\nHost: app.example\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	reader := bufio.NewReader(conn)
	if response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet}); err != nil || response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade response = %v, %v", response, err)
	}
	fmt.Fprint(conn, "ping\n")
	if got, err := reader.ReadString('\n'); err != nil || got != "ping\n" {
		t.Fatalf("echo = %q, %v", got, err)
	}
	socket := <-leases
	socket.breakConnection()
	if _, err := reader.ReadString('\n'); err == nil {
		t.Fatal("WebSocket stayed open after its lease was broken")
	}
	wait(t, socket.released)
	if got := <-backendDone; got != "socket" {
		t.Fatalf("backend finished %q", got)
	}

	// A streaming response is cut off, and the backend sees the cancellation.
	response, err := http.Get(front.URL + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	streamReader := bufio.NewReader(response.Body)
	if line, err := streamReader.ReadString('\n'); err != nil || line != "first\n" {
		t.Fatalf("first chunk = %q, %v", line, err)
	}
	stream := <-leases
	stream.breakConnection()
	if rest, err := io.ReadAll(streamReader); err == nil || strings.Contains(string(rest), "never broken") {
		t.Fatalf("stream completed after its lease was broken: %q, %v", rest, err)
	}
	wait(t, stream.released)
	if got := <-backendDone; got != "stream" {
		t.Fatalf("backend finished %q", got)
	}
	if failures.Load() != 0 {
		t.Fatal("breaking a lease was reported as an application failure")
	}
}

// wakingManager simulates an application that becomes ready when wake is closed.
func wakingManager(backend *httptest.Server, wake <-chan struct{}, releases *atomic.Int32) acquireFunc {
	return func(ctx context.Context, _ string) (*orchestrator.Lease, error) {
		select {
		case <-wake:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &orchestrator.Lease{Upstream: strings.TrimPrefix(backend.URL, "http://"), Release: func() { releases.Add(1) }}, nil
	}
}

func navigate(h http.Handler, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://app.example:8443/page?x=1", nil)
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, req)
	return response
}

func TestSlowWakeShowsResumingPageUntilReady(t *testing.T) {
	defer gateway.SetResumeTimings(20*time.Millisecond, 100*time.Millisecond)()
	var calls, releases atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("X-Site-Deploy-Resume") != "" {
			t.Error("status check header reached the application")
		}
		io.WriteString(w, "app page")
	}))
	defer backend.Close()
	wake := make(chan struct{})
	h := gateway.New(wakingManager(backend, wake, &releases))

	page := navigate(h)
	body := page.Body.String()
	if page.Code != http.StatusServiceUnavailable || page.Header().Get("Cache-Control") != "no-store" || page.Header().Get("Retry-After") != "2" ||
		!strings.HasPrefix(page.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("resuming page status/headers = %d %v", page.Code, page.Header())
	}
	for _, want := range []string{"Starting up", "app.example was asleep", `class="spinner`, `"X-Site-Deploy-Resume"`, `document.title = "app.example" + `} {
		if !strings.Contains(body, want) {
			t.Errorf("resuming page lacks %q", want)
		}
	}
	if strings.Contains(body, `class="failed"`) || strings.Contains(body, "8443") {
		t.Error("resuming page shows the failure state or the port")
	}

	check := navigate(h, "X-Site-Deploy-Resume", "1")
	if check.Code != http.StatusAccepted || check.Body.String() != `{"state":"starting"}`+"\n" || check.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status while starting = %d %q", check.Code, check.Body.String())
	}
	close(wake)
	check = navigate(h, "X-Site-Deploy-Resume", "1")
	if check.Code != http.StatusOK || check.Body.String() != `{"state":"running"}`+"\n" {
		t.Fatalf("status when ready = %d %q", check.Code, check.Body.String())
	}
	if calls.Load() != 0 || releases.Load() != 1 {
		t.Fatalf("status checks reached the application %d times and released %d leases", calls.Load(), releases.Load())
	}
	// The reload is proxied normally.
	if page := navigate(h); page.Code != http.StatusOK || page.Body.String() != "app page" {
		t.Fatalf("page after wake = %d %q", page.Code, page.Body.String())
	}
}

func TestOnlyPageLoadsGetTheResumingPage(t *testing.T) {
	defer gateway.SetResumeTimings(10*time.Millisecond, 10*time.Millisecond)()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, "app response")
	}))
	defer backend.Close()
	for _, test := range []struct{ name, method, mode string }{
		{"API client", http.MethodGet, ""},
		{"fetch", http.MethodGet, "cors"},
		{"form post", http.MethodPost, "navigate"},
		{"HEAD", http.MethodHead, "navigate"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var releases atomic.Int32
			wake := make(chan struct{})
			h := gateway.New(wakingManager(backend, wake, &releases))
			time.AfterFunc(100*time.Millisecond, func() { close(wake) })
			req := httptest.NewRequest(test.method, "http://app.example/submit", strings.NewReader("form"))
			if test.mode != "" {
				req.Header.Set("Sec-Fetch-Mode", test.mode)
			}
			response := httptest.NewRecorder()
			h.ServeHTTP(response, req)
			if response.Code != http.StatusOK || releases.Load() != 1 {
				t.Fatalf("status = %d, releases = %d; the request should wait for the wake", response.Code, releases.Load())
			}
		})
	}
}

func TestResumingPageReportsFailuresWithoutDetails(t *testing.T) {
	h := gateway.New(acquireFunc(func(context.Context, string) (*orchestrator.Lease, error) {
		return nil, fmt.Errorf("%w: private Docker error", orchestrator.ErrUnavailable)
	}))
	page := navigate(h)
	body := page.Body.String()
	if page.Code != http.StatusServiceUnavailable || !strings.Contains(body, `class="failed"`) || !strings.Contains(body, "Try again") {
		t.Fatalf("failure page = %d %q", page.Code, body)
	}
	if strings.Contains(body, "<script>") || strings.Contains(body, "http-equiv") {
		t.Error("failure page keeps reloading, which would restart a failing application")
	}
	check := navigate(h, "X-Site-Deploy-Resume", "1")
	if check.Code != http.StatusServiceUnavailable || check.Body.String() != `{"state":"failed"}`+"\n" {
		t.Fatalf("status after failure = %d %q", check.Code, check.Body.String())
	}
	for _, response := range []*httptest.ResponseRecorder{page, check} {
		if strings.Contains(response.Body.String(), "private Docker") {
			t.Error("internal error leaked in public response")
		}
	}

	h = gateway.New(acquireFunc(func(context.Context, string) (*orchestrator.Lease, error) {
		return nil, orchestrator.ErrNotFound
	}))
	if page, check := navigate(h), navigate(h, "X-Site-Deploy-Resume", "1"); page.Code != http.StatusNotFound || check.Code != http.StatusNotFound {
		t.Fatalf("unknown domain: page %d, check %d", page.Code, check.Code)
	}
}
