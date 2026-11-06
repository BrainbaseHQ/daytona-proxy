package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEng1504BaselineResolveFailsAfterOnePOST(t *testing.T) {
	t.Setenv("PROXY_RESILIENCE", "0")
	for _, status := range []int{403, 404, 429, 500, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var arrivals atomic.Int32
			mas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				arrivals.Add(1)
				if r.Method != "POST" || r.URL.Path != "/internal/preview/resolve" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(status)
				w.Write([]byte(`{"detail":"upstream provider unavailable"}`))
			}))
			defer mas.Close()
			p := NewProxy(&Config{MasBaseURL: mas.URL, PreviewResolveSecret: "local", BaseDomain: "preview.test"})
			_, err := p.resolve(context.Background(), "abc")
			var resolveErr *resolveError
			if !errors.As(err, &resolveErr) || resolveErr.status != status {
				t.Fatalf("error = %v", err)
			}
			if arrivals.Load() != 1 {
				t.Fatalf("arrivals = %d", arrivals.Load())
			}
		})
	}
}

func TestEng1504BaselineExpiredCacheDoesNotServeStale(t *testing.T) {
	t.Setenv("PROXY_RESILIENCE", "0")
	var arrivals atomic.Int32
	mas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if arrivals.Add(1) == 1 {
			json.NewEncoder(w).Encode(Resolved{UpstreamURL: "http://127.0.0.1:1", CacheTTLS: 1})
			return
		}
		w.WriteHeader(502)
	}))
	defer mas.Close()
	p := NewProxy(&Config{MasBaseURL: mas.URL, PreviewResolveSecret: "local", BaseDomain: "preview.test"})
	resolved, err := p.resolve(context.Background(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	// Expire the real cache entry; no wall-clock wait or fake resolver.
	p.cache.Set("abc", resolved, time.Nanosecond)
	time.Sleep(time.Millisecond)
	_, err = p.resolve(context.Background(), "abc")
	if err == nil || arrivals.Load() != 2 {
		t.Fatalf("error=%v arrivals=%d", err, arrivals.Load())
	}
}

func TestEng1504BaselineColdResolvesAreNotCoalesced(t *testing.T) {
	t.Setenv("PROXY_RESILIENCE", "0")
	const count = 50
	var arrivals atomic.Int32
	allArrived := make(chan struct{})
	release := make(chan struct{})
	mas := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if arrivals.Add(1) == count {
			close(allArrived)
		}
		<-release
		json.NewEncoder(w).Encode(Resolved{UpstreamURL: "http://127.0.0.1:1", CacheTTLS: 1})
	}))
	defer mas.Close()
	p := NewProxy(&Config{MasBaseURL: mas.URL, PreviewResolveSecret: "local", BaseDomain: "preview.test"})
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.resolve(context.Background(), "abc"); err != nil {
				t.Error(err)
			}
		}()
	}
	select {
	case <-allArrived:
	case <-time.After(5 * time.Second):
		t.Errorf("only %d arrived", arrivals.Load())
	}
	close(release)
	wg.Wait()
	if arrivals.Load() != count {
		t.Fatalf("arrivals = %d", arrivals.Load())
	}
}
