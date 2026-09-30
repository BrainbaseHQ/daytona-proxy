package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
