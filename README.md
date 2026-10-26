# mhk-server

The online backend of Moorhuhn Kart: Open Source (MHK:OS): player accounts, time-trial leaderboards with ghost
upload and download, in-game moderation, and real-time multiplayer (public lobbies, private rooms, races over UDP).
Clients speak a small binary protocol over one TCP (or TLS) connection plus UDP during races, specified in
[docs/protocol.md](docs/protocol.md). Accounts, sessions, boards, runs, run reports, content manifests and votes,
content strikes, cheat kicks and the moderation audit log live in MariaDB (10.11+); the ghost files themselves live in the
data directory. Lobbies and races live in the server's memory: a restart ends them.

## Quick start

Requires Go 1.27 and Docker.

```sh
docker compose up -d db
MHK_DB_DSN='mhk:mhk@tcp(localhost:3307)/mhk?parseTime=true' go run ./cmd/mhk-server serve
```

The server applies its database migrations at startup and listens on `:7350` in plaintext, which the game
accepts for `localhost` and private networks (editor builds connect to `tcp://localhost:7350` by default).

`docker compose up -d` runs MariaDB and the server together (image built from the `Dockerfile`).

### Tests

```sh
go vet ./...
go test ./...                                    # unit tests; integration tests are skipped
MHK_TEST_DSN='mhk:mhk@tcp(localhost:3307)/mhk_test?parseTime=true' go test -race ./...
```

The compose MariaDB creates the `mhk_test` database for the integration tests on first start
(`deploy/db-init`). `internal/testclient` is a protocol client for tests, bots and load tests.

### Bots

`cmd/mhk-bot` joins a lobby and races by replaying a ghost, to test the game against remote karts:

```sh
go run ./cmd/mhk-bot dump-manifests -dsn "$MHK_DB_DSN" -sim 4 -out packs.json game mhk2 mhk3 mhke mhkt
go run ./cmd/mhk-bot -manifests packs.json -ghost run.mhkghost -user bot -pass kart-racer-2026 -join ABC234 -n 3
```

