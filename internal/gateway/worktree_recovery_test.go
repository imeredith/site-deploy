package gateway_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"site-deploy/internal/gateway"
	"site-deploy/internal/orchestrator"
)

func TestWorktreeProxyPreservesOpaqueQueries(t *testing.T) {
	queries := []string{"tag=one&tag=two&literal=x;y", "z=last&signed=x%20y&x=1%2f2&empty=&plus=a+b", "opaque=%zz&part=x;y"}
	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			seen := make(chan string, 1)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- r.URL.RawQuery
				w.WriteHeader(http.StatusNoContent)
			}))
			defer backend.Close()
			h := gateway.New(acquireFunc(func(context.Context, string) (*orchestrator.Lease, error) {
				return &orchestrator.Lease{Upstream: strings.TrimPrefix(backend.URL, "http://")}, nil
			}))
			response := httptest.NewRecorder()
			h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://example.localhost/?"+query, nil))
			if response.Code != http.StatusNoContent {
				t.Fatalf("proxy status %d", response.Code)
			}
			actual := <-seen
			if actual != query {
				t.Fatalf("raw query %q became %q (status %d)", query, actual, response.Code)
			}
		})
	}
}

func TestWorktreeProxyReportsConnectionFailureWithoutReplay(t *testing.T) {
	var requests, reports, releases atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		io.Copy(io.Discard, r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	defer backend.Close()
	h := gateway.New(acquireFunc(func(context.Context, string) (*orchestrator.Lease, error) {
		return &orchestrator.Lease{
			Upstream: strings.TrimPrefix(backend.URL, "http://"),
			ReportFailure: func(err error) {
				if err == nil {
					t.Error("reported nil error")
				}
				if releases.Load() != 0 {
					t.Error("failure reported after releasing lease")
				}
				reports.Add(1)
			},
			Release: func() { releases.Add(1) },
		}, nil
	}))
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "http://example.localhost/write", strings.NewReader("must not replay")))
	if requests.Load() != 1 || reports.Load() != 1 || releases.Load() != 1 || response.Code != http.StatusServiceUnavailable {
		t.Fatalf("requests/reports/releases/status: %d/%d/%d/%d", requests.Load(), reports.Load(), releases.Load(), response.Code)
	}
}

func TestWorktreeProxyClientCancellationDoesNotInvalidate(t *testing.T) {
	var reports, releases atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer backend.Close()
	h := gateway.New(acquireFunc(func(context.Context, string) (*orchestrator.Lease, error) {
		return &orchestrator.Lease{Upstream: strings.TrimPrefix(backend.URL, "http://"), ReportFailure: func(error) { reports.Add(1) }, Release: func() { releases.Add(1) }}, nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodGet, "http://example.localhost/", nil).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), request)
	if reports.Load() != 0 || releases.Load() != 1 {
		t.Fatalf("cancelled request invalidated app: reports=%d releases=%d", reports.Load(), releases.Load())
	}
}
