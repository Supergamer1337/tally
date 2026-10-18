// tally tracks working time that differs from a normal day.
//
// Configuration is read from the environment:
//
//	TALLY_ADDR           where to listen, default :8080
//	TALLY_DB             the SQLite file, default tally.db
//	TALLY_PASSWORD       the password to sign in with (or TALLY_PASSWORD_FILE to read it from a file)
//	TALLY_AUTH=off       no sign in, for running behind a proxy that does its own
//	TALLY_OIDC_ISSUER    sign in with an OpenID Connect provider such as Authelia, see the README
//	TALLY_PUBLIC_URL     the address people use, like https://tally.example.com
//	TALLY_TRUSTED_PROXIES addresses of reverse proxies whose X-Forwarded-For to believe, like 172.16.0.0/12
//	TZ                   the time zone that decides what today is, like Europe/Stockholm
//	TALLY_LOG=debug      log every MCP request, to see what an agent sends
//	PUID, PGID           started as root, as the image is, tally makes its data directory its
//	                     own and then runs as this user and group (65532 in the image)
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // zone names work in a scratch image too

	"tally/internal/oidc"
	"tally/internal/server"
	"tally/internal/store"
	"tally/internal/tally"
)

var version = "dev"

const usage = `tally tracks working time that differs from a normal day.

Usage:
  tally [serve]        run the server
  tally healthcheck    exit 0 when the server on TALLY_ADDR answers
  tally token          print the token agents use
  tally rotate-token   replace that token, agents with the old one stop working
  tally backup FILE    write a consistent copy of the database to FILE, or to stdout with -
  tally demo           fill an empty database with sample data
  tally version        print the version

Settings come from TALLY_ADDR, TALLY_DB, TALLY_PASSWORD or TALLY_PASSWORD_FILE,
TALLY_AUTH=off, TALLY_PUBLIC_URL, TALLY_TRUSTED_PROXIES, TALLY_OIDC_* and TZ.
See the README.
`

