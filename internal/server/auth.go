package server

import (
	"bytes"
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// A sign in lasts half a year and is renewed while it is used, so a phone stays signed in.
const (
	cookieName  = "tally_session"
	sessionLife = 180 * 24 * time.Hour
)

// The cookie holds a random secret. The database only ever sees its hash.
func sessionID(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func (s *Server) signedIn(w http.ResponseWriter, r *http.Request) (bool, error) {
	if s.cfg.NoAuth {
		return true, nil
	}
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return false, nil
	}
	id := sessionID(c.Value)
	exp, ok, err := s.svc.Store.Session(r.Context(), id, time.Now())
	if err != nil || !ok {
		return false, err
	}
	// Renew at most once a day.
	if time.Until(exp) < sessionLife-24*time.Hour {
		exp = time.Now().Add(sessionLife)
		if err := s.svc.Store.ExtendSession(r.Context(), id, exp); err != nil {
			s.log.Error("extend session", "err", err)
		} else {
			s.setCookie(w, r, c.Value, exp)
		}
	}
	return true, nil
}

func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, err := s.signedIn(w, r)
		if err != nil {
			s.log.Error("check session", "err", err)
			writeError(w, http.StatusInternalServerError, "Something went wrong")
			return
		}
		if !ok {
			writeError(w, http.StatusUnauthorized, "Sign in")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// secure says whether cookies get the Secure flag: on HTTPS, and whenever tally is reached
// through an https public address.
func (s *Server) secure(r *http.Request) bool {
	return isHTTPS(r) || strings.HasPrefix(s.cfg.PublicURL, "https://")
}

func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, value string, exp time.Time) {
	c := &http.Cookie{
		Name: cookieName, Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: s.secure(r),
	}
	if exp.IsZero() {
		c.MaxAge = -1
	} else {
		c.Expires, c.MaxAge = exp, int(time.Until(exp).Seconds())
	}
	http.SetCookie(w, c)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.cfg.NoAuth {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if s.cfg.Password == "" {
		s.renderLogin(w, r, http.StatusForbidden, "Sign in with "+s.cfg.SSO.Name)
		return
	}
	ip := s.clientIP(r)
	// The attempt is counted before the password is read, so attempts sent at the same time
	// cannot all get past the limit.
	if !s.limit.attempt(ip) {
		s.log.Warn("sign in refused, too many tries", "ip", ip)
		s.renderLogin(w, r, http.StatusTooManyRequests, "Too many tries. Wait a few minutes")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if !s.passwordMatches(r.PostFormValue("password")) {
		s.log.Warn("sign in failed", "ip", ip)
		s.renderLogin(w, r, http.StatusUnauthorized, "Wrong password")
		return
	}
	s.limit.reset(ip)
	if err := s.startSession(w, r); err != nil {
		s.log.Error("create session", "err", err)
		s.renderLogin(w, r, http.StatusInternalServerError, "Something went wrong")
		return
	}
	s.log.Info("signed in", "via", "password", "ip", ip)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// startSession signs this browser in, however it proved who it is.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request) error {
	b := make([]byte, 32)
	rand.Read(b)
	secret, exp := base64.RawURLEncoding.EncodeToString(b), time.Now().Add(sessionLife)
	if err := s.svc.Store.CreateSession(r.Context(), sessionID(secret), exp); err != nil {
		return err
	}
	if err := s.svc.Store.Prune(r.Context(), time.Now()); err != nil {
		s.log.Error("prune sessions", "err", err)
	}
	s.setCookie(w, r, secret, exp)
	return nil
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		if err := s.svc.Store.DeleteSession(r.Context(), sessionID(c.Value)); err != nil {
			s.log.Error("delete session", "err", err)
		}
	}
	s.setCookie(w, r, "", time.Time{})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) passwordMatches(got string) bool {
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(s.cfg.Password))
	return got != "" && s.cfg.Password != "" && subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, status int, msg string) {
	data := struct {
		V, Error, SSO string
		Password      bool
	}{V: s.assets.version, Error: msg, Password: s.cfg.Password != ""}
	if s.cfg.SSO != nil {
		data.SSO = s.cfg.SSO.Name
	}
	var page bytes.Buffer
	if err := s.assets.login.Execute(&page, data); err != nil {
		s.log.Error("render sign in", "err", err)
		http.Error(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	writeBody(w, r, status, page.Bytes())
}

// A new password signs every browser out. The database keeps a slow salted hash of the
// password, only to notice that it changed.
const settingPasswordCheck = "password_check"

func (s *Server) checkPasswordChange(ctx context.Context) error {
	if s.cfg.NoAuth || s.cfg.Password == "" {
		return nil
	}
	stored, ok, err := s.svc.Store.Setting(ctx, settingPasswordCheck)
	if err != nil {
		return err
	}
	if ok {
		if salt, sum, found := strings.Cut(stored, "$"); found {
			saltBytes, _ := base64.RawStdEncoding.DecodeString(salt)
			if bytes.Equal([]byte(base64.RawStdEncoding.EncodeToString(passwordKey(s.cfg.Password, saltBytes))), []byte(sum)) {
				return nil
			}
		}
		s.log.Info("the password changed, signing every browser out")
		if err := s.svc.Store.DeleteAllSessions(ctx); err != nil {
			return err
		}
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	return s.svc.Store.SetSetting(ctx, settingPasswordCheck, base64.RawStdEncoding.EncodeToString(salt)+"$"+base64.RawStdEncoding.EncodeToString(passwordKey(s.cfg.Password, salt)))
}

// pbkdf2Rounds is lowered by tests only.
var pbkdf2Rounds = 600_000

func passwordKey(password string, salt []byte) []byte {
	k, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Rounds, 32)
	if err != nil {
		panic(err) // only for impossible parameters
	}
	return k
}

// clientIP is the address a request came from. Behind a trusted proxy that is the last address
// in X-Forwarded-For that is not itself a trusted proxy, otherwise the connection's own address.
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !s.trusted(addr) {
		return host
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		if addr = hop.Unmap(); !s.trusted(addr) {
			return addr.String()
		}
	}
	return addr.String()
}

func (s *Server) trusted(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, p := range s.cfg.TrustedProxies {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// limiter allows each address a few sign in attempts per window. A correct password clears
// the count.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	tries  map[string]*tries
}

type tries struct {
	n     int
	first time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, tries: map[string]*tries{}}
}

// attempt counts one try for key, and returns false when key has none left.
func (l *limiter) attempt(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.tries) > 10_000 {
		for k, t := range l.tries {
			if time.Since(t.first) > l.window {
				delete(l.tries, k)
			}
		}
	}
	t := l.tries[key]
	if t == nil || time.Since(t.first) > l.window {
		l.tries[key] = &tries{n: 1, first: time.Now()}
		return true
	}
	if t.n >= l.max {
		return false
	}
	t.n++
	return true
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.tries, key)
}
