// Package oidc signs a person in with an OpenID Connect provider such as Authelia. It is a
// small relying party on the standard library: the authorization code flow with PKCE, state
// and nonce, an ID token whose signature is checked against the provider's published keys,
// and the userinfo endpoint for claims the ID token leaves out.
package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	_ "crypto/sha512" // SHA-384 and SHA-512 for the *384 and *512 algorithms
)

type Config struct {
	// Issuer is the provider's address, like https://auth.example.com.
	Issuer   string
	ClientID string
	// ClientSecret is empty for a public client.
	ClientSecret string
	// RedirectURL is this app's callback, registered with the provider.
	RedirectURL string
	// AuthMethod is how the secret reaches the token endpoint: client_secret_basic or
	// client_secret_post. Without a secret it is none.
	AuthMethod string
	Scopes     []string
	// HTTP talks to the provider. Nil means a client with a 10 second timeout.
	HTTP *http.Client
}

// Identity is who the provider says signed in.
type Identity struct {
	Subject  string
	Username string
	Email    string
	// EmailVerified is the provider's word that Email belongs to this person. An email that
	// is not verified must not decide who gets in.
	EmailVerified bool
	Name          string
	Groups        []string
}

// ErrDenied means the person or the provider stopped the sign in, for example by cancelling.
var ErrDenied = errors.New("sign in was not allowed")

type Provider struct {
	cfg    Config
	http   *http.Client
	mu     sync.Mutex
	meta   *metadata
	metaAt time.Time
	keys   []jwk
	keysAt time.Time
	now    func() time.Time
}

func New(cfg Config) *Provider {
	cfg.Issuer = strings.TrimRight(cfg.Issuer, "/")
	if cfg.ClientSecret == "" {
		cfg.AuthMethod = "none"
	} else if cfg.AuthMethod == "" {
		cfg.AuthMethod = "client_secret_basic"
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{"openid"}
	}
	if !slices.Contains(cfg.Scopes, "openid") {
		cfg.Scopes = append([]string{"openid"}, cfg.Scopes...)
	}
	h := cfg.HTTP
	if h == nil {
		h = &http.Client{Timeout: 10 * time.Second}
	}
	// A redirect never takes a request to plain HTTP.
	h.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return secureURL(req.URL.String())
	}
	return &Provider{cfg: cfg, http: h, now: time.Now}
}

// secureURL accepts HTTPS, and plain HTTP only to this machine, where nothing crosses a network.
// Codes, the client secret and tokens travel to these addresses.
func secureURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%q is not an absolute URL", raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	if ip, err := netip.ParseAddr(host); u.Scheme == "http" && (host == "localhost" || (err == nil && ip.IsLoopback())) {
		return nil
	}
	return fmt.Errorf("%s must use https", raw)
}

type metadata struct {
	Issuer           string   `json:"issuer"`
	Authorization    string   `json:"authorization_endpoint"`
	Token            string   `json:"token_endpoint"`
	Userinfo         string   `json:"userinfo_endpoint"`
	JWKS             string   `json:"jwks_uri"`
	IDTokenAlgs      []string `json:"id_token_signing_alg_values_supported"`
	IssParameter     bool     `json:"authorization_response_iss_parameter_supported"`
	CodeChallenges   []string `json:"code_challenge_methods_supported"`
	TokenAuthMethods []string `json:"token_endpoint_auth_methods_supported"`
}

// Check fetches the provider's discovery document, to find out early whether it can be reached.
func (p *Provider) Check(ctx context.Context) error {
	_, err := p.discover(ctx)
	return err
}

