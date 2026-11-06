package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
		{
			name: "absolute (FQDN) host with trailing dot still matches",
			host: "abc.brainbaselabs.space.",
			want: "abc",
		},
		{
			name: "absolute host with trailing dot and port",
			host: "abc.brainbaselabs.space.:443",
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

// TestPreviewIdFromHostTrailingDotBaseDomain guards that a PREVIEW_BASE_DOMAIN
// configured in absolute (trailing-dot) form still resolves ordinary hosts,
// rather than rejecting every request because the suffix never matches.
func TestPreviewIdFromHostTrailingDotBaseDomain(t *testing.T) {
	if got := previewIdFromHost("abc.brainbaselabs.space", "brainbaselabs.space."); got != "abc" {
		t.Errorf("previewIdFromHost with trailing-dot baseDomain = %q, want %q", got, "abc")
	}
}

// TestResolveHonorsMasCacheTTL guards that the proxy caches a resolution for
// the window mas dictates via cache_ttl_s, so revocation/expiry take effect
// within that window instead of a fixed 2-minute lag.
func TestResolveHonorsMasCacheTTL(t *testing.T) {
	t.Run("honors mas cache_ttl_s", func(t *testing.T) {
		p := newTestProxy(t, Resolved{UpstreamURL: "http://x", CacheTTLS: 10})
		if _, err := p.resolve(context.Background(), "abc"); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		_, exp, found := p.cache.GetWithExpiration("abc")
		if !found {
			t.Fatal("expected resolution to be cached")
		}
		if ttl := time.Until(exp); ttl <= 0 || ttl > 20*time.Second {
			t.Errorf("cache ttl = %v, want ~10s (honoring mas cache_ttl_s, not the 2m default)", ttl)
		}
	})

	t.Run("falls back to default when mas omits cache_ttl_s", func(t *testing.T) {
		p := newTestProxy(t, Resolved{UpstreamURL: "http://x"}) // CacheTTLS == 0
		if _, err := p.resolve(context.Background(), "abc"); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		_, exp, found := p.cache.GetWithExpiration("abc")
		if !found {
			t.Fatal("expected resolution to be cached")
		}
		ttl := time.Until(exp)
		if ttl <= 0 || ttl > defaultResolveCacheTTL+5*time.Second {
			t.Errorf("cache ttl = %v, want ~%v (default)", ttl, defaultResolveCacheTTL)
		}
	})

	t.Run("caps an oversized cache_ttl_s", func(t *testing.T) {
		p := newTestProxy(t, Resolved{UpstreamURL: "http://x", CacheTTLS: 86400})
		if _, err := p.resolve(context.Background(), "abc"); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		_, exp, found := p.cache.GetWithExpiration("abc")
		if !found {
			t.Fatal("expected resolution to be cached")
		}
		if ttl := time.Until(exp); ttl > maxResolveCacheTTL {
			t.Errorf("cache ttl = %v, want <= %v (capped)", ttl, maxResolveCacheTTL)
		}
	})
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

// TestProxyDoesNotForwardMasSecretUpstream guards against the internal
// PREVIEW_RESOLVE_SECRET (sent to mas as X-Internal-Secret) ever leaking to
// the upstream user app. director() should only ever set the resolved
// token/tokenHeader from mas's response, never the proxy's own resolve
// credentials, on the outgoing upstream request.
func TestProxyDoesNotForwardMasSecretUpstream(t *testing.T) {
	const secret = "test-secret"

	var upstreamHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	mas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Internal-Secret"); got != secret {
			t.Errorf("mas received X-Internal-Secret = %q, want %q", got, secret)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Resolved{
			UpstreamURL: upstream.URL,
			Token:       "sandbox-tok",
			TokenHeader: "X-Daytona-Preview-Token",
		})
	}))
	defer mas.Close()

	config := &Config{
		MasBaseURL:           mas.URL,
		PreviewResolveSecret: secret,
		BaseDomain:           "preview.test",
	}
	proxy := NewProxy(config)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "abc.preview.test"
	rec := httptest.NewRecorder()

	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if upstreamHeaders == nil {
		t.Fatal("upstream never received a request")
	}

	if got := upstreamHeaders.Get("X-Internal-Secret"); got != "" {
		t.Errorf("upstream received X-Internal-Secret header = %q, want empty", got)
	}
	for name, values := range upstreamHeaders {
		for _, v := range values {
			if v == secret {
				t.Errorf("upstream header %q carried the mas resolve secret verbatim: %q", name, v)
			}
		}
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

// TestCleanRequestPathResolvesTraversal covers the path-cleaning guard directly.
//
// Cloudflare sandbox upstreams put the sandbox id and the port in the path
// rather than the host, so a "..", bare or percent-encoded, would otherwise let
// a shared preview link reach a different port or a different sandbox on the
// same upstream.
func TestCleanRequestPathResolvesTraversal(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain path is unchanged", "/app/index.html", "/app/index.html"},
		{"root stays root", "/", "/"},
		{"empty becomes root", "", "/"},
		{"trailing slash is preserved", "/app/", "/app/"},
		{"single dot is removed", "/app/./index.html", "/app/index.html"},
		{"parent segment is resolved", "/app/sub/../index.html", "/app/index.html"},
		{"traversal above root is clamped", "/../../etc/passwd", "/etc/passwd"},
		{"repeated traversal is clamped", "/a/../../../b", "/b"},
		{"dots inside a name are untouched", "/a..b/c", "/a..b/c"},
		{"terminal dot keeps directory semantics", "/docs/.", "/docs/"},
		{"terminal parent keeps directory semantics", "/docs/sub/..", "/docs/"},
		{"terminal parent at root stays root", "/docs/..", "/"},
		{"dotfile is untouched", "/.env", "/.env"},
		{"relative path is absolutised", "app/index.html", "/app/index.html"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cleanRequestPath(tc.in); got != tc.want {
				t.Fatalf("cleanRequestPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDirectorCannotEscapeUpstreamBasePath is the property that matters: whatever
// the client sends, the forwarded path stays under the resolved upstream path.
func TestDirectorCannotEscapeUpstreamBasePath(t *testing.T) {
	const base = "/v1/sandbox/m-abc12345/port/8421"
	p := newTestProxy(t, Resolved{
		UpstreamURL: "https://bridge.example" + base,
		Token:       "tok",
		TokenHeader: "x-brainbase-sandbox-token",
	})

	// These are client paths as the proxy sees them: the preview host label has
	// already been resolved to the upstream base, so what remains is whatever the
	// caller put after the hostname.
	hostile := []string{
		"/../8422/admin",
		"/../../m-other99/port/8421/",
		"/%2e%2e/8422/admin",
		"/a/../../../../etc/passwd",
		"/app/../../../8422/",
	}

	for _, raw := range hostile {
		t.Run(raw, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, raw, nil)
			resolved, err := p.resolve(context.Background(), "preview-id")
			if err != nil {
				t.Fatalf("resolve failed: %v", err)
			}
			ctx := context.WithValue(req.Context(), resolvedContextKey, resolved)
			req = req.WithContext(ctx)

			p.director(req)

			if !strings.HasPrefix(req.URL.Path, base) {
				t.Fatalf("forwarded path %q escaped the upstream base %q", req.URL.Path, base)
			}
			if strings.Contains(req.URL.Path, "..") {
				t.Fatalf("forwarded path %q still contains a traversal segment", req.URL.Path)
			}
			// RawPath must be cleared, or the original escaped form is what
			// actually goes on the wire when the URL is re-encoded. Asserting
			// only on URL.Path would let the safeguard be deleted silently.
			if req.URL.RawPath != "" {
				t.Fatalf("RawPath %q survived; the escaped form would be sent instead", req.URL.RawPath)
			}
			if escaped := req.URL.EscapedPath(); !strings.HasPrefix(escaped, base) ||
				strings.Contains(escaped, "..") || strings.Contains(strings.ToLower(escaped), "%2e") {
				t.Fatalf("serialized path %q escaped the upstream base or kept a traversal", escaped)
			}
		})
	}
}

// --- Interfaces dashboard hosts -------------------------------------------
//
// A dashboard is reachable only if a hyphenated host is recognised, forwarded to
// the gateway, and arrives there still carrying the host the viewer asked for.
// The first of those is a pure function and is tested as one; the other two are
// only meaningful against a real HTTP server, so every test below that claims
// something about the wire stands up an httptest server and reads what it
// received.

func TestIsInterfaceLabel(t *testing.T) {
	tests := []struct {
		name  string
		label string
		want  bool
	}{
		{name: "a generated slug", label: "uhbn9-egrjs", want: true},
		{name: "its frame sibling", label: "app--uhbn9-egrjs", want: true},
		{name: "a readable slug", label: "ops-console", want: true},
		{name: "three hyphen groups", label: "a-b-c", want: true},
		{name: "the shortest legal slug", label: "a-b", want: true},
		{name: "a slug that merely starts with app", label: "app-console", want: true},

		// An interface's preview address, `<slug>-preview`, is served by the same
		// gateway. mas refuses a slug that ends in -preview, and caps slugs at 50
		// so the preview label stays within the 58 characters this proxy accepts.
		{name: "a preview host", label: "uhbn9-egrjs-preview", want: true},
		{name: "the longest slug's preview host", label: "a-" + strings.Repeat("b", 48) + "-preview", want: true},

		// Everything the preview path owns, and must keep owning.
		{name: "a preview id", label: "abc", want: false},
		{name: "a long preview id", label: "b7f3a9c1d2e4", want: false},
		{name: "no label at all", label: "", want: false},

		// Hyphenated names that are not dashboards.
		{name: "punycode", label: "xn--80ak6aa92e", want: false},
		{name: "an underscore service record", label: "_acme-challenge", want: false},
		{name: "a doubled hyphen", label: "ops--console", want: false},
		{name: "a leading hyphen", label: "-ops-console", want: false},
		{name: "a trailing hyphen", label: "ops-console-", want: false},
		{name: "uppercase, which a DNS label is not", label: "Ops-Console", want: false},

		// The reserved frame prefix, in the shapes mas refuses to allocate.
		{name: "the frame prefix alone", label: "app--", want: false},
		{name: "the frame prefix doubled", label: "app--app--ops-console", want: false},

		// Length, which is what makes both labels fit one DNS label each.
		{name: "one character short", label: "ab", want: false},
		{name: "the longest slug that fits app-- too", label: "a-" + strings.Repeat("b", 56), want: true},
		{name: "one character too long", label: "a-" + strings.Repeat("b", 57), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isInterfaceLabel(tt.label); got != tt.want {
				t.Errorf("isInterfaceLabel(%q) = %v, want %v", tt.label, got, tt.want)
			}
		})
	}
}

// TestInterfaceLabelsAndPreviewIdsAreDisjoint is the claim the routing order
// rests on. Asserted over the whole alphabet a DNS label may contain rather than
// over a handful of examples, because "these two predicates cannot both be true"
// is not a property a list of names can establish.
func TestInterfaceLabelsAndPreviewIdsAreDisjoint(t *testing.T) {
	const alphabet = "ab9-_"
	var label []byte
	var walk func(depth int)
	checked := 0
	walk = func(depth int) {
		if depth == 0 {
			candidate := string(label)
			checked++
			if isInterfaceLabel(candidate) && previewIDRegex.MatchString(candidate) {
				t.Fatalf("label %q is claimed by both routes", candidate)
			}
			return
		}
		for i := 0; i < len(alphabet); i++ {
			label = append(label, alphabet[i])
			walk(depth - 1)
			label = label[:len(label)-1]
		}
	}
	for length := 1; length <= 6; length++ {
		walk(length)
	}
	if checked < 15000 {
		t.Fatalf("only %d candidates checked; the walk is not covering what it claims", checked)
	}
}

// newInterfaceProxy stands up a stub gateway that records the request it was
// given, plus a mas stub that fails the test if it is ever called: resolving a
// dashboard host is the gateway's job, and a proxy-side resolve would show up
// here as a call.
func newInterfaceProxy(t *testing.T, handler http.HandlerFunc) *Proxy {
	t.Helper()

	gateway := httptest.NewServer(handler)
	t.Cleanup(gateway.Close)

	mas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("mas was called for a dashboard host: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(mas.Close)

	return NewProxy(&Config{
		MasBaseURL:           mas.URL,
		PreviewResolveSecret: "test-secret",
		BaseDomain:           "brainbaselabs.space",
		InterfacesGatewayURL: interfacesGatewayURL(gateway.URL),
	})
}

func TestDashboardHostReachesTheGatewayCarryingTheViewersHost(t *testing.T) {
	for _, host := range []string{
		"uhbn9-egrjs.brainbaselabs.space",
		"app--uhbn9-egrjs.brainbaselabs.space",
	} {
		t.Run(host, func(t *testing.T) {
			var seen http.Header
			var seenHost, seenTarget string
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.Header.Clone()
				seenHost = r.Host
				seenTarget = r.URL.RequestURI()
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("<title>Dashboard</title>"))
			}))
			defer gateway.Close()

			proxy := NewProxy(&Config{
				MasBaseURL:           "http://mas.invalid",
				PreviewResolveSecret: "test-secret",
				BaseDomain:           "brainbaselabs.space",
				InterfacesGatewayURL: interfacesGatewayURL(gateway.URL),
			})

			req := httptest.NewRequest(http.MethodGet, "/dash?tab=1", nil)
			req.Host = host
			rec := httptest.NewRecorder()
			proxy.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
			}
			if got := seen.Get("X-Forwarded-Host"); got != host {
				t.Errorf("gateway saw X-Forwarded-Host = %q, want %q", got, host)
			}
			// The Host the gateway is reached by is its own, or it would not
			// route; the viewer's host survives only in the forwarded header.
			if want := interfacesGatewayURL(gateway.URL).Host; seenHost != want {
				t.Errorf("gateway saw Host = %q, want %q", seenHost, want)
			}
			if seenTarget != "/dash?tab=1" {
				t.Errorf("gateway saw target = %q, want %q", seenTarget, "/dash?tab=1")
			}
		})
	}
}

