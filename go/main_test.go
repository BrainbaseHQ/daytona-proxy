package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/patrickmn/go-cache"
)

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
