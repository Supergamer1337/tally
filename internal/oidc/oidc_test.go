package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fake is a small OpenID provider: discovery, keys, a token endpoint that checks PKCE and the
// client secret, and userinfo.
type fake struct {
	t      *testing.T
	srv    *httptest.Server
	rsa    *rsa.PrivateKey
	ec     *ecdsa.PrivateKey
	alg    string                 // how the next ID token is signed
	claims func(c map[string]any) // changes the next ID token's claims
	issued map[string]string      // code -> code_challenge
	nonce  map[string]string      // code -> nonce
	post   bool                   // expect client_secret_post
	issRsp bool                   // advertise RFC 9207
	info   map[string]any         // userinfo
	header func(h map[string]any) // changes the next ID token's header
	sign   func(in []byte) []byte // replaces the signature, to tamper
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t, alg: "RS256", issued: map[string]string{}, nonce: map[string]string{}}
	var err error
	if f.rsa, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
		t.Fatal(err)
	}
	if f.ec, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/authorize", "token_endpoint": f.srv.URL + "/token",
			"userinfo_endpoint": f.srv.URL + "/userinfo", "jwks_uri": f.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported":          []string{"RS256", "ES256"},
			"code_challenge_methods_supported":               []string{"S256"},
			"authorization_response_iss_parameter_supported": f.issRsp,
		})
	})
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{
			map[string]any{"kty": "RSA", "kid": "rsa", "use": "sig", "alg": "RS256", "n": b64(f.rsa.N.Bytes()), "e": b64(big.NewInt(int64(f.rsa.E)).Bytes())},
			map[string]any{"kty": "EC", "kid": "ec", "use": "sig", "alg": "ES256", "crv": "P-256", "x": b64(f.ec.X.FillBytes(make([]byte, 32))), "y": b64(f.ec.Y.FillBytes(make([]byte, 32)))},
		}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		id, secret, basic := r.BasicAuth()
		if f.post {
			id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
		} else if !basic {
			http.Error(w, `{"error":"invalid_client"}`, 401)
			return
		}
		if id != "tally" || secret != "s3cret" {
			http.Error(w, `{"error":"invalid_client"}`, 401)
			return
		}
		code := r.PostForm.Get("code")
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if f.issued[code] == "" || f.issued[code] != b64(sum[:]) || r.PostForm.Get("redirect_uri") != "https://tally.test/auth/callback" {
			http.Error(w, `{"error":"invalid_grant"}`, 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id_token": f.idToken(code), "access_token": "at-" + code, "token_type": "Bearer"})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer at-") {
			http.Error(w, "no", 401)
			return
		}
		json.NewEncoder(w).Encode(f.info)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	f.info = map[string]any{"sub": "u-1", "preferred_username": "felix", "email": "felix@example.com", "groups": []string{"admins"}}
	return f
}