// TestAClientCannotChooseWhichDashboardItIsTreatedAs is the test this branch
// exists to survive. The gateway decides which interface is being asked for, and
// whether this viewer may see it, from the forwarded host; a client that could
// set that header would be choosing which dashboard its own session is checked
// against.
func TestAClientCannotChooseWhichDashboardItIsTreatedAs(t *testing.T) {
	spoofs := []struct {
		name   string
		header string
		value  string
	}{
		{name: "the header itself", header: "X-Forwarded-Host", value: "payroll-hq.brainbaselabs.space"},
		{name: "lowercased", header: "x-forwarded-host", value: "payroll-hq.brainbaselabs.space"},
		{name: "as a proxy chain", header: "X-Forwarded-Host", value: "payroll-hq.brainbaselabs.space, ops-console.brainbaselabs.space"},
		{name: "with a comma appended", header: "X-Forwarded-Host", value: "ops-console.brainbaselabs.space, payroll-hq.brainbaselabs.space"},
		{name: "the RFC 7239 header", header: "Forwarded", value: "host=payroll-hq.brainbaselabs.space"},
		{name: "an aliased header", header: "X-Original-Host", value: "payroll-hq.brainbaselabs.space"},
		{name: "another aliased header", header: "X-Host", value: "payroll-hq.brainbaselabs.space"},
	}

	for _, spoof := range spoofs {
		t.Run(spoof.name, func(t *testing.T) {
			var seen http.Header
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.Header.Clone()
				w.WriteHeader(http.StatusOK)
			}))
			defer gateway.Close()

			proxy := NewProxy(&Config{
				MasBaseURL:           "http://mas.invalid",
				PreviewResolveSecret: "test-secret",
				BaseDomain:           "brainbaselabs.space",
				InterfacesGatewayURL: interfacesGatewayURL(gateway.URL),
			})

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Host = "ops-console.brainbaselabs.space"
			req.Header.Set(spoof.header, spoof.value)
			rec := httptest.NewRecorder()
			proxy.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := seen.Get("X-Forwarded-Host"); got != "ops-console.brainbaselabs.space" {
				t.Errorf("gateway saw X-Forwarded-Host = %q, want the host the request arrived on", got)
			}
			for _, name := range hostBearingHeaders {
				if name == "X-Forwarded-Host" {
					continue
				}
				if got := seen.Get(name); got != "" {
					t.Errorf("gateway saw %s = %q, want it stripped", name, got)
				}
			}
		})
	}
}

