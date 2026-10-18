package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tally/internal/oidc"
	"tally/internal/oidc/oidctest"
	"tally/internal/store"
	"tally/internal/tally"
)

func TestMain(m *testing.M) {
	pbkdf2Rounds = 1000
	os.Exit(m.Run())
}

func newTestServer(t *testing.T, cfg Config) (*httptest.Server, *tally.Service) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "tally.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc := tally.New(st, time.UTC)
	svc.Now = func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) }
	s, err := New(svc, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, svc
}

func client() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func body(t *testing.T, res *http.Response) string {
	t.Helper()
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return string(b)
}

func send(t *testing.T, c *http.Client, method, u, payload string, headers ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, u, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func signIn(t *testing.T, c *http.Client, base, pw string) int {
	t.Helper()
	res, err := c.PostForm(base+"/login", url.Values{"password": {pw}})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func TestNeedsAPassword(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "tally.db"))
	defer st.Close()
	if _, err := New(tally.New(st, time.UTC), Config{}, slog.Default()); err == nil {
		t.Fatal("a server without a password and with sign in on should not start")
	}
}

func TestSignInAndAPI(t *testing.T) {
	srv, svc := newTestServer(t, Config{Password: "secret"})
	c := client()

	if res := send(t, c, "GET", srv.URL+"/api/state", ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("state before sign in: %d", res.StatusCode)
	}
	if page := body(t, send(t, c, "GET", srv.URL+"/", "")); !strings.Contains(page, `action="/login"`) {
		t.Fatal("the app should show sign in first")
	}
	if code := signIn(t, c, srv.URL, "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", code)
	}
	if code := signIn(t, c, srv.URL, "secret"); code != http.StatusSeeOther {
		t.Fatalf("right password: %d", code)
	}

	svc.Add(context.Background(), "2026-10-16", 30, "</script><script>alert(1)</script>")
	page := body(t, send(t, c, "GET", srv.URL+"/", ""))
	if !strings.Contains(page, `id="state">{"today":"2026-10-18"`) || strings.Contains(page, "</script><script>alert") {
		t.Fatalf("the state should be in the page with markup escaped:\n%s", page[strings.Index(page, `id="state"`):])
	}

	res := send(t, c, "POST", srv.URL+"/api/days/2026-10-18/add", `{"minutes":-45,"note":"Dentist"}`)
	if got := body(t, res); res.StatusCode != 200 || !strings.Contains(got, `"after":{"date":"2026-10-18","minutes":-45,"note":"Dentist"}`) {
		t.Fatalf("add: %d %s", res.StatusCode, got)
	}
	if res := send(t, c, "POST", srv.URL+"/api/days/2026-10-19/add", `{"minutes":15}`); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("future day: %d", res.StatusCode)
	}
	if res := send(t, c, "PUT", srv.URL+"/api/days/2026-10-18", `{"minutes":0,"note":""}`); res.StatusCode != 200 {
		t.Fatalf("undo: %d", res.StatusCode)
	}
	if res := send(t, c, "PUT", srv.URL+"/api/agents/write", `{"on":true}`); res.StatusCode != 200 {
		t.Fatalf("write switch: %d", res.StatusCode)
	}
	if on, _ := svc.AgentsCanWrite(context.Background()); !on {
		t.Fatal("the write switch should be stored")
	}
	if got := body(t, send(t, c, "GET", srv.URL+"/api/state", "")); !strings.Contains(got, `"url":"`+srv.URL+`/mcp"`) || !strings.Contains(got, `"token":"tly_`) {
		t.Fatalf("state: %s", got)
	}
}

func TestCrossSiteWritesAreRefused(t *testing.T) {
	srv, _ := newTestServer(t, Config{Password: "secret"})
	c := client()
	signIn(t, c, srv.URL, "secret")
	for _, h := range [][]string{{"Sec-Fetch-Site", "cross-site"}, {"Origin", "https://evil.example"}} {
		if res := send(t, c, "DELETE", srv.URL+"/api/days/2026-10-16", "", h...); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s: %d", h[0], res.StatusCode)
		}
	}
	if res := send(t, c, "DELETE", srv.URL+"/api/days/2026-10-16", "", "Sec-Fetch-Site", "same-origin"); res.StatusCode != 200 {
		t.Errorf("same origin: %d", res.StatusCode)
	}
}