func (f *fake) idToken(code string) string {
	head := map[string]any{"alg": f.alg, "typ": "JWT", "kid": map[string]string{"RS256": "rsa", "ES256": "ec"}[f.alg]}
	if f.header != nil {
		f.header(head)
	}
	now := time.Now().Unix()
	claims := map[string]any{"iss": f.srv.URL, "sub": "u-1", "aud": "tally", "exp": now + 300, "iat": now, "nonce": f.nonce[code]}
	if f.claims != nil {
		f.claims(claims)
	}
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	in := enc(head) + "." + enc(claims)
	sum := sha256.Sum256([]byte(in))
	var sig []byte
	switch f.alg {
	case "RS256":
		sig, _ = rsa.SignPKCS1v15(rand.Reader, f.rsa, crypto.SHA256, sum[:])
	case "ES256":
		r, s, _ := ecdsa.Sign(rand.Reader, f.ec, sum[:])
		sig = append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	}
	if f.sign != nil {
		sig = f.sign(sig)
	}
	return in + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// start begins a sign in and plays the provider's part up to the redirect back.
func (f *fake) start(p *Provider) (Login, url.Values) {
	f.t.Helper()
	l := NewLogin()
	u, err := p.AuthURL(context.Background(), l)
	if err != nil {
		f.t.Fatal(err)
	}
	q, _ := url.Parse(u)
	if q.Query().Get("code_challenge_method") != "S256" || q.Query().Get("state") != l.State || !strings.Contains(q.Query().Get("scope"), "openid") {
		f.t.Fatalf("authorization URL: %s", u)
	}
	code := "c-" + random()[:8]
	f.issued[code] = q.Query().Get("code_challenge")
	f.nonce[code] = q.Query().Get("nonce")
	return l, url.Values{"code": {code}, "state": {l.State}}
}

func (f *fake) provider(method string) *Provider {
	return New(Config{Issuer: f.srv.URL + "/", ClientID: "tally", ClientSecret: "s3cret", RedirectURL: "https://tally.test/auth/callback",
		AuthMethod: method, Scopes: []string{"openid", "profile", "email", "groups"}})
}

func TestSignIn(t *testing.T) {
	for _, alg := range []string{"RS256", "ES256"} {
		f := newFake(t)
		f.alg = alg
		p := f.provider("")
		l, q := f.start(p)
		id, err := p.Finish(context.Background(), l, q)
		if err != nil {
			t.Fatalf("%s: %v", alg, err)
		}
		// The ID token has only sub, so the rest comes from userinfo.
		if id.Subject != "u-1" || id.Username != "felix" || id.Email != "felix@example.com" || len(id.Groups) != 1 || id.Groups[0] != "admins" {
			t.Fatalf("%s: %+v", alg, id)
		}
	}
}

func TestSecretInTheForm(t *testing.T) {
	f := newFake(t)
	f.post = true
	p := f.provider("client_secret_post")
	l, q := f.start(p)
	if _, err := p.Finish(context.Background(), l, q); err != nil {
		t.Fatal(err)
	}
}

func TestBadTokensAreRefused(t *testing.T) {
	cases := map[string]func(f *fake){
		"wrong nonce":    func(f *fake) { f.claims = func(c map[string]any) { c["nonce"] = "other" } },
		"wrong audience": func(f *fake) { f.claims = func(c map[string]any) { c["aud"] = "someone-else" } },
		"wrong issuer":   func(f *fake) { f.claims = func(c map[string]any) { c["iss"] = "https://evil.example" } },
		"expired":        func(f *fake) { f.claims = func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() } },
		"other azp":      func(f *fake) { f.claims = func(c map[string]any) { c["aud"] = []string{"tally", "x"}; c["azp"] = "x" } },
		"tampered":       func(f *fake) { f.sign = func(s []byte) []byte { s[0] ^= 1; return s } },
		"alg none": func(f *fake) {
			f.header = func(h map[string]any) { h["alg"] = "none" }
			f.sign = func([]byte) []byte { return nil }
		},
		"unknown kid":    func(f *fake) { f.header = func(h map[string]any) { h["kid"] = "gone" } },
		"rsa key as ec":  func(f *fake) { f.header = func(h map[string]any) { h["kid"] = "rsa"; h["alg"] = "ES256" } },
		"extra audience": func(f *fake) { f.claims = func(c map[string]any) { c["aud"] = []string{"tally", "untrusted"} } },
		"azp not string": func(f *fake) { f.claims = func(c map[string]any) { c["azp"] = 7 } },
		"no iat":         func(f *fake) { f.claims = func(c map[string]any) { delete(c, "iat") } },
		"no exp":         func(f *fake) { f.claims = func(c map[string]any) { delete(c, "exp") } },
		"aud not string": func(f *fake) { f.claims = func(c map[string]any) { c["aud"] = []any{"tally", 3} } },
	}
	for name, setup := range cases {
		f := newFake(t)
		setup(f)
		p := f.provider("")
		l, q := f.start(p)
		if _, err := p.Finish(context.Background(), l, q); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBadResponsesAreRefused(t *testing.T) {
	f := newFake(t)
	p := f.provider("")
	l, q := f.start(p)
	q.Set("state", "forged")
	if _, err := p.Finish(context.Background(), l, q); err == nil {
		t.Error("a forged state was accepted")
	}
	l, q = f.start(p)
	q.Set("iss", "https://evil.example")
	if _, err := p.Finish(context.Background(), l, q); err == nil {
		t.Error("another issuer was accepted")
	}
	l, _ = f.start(p)
	_, err := p.Finish(context.Background(), l, url.Values{"state": {l.State}, "error": {"access_denied"}})
	if !errors.Is(err, ErrDenied) {
		t.Errorf("a cancelled sign in: %v", err)
	}
	// Userinfo about someone else is not mixed in.
	f.info = map[string]any{"sub": "u-2", "preferred_username": "mallory"}
	l, q = f.start(p)
	if _, err := p.Finish(context.Background(), l, q); err == nil {
		t.Error("userinfo for another subject was accepted")
	}
}

func TestIssuerParameterRequiredWhenAdvertised(t *testing.T) {
	f := newFake(t)
	f.issRsp = true
	p := f.provider("")
	l, q := f.start(p)
	if _, err := p.Finish(context.Background(), l, q); err == nil {
		t.Fatal("a response without iss was accepted from a provider that sends it")
	}
	l, q = f.start(p)
	q.Set("iss", f.srv.URL)
	if _, err := p.Finish(context.Background(), l, q); err != nil {
		t.Fatal(err)
	}
}

func TestPlainHTTPIsRefused(t *testing.T) {
	for _, u := range []string{"http://auth.example.com", "http://10.0.0.5/x", "ftp://auth.example.com"} {
		if secureURL(u) == nil {
			t.Errorf("%s accepted", u)
		}
	}
	for _, u := range []string{"https://auth.example.com", "http://127.0.0.1:9091", "http://localhost/x", "http://[::1]:80"} {
		if err := secureURL(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	p := New(Config{Issuer: "http://auth.example.com", ClientID: "tally"})
	if _, err := p.AuthURL(context.Background(), NewLogin()); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("an http issuer: %v", err)
	}
}

func TestEmailVerified(t *testing.T) {
	for v, want := range map[any]bool{true: true, false: false, "true": true, nil: false} {
		c := map[string]any{"sub": "x", "email": "a@b"}
		if v != nil {
			c["email_verified"] = v
		}
		if got := identity(c).EmailVerified; got != want {
			t.Errorf("email_verified %v: %v", v, got)
		}
	}
}