// TestAPortOrTrailingDotDoesNotReachTheGateway guards the rebuild rather than a
// sanitise: the forwarded host is assembled from the matched label and the
// configured base domain, so nothing the client wrote can arrive even in part.
func TestAPortOrTrailingDotDoesNotReachTheGateway(t *testing.T) {
	for _, host := range []string{
		"ops-console.brainbaselabs.space:443",
		"ops-console.brainbaselabs.space.",
		"OPS-CONSOLE.brainbaselabs.space",
		"ops-console.brainbaselabs.space.:8080",
	} {
		t.Run(host, func(t *testing.T) {
			var seen string
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.Header.Get("X-Forwarded-Host")
				w.WriteHeader(http.StatusOK)
			}))
			defer gateway.Close()

			proxy := NewProxy(&Config{
				MasBaseURL:           "http://mas.invalid",
				PreviewResolveSecret: "test-secret",
				BaseDomain:           "brainbaselabs.space",
				InterfacesGatewayURL: interfacesGatewayURL(gateway.URL),
			})

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Host = host
			rec := httptest.NewRecorder()
			proxy.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if seen != "ops-console.brainbaselabs.space" {
				t.Errorf("gateway saw X-Forwarded-Host = %q, want the canonical host", seen)
			}
		})
	}
}

// TestTheProxyDoesNotResolveADashboardHost is the "who resolves it" decision,
// asserted rather than described. The mas stub fails the test if it is called at
// all, so a proxy-side resolution cannot be added without this going red.
func TestTheProxyDoesNotResolveADashboardHost(t *testing.T) {
	proxy := newInterfaceProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "ops-console.brainbaselabs.space"
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if _, found := proxy.cache.Get("ops-console"); found {
		t.Error("a dashboard host was cached as a preview resolution")
	}
}

// TestTheGatewaysOwnRefusalReachesTheViewer: an unknown dashboard host fails
// closed, and it is the gateway's 404 that is served, not this proxy's page.
// Both halves matter - a proxy that replaced the gateway's answer would show a
// person a page about previews.
func TestTheGatewaysOwnRefusalReachesTheViewer(t *testing.T) {
	const gatewayRefusal = "This dashboard is not available"
	proxy := newInterfaceProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(gatewayRefusal))
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "no-such-dashboard.brainbaselabs.space"
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if got := rec.Body.String(); got != gatewayRefusal {
		t.Errorf("body = %q, want the gateway's own refusal", got)
	}
}