func TestTooManyTries(t *testing.T) {
	srv, _ := newTestServer(t, Config{Password: "secret"})
	c := client()
	for i := 0; i < 10; i++ {
		signIn(t, c, srv.URL, "wrong")
	}
	if code := signIn(t, c, srv.URL, "secret"); code != http.StatusTooManyRequests {
		t.Fatalf("after ten wrong tries even the right one waits, got %d", code)
	}
}

func TestPasswordChangeSignsOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tally.db")
	start := func(pw string) (*httptest.Server, func()) {
		st, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		s, err := New(tally.New(st, time.UTC), Config{Password: pw}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(s.Handler())
		return srv, func() { srv.Close(); st.Close() }
	}
	srv, stop := start("one")
	c := client()
	signIn(t, c, srv.URL, "one")
	u, _ := url.Parse(srv.URL)
	cookies := c.Jar.Cookies(u)
	stop()

	srv, stop = start("one")
	c.Jar.SetCookies(mustURL(srv.URL), cookies)
	if res := send(t, c, "GET", srv.URL+"/api/state", ""); res.StatusCode != 200 {
		t.Fatalf("same password, still signed in: %d", res.StatusCode)
	}
	stop()

	srv, stop = start("two")
	defer stop()
	c.Jar.SetCookies(mustURL(srv.URL), cookies)
	if res := send(t, c, "GET", srv.URL+"/api/state", ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("new password, signed out: %d", res.StatusCode)
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

func TestSignInPageSaysAChangeWasLost(t *testing.T) {
	srv, _ := newTestServer(t, Config{Password: "secret"})
	if page := body(t, send(t, client(), "GET", srv.URL+"/?unsaved", "")); !strings.Contains(page, "Signed out before the last change was saved") {
		t.Fatal("sign in after a lost change should say so")
	}
	if res := send(t, client(), "GET", srv.URL+"/login", ""); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /login: %d", res.StatusCode)
	}
}

func TestNoAuth(t *testing.T) {
	srv, _ := newTestServer(t, Config{NoAuth: true, PublicURL: "http://127.0.0.1"})
	if res := send(t, client(), "GET", srv.URL+"/api/state", ""); res.StatusCode != 200 {
		t.Fatalf("state without sign in: %d", res.StatusCode)
	}
}

func TestStatic(t *testing.T) {
	srv, _ := newTestServer(t, Config{Password: "secret"})
	c := client()
	page := body(t, send(t, c, "GET", srv.URL+"/", ""))
	i := strings.Index(page, "/static/app.css?v=")
	if i < 0 {
		t.Fatal("pages should link versioned assets")
	}
	versioned := page[i : i+len("/static/app.css?v=")+10]
	res := send(t, c, "GET", srv.URL+versioned, "", "Accept-Encoding", "gzip")
	res.Body.Close()
	if res.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" || res.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("versioned css: %v", res.Header)
	}
	if res := send(t, c, "GET", srv.URL+"/static/app.css", ""); res.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("unversioned css should revalidate: %v", res.Header)
	}
	if res := send(t, c, "GET", srv.URL+"/static/nope.js", ""); res.StatusCode != 404 {
		t.Fatalf("missing asset: %d", res.StatusCode)
	}
	if res := send(t, c, "GET", srv.URL+"/", ""); !strings.Contains(res.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatal("pages should carry a content security policy")
	}
}

func TestConcurrentGuessesAreCounted(t *testing.T) {
	srv, _ := newTestServer(t, Config{Password: "secret"})
	var wg sync.WaitGroup
	codes := make(chan int, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- signIn(t, client(), srv.URL, "wrong")
		}()
	}
	wg.Wait()
	close(codes)
	checked := 0
	for c := range codes {
		if c == http.StatusUnauthorized {
			checked++
		}
	}
	if checked != 10 {
		t.Fatalf("40 guesses at once: %d were checked, want 10", checked)
	}
}

