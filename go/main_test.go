package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/patrickmn/go-cache"
)

// newTestProxy stands up a stub mas server that answers
// POST /internal/preview/resolve with the given Resolved payload, and
// returns a fully-wired Proxy (via NewProxy, so the real ReverseProxy/
// ErrorHandler are exercised) pointed at it. The mas stub is closed
// automatically via t.Cleanup.
func newTestProxy(t *testing.T, resolved Resolved) *Proxy {
	t.Helper()

	mas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resolved); err != nil {
			t.Fatalf("failed to encode mas stub response: %v", err)
		}
	}))
	t.Cleanup(mas.Close)

	config := &Config{
		MasBaseURL:           mas.URL,
		PreviewResolveSecret: "test-secret",
		BaseDomain:           "preview.test",
	}
	return NewProxy(config)
}

func TestPreviewIdFromHost(t *testing.T) {
	const baseDomain = "brainbaselabs.space"

	tests := []struct {
		name string
		host string
		want string
	}{
		{
			name: "simple previewId label",
			host: "abc.brainbaselabs.space",
			want: "abc",
		},
		{
			name: "bare base domain has no label",
			host: "brainbaselabs.space",
			want: "",
		},
		{
			name: "foreign domain is rejected",
			host: "evil.com",
			want: "",
		},
		{
			name: "multi-label host is rejected",
			host: "a.b.brainbaselabs.space",
			want: "",
		},
		{
			name: "port suffix is stripped",
			host: "abc.brainbaselabs.space:443",
			want: "abc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := previewIdFromHost(tt.host, baseDomain)
			if got != tt.want {
				t.Errorf("previewIdFromHost(%q, %q) = %q, want %q", tt.host, baseDomain, got, tt.want)
			}
		})
	}
}

func TestResolveMapsMas410ToResolveError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/preview/resolve" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("X-Internal-Secret"); got != "test-secret" {
			t.Errorf("X-Internal-Secret = %q, want %q", got, "test-secret")
		}
		w.WriteHeader(http.StatusGone) // 410
		w.Write([]byte(`{"detail":"preview id expired"}`))
	}))
	defer server.Close()

	p := &Proxy{
		cache: cache.New(2*time.Minute, 5*time.Minute),
		config: &Config{
			MasBaseURL:           server.URL,
			PreviewResolveSecret: "test-secret",
			BaseDomain:           "brainbaselabs.space",
		},
		apiClient: &http.Client{Timeout: 5 * time.Second},
	}

	_, err := p.resolve(context.Background(), "expiredid")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	rerr, ok := err.(*resolveError)
	if !ok {
		t.Fatalf("expected *resolveError, got %T: %v", err, err)
	}
	if rerr.status != http.StatusGone {
		t.Errorf("resolveError.status = %d, want %d", rerr.status, http.StatusGone)
	}
}

// TestProxyInjectsResolvedTokenHeaderProviderAgnostically guards the
// provider-agnostic header injection in director(): whatever token/header
// mas resolves must be set verbatim on the upstream request. A Daytona-
// hardcoded implementation (e.g. always setting x-daytona-preview-token)
// would fail this test, since the resolved header here is e2b's.
func TestProxyInjectsResolvedTokenHeaderProviderAgnostically(t *testing.T) {
	var gotHeader string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("e2b-traffic-access-token")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	proxy := newTestProxy(t, Resolved{
		UpstreamURL: upstream.URL,
		Token:       "e2b-tok",
		TokenHeader: "e2b-traffic-access-token",
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "abc.preview.test"
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if gotHeader != "e2b-tok" {
		t.Errorf("upstream received e2b-traffic-access-token = %q, want %q", gotHeader, "e2b-tok")
	}
}

// TestProxyPassesNon200UpstreamResponsesThrough guards the fix for the
// non-200-response-clobbering bug: now that mas resolve + server-side token
// injection means the upstream is the user's real app, its redirects,
// not-founds, etc. must reach the client unchanged instead of being
// replaced with the branded error page.
func TestProxyPassesNon200UpstreamResponsesThrough(t *testing.T) {
	t.Run("302 redirect passes through", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "https://example.com/somewhere")
			w.WriteHeader(http.StatusFound)
		}))
		defer upstream.Close()

		proxy := newTestProxy(t, Resolved{UpstreamURL: upstream.URL})

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "abc.preview.test"
		rec := httptest.NewRecorder()

		proxy.ServeHTTP(rec, req)

		if rec.Code != http.StatusFound {
			t.Errorf("status = %d, want %d (body: %s)", rec.Code, http.StatusFound, rec.Body.String())
		}
		if got := rec.Header().Get("Location"); got != "https://example.com/somewhere" {
			t.Errorf("Location = %q, want %q", got, "https://example.com/somewhere")
		}
	})

	t.Run("app's own 404 passes through with its body", func(t *testing.T) {
		const appBody = "this app says: not found here"
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(appBody))
		}))
		defer upstream.Close()

		proxy := newTestProxy(t, Resolved{UpstreamURL: upstream.URL})

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "abc.preview.test"
		rec := httptest.NewRecorder()

		proxy.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
		}
		if got := rec.Body.String(); got != appBody {
			t.Errorf("body = %q, want %q (i.e. NOT the branded error page)", got, appBody)
		}
	})
}