func TestADashboardHostIsRefusedWhenNoGatewayIsConfigured(t *testing.T) {
	mas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("mas was called with no gateway configured: %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer mas.Close()

	proxy := NewProxy(&Config{
		MasBaseURL:           mas.URL,
		PreviewResolveSecret: "test-secret",
		BaseDomain:           "brainbaselabs.space",
		// InterfacesGatewayURL deliberately nil.
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "ops-console.brainbaselabs.space"
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (fail closed)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "Preview Unavailable") {
		t.Error("a dashboard host was refused with the sandbox-preview error page")
	}
}

func TestAnUnreachableGatewayIs502AndNotThePreviewPage(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := gateway.URL
	gateway.Close() // nothing is listening there now

	proxy := NewProxy(&Config{
		MasBaseURL:           "http://mas.invalid",
		PreviewResolveSecret: "test-secret",
		BaseDomain:           "brainbaselabs.space",
		InterfacesGatewayURL: interfacesGatewayURL(url),
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "ops-console.brainbaselabs.space"
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "Preview Unavailable") {
		t.Error("a dashboard host got the sandbox-preview error page")
	}
}

// TestTheBodyAndMethodSurviveTheHop: the dashboard's data path is POST
// /__bb/api/{action} with a JSON body, so a hop that dropped either would leave
// a dashboard that loads and does nothing.
func TestTheBodyAndMethodSurviveTheHop(t *testing.T) {
	var seenMethod, seenPath string
	var seenBody []byte
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMethod, seenPath = r.Method, r.URL.Path
		seenBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer gateway.Close()

	proxy := NewProxy(&Config{
		MasBaseURL:           "http://mas.invalid",
		PreviewResolveSecret: "test-secret",
		BaseDomain:           "brainbaselabs.space",
		InterfacesGatewayURL: interfacesGatewayURL(gateway.URL),
	})

	req := httptest.NewRequest(http.MethodPost, "/__bb/api/messages.send",
		strings.NewReader(`{"thread_id":"thr_1"}`))
	req.Host = "app--ops-console.brainbaselabs.space"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if seenMethod != http.MethodPost || seenPath != "/__bb/api/messages.send" {
		t.Errorf("gateway saw %s %s, want POST /__bb/api/messages.send", seenMethod, seenPath)
	}
	if string(seenBody) != `{"thread_id":"thr_1"}` {
		t.Errorf("gateway saw body %q, want the JSON that was sent", seenBody)
	}
}

func TestInterfacesGatewayURLRefusesAnythingButABareOrigin(t *testing.T) {
	if got := interfacesGatewayURL(""); got != nil {
		t.Errorf("empty INTERFACES_GATEWAY_URL = %v, want nil", got)
	}
	if got := interfacesGatewayURL("  https://gw.internal:8443  "); got == nil || got.Host != "gw.internal:8443" {
		t.Errorf("a padded origin did not parse: %v", got)
	}
	// A trailing slash is the one path that is really no path.
	if got := interfacesGatewayURL("https://gw.internal/"); got == nil || got.Path != "" {
		t.Errorf("a trailing slash was not normalised away: %v", got)
	}
}

// TestAnEventStreamIsNotBufferedOnTheWayToTheViewer: the dashboard's Activity
// panel is one long-lived `GET /__bb/api/events.subscribe`, so a hop that
// buffered it would leave a panel that shows everything at once when the stream
// finally closes, and nothing before then. Run over a real listener and a real
// client rather than a recorder, because a recorder cannot tell a flush from a
// buffer.
func TestAnEventStreamIsNotBufferedOnTheWayToTheViewer(t *testing.T) {
	released := make(chan struct{})
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("event: first\ndata: 1\n\n"))
		w.(http.Flusher).Flush()
		// The second event is written only once the client has been seen to
		// receive the first, so a pass cannot come from lucky timing.
		<-released
		_, _ = w.Write([]byte("event: second\ndata: 2\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer gateway.Close()

	front := httptest.NewServer(NewProxy(&Config{
		MasBaseURL:           "http://mas.invalid",
		PreviewResolveSecret: "test-secret",
		BaseDomain:           "brainbaselabs.space",
		InterfacesGatewayURL: interfacesGatewayURL(gateway.URL),
	}))
	defer front.Close()

	req, err := http.NewRequest(http.MethodGet, front.URL+"/__bb/api/events.subscribe", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "app--ops-console.brainbaselabs.space"
	// A deadline on the whole exchange, so a hop that buffers fails here in
	// seconds instead of hanging until the package timeout and looking like a
	// broken test rather than a broken proxy.
	client := front.Client()
	client.Timeout = 4 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		close(released)
		t.Fatalf("stream request never returned headers, which is what buffering looks like: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}

	buf := make([]byte, 64)
	type read struct {
		n   int
		err error
	}
	got := make(chan read, 1)
	go func() {
		n, err := resp.Body.Read(buf)
		got <- read{n, err}
	}()

	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("first read: %v", r.err)
		}
		if want := "event: first"; !strings.Contains(string(buf[:r.n]), want) {
			t.Errorf("first chunk = %q, want it to contain %q", buf[:r.n], want)
		}
	case <-time.After(3 * time.Second):
		close(released)
		t.Fatal("the first event never arrived: the stream is being buffered")
	}
	close(released)
}

// TestAConnectionHeaderCannotStripTheViewersHost: ReverseProxy deletes every
// header a client names in `Connection` after the director has run, so a client
// naming X-Forwarded-Host there would remove the value the director set and the
// gateway would see no viewer host at all.
func TestAConnectionHeaderCannotStripTheViewersHost(t *testing.T) {
	const host = "ops-console.brainbaselabs.space"
	for _, connection := range []string{
		"X-Forwarded-Host",
		"keep-alive, x-forwarded-host",
		"Forwarded, X-Host, X-Original-Host, X-Forwarded-Host",
	} {
		t.Run(connection, func(t *testing.T) {
			var seen http.Header
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.Header.Clone()
				w.WriteHeader(http.StatusOK)
			}))
			defer gateway.Close()

			proxy := NewProxy(&Config{
				MasBaseURL:           "http://mas.invalid",
				PreviewResolveSecret: "test-secret",
				BaseDomain:           "brainbaselabs.space",
				InterfacesGatewayURL: interfacesGatewayURL(gateway.URL),
			})
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Host = host
			req.Header.Set("Connection", connection)
			rec := httptest.NewRecorder()
			proxy.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := seen.Get("X-Forwarded-Host"); got != host {
				t.Errorf("gateway saw X-Forwarded-Host = %q, want %q", got, host)
			}
		})
	}
}

func TestDropConnectionTokensKeepsEverythingItWasNotAskedToDrop(t *testing.T) {
	h := http.Header{}
	h.Add("Connection", "Upgrade, X-Forwarded-Host")
	h.Add("Connection", "keep-alive,x-host")
	dropConnectionTokens(h, hostBearingHeaders)
	if got := h.Values("Connection"); len(got) != 1 || got[0] != "Upgrade, keep-alive" {
		t.Errorf("Connection = %q, want [\"Upgrade, keep-alive\"]", got)
	}

	only := http.Header{"Connection": {"X-Forwarded-Host"}}
	dropConnectionTokens(only, hostBearingHeaders)
	if _, ok := only["Connection"]; ok {
		t.Errorf("an emptied Connection header was kept: %q", only.Values("Connection"))
	}
}

func TestParseInterfacesGatewayURLRefusesWhatCannotBeDialled(t *testing.T) {
	for _, raw := range []string{
		"https://gw.internal:99999",
		"https://gw.internal:0",
		"https://gw.internal:",
		"http://:8080",
		"ftp://gw.internal",
		"https://gw.internal/base",
		"https://gw.internal?x=1",
		"https://user:pass@gw.internal",
	} {
		if got, err := parseInterfacesGatewayURL(raw); err == nil {
			t.Errorf("%q was accepted as %v, want an error", raw, got)
		}
	}
	for raw, wantHost := range map[string]string{
		"http://brainbase-interfaces-gateway:8080": "brainbase-interfaces-gateway:8080",
		"https://gw.internal:65535":                "gw.internal:65535",
		"https://gw.internal":                      "gw.internal",
	} {
		got, err := parseInterfacesGatewayURL(raw)
		if err != nil || got == nil || got.Host != wantHost {
			t.Errorf("%q = %v, %v; want host %q", raw, got, err, wantHost)
		}
	}
	if got, err := parseInterfacesGatewayURL("   "); got != nil || err != nil {
		t.Errorf("blank = %v, %v; want unset", got, err)
	}
}

// ---------------------------------------------------------------------------
// ENG-1504 U24 steps 7-8 (PROXY_RESILIENCE). Every test states the switch it
// runs under; the default everywhere else in this file is unset = off, so the
// pre-existing tests keep pinning today's behaviour.
// ---------------------------------------------------------------------------

// setResilience pins PROXY_RESILIENCE for one test via the environment, the
// way NewProxy reads it in production.
func setResilience(t *testing.T, on bool) {
	t.Helper()
	if on {
		t.Setenv("PROXY_RESILIENCE", "1")
	} else {
		t.Setenv("PROXY_RESILIENCE", "")
	}
}

// newScriptedProxy builds a Proxy via NewProxy (real reverse proxies, real
// transport) against a counting local mas stub whose responses `handler`
// scripts. The mas stub asserts the resolve request shape on every POST.
func newScriptedProxy(t *testing.T, on bool, handler http.HandlerFunc) (*Proxy, *int32) {
	t.Helper()
	setResilience(t, on)
	var posts int32
	mas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected mas method: %s", r.Method)
		}
		if r.URL.Path != "/internal/preview/resolve" {
			t.Errorf("unexpected mas path: %s", r.URL.Path)
		}
		if got := r.Header.Get("X-Internal-Secret"); got != "test-secret" {
			t.Errorf("X-Internal-Secret = %q, want %q", got, "test-secret")
		}
		atomic.AddInt32(&posts, 1)
		handler(w, r)
	}))
	t.Cleanup(mas.Close)
	p := NewProxy(&Config{
		MasBaseURL:           mas.URL,
		PreviewResolveSecret: "test-secret",
		BaseDomain:           "preview.test",
	})
	return p, &posts
}

// newUpstream stands up a real local app for the proxy to forward to.
func newUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "upstream ok")
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// deadMasURL returns a URL whose server has already been closed, so dialing
// it is a connection refused - mas down at the wire level.
func deadMasURL(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	server.Close()
	return server.URL
}

