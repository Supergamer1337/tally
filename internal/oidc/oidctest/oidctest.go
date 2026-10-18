// Package oidctest is a fake OpenID Connect provider for tests: it signs in whoever User says,
// the way Authelia does, with profile claims in userinfo only.
package oidctest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"time"
)

type User struct {
	Subject, Username, Email string
	Groups                   []string
}

type Provider struct {
	URL    string
	User   User
	srv    *httptest.Server
	key    *rsa.PrivateKey
	mu     sync.Mutex
	codes  map[string]grant
	Secret string
}

type grant struct {
	challenge, nonce, redirect string
	user                       User
}

func New() *Provider {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	p := &Provider{key: key, codes: map[string]grant{}, Secret: "s3cret",
		User: User{Subject: "u-1", Username: "felix", Email: "felix@example.com", Groups: []string{"admins"}}}
	mux := http.NewServeMux()
	b64 := base64.RawURLEncoding.EncodeToString
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": p.URL, "authorization_endpoint": p.URL + "/authorize", "token_endpoint": p.URL + "/token",
			"userinfo_endpoint": p.URL + "/userinfo", "jwks_uri": p.URL + "/jwks",
			"id_token_signing_alg_values_supported":          []string{"RS256"},
			"code_challenge_methods_supported":               []string{"S256"},
			"authorization_response_iss_parameter_supported": true,
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "kid": "main", "use": "sig", "alg": "RS256", "n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes())}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		id, secret, _ := r.BasicAuth()
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
		p.mu.Lock()
		g, ok := p.codes[r.PostForm.Get("code")]
		delete(p.codes, r.PostForm.Get("code"))
		p.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if id != "tally" || secret != p.Secret || !ok || g.challenge != b64(sum[:]) || g.redirect != r.PostForm.Get("redirect_uri") {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		now := time.Now().Unix()
		claims := map[string]any{"iss": p.URL, "sub": g.user.Subject, "aud": []string{"tally"}, "azp": "tally", "exp": now + 300, "iat": now, "nonce": g.nonce}
		json.NewEncoder(w).Encode(map[string]any{"id_token": p.sign(claims), "access_token": "at:" + g.user.Subject, "token_type": "Bearer"})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at:"+p.User.Subject {
			w.WriteHeader(401)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"sub": p.User.Subject, "preferred_username": p.User.Username, "email": p.User.Email, "groups": p.User.Groups})
	})
	p.srv = httptest.NewServer(mux)
	p.URL = p.srv.URL
	return p
}

func (p *Provider) Close() { p.srv.Close() }

func (p *Provider) sign(claims map[string]any) string {
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	in := enc(map[string]any{"alg": "RS256", "kid": "main", "typ": "JWT"}) + "." + enc(claims)
	sum := sha256.Sum256([]byte(in))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, sum[:])
	return in + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// Approve plays the person signing in at the provider: it takes the authorization URL the app
// sent the browser to, and returns where the provider sends the browser back.
func (p *Provider) Approve(authURL string) string {
	u, _ := url.Parse(authURL)
	q := u.Query()
	code := make([]byte, 12)
	rand.Read(code)
	c := base64.RawURLEncoding.EncodeToString(code)
	p.mu.Lock()
	p.codes[c] = grant{challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), redirect: q.Get("redirect_uri"), user: p.User}
	p.mu.Unlock()
	back := url.Values{"code": {c}, "state": {q.Get("state")}, "iss": {p.URL}}
	return q.Get("redirect_uri") + "?" + back.Encode()
}

// Deny plays the person cancelling at the provider.
func (p *Provider) Deny(authURL string) string {
	u, _ := url.Parse(authURL)
	q := u.Query()
	back := url.Values{"error": {"access_denied"}, "state": {q.Get("state")}, "iss": {p.URL}}
	return q.Get("redirect_uri") + "?" + back.Encode()
}
