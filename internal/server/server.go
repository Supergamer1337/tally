// Package server is tally's web front: the app, its JSON API, sign in and the MCP endpoint.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"tally/internal/mcp"
	"tally/internal/tally"
)

type Config struct {
	// Password signs a browser in. Empty only when NoAuth is set.
	Password string
	// NoAuth turns sign in off, for running behind a proxy that does its own.
	NoAuth bool
	// PublicURL is where people reach tally, like https://tally.example.com. Without it the
	// address is worked out from each request. Required when NoAuth is set.
	PublicURL string
	// TrustedProxies may set X-Forwarded-For, which then decides whose sign in attempts are counted.
	TrustedProxies []netip.Prefix
	// SSO, when set, adds sign in through an OpenID Connect provider. Password may then be empty.
	SSO     *SSO
	Version string
}

type Server struct {
	svc    *tally.Service
	cfg    Config
	log    *slog.Logger
	assets *assets
	limit  *limiter
	mcp    *mcp.Handler
	sso    pending
}

func New(svc *tally.Service, cfg Config, log *slog.Logger) (*Server, error) {
	if cfg.Password == "" && !cfg.NoAuth && cfg.SSO == nil {
		return nil, errors.New("a password or single sign on is required unless sign in is turned off")
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	if cfg.PublicURL != "" {
		u, err := url.Parse(cfg.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(cfg.PublicURL, "#") {
			return nil, fmt.Errorf("TALLY_PUBLIC_URL must be only a scheme and host like https://tally.example.com, tally cannot live under a path: %q", cfg.PublicURL)
		}
		cfg.PublicURL = u.Scheme + "://" + u.Host
	}
	if cfg.NoAuth && cfg.PublicURL == "" {
		return nil, errors.New("with sign in off, set TALLY_PUBLIC_URL so tally can refuse requests that did not come through your proxy's address")
	}
	if cfg.SSO != nil && cfg.PublicURL == "" {
		return nil, errors.New("single sign on needs TALLY_PUBLIC_URL, the provider sends the browser back to it")
	}
	a, err := loadAssets()
	if err != nil {
		return nil, err
	}
	s := &Server{svc: svc, cfg: cfg, log: log, assets: a, limit: newLimiter(10, 15*time.Minute), mcp: mcp.NewHandler(svc, cfg.Version, log)}
	// Make the MCP token now, so the first agent never races the first page load for it.
	if _, err := svc.MCPToken(context.Background()); err != nil {
		return nil, err
	}
	if err := s.checkPasswordChange(context.Background()); err != nil {
		return nil, err
	}
	if cfg.PublicURL != "" {
		if err := http.NewCrossOriginProtection().AddTrustedOrigin(cfg.PublicURL); err != nil {
			return nil, fmt.Errorf("TALLY_PUBLIC_URL: %w", err)
		}
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("POST /login", s.login)
	// After a wrong password the address bar shows /login. Opening it again should not fail.
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/", http.StatusSeeOther) })
	if s.cfg.SSO != nil {
		mux.HandleFunc("GET /auth/login", s.ssoStart)
		mux.HandleFunc("GET /auth/callback", s.ssoFinish)
	}
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /static/{path...}", s.static)
	mux.HandleFunc("GET /healthz", s.health)

	mux.Handle("GET /api/state", s.requireSession(http.HandlerFunc(s.getState)))
	mux.Handle("POST /api/days/{date}/add", s.requireSession(http.HandlerFunc(s.addDay)))
	mux.Handle("PUT /api/days/{date}", s.requireSession(http.HandlerFunc(s.setDay)))
	mux.Handle("DELETE /api/days/{date}", s.requireSession(http.HandlerFunc(s.deleteDay)))
	mux.Handle("PUT /api/agents/write", s.requireSession(http.HandlerFunc(s.setWrite)))

	mux.Handle("/mcp", s.mcp)

	// Browsers send Sec-Fetch-Site and Origin with every request that changes something, so a
	// page on another site can never post to tally with the user's cookie. Agents send neither
	// and pass.
	cop := http.NewCrossOriginProtection()
	if s.cfg.PublicURL != "" {
		cop.AddTrustedOrigin(s.cfg.PublicURL) // checked in New
	}
	return s.logRequests(securityHeaders(s.checkHost(cop.Handler(mux))))
}

// checkHost matters only with sign in off. A page on another site could otherwise point its own
// domain name at this server (DNS rebinding) and read and change everything. Such a request
// carries that other name in its Host header, so only the public host, localhost and plain IP
// addresses are let through.
func (s *Server) checkHost(next http.Handler) http.Handler {
	if !s.cfg.NoAuth {
		return next
	}
	public, _ := url.Parse(s.cfg.PublicURL)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		_, ipErr := netip.ParseAddr(host)
		if ipErr != nil && !strings.EqualFold(host, public.Hostname()) && !strings.EqualFold(host, "localhost") {
			s.log.Warn("refused a request for an unknown host, check that your proxy passes the Host header", "host", r.Host)
			http.Error(w, "Unknown host", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Close ends what outlives a request, such as agents' subscription streams.
func (s *Server) Close() { s.mcp.Close() }

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; manifest-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, sw := time.Now(), &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" {
			return
		}
		s.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.svc.Store.Ping(ctx); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte("ok\n"))
}

// publicURL is where this tally is reached from outside, without a trailing slash.
func (s *Server) publicURL(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL
	}
	scheme := "http"
	if isHTTPS(r) {
		scheme = "https"
	}
	host := r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		host = strings.TrimSpace(strings.Split(fh, ",")[0])
	}
	return scheme + "://" + host
}

func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
	return strings.EqualFold(proto, "https")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