// newDeadMasProxy is newScriptedProxy with mas unreachable. POSTs cannot be
// counted against a closed port; the counter returned counts dials observed
// by an instrumented transport is not needed - connection refused is the
// scripted failure itself.
func newDeadMasProxy(t *testing.T, on bool) *Proxy {
	t.Helper()
	setResilience(t, on)
	p := NewProxy(&Config{
		MasBaseURL:           deadMasURL(t),
		PreviewResolveSecret: "test-secret",
		BaseDomain:           "preview.test",
	})
	return p
}

func previewTestRequest(previewID string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = previewID + ".preview.test"
	return req
}

func servePreview(p *Proxy, previewID string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, previewTestRequest(previewID))
	return rec
}

// TestResolveRetries502ThenSucceeds is the U24 step 7 scenario: a local mas
// stub returns 502 once (deploy blip), then 200; with PROXY_RESILIENCE on the
// retry rides it out and the preview request reaches the real upstream.
func TestResolveRetries502ThenSucceeds(t *testing.T) {
	setResilience(t, true)
	upstream := newUpstream(t)

	resolvedBody := func() []byte {
		b, _ := json.Marshal(Resolved{UpstreamURL: upstream.URL})
		return b
	}

	var masPosts int32
	p, posts := newScriptedProxy(t, true, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&masPosts, 1) == 1 {
			// contract/fixtures/daytona-5xx.json, step 1 response verbatim.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"error":{"code":"api_error","message":"upstream provider unavailable","status":502}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(resolvedBody())
	})

	rec := servePreview(p, "abc")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "upstream ok" {
		t.Errorf("body = %q, want the upstream's", got)
	}
	if n := atomic.LoadInt32(posts); n != 2 {
		t.Errorf("mas saw %d POSTs, want 2 (one 502, one 200)", n)
	}
	if got := rec.Header().Get("Stale"); got != "" {
		t.Errorf("Stale header = %q on a fresh resolution, want unset", got)
	}
}

// TestResolveDoesNotRetryWhenOff pins today's behaviour with the switch off:
// one 502 from mas becomes the branded 502 page after exactly one POST, and
// no Stale header is ever set.
func TestResolveDoesNotRetryWhenOff(t *testing.T) {
	p, posts := newScriptedProxy(t, false, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"detail":"deploy blip"}`)
	})

	rec := servePreview(p, "abc")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if got := rec.Body.String(); got != errorPageHTML {
		t.Errorf("body is not the branded error page")
	}
	if n := atomic.LoadInt32(posts); n != 1 {
		t.Errorf("mas saw %d POSTs, want exactly 1 (no retry when off)", n)
	}
	if got := rec.Header().Get("Stale"); got != "" {
		t.Errorf("Stale header = %q when off, want unset", got)
	}
}

// TestResolveRetriesUnsentFailure scripts the unsent class at the wire: the
// first dial to mas is refused, the second reaches a real server. The retry
// must ride it out (KTD1: unsent is always retryable).
func TestResolveRetriesUnsentFailure(t *testing.T) {
	setResilience(t, true)
	upstream := newUpstream(t)

	p, posts := newScriptedProxy(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Resolved{UpstreamURL: upstream.URL})
	})

	// Fail exactly the first dial at the transport level; the rest dial for
	// real. apiClient carries the tuned transport, whose DialContext we wrap.
	tr := p.apiClient.Transport.(*http.Transport)
	baseDial := tr.DialContext
	// Count only dials to mas: the transport is shared, so the proxied
	// request to the upstream dials through it too.
	masAddr := strings.TrimPrefix(p.config.MasBaseURL, "http://")
	refusedAddr := strings.TrimPrefix(deadMasURL(t), "http://")
	var dials int32
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != masAddr {
			return baseDial(ctx, network, addr)
		}
		if atomic.AddInt32(&dials, 1) == 1 {
			return baseDial(ctx, network, refusedAddr)
		}
		return baseDial(ctx, network, addr)
	}

	rec := servePreview(p, "abc")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after one unsent retry (body: %s)", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&dials); n != 2 {
		t.Errorf("dials = %d, want 2", n)
	}
	if n := atomic.LoadInt32(posts); n != 1 {
		t.Errorf("mas saw %d POSTs, want 1 (the refused dial never sent)", n)
	}
}

// TestResolveHonoursRetryAfterSeconds: a 503 carrying Retry-After: 1 delays
// the retry by that second (not the jittered schedule) and then succeeds.
func TestResolveHonoursRetryAfterSeconds(t *testing.T) {
	setResilience(t, true)
	upstream := newUpstream(t)

	var masPosts int32
	p, posts := newScriptedProxy(t, true, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&masPosts, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"detail":"restarting"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Resolved{UpstreamURL: upstream.URL})
	})

	started := time.Now()
	rec := servePreview(p, "abc")
	elapsed := time.Since(started)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if elapsed < time.Second {
		t.Errorf("retry after %v, want the Retry-After second to be honoured", elapsed)
	}
	if n := atomic.LoadInt32(posts); n != 2 {
		t.Errorf("mas saw %d POSTs, want 2", n)
	}
}

// TestResolveRetryAfterLongerThanBudgetEndsExhausted: a Retry-After that
// cannot fit the remaining budget ends the retry at once (KTD3) - one POST,
// branded 502, no further attempts.
func TestResolveRetryAfterLongerThanBudgetEndsExhausted(t *testing.T) {
	setResilience(t, true)

	p, posts := newScriptedProxy(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"detail":"restarting"}`)
	})

	// A caller context with half a second left: 60s of Retry-After cannot fit.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req := previewTestRequest("abc").WithContext(ctx)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if n := atomic.LoadInt32(posts); n != 1 {
		t.Errorf("mas saw %d POSTs, want exactly 1 (Retry-After ended the retry)", n)
	}
}

// TestConcurrentResolvesCollapseToOnePost is the U24 step 7 scenario: 50
// concurrent requests for one cold preview id produce one resolve POST. All
// 50 are served from the shared result.
func TestConcurrentResolvesCollapseToOnePost(t *testing.T) {
	setResilience(t, true)
	upstream := newUpstream(t)

	p, posts := newScriptedProxy(t, true, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond) // wide window for the herd
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Resolved{UpstreamURL: upstream.URL})
	})

	const n = 50
	var wg sync.WaitGroup
	results := make([]*httptest.ResponseRecorder, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = servePreview(p, "cold")
		}(i)
	}
	wg.Wait()

	for i, rec := range results {
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 (body: %s)", i, rec.Code, rec.Body.String())
		}
	}
	if got := atomic.LoadInt32(posts); got != 1 {
		t.Errorf("mas saw %d POSTs, want exactly 1 for 50 concurrent cold requests", got)
	}
}

// TestConcurrentResolvesDoNotCollapseWhenOff pins today's behaviour with the
// switch off: no collapsing, so the same 50 requests issue 50 POSTs.
func TestConcurrentResolvesDoNotCollapseWhenOff(t *testing.T) {
	setResilience(t, false)
	upstream := newUpstream(t)
	const n = 50
	allArrived := make(chan struct{})
	var arrived int32

	p, posts := newScriptedProxy(t, false, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&arrived, 1) == n {
			close(allArrived)
		}
		select {
		case <-allArrived:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Resolved{UpstreamURL: upstream.URL})
	})

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := servePreview(p, "cold")
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rec.Code)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(posts); got != n {
		t.Errorf("mas saw %d POSTs, want %d (no collapsing when off)", got, n)
	}
}