// discover reads /.well-known/openid-configuration, kept for an hour. A provider that cannot be
// reached does not stop sign in while a recent copy is at hand.
func (p *Provider) discover(ctx context.Context) (*metadata, error) {
	p.mu.Lock()
	m, at := p.meta, p.metaAt
	p.mu.Unlock()
	if m != nil && p.now().Sub(at) < time.Hour {
		return m, nil
	}
	if err := secureURL(p.cfg.Issuer); err != nil {
		return nil, fmt.Errorf("the issuer: %w", err)
	}
	var fresh metadata
	if err := p.getJSON(ctx, p.cfg.Issuer+"/.well-known/openid-configuration", "", &fresh); err != nil {
		if m != nil {
			return m, nil
		}
		return nil, fmt.Errorf("read the discovery document of %s: %w", p.cfg.Issuer, err)
	}
	// The issuer in the document must be the one configured, or tokens could come from anywhere.
	if strings.TrimRight(fresh.Issuer, "/") != p.cfg.Issuer {
		return nil, fmt.Errorf("the provider calls itself %q, not %q", fresh.Issuer, p.cfg.Issuer)
	}
	if fresh.Authorization == "" || fresh.Token == "" || fresh.JWKS == "" {
		return nil, errors.New("the discovery document has no authorization, token or jwks endpoint")
	}
	for _, u := range []string{fresh.Authorization, fresh.Token, fresh.JWKS, fresh.Userinfo} {
		if u == "" {
			continue
		}
		if err := secureURL(u); err != nil {
			return nil, fmt.Errorf("the discovery document: %w", err)
		}
	}
	if len(fresh.CodeChallenges) > 0 && !slices.Contains(fresh.CodeChallenges, "S256") {
		return nil, errors.New("the provider does not support PKCE with S256")
	}
	p.mu.Lock()
	p.meta, p.metaAt = &fresh, p.now()
	p.mu.Unlock()
	return &fresh, nil
}

func (p *Provider) getJSON(ctx context.Context, u, bearer string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", u, res.Status)
	}
	return json.Unmarshal(body, v)
}

// Login is what a sign in in progress keeps until the provider sends the browser back.
type Login struct {
	State    string
	Nonce    string
	Verifier string
}

func random() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func NewLogin() Login { return Login{State: random(), Nonce: random(), Verifier: random()} }

