<img src="assets/icon.svg" width="64" height="64" alt="">

# tally

Created in [T3 Code](https://t3.codes).

Design handoff and the clickable reference live in [design/](design/README.md).

tally tracks working time that differs from a normal day. Log only the days that were noticeably
longer or shorter, and tally keeps a running balance. Add from a phone in a few taps, read the
balance and its history on Stats, and let AI agents read it (and, if you allow it, add to it) over
MCP.

One Go binary with the web app built in, data in one SQLite file, and a ~17 MB container image.
No JavaScript build step and no external services: the fonts are self hosted.

## Run it

GitHub Actions builds the image and publishes it to `ghcr.io/supergamer1337/tally` on every push
to `main` (`latest`) and every `v*` tag (`1.2.3`, `1.2`), for amd64 and arm64. A new package on
GitHub is private: either make it public under the package's settings, or `docker login ghcr.io`
on the server with a token that has `read:packages`.

tally belongs behind a reverse proxy that terminates HTTPS (Caddy, Traefik, nginx, Tailscale
Serve), at the root of its own host name. Copy [compose.yaml](compose.yaml), put the settings in a
`.env` file next to it and start it:

```sh
cat > .env <<'END'
TALLY_PASSWORD='something long'
TALLY_PUBLIC_URL=https://tally.example.com
END
docker compose up -d
```

Keep the single quotes: without them compose reads `$` in a password as a variable and ` #` as
the start of a comment, and tally gets a different password than the one you type. Or put the
password in a file and use `TALLY_PASSWORD_FILE` with a Docker secret.

The compose file publishes tally on `127.0.0.1:8080` only, so it is reached through the proxy and
never straight from the network. Point the proxy at `http://127.0.0.1:8080`, and let `/mcp`
through without the proxy's own login, since agents sign in with their token. Caddy needs nothing
more than `tally.example.com { reverse_proxy 127.0.0.1:8080 }`.

If the proxy runs in Docker too, drop `ports:` from compose.yaml, put tally on the proxy's network
and point the proxy at `tally:8080`:

```yaml
services:
  tally:
    networks: [proxy]
networks:
  proxy:
    external: true # the network your proxy container is on
```

nginx needs the headers passed on:

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

The container runs as user 65532 and keeps everything in `/data`. A named volume works as is.
A bind mount needs to be writable by that user: `chown 65532:65532 ./data`.

To update, `docker compose pull && docker compose up -d`. After changing `.env`, run
`docker compose up -d` as well: `restart` keeps the old settings.

To restore a backup, stop tally, put the file in the volume as `tally.db` owned by 65532, delete
any `tally.db-wal` and `tally.db-shm` next to it, and start tally again:

```sh
docker compose stop tally
docker run --rm -v tally_tally-data:/data -v "$PWD":/backup alpine \
  sh -c 'cp /backup/tally-backup.db /data/tally.db && rm -f /data/tally.db-wal /data/tally.db-shm && chown 65532:65532 /data/tally.db'
docker compose start tally
```

The volume's name is the compose project (the directory's name) followed by `_tally-data`;
`docker volume ls` shows it.

### Settings