// TestStaleResolutionServedWithinWindow is the U24 step 7 scenario: mas is
// down and the cached resolution is 30s past its TTL. The request is served
// - from the stale shelf, marked with a Stale header.
func TestStaleResolutionServedWithinWindow(t *testing.T) {
	upstream := newUpstream(t)
	p := newDeadMasProxy(t, true)

	// A resolution whose TTL ran out 30s ago, with no live cache entry.
	p.stale.record("abc", &Resolved{UpstreamURL: upstream.URL}, -30*time.Second)

	rec := servePreview(p, "abc")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 served stale (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "upstream ok" {
		t.Errorf("body = %q, want the upstream's", got)
	}
	if got := rec.Header().Get("Stale"); got != "true" {
		t.Errorf("Stale header = %q, want %q", got, "true")
	}
}

// TestStaleResolutionPastWindowIs502 is the other half of the scenario: at
// 90s past TTL - beyond the 60s stale-if-error window - the same outage
// answers 502, so a revoked preview cannot linger indefinitely.
func TestStaleResolutionPastWindowIs502(t *testing.T) {
	upstream := newUpstream(t)
	p := newDeadMasProxy(t, true)

	p.stale.record("abc", &Resolved{UpstreamURL: upstream.URL}, -90*time.Second)

	rec := servePreview(p, "abc")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 past the stale window", rec.Code)
	}
	if got := rec.Header().Get("Stale"); got != "" {
		t.Errorf("Stale header = %q, want unset", got)
	}
}

// TestStaleIsNotServedWhenOff pins today's behaviour: with the switch off a
// cache miss plus an unreachable mas is a 502, shelf or no shelf (the shelf
// does not even exist when off).
func TestStaleIsNotServedWhenOff(t *testing.T) {
	upstream := newUpstream(t)
	p := newDeadMasProxy(t, false)

	if p.stale != nil {
		t.Fatal("stale shelf built with the switch off")
	}
	rec := servePreview(p, "abc")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (today's behaviour)", rec.Code)
	}
	_ = upstream
}

// TestNegativeCacheFor404And410: 404 and 410 answers are cached for
// negativeCacheTTL, so a burst of requests for a dead id costs one POST.
func TestNegativeCacheFor404And410(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(fmt.Sprintf("status %d", status), func(t *testing.T) {
			p, posts := newScriptedProxy(t, true, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				fmt.Fprintf(w, `{"detail":"preview id %s"}`, map[int]string{404: "not found", 410: "expired"}[status])
			})

			for i := 0; i < 3; i++ {
				rec := servePreview(p, "gone")
				if rec.Code != status {
					t.Fatalf("request %d: status = %d, want %d", i, rec.Code, status)
				}
			}
			if n := atomic.LoadInt32(posts); n != 1 {
				t.Errorf("mas saw %d POSTs, want 1 (negative-cached)", n)
			}

			_, exp, found := p.cache.GetWithExpiration(negativeCacheKey("gone"))
			if !found {
				t.Fatal("no negative-cache entry")
			}
			if ttl := time.Until(exp); ttl <= 0 || ttl > negativeCacheTTL {
				t.Errorf("negative cache ttl = %v, want in (0, %v]", ttl, negativeCacheTTL)
			}
		})
	}
}

