package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/patrickmn/go-cache"
)

type Config struct {
	MasBaseURL           string
	PreviewResolveSecret string
	BaseDomain           string
	Port                 string
}

//go:embed error.html
var errorPageHTML string

type Proxy struct {
	proxy     *httputil.ReverseProxy
	cache     *cache.Cache
	apiClient *http.Client
	config    *Config
}

var previewIDRegex = regexp.MustCompile(`^[a-zA-Z0-9]+$`)

// Resolved is the response from mas's POST /internal/preview/resolve.
type Resolved struct {
	UpstreamURL string `json:"upstream_url"`
	Token       string `json:"token"`
	TokenHeader string `json:"token_header"`
}

// resolveError carries the HTTP status mas returned so callers can map it
// to a branded error page (e.g. 404/410) instead of always answering 502.
type resolveError struct {
	status int
}

func (e *resolveError) Error() string {
	return fmt.Sprintf("mas resolve returned status %d", e.status)
}

// contextKey avoids collisions with other packages' context keys.
type contextKey int

const resolvedContextKey contextKey = iota

func validateInputs(previewID string) error {
	if previewID == "" {
		return fmt.Errorf("preview ID cannot be empty")
	}
	if !previewIDRegex.MatchString(previewID) {
		return fmt.Errorf("invalid format")
	}
	return nil
}

func NewProxy(config *Config) *Proxy {
	p := &Proxy{
		cache:     cache.New(2*time.Minute, 5*time.Minute),
		config:    config,
		apiClient: &http.Client{Timeout: 30 * time.Second},
	}

	p.proxy = &httputil.ReverseProxy{
		Director: p.director,
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode != http.StatusOK {
				return p.serveErrorPage(resp, http.StatusBadGateway)
			}

			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("Proxy error: %v", err)
			p.writeErrorPage(w, http.StatusBadGateway)
		},
	}

	return p
}

func (p *Proxy) serveErrorPage(resp *http.Response, status int) error {
	resp.Body.Close()
	resp.StatusCode = status
	resp.Header.Set("Content-Type", "text/html; charset=utf-8")
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(errorPageHTML)))
	resp.Header.Del("Content-Encoding")
	resp.Body = io.NopCloser(strings.NewReader(errorPageHTML))
	return nil
}

func (p *Proxy) writeErrorPage(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write([]byte(errorPageHTML))
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		return
	}

	previewID := previewIdFromHost(r.Host, p.config.BaseDomain)
	log.Printf("Request: host=%s previewId=%s", r.Host, previewID)
	if err := validateInputs(previewID); err != nil {
		log.Printf("Invalid request: %v", err)
		p.writeErrorPage(w, http.StatusNotFound)
		return
	}

	// Resolve up front so we can map mas errors (404/410) to the correct
	// status on the branded error page. Upstream/proxy failures after this
	// point stay 502 (handled by ModifyResponse/ErrorHandler).
	resolved, err := p.resolve(r.Context(), previewID)
	if err != nil {
		if rerr, ok := err.(*resolveError); ok && (rerr.status == http.StatusNotFound || rerr.status == http.StatusGone) {
			log.Printf("previewId %s unresolvable: %v", previewID, err)
			p.writeErrorPage(w, rerr.status)
			return
		}
		log.Printf("Failed to resolve previewId %s: %v", previewID, err)
		p.writeErrorPage(w, http.StatusBadGateway)
		return
	}

	ctx := context.WithValue(r.Context(), resolvedContextKey, resolved)
	p.proxy.ServeHTTP(w, r.WithContext(ctx))
}

