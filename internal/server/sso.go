package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"tally/internal/oidc"
)

// SSO signs in through an OpenID Connect provider such as Authelia. The password, when there is
// one, stays as a way in for when the provider is down.
type SSO struct {
	Provider *oidc.Provider
	// Name is on the button: Sign in with Name.
	Name string
	// Users are the usernames or email addresses allowed in, lower case. An email counts only
	// when the provider says it is verified. "*" lets in anyone the provider signs in.
	Users []string
	// Groups lets in members of any of these groups.
	Groups []string
}

func (s *SSO) allows(id oidc.Identity) bool {
	if slices.Contains(s.Users, "*") {
		return true
	}
	if id.Username != "" && slices.Contains(s.Users, strings.ToLower(id.Username)) {
		return true
	}
	if id.Email != "" && id.EmailVerified && slices.Contains(s.Users, strings.ToLower(id.Email)) {
		return true
	}
	for _, g := range id.Groups {
		if slices.Contains(s.Groups, g) {
			return true
		}
	}
	return false
}

// A sign in waits at most this long for the provider to send the browser back.
const ssoWait = 10 * time.Minute

const ssoCookie = "tally_sso"

// pending holds sign ins that went to the provider and have not come back yet. The cookie with
// the state ties each one to the browser that started it.
type pending struct {
	mu     sync.Mutex
	logins map[string]pendingLogin
}

type pendingLogin struct {
	login oidc.Login
	at    time.Time
}

func (p *pending) put(l oidc.Login) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.logins == nil {
		p.logins = map[string]pendingLogin{}
	}
	for k, v := range p.logins {
		if time.Since(v.at) > ssoWait {
			delete(p.logins, k)
		}
	}
	if len(p.logins) >= 1000 {
		return // a flood of started sign ins does not grow memory
	}
	p.logins[l.State] = pendingLogin{l, time.Now()}
}

func (p *pending) take(state string) (oidc.Login, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.logins[state]
	delete(p.logins, state)
	return v.login, ok && time.Since(v.at) <= ssoWait
}

// ssoStart sends the browser to the provider.
func (s *Server) ssoStart(w http.ResponseWriter, r *http.Request) {
	l := oidc.NewLogin()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	u, err := s.cfg.SSO.Provider.AuthURL(ctx, l)
	if err != nil {
		// The log says why: down, an unknown certificate authority, a wrong issuer.
		s.log.Error("sso: start", "err", err)
		s.renderLogin(w, r, http.StatusBadGateway, "Sign in with "+s.cfg.SSO.Name+" is not working right now")
		return
	}
	s.sso.put(l)
	http.SetCookie(w, &http.Cookie{Name: ssoCookie, Value: l.State, Path: "/auth", MaxAge: int(ssoWait.Seconds()),
		HttpOnly: true, Secure: s.secure(r), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, u, http.StatusFound)
}

// ssoFinish is where the provider sends the browser back.
func (s *Server) ssoFinish(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: ssoCookie, Path: "/auth", MaxAge: -1, HttpOnly: true, Secure: s.secure(r), SameSite: http.SameSiteLaxMode})
	state := r.URL.Query().Get("state")
	c, err := r.Cookie(ssoCookie)
	if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		s.renderLogin(w, r, http.StatusBadRequest, "Sign in did not finish. Try again")
		return
	}
	l, ok := s.sso.take(state)
	if !ok {
		s.renderLogin(w, r, http.StatusBadRequest, "Sign in took too long. Try again")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	id, err := s.cfg.SSO.Provider.Finish(ctx, l, r.URL.Query())
	if errors.Is(err, oidc.ErrDenied) {
		s.log.Warn("sso: not signed in", "err", err)
		s.renderLogin(w, r, http.StatusForbidden, "Sign in with "+s.cfg.SSO.Name+" was stopped")
		return
	}
	if err != nil {
		s.log.Error("sso: finish", "err", err)
		s.renderLogin(w, r, http.StatusBadGateway, "Sign in with "+s.cfg.SSO.Name+" failed")
		return
	}
	if !s.cfg.SSO.allows(id) {
		s.log.Warn("sso: account not allowed", "username", id.Username, "email", id.Email, "groups", id.Groups)
		s.renderLogin(w, r, http.StatusForbidden, "This account cannot use tally. Sign out of "+s.cfg.SSO.Name+" to use another")
		return
	}
	if err := s.startSession(w, r); err != nil {
		s.log.Error("create session", "err", err)
		s.renderLogin(w, r, http.StatusInternalServerError, "Something went wrong")
		return
	}
	s.log.Info("signed in", "via", "sso", "username", id.Username)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