| Variable | Default | |
|---|---|---|
| `TALLY_PASSWORD` | | The password to sign in with. Required unless single sign on is set up or `TALLY_AUTH=off`. Next to single sign on it is the way in for when the provider is down. Changing it signs every browser out. |
| `TALLY_PASSWORD_FILE` | | Read the password from a file instead, for Docker secrets. |
| `TALLY_PUBLIC_URL` | | The address you use, like `https://tally.example.com`. Set it behind a proxy: it marks the cookie Secure, lets older browsers pass the cross origin check and gives agents the right address. Sign in through this address, not a LAN one. |
| `TALLY_TRUSTED_PROXIES` | | Addresses or ranges of your proxies, like `127.0.0.1,172.16.0.0/12`. Their `X-Forwarded-For` decides whose wrong passwords are counted, so a stranger cannot lock you out. Without it every visitor through the proxy shares one count of 10 tries per 15 minutes. |
| `TZ` | `UTC` | Decides what today is, for the app and for agents. Set it to your own zone, like `Europe/Stockholm`. |
| `TALLY_AUTH` | | `off` turns sign in off, for a proxy that does its own. Needs `TALLY_PUBLIC_URL`, and then only requests for that host, `localhost` or a plain IP address are answered. |
| `TALLY_OIDC_*` | | Sign in with Authelia or another OpenID Connect provider. See [Sign in with Authelia](#sign-in-with-authelia). |
| `TALLY_LOG` | | `debug` logs every MCP request with its protocol version, method and agent. |
| `TALLY_DB` | `/data/tally.db` in the image | The SQLite file. |
| `TALLY_ADDR` | `:8080` | Where to listen. |

### Commands

The binary doubles as a small admin tool. In the container, run them with `docker exec tally /tally <command>`.

| | |
|---|---|
| `tally token` | Print the token agents use. |
| `tally rotate-token` | Replace it. Agents with the old token stop working at once. |
| `tally backup FILE` | Write a consistent copy of the database while tally runs, replacing FILE. `tally backup -` writes it to stdout: `docker exec tally /tally backup - > tally-$(date +%F).db` keeps it off the server's volume. |
| `tally demo` | Fill an empty database with the sample data from the design reference. It cannot be taken out again, so try it on a volume of its own, as below. |
| `tally healthcheck` | Used by the image's `HEALTHCHECK`. |

To look at tally with the sample data, away from your real data:

```sh
docker run --rm -v tally-demo:/data ghcr.io/supergamer1337/tally demo
docker run --rm -p 127.0.0.1:8081:8080 -e TALLY_PASSWORD=demo -e TZ=Europe/Stockholm \
  -v tally-demo:/data ghcr.io/supergamer1337/tally      # http://localhost:8081, password demo
docker volume rm tally-demo                      # when done
```

## Sign in with Authelia

tally can sign you in through Authelia, or any OpenID Connect provider, so there is no separate
password to remember. It uses the authorization code flow with PKCE as a confidential client.
Keep `TALLY_PASSWORD` set as well if you want a way in while Authelia is down: the sign in page
then shows "Sign in with Authelia" first and the password under it.

Generate a client secret. Authelia keeps only the digest:

```sh
docker run --rm authelia/authelia:4.39 authelia crypto hash generate pbkdf2 --variant sha512 \
  --random --random.length 72 --random.charset rfc3986
```

Add tally as a client in Authelia's `configuration.yml` (Authelia 4.39), with the `Digest:` line
as `client_secret`. If OpenID Connect is not on in your Authelia yet, it also needs a signing key
under `identity_providers.oidc.jwks`, see
[Authelia's provider settings](https://www.authelia.com/configuration/identity-providers/openid-connect/provider/).

```yaml
identity_providers:
  oidc:
    clients:
      - client_id: 'tally'
        client_name: 'tally'
        client_secret: '$pbkdf2-sha512$310000$...'
        public: false
        authorization_policy: 'two_factor'   # or one_factor
        require_pkce: true
        pkce_challenge_method: 'S256'
        redirect_uris:
          - 'https://tally.example.com/auth/callback'
        scopes: ['openid', 'profile', 'email', 'groups']
        response_types: ['code']
        grant_types: ['authorization_code']
        token_endpoint_auth_method: 'client_secret_basic'
        id_token_signed_response_alg: 'RS256'
        userinfo_signed_response_alg: 'none'
        consent_mode: 'pre-configured'       # tick "remember" once instead of every sign in
        pre_configured_consent_duration: '1 year'
```

Then give tally the other half, in `.env` next to the compose file:

```sh
TALLY_PUBLIC_URL=https://tally.example.com
TALLY_OIDC_ISSUER=https://auth.example.com
TALLY_OIDC_CLIENT_SECRET='the random secret, not the digest'
TALLY_OIDC_USERS=felix
TALLY_OIDC_NAME=Authelia
```

`TALLY_OIDC_USERS` is your Authelia username, and `TALLY_OIDC_NAME` puts "Sign in with Authelia"
on the button. Keep comments on lines of their own: outside compose, `docker run --env-file`
reads a comment after a value as part of the value.

| Variable | Default | |
|---|---|---|
| `TALLY_OIDC_ISSUER` | | Authelia's address, exactly as in its `authelia_url`, without a trailing slash. Turns single sign on on. Needs `TALLY_PUBLIC_URL`; the redirect URI is that followed by `/auth/callback`. |
| `TALLY_OIDC_CLIENT_ID` | `tally` | |
| `TALLY_OIDC_CLIENT_SECRET` | | Or `TALLY_OIDC_CLIENT_SECRET_FILE`. Leave it out for a public client. |
| `TALLY_OIDC_USERS` | | Usernames allowed in, separated by commas. An email address works too, when the provider marks it as verified. `*` lets in anyone Authelia signs in. Authelia's `access_control` rules do not apply to OpenID Connect, so this is what keeps other accounts out. |
| `TALLY_OIDC_GROUPS` | | Or let in members of these groups. |
| `TALLY_OIDC_NAME` | `SSO` | The name on the button. |
| `TALLY_OIDC_AUTH_METHOD` | `client_secret_basic` | `client_secret_post` if that is what the client is registered with. |
| `TALLY_OIDC_SCOPES` | `openid profile email groups` | |

Good to know:

- tally fetches Authelia's discovery document, keys and userinfo itself, so the container must
  reach `TALLY_OIDC_ISSUER` at that exact address, over HTTPS. Since Authelia 4.39 the username, email and
  groups come from userinfo, which tally asks for; no claims policy is needed.
- The image trusts the usual public certificate authorities. If Authelia uses a certificate
  from a private CA, mount the CA's certificate and set `SSL_CERT_FILE` to its path, as the
  comments in compose.yaml show. The file has to be readable by user 65532: one it cannot read is
  skipped without a word, and sign in fails with "certificate signed by unknown authority".
- Signing in with Authelia gives the same 180 day session as the password.
- Agents keep using the token from the Agents screen. Authelia has neither Dynamic Client
  Registration nor Client ID Metadata Documents, the two ways MCP agents register themselves,
  so agents cannot sign in through it yet.

## Agents

tally speaks MCP over Streamable HTTP at `/mcp`, with the token from the Agents screen as a
bearer token. It speaks the current revision, 2026-07-28, where every request stands on its
own, and still answers agents that open a session with `initialize` (2025-11-25 and earlier). Agents can read by default: `get_balance`, `list_days` and `get_trend`. Turning
on "Let agents add time" adds `add_day`. Agents on 2026-07-28 that listen for changes, such as
Claude Code, hear about it the moment you flip the switch; others see it within a minute, or in
their next session. The Connected list shows which agents used which tool.

Claude Code, for every project (`--scope user`; without it the server is only added for the
directory you run it in):

```sh
claude mcp add --scope user --transport http tally https://tally.example.com/mcp \
  --header "Authorization: Bearer tly_..."
```

Codex:

```sh
export TALLY_TOKEN=tly_...
codex mcp add tally --url https://tally.example.com/mcp --bearer-token-env-var TALLY_TOKEN
```

## Develop

The toolchain comes from the Nix flake (`direnv allow`, or `nix develop`). Without Nix, any Go
1.26 works.

```sh
go test ./...
go run ./cmd/tally demo                                  # sample data in ./tally.db
TALLY_PASSWORD=dev TZ=Europe/Stockholm go run ./cmd/tally   # http://localhost:8080
go test -race ./...                                      # what CI runs
```

| | |
|---|---|
| `cmd/tally` | Entry point, settings and admin commands. |
| `internal/tally` | The rules: entries, dates, amounts, balance, periods, trend. |
| `internal/store` | SQLite. Schema changes are numbered migrations in `store.go`. |
| `internal/server` | The web app, its JSON API, sign in. |
| `internal/mcp` | The MCP endpoint and its four tools. |
| `web/` | The pages, styles, scripts and fonts, embedded into the binary as they are. |

The web app is ported from the reference in `design/reference` and keeps its look and behaviour.
What the design left open was decided like this:

- **Sign in** is a single password from the environment, with a session cookie that lasts 180
  days and renews while used. A new password signs everyone out. There is no sign out button
  yet: `POST /logout` exists for when one is designed.
- **Agent token** is one per install, made on first start. Revoke it with `tally rotate-token`.
- **MCP** is Streamable HTTP without server streams: each POST gets one JSON answer.
- **Today** is today in `TZ`, for both the app and agents. The browser keeps it in step on its
  own, so it moves on at midnight and when a phone wakes.
- **Undo** only puts a day back while it still holds what the change left. If an agent or another
  device changed the day in between, Undo says "Not undone" rather than throw that change away.
- **Offline**: a save that cannot reach the server is rolled back on screen and says "Not saved".
- **First run** shows a zero balance, empty charts and "Nothing logged yet".
- **Phone and desktop do different jobs.** On a phone you log today in a few taps, so Add,
  Stats and Agents stay separate screens. On a desktop you look back and fill in days, so Add and
  Stats are one screen, Time: the form, the charts and the period's days side by side. There a
  click on a calendar day or a row puts that day in the form, Days scrolls in its own column, and
  the cursor waits in the hours field (1, Tab, 30, Enter).
- **Saving adds to a day**, so the form says what a day already holds and what it will hold:
  "Has −50m, becomes −20m". Tapping a day in Recent or Days puts it in the form.
- **The rest of the spec's "Later" list** is done: 40px period buttons on phones, 90m becoming
  1h 30m, a dark knob on the switch, and the calendar running to the end of the week, month or
  quarter with the days to come as outlines.
- **Weekends** are always in the calendar, not only when the period has a weekend entry as the
  design had it, so weekend work is there to see and log. They sit below a small gap, and a
  weekend without an entry is dimmer than a normal weekday, with "Weekend" in the key and the
  readout.