// AuthURL is where to send the browser to sign in.
func (p *Provider) AuthURL(ctx context.Context, l Login) (string, error) {
	m, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	challenge := sha256.Sum256([]byte(l.Verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {p.cfg.ClientID},
		"redirect_uri":          {p.cfg.RedirectURL},
		"scope":                 {strings.Join(p.cfg.Scopes, " ")},
		"state":                 {l.State},
		"nonce":                 {l.Nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	sep := "?"
	if strings.Contains(m.Authorization, "?") {
		sep = "&"
	}
	return m.Authorization + sep + q.Encode(), nil
}

// Finish completes a sign in from the query the provider sent the browser back with.
func (p *Provider) Finish(ctx context.Context, l Login, q url.Values) (Identity, error) {
	m, err := p.discover(ctx)
	if err != nil {
		return Identity{}, err
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(l.State)) != 1 {
		return Identity{}, errors.New("the state does not match the sign in that was started")
	}
	// RFC 9207: a response names its issuer, so one provider cannot pass off another's code.
	if iss := q.Get("iss"); iss != "" && iss != m.Issuer {
		return Identity{}, fmt.Errorf("the response came from %q, not %q", iss, m.Issuer)
	} else if iss == "" && m.IssParameter {
		return Identity{}, errors.New("the response does not name its issuer")
	}
	if e := q.Get("error"); e != "" {
		return Identity{}, fmt.Errorf("%w: %s %s", ErrDenied, e, q.Get("error_description"))
	}
	code := q.Get("code")
	if code == "" {
		return Identity{}, errors.New("the response has no code")
	}
	tok, err := p.exchange(ctx, m, code, l.Verifier)
	if err != nil {
		return Identity{}, err
	}
	claims, err := p.verify(ctx, m, tok.IDToken, l.Nonce)
	if err != nil {
		return Identity{}, fmt.Errorf("the ID token: %w", err)
	}
	id := identity(claims)
	// Some providers, Authelia among them, leave profile claims to the userinfo endpoint.
	if (id.Username == "" || id.Email == "" || id.Groups == nil) && m.Userinfo != "" && tok.AccessToken != "" {
		more, err := p.userinfo(ctx, m, tok.AccessToken)
		if err != nil {
			return Identity{}, fmt.Errorf("userinfo: %w", err)
		}
		if s, _ := more["sub"].(string); s != id.Subject {
			return Identity{}, errors.New("userinfo is about someone else")
		}
		for k, v := range more {
			if _, ok := claims[k]; !ok {
				claims[k] = v
			}
		}
		id = identity(claims)
	}
	return id, nil
}

type tokens struct {
	IDToken     string `json:"id_token"`
	AccessToken string `json:"access_token"`
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

func (p *Provider) exchange(ctx context.Context, m *metadata, code, verifier string) (tokens, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {p.cfg.RedirectURL},
		"code_verifier": {verifier},
	}
	switch p.cfg.AuthMethod {
	case "client_secret_post":
		form.Set("client_id", p.cfg.ClientID)
		form.Set("client_secret", p.cfg.ClientSecret)
	case "none":
		form.Set("client_id", p.cfg.ClientID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return tokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if p.cfg.AuthMethod == "client_secret_basic" {
		// RFC 6749 2.3.1: both parts are form encoded before they go into Basic.
		req.SetBasicAuth(url.QueryEscape(p.cfg.ClientID), url.QueryEscape(p.cfg.ClientSecret))
	}
	res, err := p.http.Do(req)
	if err != nil {
		return tokens{}, fmt.Errorf("token endpoint: %w", err)
	}
	defer res.Body.Close()
	var t tokens
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&t); err != nil {
		return tokens{}, fmt.Errorf("token endpoint answered %s", res.Status)
	}
	if res.StatusCode != http.StatusOK || t.Error != "" {
		return tokens{}, fmt.Errorf("token endpoint answered %s: %s %s", res.Status, t.Error, t.Description)
	}
	if t.IDToken == "" {
		return tokens{}, errors.New("token endpoint sent no ID token, check that the openid scope is allowed")
	}
	return t, nil
}

func (p *Provider) userinfo(ctx context.Context, m *metadata, access string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.Userinfo, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Accept", "application/json, application/jwt")
	res, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("answered %s", res.Status)
	}
	// A provider may sign userinfo. Then it is a JWT from the same issuer, with the same keys,
	// and it has to be from that issuer and for this client.
	if ct, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type")); ct == "application/jwt" {
		claims, err := p.verifySigned(ctx, m, strings.TrimSpace(string(body)))
		if err != nil {
			return nil, err
		}
		if iss, _ := claims["iss"].(string); iss != m.Issuer {
			return nil, fmt.Errorf("signed userinfo is from %q, not %q", iss, m.Issuer)
		}
		if aud, ok := audience(claims["aud"]); !ok || !slices.Contains(aud, p.cfg.ClientID) {
			return nil, errors.New("signed userinfo is not for this client")
		}
		return claims, nil
	}
	var claims map[string]any
	return claims, json.Unmarshal(body, &claims)
}

func identity(c map[string]any) Identity {
	str := func(k string) string { s, _ := c[k].(string); return s }
	id := Identity{Subject: str("sub"), Username: str("preferred_username"), Email: str("email"), Name: str("name")}
	switch v := c["email_verified"].(type) {
	case bool:
		id.EmailVerified = v
	case string:
		id.EmailVerified = v == "true"
	}
	switch g := c["groups"].(type) {
	case []any:
		id.Groups = []string{}
		for _, v := range g {
			if s, ok := v.(string); ok {
				id.Groups = append(id.Groups, s)
			}
		}
	case string:
		id.Groups = []string{g}
	}
	return id
}

// verify checks an ID token: its signature, issuer, audience, expiry and nonce.
func (p *Provider) verify(ctx context.Context, m *metadata, raw, nonce string) (map[string]any, error) {
	claims, err := p.verifySigned(ctx, m, raw)
	if err != nil {
		return nil, err
	}
	// OpenID Connect Core 3.1.3.7, claim by claim. Every one has to be there with the right type.
	if iss, _ := claims["iss"].(string); iss != m.Issuer {
		return nil, fmt.Errorf("issued by %q, not %q", iss, m.Issuer)
	}
	// tally trusts no other audience, so the token has to be for tally alone.
	aud, ok := audience(claims["aud"])
	if !ok || len(aud) == 0 {
		return nil, errors.New("no audience")
	}
	for _, a := range aud {
		if a != p.cfg.ClientID {
			return nil, fmt.Errorf("issued for %v, not only %q", aud, p.cfg.ClientID)
		}
	}
	if v, ok := claims["azp"]; ok {
		if azp, isString := v.(string); !isString || azp != p.cfg.ClientID {
			return nil, fmt.Errorf("authorized party is %v, not %q", v, p.cfg.ClientID)
		}
	}
	now := float64(p.now().Unix())
	const leeway = 60
	exp, ok := claims["exp"].(float64)
	if !ok {
		return nil, errors.New("no expiry")
	}
	if now > exp+leeway {
		return nil, errors.New("expired")
	}
	iat, ok := claims["iat"].(float64)
	if !ok {
		return nil, errors.New("no issue time")
	}
	if iat > now+leeway {
		return nil, errors.New("issued in the future")
	}
	if got, _ := claims["nonce"].(string); subtle.ConstantTimeCompare([]byte(got), []byte(nonce)) != 1 {
		return nil, errors.New("the nonce does not match")
	}
	if s, _ := claims["sub"].(string); s == "" {
		return nil, errors.New("no subject")
	}
	return claims, nil
}

// verifySigned checks a JWT's signature against the provider's keys and returns its claims.
func (p *Provider) verifySigned(ctx context.Context, m *metadata, raw string) (map[string]any, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("not a signed JWT")
	}
	var head struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodePart(parts[0], &head); err != nil {
		return nil, err
	}
	if _, ok := algorithms[head.Alg]; !ok {
		return nil, fmt.Errorf("signed with %q, which tally does not accept", head.Alg)
	}
	if len(m.IDTokenAlgs) > 0 && !slices.Contains(m.IDTokenAlgs, head.Alg) {
		return nil, fmt.Errorf("signed with %q, which the provider does not list", head.Alg)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("signature is not base64url")
	}
	keys, err := p.keysFor(ctx, m, head.Kid)
	if err != nil {
		return nil, err
	}
	signed := []byte(parts[0] + "." + parts[1])
	ok := false
	for _, k := range keys {
		if k.alg != "" && k.alg != head.Alg {
			continue
		}
		if algorithms[head.Alg](k.key, signed, sig) {
			ok = true
			break
		}
	}
	if !ok {
		return nil, errors.New("the signature does not verify")
	}
	var claims map[string]any
	if err := decodePart(parts[1], &claims); err != nil {
		return nil, err
	}
	return claims, nil
}

// audience reads aud, which is one string or an array of strings. ok is false for anything else.
func audience(v any) ([]string, bool) {
	switch a := v.(type) {
	case string:
		return []string{a}, true
	case []any:
		out := make([]string, 0, len(a))
		for _, x := range a {
			s, isString := x.(string)
			if !isString {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}

func decodePart(s string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return errors.New("a JWT part is not base64url")
	}
	return json.Unmarshal(b, v)
}

type jwk struct {
	kid string
	alg string
	key crypto.PublicKey
}

// keysFor returns the keys that may have signed a token with this kid. An unknown kid fetches
// the key set again, at most once a minute, so a provider can rotate keys without a restart.
func (p *Provider) keysFor(ctx context.Context, m *metadata, kid string) ([]jwk, error) {
	pick := func(all []jwk) []jwk {
		var out []jwk
		for _, k := range all {
			if kid == "" || k.kid == kid {
				out = append(out, k)
			}
		}
		return out
	}
	p.mu.Lock()
	keys, at := p.keys, p.keysAt
	p.mu.Unlock()
	if found := pick(keys); len(found) > 0 && p.now().Sub(at) < 24*time.Hour {
		return found, nil
	}
	if keys != nil && p.now().Sub(at) < time.Minute {
		return nil, fmt.Errorf("no key %q in the provider's key set", kid)
	}
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := p.getJSON(ctx, m.JWKS, "", &set); err != nil {
		return nil, fmt.Errorf("read the provider's keys: %w", err)
	}
	keys = []jwk{}
	for _, raw := range set.Keys {
		if k, err := parseJWK(raw); err == nil {
			keys = append(keys, k)
		}
	}
	p.mu.Lock()
	p.keys, p.keysAt = keys, p.now()
	p.mu.Unlock()
	if found := pick(keys); len(found) > 0 {
		return found, nil
	}
	return nil, fmt.Errorf("no key %q in the provider's key set", kid)
}

func parseJWK(raw json.RawMessage) (jwk, error) {
	var k struct {
		Kty, Kid, Alg, Use, Crv, N, E, X, Y string
	}
	if err := json.Unmarshal(raw, &k); err != nil {
		return jwk{}, err
	}
	if k.Use != "" && k.Use != "sig" {
		return jwk{}, errors.New("not a signing key")
	}
	b := func(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
	out := jwk{kid: k.Kid, alg: k.Alg}
	switch k.Kty {
	case "RSA":
		n, err1 := b(k.N)
		e, err2 := b(k.E)
		if err1 != nil || err2 != nil || len(e) > 4 {
			return jwk{}, errors.New("bad RSA key")
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if pub.N.BitLen() < 2048 {
			return jwk{}, errors.New("RSA key too short")
		}
		out.key = pub
	case "EC":
		curve := map[string]elliptic.Curve{"P-256": elliptic.P256(), "P-384": elliptic.P384(), "P-521": elliptic.P521()}[k.Crv]
		x, err1 := b(k.X)
		y, err2 := b(k.Y)
		if curve == nil || err1 != nil || err2 != nil {
			return jwk{}, errors.New("bad EC key")
		}
		size := (curve.Params().BitSize + 7) / 8
		if len(x) > size || len(y) > size {
			return jwk{}, errors.New("bad EC key")
		}
		point := make([]byte, 1+2*size)
		point[0] = 4
		copy(point[1+size-len(x):], x)
		copy(point[1+2*size-len(y):], y)
		pub, err := ecdsa.ParseUncompressedPublicKey(curve, point)
		if err != nil {
			return jwk{}, err
		}
		out.key = pub
	case "OKP":
		x, err := b(k.X)
		if k.Crv != "Ed25519" || err != nil || len(x) != ed25519.PublicKeySize {
			return jwk{}, errors.New("bad OKP key")
		}
		out.key = ed25519.PublicKey(x)
	default:
		return jwk{}, errors.New("unknown key type")
	}
	return out, nil
}

// algorithms are the JWS algorithms tally accepts. none is never among them.
var algorithms = map[string]func(key crypto.PublicKey, signed, sig []byte) bool{
	"RS256": rsaPKCS(crypto.SHA256), "RS384": rsaPKCS(crypto.SHA384), "RS512": rsaPKCS(crypto.SHA512),
	"PS256": rsaPSS(crypto.SHA256), "PS384": rsaPSS(crypto.SHA384), "PS512": rsaPSS(crypto.SHA512),
	"ES256": ecdsaSig(crypto.SHA256, elliptic.P256()), "ES384": ecdsaSig(crypto.SHA384, elliptic.P384()), "ES512": ecdsaSig(crypto.SHA512, elliptic.P521()),
	"EdDSA": func(key crypto.PublicKey, signed, sig []byte) bool {
		k, ok := key.(ed25519.PublicKey)
		return ok && ed25519.Verify(k, signed, sig)
	},
}

func digest(h crypto.Hash, b []byte) []byte {
	w := h.New()
	w.Write(b)
	return w.Sum(nil)
}

func rsaPKCS(h crypto.Hash) func(crypto.PublicKey, []byte, []byte) bool {
	return func(key crypto.PublicKey, signed, sig []byte) bool {
		k, ok := key.(*rsa.PublicKey)
		return ok && rsa.VerifyPKCS1v15(k, h, digest(h, signed), sig) == nil
	}
}

func rsaPSS(h crypto.Hash) func(crypto.PublicKey, []byte, []byte) bool {
	return func(key crypto.PublicKey, signed, sig []byte) bool {
		k, ok := key.(*rsa.PublicKey)
		return ok && rsa.VerifyPSS(k, h, digest(h, signed), sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}) == nil
	}
}

// ecdsaSig checks a JWS ECDSA signature, which is r and s side by side, not ASN.1. Each
// algorithm goes with one curve.
func ecdsaSig(h crypto.Hash, curve elliptic.Curve) func(crypto.PublicKey, []byte, []byte) bool {
	return func(key crypto.PublicKey, signed, sig []byte) bool {
		k, ok := key.(*ecdsa.PublicKey)
		if !ok || k.Curve != curve {
			return false
		}
		size := (k.Curve.Params().BitSize + 7) / 8
		if len(sig) != 2*size {
			return false
		}
		r, s := new(big.Int).SetBytes(sig[:size]), new(big.Int).SetBytes(sig[size:])
		return ecdsa.Verify(k, digest(h, signed), r, s)
	}
}
