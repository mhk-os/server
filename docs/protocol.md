# MHK:OS online protocol

Binary protocol between Moorhuhn Kart: Open Source (Unity client, `Packages/org.mhkose.sdk/Runtime/Online`) and
`mhk-server`. This file is the contract: the Go codec (`internal/wire`, `internal/proto`) and the C# codec must match it
byte for byte. Change both sides and this file together; bump `Protocol` for incompatible changes.

## Transport

- **Control connection**: one TCP connection per client, URI `tls://host:port` (TLS 1.2+, the server certificate must
  validate against the system roots) or `tcp://host:port` (plaintext). Default port **7350**.
  - Clients refuse plaintext unless the host is `localhost`, a loopback address or a private/link-local address
    (10/8, 172.16/12, 192.168/16, 169.254/16, fc00::/7, fe80::/10). Passwords never cross the internet unencrypted.
  - Default endpoint: editor and unstamped builds (`BuildInfo.Number == 0`) `tcp://localhost:7350`, release builds
    `tls://mhk-os.svit.ac:7350`. Overrides: command line `-server <uri>`, then `UserSettings.onlineServer`.
  - A bare `host[:port]` means `tls://` unless the host qualifies for plaintext, then `tcp://`.
- **Realtime**: UDP on the server's `udpPort` (announced in `HelloOk`), phase 3. Specified in its own section later.

## Frames

Every frame is `u32 length` (little-endian, number of bytes that follow) and then:

| Field  | Type | Meaning |
|--------|------|---------|
| `kind` | u8   | 0 = request (client→server), 1 = response (server→client), 2 = push (server→client), 3 = notify (client→server, never answered) |
| `type` | u16  | message type (table below); a response carries the type of its request |
| `id`   | u32  | request id chosen by the client: non-zero, unique among its requests in flight; echoed by the response; 0 for push and notify |
| body   | …    | message payload |

A **response** body starts with `u16 status`. Status 0 (`Ok`) is followed by the response payload of the message;
any other status is followed by `string message` (English, shown to the player).

Limits: a frame longer than **64 KiB** before login or **16 MiB** after login makes the receiver close the
connection. A request with an unknown `type` is answered with `Unsupported`.

Bodies are **append-only**: newer protocol revisions only add fields at the end, readers ignore trailing bytes.

## Primitive encoding

| Type | Encoding |
|------|----------|
| `u8 u16 u32 u64 i32 i64` | fixed width, little-endian, two's complement for signed |
| `f32` | IEEE 754 binary32, little-endian; NaN and ±Inf are invalid unless a field says otherwise |
| `bool` | u8, 0 or 1 (anything else is invalid) |
| `varuint` | unsigned LEB128, at most 5 bytes, value ≤ 2^31−1 (lengths and counts) |
| `string` | `varuint byteLength` + UTF-8 bytes. Invalid UTF-8 is invalid. Each field has a max byte length (default 256) |
| `bytes` | `varuint length` + raw bytes, max length per field |
| `list<T>` | `varuint count` + items, max count per field |
| `time` | i64 Unix milliseconds UTC |

A malformed body (truncated, over a limit, invalid value) is answered with `BadRequest` (requests) or ignored
(notify). The server closes connections that keep sending malformed frames.

## Status codes (u16)

| Code | Name | Meaning |
|------|------|---------|
| 0 | `Ok` | |
| 1 | `BadRequest` | malformed or invalid request |
| 2 | `NotLoggedIn` | the request needs a logged-in account |
| 3 | `InvalidCredentials` | wrong username or password |
| 4 | `UsernameTaken` | |
| 5 | `InvalidUsername` | does not follow the username rules |
| 6 | `InvalidPassword` | does not follow the password rules |
| 7 | `RateLimited` | too many attempts, try later |
| 8 | `AccountDisabled` | disabled by a moderator; message holds the reason |
| 9 | `VersionMismatch` | protocol version differs: update the game |
| 10 | `NotFound` | |
| 11 | `Forbidden` | logged in but not allowed (e.g. not a moderator) |
| 12 | `TooLarge` | payload over a limit |
| 13 | `ServerError` | unexpected server failure |
| 14 | `SessionInvalid` | session token unknown, expired or revoked: log in again |
| 15 | `Unsupported` | unknown message type |

