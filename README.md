# mhk-server

The online backend of Moorhuhn Kart: Open Source (MHK:OS): player accounts
today; time-trial leaderboards with ghost exchange and real-time races later. Clients speak a small binary
protocol over one TCP (or TLS) connection, specified in [docs/protocol.md](docs/protocol.md). Accounts, sessions
and the moderation audit log live in MySQL 8.4.

## Quick start

Requires Go 1.27 and Docker.

```sh
docker compose up -d mysql
MHK_DB_DSN='mhk:mhk@tcp(localhost:3307)/mhk?parseTime=true' go run ./cmd/mhk-server serve
```

The server applies its database migrations at startup and listens on `:7350` in plaintext, which the game
accepts for `localhost` and private networks (editor builds connect to `tcp://localhost:7350` by default).

`docker compose up -d` runs MySQL and the server together (image built from the `Dockerfile`).

### Tests

```sh
go vet ./...
go test ./...                                    # unit tests; integration tests are skipped
MHK_TEST_DSN='mhk:mhk@tcp(localhost:3307)/mhk_test?parseTime=true' go test -race ./...
```

The compose MySQL creates the `mhk_test` database for the integration tests on first start
(`deploy/mysql-init`). `internal/testclient` is a protocol client for tests, bots and load tests.

## Configuration

Every `serve` flag falls back to an environment variable.

| Flag | Environment | Default | |
|------|-------------|---------|-|
| `-listen` | `MHK_LISTEN` | `:7350` | TCP listen address |
| `-db` | `MHK_DB_DSN` | | MySQL DSN, e.g. `user:pass@tcp(host:3306)/mhk?parseTime=true` (time zone and charset are forced to UTC/utf8mb4) |
| `-tls-cert`, `-tls-key` | `MHK_TLS_CERT`, `MHK_TLS_KEY` | | PEM certificate chain and key; reloaded when the files change |
| `-acme-domain` | `MHK_ACME_DOMAIN` | | obtain a Let's Encrypt certificate for this domain instead |
| `-acme-cache` | `MHK_ACME_CACHE` | | directory for ACME account and certificates (required with ACME) |
| `-acme-http` | `MHK_ACME_HTTP` | `:80` | HTTP-01 challenge listener |
| `-geoip` | `MHK_GEOIP` | | ISO country `.mmdb` (DB-IP Lite Country, GeoLite2-Country); without it countries are unknown until players pick one |
| `-data-dir` | `MHK_DATA_DIR` | `./data` | server data (ghost files from phase 2) |
| `-auto-migrate` | `MHK_AUTO_MIGRATE` | `true` | apply migrations at startup |
| `-log-level` | `MHK_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

Without a certificate or ACME domain the server speaks plaintext and warns when the listen address is not
loopback. `mhk-server migrate` applies migrations without starting the server. The version label shown to
clients is set at build time: `go build -ldflags "-X main.version=v1.2.3" ./cmd/mhk-server`.

## Administration

The sysadmin works on the database directly; the server does not need to be stopped. Disabling an account or
deleting it drops the player's live connections within 30 seconds (the server re-checks sessions periodically).
Every mutating command is written to the moderation audit log with the sysadmin as actor.

```sh
export MHK_DB_DSN='mhk:mhk@tcp(localhost:3307)/mhk?parseTime=true'
mhk-server admin account list [--role moderator] [--limit N]
mhk-server admin account show <username>
mhk-server admin account set-role <username> player|moderator
mhk-server admin account disable <username> --reason "Cheating in time trials"   # also revokes sessions
mhk-server admin account enable <username>
mhk-server admin account delete <username> --yes                                # permanent
mhk-server admin audit [--limit N]
```

Usernames are matched case-insensitively. The disable reason is shown to the player, so write it for them.
Moderators (role `moderator`) act from inside the game once moderation lands in later phases.

## Deployment

- **Ports**: 7350/tcp for the control connection and 7350/udp for real-time races (phase 3). Open both.
- **TLS**: release builds of the game connect to `tls://mhk-os.svit.ac:7350` and validate the certificate
  against the system roots (TLS 1.2+, SNI). Either point `MHK_TLS_CERT`/`MHK_TLS_KEY` at certificates renewed by
  another tool (picked up without restart), or set `MHK_ACME_DOMAIN` and `MHK_ACME_CACHE` and keep port 80 reachable
  for the HTTP-01 challenge. The protocol is raw TLS over TCP, so no HTTP reverse proxy or CDN in front.
- **GeoIP**: download a country database (e.g. [DB-IP Lite Country](https://db-ip.com/db/download/ip-to-country-lite),
  CC BY 4.0, monthly updates) and set `MHK_GEOIP`. Restart to load a newer file.
- **Database**: MySQL 8.4 with utf8mb4. Back it up; it holds all accounts.
- **Container**: the image runs as a non-root user from a distroless base; mount a volume at `/data`.

## License

MIT, see [LICENSE](LICENSE).