func TestTrustedProxyKeysTheLimit(t *testing.T) {
	// httptest connects from 127.0.0.1, which plays the proxy here.
	srv, _ := newTestServer(t, Config{Password: "secret", TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	from := func(ip, pw string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/login", strings.NewReader(url.Values{"password": {pw}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Forwarded-For", "203.0.113.9, "+ip)
		res, err := client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	for i := 0; i < 12; i++ {
		from("198.51.100.7", "wrong")
	}
	if code := from("198.51.100.7", "secret"); code != http.StatusTooManyRequests {
		t.Fatalf("the guesser should be locked out, got %d", code)
	}
	// The address the proxy saw is the client. Whatever the client wrote before it does not count.
	if code := from("192.0.2.1", "secret"); code != http.StatusSeeOther {
		t.Fatalf("the owner, from another address, should get in: %d", code)
	}
}

func TestUndoConflict(t *testing.T) {
	srv, svc := newTestServer(t, Config{Password: "secret"})
	c := client()
	signIn(t, c, srv.URL, "secret")
	body(t, send(t, c, "POST", srv.URL+"/api/days/2026-10-16/add", `{"minutes":30,"note":"x"}`))
	svc.Add(context.Background(), "2026-10-16", 45, "")
	res := send(t, c, "PUT", srv.URL+"/api/days/2026-10-16", `{"minutes":0,"note":"","expect":{"minutes":30,"note":"x"}}`)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("undo over an agent's change: %d", res.StatusCode)
	}
	res = send(t, c, "PUT", srv.URL+"/api/days/2026-10-16", `{"minutes":0,"note":"","expect":{"minutes":75,"note":"x"}}`)
	if got := body(t, res); res.StatusCode != 200 || !strings.Contains(got, `"after":null`) {
		t.Fatalf("undo with the right expectation: %d %s", res.StatusCode, got)
	}
	res = send(t, c, "PUT", srv.URL+"/api/days/2026-10-16", `{"minutes":30,"note":"x","expect":null}`)
	if got := body(t, res); res.StatusCode != 200 || !strings.Contains(got, `"minutes":30`) {
		t.Fatalf("undo of a delete: %d %s", res.StatusCode, got)
	}
}

func TestNoAuthNeedsItsHost(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "tally.db"))
	defer st.Close()
	if _, err := New(tally.New(st, time.UTC), Config{NoAuth: true}, slog.Default()); err == nil {
		t.Fatal("sign in off without TALLY_PUBLIC_URL should not start")
	}
	srv, _ := newTestServer(t, Config{NoAuth: true, PublicURL: "https://tally.example.com"})
	for host, want := range map[string]int{"tally.example.com": 200, "127.0.0.1:8080": 200, "localhost": 200, "rebind.attacker.example": http.StatusMisdirectedRequest} {
		req, _ := http.NewRequest("GET", srv.URL+"/api/state", nil)
		req.Host = host
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("Host %s: %d, want %d", host, res.StatusCode, want)
		}
	}
}