## Message type ranges

| Range | Area |
|-------|------|
| 1–15 | connection |
| 16–63 | accounts |
| 64–127 | moderation (phase 2/3) |
| 128–255 | time trials, leaderboards, ghosts (phase 2) |
| 256–511 | lobbies (phase 3) |
| 512–767 | race control over TCP (phase 3) |

Push types use their own numbering in the same ranges (kind distinguishes them).

## Connection

### Hello (request 1)

Must be the first request of a connection; anything else first closes the connection.

| Field | Type | |
|-------|------|-|
| magic | u32 | `0x4F4B484D` (bytes `M H K O`) |
| protocol | u16 | **1** |
| gameCommit | string ≤ 40 | `BuildInfo.Commit` |
| gameBuild | i32 | `BuildInfo.Number` |
| simVersion | i32 | `SimulationVersion.Current` |
| modApi | i32 | `ModSdk.ApiLevel` |
| platform | string ≤ 32 | `Application.platform` name |

Response `HelloOk`:

| Field | Type | |
|-------|------|-|
| protocol | u16 | server protocol |
| serverVersion | string ≤ 64 | build label of the server |
| udpPort | u16 | realtime port, 0 = none |
| serverTime | time | |

Status `VersionMismatch` when `protocol` differs (message tells the player to update).

### Ping (request 2)

Request `clientTime: time`; response `clientTime: time` (echo), `serverTime: time`. Clients ping every **15 s**;
the server closes a connection that sent nothing for **45 s**. Clients reconnect with backoff (1, 2, 4, 8, 15, 30 s)
and resume their session with `Resume`.

### Push: Disconnect (push 1)

`status: u16`, `message: string`. The server is closing the connection (account disabled, session revoked, server
shutting down). The client does not reconnect automatically after `AccountDisabled` or `SessionInvalid`.

## Accounts

Rules (server-enforced, clients pre-check for messages):

- **Username**: 3–20 characters of `A–Z a–z 0–9 _ . -`, first and last a letter or digit. Unique case-insensitively;
  the account keeps the case it registered with. Reserved: `admin`, `administrator`, `moderator`, `mod`, `system`,
  `server`, `root`, `mhk`, `mhkos`, `mhk-os`.
- **Password**: 8–128 characters (Unicode code points), not equal to the username ignoring case. Stored as argon2id.
- **Country**: ISO 3166-1 alpha-2 (`DE`, `SK`, …) looked up from the registration IP (GeoIP database, `""` when
  unknown). The player may choose another one; choosing `""` returns to the IP-detected country.
- Registration needs only username + password (the client asks for the password twice). No e-mail, no recovery.

### Structures

`Account`:

| Field | Type | |
|-------|------|-|
| id | u64 | |
| username | string ≤ 20 | |
| country | string ≤ 2 | effective country, `""` = unknown |
| countryFromIp | bool | true while the country is the IP-detected one |
| role | u8 | 0 = player, 1 = moderator |
| created | time | |

`Session`:

| Field | Type | |
|-------|------|-|
| token | string ≤ 128 | opaque; the client stores it and sends it with `Resume` |
| expires | time | sliding: every `Resume` extends it to now + 90 days |
| account | Account | |

### Register (request 16)

`username: string ≤ 64`, `password: string ≤ 512` → `Session`. Errors: `InvalidUsername`, `InvalidPassword`,
`UsernameTaken`, `RateLimited` (3 registrations per IP per hour).

### Login (request 17)

`username: string ≤ 64` (any case), `password: string ≤ 512` → `Session`. Errors: `InvalidCredentials`,
`AccountDisabled` (message = reason), `RateLimited` (10 failures per 10 min per IP, and per username).

### Resume (request 18)

`token: string ≤ 128` → `Session` (same token, new expiry, current account data). Errors: `SessionInvalid`,
`AccountDisabled`.

### Logout (request 19)

Empty → empty. Revokes the session of this connection; the connection stays open, logged out.

### SetCountry (request 20)

`country: string ≤ 2` (`""` or two letters A–Z of a known ISO 3166-1 code) → `Account`. Needs login.