// TestNegativeCacheOnlyWhenOn: with the switch off, two misses are two POSTs.
func TestNegativeCacheOnlyWhenOn(t *testing.T) {
	p, posts := newScriptedProxy(t, false, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"detail":"nope"}`)
	})

	servePreview(p, "gone")
	servePreview(p, "gone")

	if n := atomic.LoadInt32(posts); n != 2 {
		t.Errorf("mas saw %d POSTs, want 2 (no negative cache when off)", n)
	}
}

// TestTransportSharedWhenResilienceOn is U24 step 8: one tuned transport -
// maxIdleConnsPerHost per the contract, bounded idle/response-header/TLS
// waits - shared by apiClient, the preview proxy and the gateway proxy.
func TestTransportSharedWhenResilienceOn(t *testing.T) {
	setResilience(t, true)
	p := NewProxy(&Config{MasBaseURL: "http://mas.test", PreviewResolveSecret: "s", BaseDomain: "preview.test"})

	shared, ok := p.apiClient.Transport.(*http.Transport)
	if !ok || shared == nil {
		t.Fatalf("apiClient transport = %v, want the tuned *http.Transport", p.apiClient.Transport)
	}
	if p.proxy.Transport != http.RoundTripper(shared) || p.gateway.Transport != http.RoundTripper(shared) {
		t.Error("apiClient, proxy and gateway do not share one transport")
	}
	if shared.MaxIdleConnsPerHost != maxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d, want %d", shared.MaxIdleConnsPerHost, maxIdleConnsPerHost)
	}
	if shared.IdleConnTimeout != transportIdleConnTimeout {
		t.Errorf("IdleConnTimeout = %v, want %v", shared.IdleConnTimeout, transportIdleConnTimeout)
	}
	if shared.IdleConnTimeout >= 5*time.Second {
		t.Errorf("IdleConnTimeout %v is not below the upstream idle timeouts", shared.IdleConnTimeout)
	}
	if shared.ResponseHeaderTimeout != transportResponseHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout = %v, want %v", shared.ResponseHeaderTimeout, transportResponseHeaderTimeout)
	}
	if shared.TLSHandshakeTimeout != transportTLSHandshakeTimeout {
		t.Errorf("TLSHandshakeTimeout = %v, want %v", shared.TLSHandshakeTimeout, transportTLSHandshakeTimeout)
	}
}

// TestTransportDefaultWhenOff pins today's behaviour: every client on the
// default transport (nil), exactly as before U24.
func TestTransportDefaultWhenOff(t *testing.T) {
	setResilience(t, false)
	p := NewProxy(&Config{MasBaseURL: "http://mas.test", PreviewResolveSecret: "s", BaseDomain: "preview.test"})

	if p.apiClient.Transport != nil || p.proxy.Transport != nil || p.gateway.Transport != nil {
		t.Error("transports set with the switch off; want nil (default) everywhere")
	}
}

// TestParseRetryAfter covers both header forms per failure-classes.md.
func TestParseRetryAfter(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)

	tests := []struct {
		name   string
		header string
		want   time.Duration
		ok     bool
	}{
		{"delay-seconds", "3", 3 * time.Second, true},
		{"zero seconds", "0", 0, true},
		{"duration overflow", "9223372036854775807", time.Duration(math.MaxInt64), true},
		{"integer overflow", "999999999999999999999999999999", time.Duration(math.MaxInt64), true},
		{"negative is absent", "-2", 0, false},
		{"empty is absent", "", 0, false},
		{"garbage is absent", "soon", 0, false},
		{"whitespace is trimmed", " 2 ", 2 * time.Second, true},
		{"http-date", now.Add(5 * time.Second).Format(http.TimeFormat), 5 * time.Second, true},
		{"http-date in the past clamps to zero", now.Add(-5 * time.Second).Format(http.TimeFormat), 0, true},
		{"unparseable date is absent", "Wed, 32 Foo 2026 00:00:00 GMT", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseRetryAfter(tt.header, now)
			if ok != tt.ok || got != tt.want {
				t.Errorf("parseRetryAfter(%q) = %v, %v; want %v, %v", tt.header, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestClassifyTransportError pins the Go failure-class table for the signals
// resolve() can actually see.
func TestClassifyTransportError(t *testing.T) {
	dialRefused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"dial op error", dialRefused, failureClassUnsent},
		{"dial op error wrapped in url.Error", &url.Error{Op: "Post", URL: "http://mas", Err: dialRefused}, failureClassUnsent},
		{"dns failure", &net.DNSError{Err: "no such host", Name: "mas.invalid"}, failureClassUnsent},
		{"tls handshake timeout", errors.New("net/http: TLS handshake timeout"), failureClassUnsent},
		{"reset after send", &net.OpError{Op: "read", Net: "tcp", Err: io.EOF}, failureClassAmbiguous},
		{"body cut", io.ErrUnexpectedEOF, failureClassAmbiguous},
		{"deadline after send", context.DeadlineExceeded, failureClassAmbiguous},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyTransportError(tt.err); got != tt.want {
				t.Errorf("classifyTransportError(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

// TestJitteredRetryDelayBounds: full jitter with caps growing exponentially
// from the 250ms base (KTD3).
func TestJitteredRetryDelayBounds(t *testing.T) {
	cases := []struct {
		attempt int
		upper   time.Duration
	}{
		{1, 250 * time.Millisecond},
		{2, 500 * time.Millisecond},
		{99, 2 * time.Second},
	}
	for _, c := range cases {
		for i := 0; i < 200; i++ {
			d := jitteredRetryDelay(c.attempt)
			if d < 0 || d >= c.upper {
				t.Errorf("jitteredRetryDelay(%d) = %v, want [0, %v)", c.attempt, d, c.upper)
			}
		}
	}
}

// capturingHandler collects slog records so tests can assert the KTD14
// telemetry without parsing text output.
type capturingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}
func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

// TestRetryTelemetryEmitted: a mas that always 502s emits one retry_attempt
// per failed retryable attempt (Warn) and a final retry_exhausted (Error)
// with the contract's field names (events.md, Go rendering).
func TestRetryTelemetryEmitted(t *testing.T) {
	setResilience(t, true)

	p, _ := newScriptedProxy(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"detail":"down"}`)
	})

	capture := &capturingHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(capture))
	defer slog.SetDefault(previous)

	rec := servePreview(p, "abc")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}

	var attempts, exhausted int
	for _, r := range capture.records {
		if r.Message != "retry_attempt" && r.Message != "retry_exhausted" {
			// slog.SetDefault also routes the log package through the
			// handler; only the retry telemetry is under test here.
			continue
		}
		fields := map[string]slog.Value{}
		r.Attrs(func(a slog.Attr) bool {
			fields[a.Key] = a.Value
			return true
		})
		for _, key := range []string{"layer", "op", "attempt", "class", "status", "elapsed_ms", "budget_ms", "error"} {
			if _, ok := fields[key]; !ok {
				t.Errorf("%s record missing field %q", r.Message, key)
			}
		}
		switch r.Message {
		case "retry_attempt":
			attempts++
			if r.Level != slog.LevelWarn {
				t.Errorf("retry_attempt level = %v, want Warn", r.Level)
			}
			if _, ok := fields["delay_ms"]; !ok {
				t.Error("retry_attempt missing delay_ms")
			}
			if got := fields["class"].String(); got != failureClassRetryableStatus {
				t.Errorf("retry_attempt class = %q, want %q", got, failureClassRetryableStatus)
			}
			if got := fields["status"].Int64(); got != http.StatusBadGateway {
				t.Errorf("retry_attempt status = %d, want 502", got)
			}
			if got := fields["op"].String(); got != "resolve" {
				t.Errorf("retry_attempt op = %q, want resolve", got)
			}
			if got := fields["layer"].String(); got != "http" {
				t.Errorf("retry_attempt layer = %q, want http", got)
			}
			if got := fields["budget_ms"].Int64(); got != resolveBudget.Milliseconds() {
				t.Errorf("retry_attempt budget_ms = %d, want %d", got, resolveBudget.Milliseconds())
			}
		case "retry_exhausted":
			exhausted++
			if r.Level != slog.LevelError {
				t.Errorf("retry_exhausted level = %v, want Error", r.Level)
			}
			if _, ok := fields["delay_ms"]; ok {
				t.Error("retry_exhausted carries delay_ms; want absent")
			}
		}
	}
	if attempts != resolveRetries {
		t.Errorf("retry_attempt records = %d, want %d", attempts, resolveRetries)
	}
	if exhausted != 1 {
		t.Errorf("retry_exhausted records = %d, want 1", exhausted)
	}
}

// TestRetryTelemetryNotEmittedWhenOff: no retry loop runs with the switch
// off, so no retry telemetry exists.
func TestRetryTelemetryNotEmittedWhenOff(t *testing.T) {
	p, _ := newScriptedProxy(t, false, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"detail":"down"}`)
	})

	capture := &capturingHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(capture))
	defer slog.SetDefault(previous)

	if rec := servePreview(p, "abc"); rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	for _, r := range capture.records {
		if r.Message == "retry_attempt" || r.Message == "retry_exhausted" {
			t.Errorf("%s emitted with the switch off; today's behaviour has no retry telemetry", r.Message)
		}
	}
}

// TestStaleServeCoversTimeout: mas accepting but never answering (a timeout,
// ambiguous per KTD1 and so never retried) still falls inside the
// stale-if-error window - "does not answer" per budgets.md §2.7.
func TestStaleServeCoversTimeout(t *testing.T) {
	setResilience(t, true)
	upstream := newUpstream(t)

	release := make(chan struct{})
	mas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Accept, then never respond. Reading the body lets the server
		// notice the client hanging up; release unblocks Close regardless.
		io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(mas.Close)
	t.Cleanup(func() { close(release) }) // runs before mas.Close (LIFO)

	p := NewProxy(&Config{MasBaseURL: mas.URL, PreviewResolveSecret: "test-secret", BaseDomain: "preview.test"})
	p.stale.record("abc", &Resolved{UpstreamURL: upstream.URL}, -10*time.Second)

	rec := servePreview(p, "abc")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 served stale while mas hangs (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Stale"); got != "true" {
		t.Errorf("Stale header = %q, want true", got)
	}
}

// TestSingleflightLeaderCancelDoesNotFailFollowers: the first request for a
// cold id hangs up mid-resolve; the requests sharing that resolve still get
// the resolution, and only one POST is made.
func TestSingleflightLeaderCancelDoesNotFailFollowers(t *testing.T) {
	upstream := newUpstream(t)
	entered := make(chan struct{}, 1)
	p, posts := newScriptedProxy(t, true, func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Resolved{UpstreamURL: upstream.URL})
	})

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, previewTestRequest("cold").WithContext(leaderCtx))
		leaderDone <- rec
	}()
	<-entered // the leader's POST is in flight at mas

	followerDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { followerDone <- servePreview(p, "cold") }()
	time.Sleep(20 * time.Millisecond)
	cancelLeader()

	<-leaderDone
	rec := <-followerDone
	if rec.Code != http.StatusOK {
		t.Fatalf("follower status = %d, want 200 after the leader hung up (body: %s)", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(posts); n != 1 {
		t.Errorf("mas saw %d POSTs, want 1", n)
	}
}

// TestStaleShelfDropsEntriesPastWindow: the shelf does not keep resolutions
// that can no longer be served, so it stays bounded.
func TestStaleShelfDropsEntriesPastWindow(t *testing.T) {
	s := newStaleShelf()
	s.record("old", &Resolved{UpstreamURL: "http://x"}, -staleIfErrorWindow-time.Second)
	if s.entries.ItemCount() != 0 {
		t.Errorf("shelf holds %d entries for a resolution past the window, want 0", s.entries.ItemCount())
	}
	s.record("fresh", &Resolved{UpstreamURL: "http://x"}, 45*time.Second)
	_, exp, found := s.entries.GetWithExpiration("fresh")
	if !found {
		t.Fatal("fresh resolution not on the shelf")
	}
	if want := 45*time.Second + staleIfErrorWindow; time.Until(exp) > want || time.Until(exp) < want-time.Second {
		t.Errorf("shelf expiry in %v, want ~%v", time.Until(exp), want)
	}
}

// TestRevokedPreviewNotReopenedByStale: once mas answers 410 for an id, a
// later outage inside the stale window must not serve the old resolution.
func TestRevokedPreviewNotReopenedByStale(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone, http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			upstream := newUpstream(t)
			var mode int32 // 0 = authoritative refusal, 1 = 503
			p, _ := newScriptedProxy(t, true, func(w http.ResponseWriter, r *http.Request) {
				if atomic.LoadInt32(&mode) == 0 {
					w.WriteHeader(status)
					return
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			})
			p.stale.record("abc", &Resolved{UpstreamURL: upstream.URL}, -5*time.Second)

			if _, _, err := p.resolveWithStale(context.Background(), "abc"); err == nil {
				t.Fatal("expected authoritative refusal")
			}
			p.cache.Delete(negativeCacheKey("abc")) // the 10 s negative cache ran out
			atomic.StoreInt32(&mode, 1)

			rec := servePreview(p, "abc")
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502: a revoked preview must not be served stale", rec.Code)
			}
			if rec.Header().Get("Stale") != "" {
				t.Error("Stale header set for a revoked preview")
			}
		})
	}
}

// TestStaleServedWhenResolveBodyDrops: mas sends 200 headers then drops the
// connection mid-body. That is "mas did not answer", so stale is served.
func TestStaleServedWhenResolveBodyDrops(t *testing.T) {
	upstream := newUpstream(t)
	p, _ := newScriptedProxy(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "500")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"upstream_url":"`)
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	})
	p.stale.record("abc", &Resolved{UpstreamURL: upstream.URL}, -5*time.Second)

	rec := servePreview(p, "abc")
	if rec.Code != http.StatusOK || rec.Header().Get("Stale") != "true" {
		t.Fatalf("status = %d Stale = %q, want 200 served stale", rec.Code, rec.Header().Get("Stale"))
	}
}