func (p *Proxy) director(req *http.Request) {
	resolved, ok := req.Context().Value(resolvedContextKey).(*Resolved)
	if !ok || resolved == nil {
		log.Printf("director: no resolved target in context")
		req.URL.Host = "invalid.local"
		return
	}

	targetUrl, err := url.Parse(resolved.UpstreamURL)
	if err != nil {
		log.Printf("Invalid target URL: %v", err)
		req.URL.Host = "invalid.local"
		return
	}

	req.URL.Scheme = targetUrl.Scheme
	req.URL.Host = targetUrl.Host
	req.URL.Path = singleJoiningSlash(targetUrl.Path, req.URL.Path)
	req.Host = targetUrl.Host
	if resolved.Token != "" && resolved.TokenHeader != "" {
		req.Header.Set(resolved.TokenHeader, resolved.Token) // e.g. x-daytona-preview-token OR e2b-traffic-access-token
	}
}

// resolve calls mas's POST /internal/preview/resolve to turn an opaque
// previewId into an upstream URL + auth token/header, provider-agnostically
// (Daytona and e2b both flow through this). Results are cached by previewId.
func (p *Proxy) resolve(ctx context.Context, previewID string) (*Resolved, error) {
	if x, found := p.cache.Get(previewID); found {
		return x.(*Resolved), nil
	}

	body, _ := json.Marshal(map[string]string{"preview_id": previewID})
	reqUrl := strings.TrimRight(p.config.MasBaseURL, "/") + "/internal/preview/resolve"
	req, err := http.NewRequestWithContext(ctx, "POST", reqUrl, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Secret", p.config.PreviewResolveSecret)

	resp, err := p.apiClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &resolveError{status: resp.StatusCode}
	}

	var r Resolved
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}

	p.cache.Set(previewID, &r, cache.DefaultExpiration)
	return &r, nil
}

func loadConfig() *Config {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, relying on environment variables")
	}

	config := &Config{
		MasBaseURL:           os.Getenv("MAS_BASE_URL"),
		PreviewResolveSecret: os.Getenv("PREVIEW_RESOLVE_SECRET"),
		BaseDomain:           os.Getenv("PREVIEW_BASE_DOMAIN"),
		Port:                 os.Getenv("PORT"),
	}

	if config.Port == "" {
		config.Port = "3000"
	}

	if config.BaseDomain == "" {
		config.BaseDomain = "brainbaselabs.space"
	}

	if config.MasBaseURL == "" || config.PreviewResolveSecret == "" {
		log.Fatal("MAS_BASE_URL and PREVIEW_RESOLVE_SECRET must be set")
	}

	return config
}

func main() {
	config := loadConfig()

	proxy := NewProxy(config)

	server := &http.Server{
		Addr:           ":" + config.Port,
		Handler:        proxy,
		ReadTimeout:    30 * time.Second,
		WriteTimeout:   30 * time.Second,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1 MB
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("Starting proxy server on port %s", config.Port)
		log.Printf("mas base URL: %s", config.MasBaseURL)
		log.Printf("Preview base domain: %s", config.BaseDomain)
		log.Printf("Server ready to accept connections")

		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start server: %v", err)
		}
	}()

	<-stop
	log.Println("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("Server forced to shutdown: %v", err)
	} else {
		log.Println("Server gracefully stopped")
	}
}

// previewIdFromHost returns the leftmost DNS label as the opaque preview id.
// Host form: {previewId}.<baseDomain>. Empty string if it doesn't match.
func previewIdFromHost(host, baseDomain string) string {
	if i := strings.Index(host, ":"); i != -1 {
		host = host[:i]
	}
	host = strings.ToLower(host)
	suffix := "." + strings.ToLower(baseDomain)
	if !strings.HasSuffix(host, suffix) {
		return ""
	}
	label := strings.TrimSuffix(host, suffix)
	if label == "" || strings.Contains(label, ".") {
		return ""
	}
	return label
}

func singleJoiningSlash(a, b string) string {
	aSlash := strings.HasSuffix(a, "/")
	bSlash := strings.HasPrefix(b, "/")
	switch {
	case aSlash && bSlash:
		return a + b[1:]
	case !aSlash && !bSlash:
		return a + "/" + b
	}
	return a + b
}