`dump-manifests` writes the pack manifests players reported most for that `simVersion` (the game must have
announced them once); the bot announces them in `ContentHello`. `-quick CATEGORY` joins a public lobby instead of
a room code, `-n` starts that many bots (`bot1` … `bot3`, registered when missing), `-character`/`-kart` default to
the ghost's, and `-platform NAME` sets the Hello platform (a Unity `RuntimePlatform` name such as `Android` or
`WindowsPlayer`; default `mhk-bot`, an unknown device) to test device icons. Bots ping over TCP every 15 s
(`-keepalive`) so the idle timeout never drops them during UDP-only races. They vote for the ghost's track (else
Random), drive the ghost's path from GO, send `Finished` at its finish time and never use items. They cannot know the
track data, so each sends a unique `RaceLoaded` hash: with
one human (or bots only) nobody has a content majority and everybody races with the lowest grid slot's content
(when that is a bot, the server knows no item boxes and denies the human's pickups); with two or more humans the
bots leave as desync.

## Configuration

Every `serve` flag falls back to an environment variable.

| Flag | Environment | Default | |
|------|-------------|---------|-|
| `-listen` | `MHK_LISTEN` | `:7350` | TCP listen address |
| `-udp-listen` | `MHK_UDP_LISTEN` | `:7350` | UDP realtime listen address (races); its port is announced in `HelloOk` |
| `-db` | `MHK_DB_DSN` | | MariaDB DSN, e.g. `user:pass@tcp(host:3306)/mhk?parseTime=true` (time zone and charset are forced to UTC/utf8mb4) |
| `-tls-cert`, `-tls-key` | `MHK_TLS_CERT`, `MHK_TLS_KEY` | | PEM certificate chain and key; reloaded when the files change |
| `-acme-domain` | `MHK_ACME_DOMAIN` | | obtain a Let's Encrypt certificate for this domain instead |
| `-acme-cache` | `MHK_ACME_CACHE` | | directory for ACME account and certificates (required with ACME) |
| `-acme-http` | `MHK_ACME_HTTP` | `:80` | HTTP-01 challenge listener |
| `-geoip` | `MHK_GEOIP` | | ISO country `.mmdb` (DB-IP Lite Country, GeoLite2-Country); without it countries are unknown until players pick one |
| `-data-dir` | `MHK_DATA_DIR` | `./data` | server data: uploaded ghost files under `ghosts/` (the admin CLI needs it too, to delete files) |
| `-auto-migrate` | `MHK_AUTO_MIGRATE` | `true` | apply migrations at startup |
| `-log-level` | `MHK_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

Without a certificate or ACME domain the server speaks plaintext and warns when the listen address is not
loopback. `mhk-server migrate` applies migrations without starting the server. The version label shown to
clients is set at build time: `go build -ldflags "-X main.version=v1.2.3" ./cmd/mhk-server`.

### Ghost files

Every upload is parsed and validated completely (docs/protocol.md, "Upload validation") before it is stored as
`<data dir>/ghosts/<first 2 hex>/<sha256>.mhkghost`, written atomically and shared by identical files. Accounts may
upload 30 runs per hour. Only the sysadmin deletes files: `run delete` and `account delete` remove a file once no run
references it any more. Back the data directory up together with the database.

## Administration

The sysadmin works on the database directly; the server does not need to be stopped. Disabling an account or
deleting it drops the player's live connections within 30 seconds (the server re-checks sessions periodically).
Every mutating command is written to the moderation audit log with the sysadmin as actor. Commands that delete runs
need the data directory (`-data-dir` / `MHK_DATA_DIR`) to remove the ghost files.

```sh
export MHK_DB_DSN='mhk:mhk@tcp(localhost:3307)/mhk?parseTime=true'
mhk-server admin account list [--role moderator] [--limit N]
mhk-server admin account show <username>
mhk-server admin account set-role <username> player|moderator
mhk-server admin account disable <username> --reason "Cheating in time trials"   # also revokes sessions
mhk-server admin account enable <username>
mhk-server admin account delete <username> --yes                                # permanent, with runs and reports
mhk-server admin run list [--account U] [--track T] [--limit N]                 # newest first
mhk-server admin run show <id>
mhk-server admin run hide <id> --reason "Impossible lap"
mhk-server admin run unhide <id>
mhk-server admin run delete <id> --yes                                          # permanent, removes the ghost file
mhk-server admin run reports <id>
mhk-server admin board list [--track T]
mhk-server admin audit [--limit N]
mhk-server admin content strikes [--account U] [--limit N]                       # copies differing from the majority
mhk-server admin kicks [--account U] [--limit N]                                 # race kicks for cheating
```

Usernames are matched case-insensitively. The disable reason is shown to the player, so write it for them.
Moderators (role `moderator`) act from inside the game: they hide and unhide runs, disable and enable player
accounts (not other moderators), and review flagged runs and their reports. Nothing a moderator does is permanent.
Reports from game clients (`flags`) never hide anything by themselves.

## Deployment

- **Ports**: 7350/tcp for the control connection and 7350/udp for real-time races. Open both.
- **TLS**: release builds of the game connect to `tls://mhk-os.svit.ac:7350` and validate the certificate
  against the system roots (TLS 1.2+, SNI). Either point `MHK_TLS_CERT`/`MHK_TLS_KEY` at certificates renewed by
  another tool (picked up without restart), or set `MHK_ACME_DOMAIN` and `MHK_ACME_CACHE` and keep port 80 reachable
  for the HTTP-01 challenge. The protocol is raw TLS over TCP, so no HTTP reverse proxy or CDN in front: the DNS record
  must stay DNS-only in Cloudflare.
- **GeoIP**: a country database (e.g. [DB-IP Lite Country](https://db-ip.com/db/download/ip-to-country-lite),
  CC BY 4.0 - attribution: IP Geolocation by DB-IP, monthly updates) in `MHK_GEOIP`. Restart to load a newer file.
- **Database**: MariaDB 10.11+ with utf8mb4. Back it up together with the data directory; they hold all accounts and runs.
- **Container**: the image runs as a non-root user from a distroless base; mount a volume at `/data` (ghost files).

### Production host (mhk-os.svit.ac)

The production server runs from systemd on an Ubuntu host next to the host's MariaDB and nginx; the files are in
`deploy/`:

| Path on the host | From | |
|------|------|-|
| `/usr/local/bin/mhk-server` | `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-X main.version=<commit>" ./cmd/mhk-server` | the binary |
| `/etc/systemd/system/mhk-server.service` | `deploy/systemd/mhk-server.service` | runs as user `mhk-server`, data in `/var/lib/mhk-server` |
| `/etc/mhk-server/env` | `deploy/env.example` | DSN (database `mhk`, user `mhk@localhost`), TLS, data and GeoIP paths; `root:mhk-server 0640` |
| `/etc/mhk-server/tls/` | `deploy/certbot/20-mhk-server.sh` | readable copies of the Let's Encrypt certificate |
| `/etc/letsencrypt/renewal-hooks/deploy/20-mhk-server.sh` | `deploy/certbot/20-mhk-server.sh` | refreshes the copies after renewal |
| `/etc/nginx/sites-enabled/mhk-os.svit.ac` | `deploy/nginx/mhk-os.svit.ac.conf` | HTTP-01 challenges (webroot `/var/www/acme`), browsers to GitHub |
| `/usr/local/lib/mhk-server/mhk-geoip-update.sh`, `mhk-geoip.{service,timer}` | `deploy/geoip/` | monthly DB-IP update, then a restart |
| `/usr/local/bin/mhk-admin` | `deploy/mhk-admin` | the admin CLI with the service's configuration, e.g. `mhk-admin account list` |

The certificate comes from `certbot certonly --webroot -w /var/www/acme -d mhk-os.svit.ac --key-type ecdsa`. To update
the server: build, copy the binary, `systemctl restart mhk-server` (migrations run at startup; running lobbies and races
end). `journalctl -u mhk-server` shows the log.

## License

MIT, see [LICENSE](LICENSE).