// TestMalformedResolveBodyIsNotServedStale: a 200 with unparseable JSON is a
// protocol failure, not an outage, so the stale shelf is not used.
func TestMalformedResolveBodyIsNotServedStale(t *testing.T) {
	for _, body := range []string{`not json`, `{"upstream_url":"`, `{"upstream_url":42}`, ``} {
		t.Run(body, func(t *testing.T) {
			upstream := newUpstream(t)
			p, posts := newScriptedProxy(t, true, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, body)
			})
			p.stale.record("abc", &Resolved{UpstreamURL: upstream.URL}, -5*time.Second)

			if rec := servePreview(p, "abc"); rec.Code != http.StatusBadGateway || rec.Header().Get("Stale") != "" {
				t.Fatalf("status = %d Stale = %q, want 502 without stale", rec.Code, rec.Header().Get("Stale"))
			}
			if got := atomic.LoadInt32(posts); got != 1 {
				t.Fatalf("malformed response retried: %d POSTs", got)
			}
		})
	}
}

func TestStaleServedWhenResolveBodyTimesOut(t *testing.T) {
	upstream := newUpstream(t)
	p, posts := newScriptedProxy(t, true, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Length", "500")
		fmt.Fprint(w, `{"upstream_url":"`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	p.apiClient.Timeout = 200 * time.Millisecond
	p.stale.record("abc", &Resolved{UpstreamURL: upstream.URL}, -5*time.Second)
	if rec := servePreview(p, "abc"); rec.Code != http.StatusOK || rec.Header().Get("Stale") != "true" {
		t.Fatalf("status = %d Stale = %q, want 200 served stale", rec.Code, rec.Header().Get("Stale"))
	}
	if got := atomic.LoadInt32(posts); got != 1 {
		t.Fatalf("body timeout retried: %d POSTs", got)
	}
}

func TestResilienceGateValues(t *testing.T) {
	for _, value := range []string{"1", "true", "on", "TRUE", "On", "", "0", "false", "off", "garbage"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("PROXY_RESILIENCE", value)
			p := NewProxy(&Config{})
			want := value == "1" || strings.EqualFold(value, "true") || strings.EqualFold(value, "on")
			if p.resilience != want || (p.inflight != nil) != want || (p.stale != nil) != want {
				t.Fatalf("gate %q: resilience state does not match %v", value, want)
			}
			if (p.apiClient.Transport != nil) != want || (p.proxy.Transport != nil) != want || (p.gateway.Transport != nil) != want {
				t.Fatalf("gate %q: transport state does not match %v", value, want)
			}
		})
	}
}

func TestGateway502NeverReplaysBody(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(strconv.FormatBool(on), func(t *testing.T) {
			p, posts := newScriptedProxy(t, on, func(w http.ResponseWriter, r *http.Request) {
				t.Error("gateway request reached preview resolve")
			})
			var arrivals int32
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&arrivals, 1)
				body, err := io.ReadAll(r.Body)
				if err != nil || r.Method != http.MethodPost || string(body) != "non-idempotent-write" {
					t.Errorf("gateway received method %s body %q error %v", r.Method, body, err)
				}
				w.WriteHeader(http.StatusBadGateway)
				fmt.Fprint(w, "gateway failure")
			}))
			defer gateway.Close()
			p.config.InterfacesGatewayURL, _ = url.Parse(gateway.URL)
			request := httptest.NewRequest(http.MethodPost, "http://my-app.preview.test/write", strings.NewReader("non-idempotent-write"))
			recorder := httptest.NewRecorder()
			p.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadGateway || recorder.Body.String() != "gateway failure" {
				t.Fatalf("gateway response changed: %d %q", recorder.Code, recorder.Body.String())
			}
			if got := atomic.LoadInt32(&arrivals); got != 1 || atomic.LoadInt32(posts) != 0 {
				t.Fatalf("gateway arrivals = %d, MAS arrivals = %d", got, atomic.LoadInt32(posts))
			}
		})
	}
}

func TestConcurrentFastResolvesReuseCompletedCache(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNotFound, http.StatusGone} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			p, posts := newScriptedProxy(t, true, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				if status == http.StatusOK {
					json.NewEncoder(w).Encode(Resolved{UpstreamURL: "http://unused.test"})
				}
			})
			const rounds = 20
			for round := 0; round < rounds; round++ {
				id := "burst" + strconv.Itoa(round)
				start := make(chan struct{})
				var group sync.WaitGroup
				for i := 0; i < 50; i++ {
					group.Add(1)
					go func() {
						defer group.Done()
						<-start
						_, err := p.resolve(context.Background(), id)
						if (err == nil) != (status == http.StatusOK) {
							t.Errorf("resolve returned unexpected error: %v", err)
						}
					}()
				}
				close(start)
				group.Wait()
			}
			if got := atomic.LoadInt32(posts); got != rounds {
				t.Fatalf("%d POSTs for %d ids: delayed callers missed completed cache", got, rounds)
			}
		})
	}
}