func main() {
	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv("TALLY_LOG"), "debug") {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	// Before anything touches the database, so no command run as root leaves files behind that
	// the server cannot write.
	if err := dropRoot(log); err != nil {
		fmt.Fprintln(os.Stderr, "tally:", err)
		os.Exit(1)
	}
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(log)
	case "healthcheck":
		err = healthcheck()
	case "token", "rotate-token", "demo":
		err = withService(func(ctx context.Context, svc *tally.Service) error {
			switch cmd {
			case "token":
				t, err := svc.MCPToken(ctx)
				fmt.Println(t)
				return err
			case "rotate-token":
				t, err := svc.RotateMCPToken(ctx)
				fmt.Println(t)
				return err
			default:
				n, err := svc.Demo(ctx)
				if err == nil {
					fmt.Printf("added %d sample days\n", n)
				}
				return err
			}
		})
	case "backup":
		if len(os.Args) != 3 {
			err = errors.New("usage: tally backup FILE")
			break
		}
		err = withService(func(ctx context.Context, svc *tally.Service) error {
			if os.Args[2] != "-" {
				return svc.Store.Backup(ctx, os.Args[2])
			}
			// To stdout, for `docker exec tally /tally backup - > tally.db`. The image has no
			// /tmp, so the copy is made next to the database and removed after.
			tmp := filepath.Join(filepath.Dir(env("TALLY_DB", "tally.db")), fmt.Sprintf(".backup-%d.db", os.Getpid()))
			defer os.Remove(tmp)
			if err := svc.Store.Backup(ctx, tmp); err != nil {
				return err
			}
			f, err := os.Open(tmp)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(os.Stdout, f)
			return err
		})
	case "version", "-v", "--version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tally:", err)
		os.Exit(1)
	}
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func withService(fn func(context.Context, *tally.Service) error) error {
	loc, err := zone()
	if err != nil {
		return err
	}
	st, err := store.Open(env("TALLY_DB", "tally.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	return fn(context.Background(), tally.New(st, loc))
}

// zone loads TZ by name, so the browser can be told the same zone and keep today in step.
func zone() (*time.Location, error) {
	name := env("TZ", "UTC")
	if strings.HasPrefix(name, ":") {
		name = name[1:]
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("TZ=%s is not a time zone tally knows, use a name like Europe/Stockholm", name)
	}
	return loc, nil
}

// trustedProxies reads TALLY_TRUSTED_PROXIES: addresses or ranges, separated by commas.
func trustedProxies() ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.FieldsFunc(env("TALLY_TRUSTED_PROXIES", ""), func(r rune) bool { return r == ',' || r == ' ' }) {
		if p, err := netip.ParsePrefix(f); err == nil {
			out = append(out, p.Masked())
		} else if a, err := netip.ParseAddr(f); err == nil {
			out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
		} else {
			return nil, fmt.Errorf("TALLY_TRUSTED_PROXIES: %q is not an address or a range", f)
		}
	}
	return out, nil
}

// secret reads KEY, or the file KEY_FILE names, for Docker secrets.
func secret(key string) (string, error) {
	if file := env(key+"_FILE", ""); file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read %s_FILE: %w", key, err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return os.Getenv(key), nil
}

func list(key string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(os.Getenv(key), func(r rune) bool { return r == ',' || r == ' ' }) {
		out = append(out, f)
	}
	return out
}

// sso reads the TALLY_OIDC_* settings. Without TALLY_OIDC_ISSUER there is no single sign on.
func sso(public string) (*server.SSO, error) {
	issuer := env("TALLY_OIDC_ISSUER", "")
	if issuer == "" {
		return nil, nil
	}
	if public == "" {
		return nil, errors.New("TALLY_OIDC_ISSUER needs TALLY_PUBLIC_URL: the provider sends the browser back to TALLY_PUBLIC_URL/auth/callback")
	}
	clientSecret, err := secret("TALLY_OIDC_CLIENT_SECRET")
	if err != nil {
		return nil, err
	}
	var users []string
	for _, u := range list("TALLY_OIDC_USERS") {
		users = append(users, strings.ToLower(u))
	}
	groups := list("TALLY_OIDC_GROUPS")
	if len(users) == 0 && len(groups) == 0 {
		return nil, errors.New("set TALLY_OIDC_USERS to your username or email (or TALLY_OIDC_GROUPS), so only you get in. TALLY_OIDC_USERS=* lets in anyone the provider signs in")
	}
	scopes := list("TALLY_OIDC_SCOPES")
	if len(scopes) == 0 {
		scopes = []string{"openid", "profile", "email", "groups"}
	}
	return &server.SSO{
		Provider: oidc.New(oidc.Config{
			Issuer:       issuer,
			ClientID:     env("TALLY_OIDC_CLIENT_ID", "tally"),
			ClientSecret: clientSecret,
			RedirectURL:  strings.TrimRight(public, "/") + "/auth/callback",
			AuthMethod:   env("TALLY_OIDC_AUTH_METHOD", ""),
			Scopes:       scopes,
		}),
		Name:   env("TALLY_OIDC_NAME", "SSO"),
		Users:  users,
		Groups: groups,
	}, nil
}

func serve(log *slog.Logger) error {
	pw, err := secret("TALLY_PASSWORD")
	if err != nil {
		return err
	}
	public := env("TALLY_PUBLIC_URL", "")
	single, err := sso(public)
	if err != nil {
		return err
	}
	noAuth := strings.EqualFold(env("TALLY_AUTH", ""), "off")
	if pw == "" && !noAuth && single == nil {
		return errors.New("set TALLY_PASSWORD (or TALLY_PASSWORD_FILE) to the password to sign in with, TALLY_OIDC_ISSUER to sign in with Authelia or another OpenID Connect provider, or TALLY_AUTH=off to run without sign in behind a proxy that does its own")
	}
	loc, err := zone()
	if err != nil {
		return err
	}
	proxies, err := trustedProxies()
	if err != nil {
		return err
	}
	path := env("TALLY_DB", "tally.db")
	st, err := store.Open(path)
	if err != nil {
		return err
	}
	defer st.Close()
	svc := tally.New(st, loc)
	srv, err := server.New(svc, server.Config{Password: pw, NoAuth: noAuth, PublicURL: public, TrustedProxies: proxies, SSO: single, Version: version}, log)
	if err != nil {
		return err
	}
	if single != nil {
		// Say early when the provider cannot be reached, rather than at the first sign in.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := single.Provider.Check(ctx); err != nil {
				log.Warn("single sign on: the provider cannot be used yet, sign in with it fails until this is fixed", "err", err)
			} else {
				log.Info("single sign on is ready", "issuer", env("TALLY_OIDC_ISSUER", ""))
			}
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		for t := time.NewTicker(24 * time.Hour); ; {
			if err := st.Prune(ctx, time.Now()); err != nil && ctx.Err() == nil {
				log.Error("prune", "err", err)
			}
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}()

	addr := env("TALLY_ADDR", ":8080")
	hs := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	// Shutdown waits for open requests, so long lived ones are told to end first.
	hs.RegisterOnShutdown(srv.Close)
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	log.Info("tally is running", "version", version, "addr", addr, "db", path, "tz", loc.String(), "today", svc.Today(), "auth", !noAuth)
	if os.Getenv("TZ") == "" {
		log.Warn("TZ is not set, so today follows UTC. Set TZ, like TZ=Europe/Stockholm, to follow your own day")
	}
	if public == "" {
		log.Warn("TALLY_PUBLIC_URL is not set. Behind a reverse proxy, set it to the address you use, like https://tally.example.com, so the cookie, cross origin checks and the agent address are right")
	}
	if noAuth {
		log.Warn("sign in is off. Anyone who can reach this port can read and change everything, so only your proxy should be able to")
	} else if pw != "" && len([]rune(pw)) < 12 {
		log.Warn("the password is shorter than 12 characters, consider a longer one")
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return hs.Shutdown(shut)
}

// healthcheck asks the local server for /healthz. The image has no curl, so tally does it.
func healthcheck() error {
	host, port, err := net.SplitHostPort(env("TALLY_ADDR", ":8080"))
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	c := &http.Client{Timeout: 3 * time.Second}
	res, err := c.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz answered %s", res.Status)
	}
	return nil
}