func TestPublicURLIsAnOrigin(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "tally.db"))
	defer st.Close()
	svc := tally.New(st, time.UTC)
	for _, bad := range []string{"https://tally.example.com/#agents", "https://tally.example.com/?x=1", "https://u:p@tally.example.com", "https://tally.example.com/tally", "tally.example.com", "ftp://tally.example.com"} {
		if _, err := New(svc, Config{Password: "pw", PublicURL: bad}, slog.Default()); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
	for _, good := range []string{"https://tally.example.com", "https://tally.example.com/", "http://127.0.0.1:8080"} {
		if _, err := New(svc, Config{Password: "pw", PublicURL: good}, slog.Default()); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
}

// ssoServer runs tally with single sign on against a fake provider. The public URL is the test
// server's own address, so the provider sends the browser back to it.
func ssoServer(t *testing.T, password string, users ...string) (*httptest.Server, *oidctest.Provider) {
	t.Helper()
	idp := oidctest.New()
	t.Cleanup(idp.Close)
	var handler http.Handler
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	public := "http://" + srv.Listener.Addr().String()
	st, err := store.Open(filepath.Join(t.TempDir(), "tally.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc := tally.New(st, time.UTC)
	sso := &SSO{
		Provider: oidc.New(oidc.Config{Issuer: idp.URL, ClientID: "tally", ClientSecret: idp.Secret, RedirectURL: public + "/auth/callback", Scopes: []string{"openid", "profile", "email", "groups"}}),
		Name:     "Authelia", Users: users,
	}
	s, err := New(svc, Config{Password: password, PublicURL: public, SSO: sso}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	handler = s.Handler()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, idp
}

// ssoSignIn goes from /auth/login to the provider and back, and returns the final answer.
func ssoSignIn(t *testing.T, c *http.Client, srv *httptest.Server, back func(string) string) *http.Response {
	t.Helper()
	res := send(t, c, "GET", srv.URL+"/auth/login", "")
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("/auth/login: %d", res.StatusCode)
	}
	return send(t, c, "GET", back(res.Header.Get("Location")), "")
}

func TestSignInWithSSO(t *testing.T) {
	srv, idp := ssoServer(t, "backup-password", "felix")
	c := client()
	page := body(t, send(t, c, "GET", srv.URL+"/", ""))
	if !strings.Contains(page, `href="/auth/login">Sign in with Authelia</a>`) || !strings.Contains(page, `action="/login"`) {
		t.Fatalf("sign in should offer Authelia and the password:\n%s", page)
	}
	res := ssoSignIn(t, c, srv, idp.Approve)
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		t.Fatalf("callback: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if res := send(t, c, "GET", srv.URL+"/api/state", ""); res.StatusCode != 200 {
		t.Fatalf("signed in with SSO, state: %d", res.StatusCode)
	}
	// The password still works, as the way in when the provider is down.
	if code := signIn(t, client(), srv.URL, "backup-password"); code != http.StatusSeeOther {
		t.Fatalf("password next to SSO: %d", code)
	}
}

func TestSSORefusals(t *testing.T) {
	srv, idp := ssoServer(t, "", "felix")
	// Only SSO: no password form, and posting one does not sign in.
	if page := body(t, send(t, client(), "GET", srv.URL+"/", "")); strings.Contains(page, `action="/login"`) {
		t.Fatal("no password form without a password")
	}
	if code := signIn(t, client(), srv.URL, "anything"); code == http.StatusSeeOther {
		t.Fatal("a password sign in without a password")
	}
	cases := []struct {
		name string
		user oidctest.User
		back func(string) string
		want int
		text string
	}{
		{"someone else", oidctest.User{Subject: "u-2", Username: "mallory", Email: "m@example.com"}, idp.Approve, http.StatusForbidden, "This account cannot use tally"},
		{"cancelled", idp.User, idp.Deny, http.StatusForbidden, "was stopped"},
	}
	for _, tc := range cases {
		idp.User = tc.user
		c := client()
		res := ssoSignIn(t, c, srv, tc.back)
		if got := body(t, res); res.StatusCode != tc.want || !strings.Contains(got, tc.text) {
			t.Errorf("%s: %d %q", tc.name, res.StatusCode, got)
		}
		if res := send(t, c, "GET", srv.URL+"/api/state", ""); res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: signed in anyway", tc.name)
		}
	}
	// A callback in a browser that did not start the sign in, and a replayed one, are refused.
	idp.User = oidctest.User{Subject: "u-1", Username: "Felix", Email: "felix@example.com"}
	c := client()
	res := send(t, c, "GET", srv.URL+"/auth/login", "")
	back := idp.Approve(res.Header.Get("Location"))
	if res := send(t, client(), "GET", back, ""); res.StatusCode != http.StatusBadRequest {
		t.Errorf("another browser's callback: %d", res.StatusCode)
	}
	if res := send(t, c, "GET", back, ""); res.StatusCode != http.StatusSeeOther {
		t.Errorf("the right browser, username in another case: %d", res.StatusCode)
	}
	if res := send(t, c, "GET", back, ""); res.StatusCode == http.StatusSeeOther {
		t.Error("a replayed callback signed in again")
	}
}

func TestSSOSettings(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "x.db"))
	defer st.Close()
	if _, err := New(tally.New(st, time.UTC), Config{SSO: &SSO{Provider: oidc.New(oidc.Config{Issuer: "https://x"}), Name: "x"}}, slog.Default()); err == nil {
		t.Fatal("SSO without a public URL should not start")
	}
	byEmail := &SSO{Users: []string{"felix@example.com"}}
	if byEmail.allows(oidc.Identity{Username: "mallory", Email: "felix@example.com"}) {
		t.Fatal("an unverified email let someone in")
	}
	if !byEmail.allows(oidc.Identity{Username: "felix", Email: "Felix@Example.com", EmailVerified: true}) {
		t.Fatal("a verified email should let its owner in")
	}
	s := &SSO{Groups: []string{"admins"}}
	if !s.allows(oidc.Identity{Username: "anyone", Groups: []string{"users", "admins"}}) || s.allows(oidc.Identity{Username: "anyone", Groups: []string{"users"}}) {
		t.Fatal("groups decide")
	}
	if !(&SSO{Users: []string{"*"}}).allows(oidc.Identity{Subject: "x"}) {
		t.Fatal("* lets anyone in")
	}
}
