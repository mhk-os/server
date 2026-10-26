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

## Time trials (phase 2)

### Concepts

- **Run**: one uploaded ghost file (`.mhkghost`, the game's own format, see the game repo
  `Packages/org.mhkose.sdk/Runtime/Ghosts/GhostFile.cs`, `GhostHeaderCodec.cs`, `GhostBodyCodec.cs`,
  `GhostChannelCodec.cs`). The server parses and validates the whole file; the owner is the uploading account, never
  the `playerName`/`playerId` inside the file. Identified by `runId` (u64). The file's header `id` (32 hex) is unique
  across the server: the same account uploading it again gets the existing run back (`duplicate`), another account
  gets `Forbidden`.
- **Board**: runs that are comparable: same `trackId` (case-insensitive), `engineClass` (0–2), `laps`, `simVersion`
  and `trackHash`. A new simulation version or changed track content therefore starts a new board; older boards
  stay browsable ("archived" in the client: any board whose `simVersion`/`trackHash` differs from the client's own).
  A leaderboard lists **one row per account: its best visible run** on the board (fastest `finishMs`, ties by earlier
  upload), filtered as requested, ranked from 1.
- **Setup mismatch**: `setupHash` covers kart physics, engine class and laps. For every
  (`kartId`, `engineClass`, `laps`, `simVersion`) the server counts distinct accounts per `setupHash`; a run whose
  `setupHash` is not the one used by the most accounts (once ≥ 3 accounts uploaded for that key) is marked
  `setupMismatch`. Display only; never hides anything.
- **Flags**: game clients validate ghosts while they play back and report what they detect (`ReportRun`). `flags` =
  number of distinct accounts that reported the run. Reports from accounts that were ever kicked from a race for
  cheating, and reports on one's own run, are accepted and ignored. Nothing is hidden automatically.
- **Hidden**: a moderator hid the run (or the owner's account is disabled). Hidden runs are excluded for everyone
  except moderators (who see them with `hidden` set when they ask for `includeHidden`). Nothing a moderator does is
  permanent; only the sysadmin CLI deletes.

### Upload validation (server)

Rejected with `BadRequest` and a message naming the problem when any check fails:

- compressed size ≤ 8 MiB, inflated size ≤ 48 MiB (GZip), magic `MHKG`, format version 1, header and body block
  lengths within the inflated data, no trailing garbage beyond the two blocks;
- header: `id` 32 hex chars; `trackId`, `trackPackId`, `kartId`, `characterId` 1–96 chars of `[A-Za-z0-9_.-]`;
  `trackHash` and `setupHash` 32 hex chars; `engineClass` 0–2; `laps` 1–99 and equal to the lap time count;
  `tickSeconds` within 1e-6 of 1/60 … 1/30 (any `1/n`, n = 30…240); `finishTime` > 0 and < 3 h; every lap time
  > 0; `sum(lapTimes) ≤ finishTime + 0.05 s` and the run-up from the grid to the first line crossing
  `finishTime − sum(lapTimes)` ≤ 60 s; `speedScale` finite and > 0; `frameCount` > 0,
  ≤ (finishTime + 10 s) / tickSeconds and ≥ finishTime / tickSeconds; all floats finite;
- body: effect string table ≤ 4096 entries of ≤ 128 bytes, every channel decodes exactly `frameCount` values,
  positions finite and within ±100 km, events ≤ 1,000,000 with valid kinds, effect indexes in range and times within
  [−10 s, finishTime + 10 s];
- the account uploaded ≤ 30 runs in the last hour (`RateLimited` otherwise).

### Structures

`BoardInfo`:

| Field | Type | |
|-------|------|-|
| boardId | u64 | |
| trackId | string ≤ 96 | |
| engineClass | u8 | |
| laps | u8 | |
| simVersion | i32 | |
| trackHash | string ≤ 32 | |
| players | u32 | accounts with a visible run |
| bestMs | i32 | fastest visible finish, 0 when none |
| lastUpload | time | 0 when none |

`RunEntry`:

| Field | Type | |
|-------|------|-|
| runId | u64 | |
| rank | u32 | rank in the requested (filtered) list, 0 when not ranked (hidden, or not the account's best) |
| accountId | u64 | |
| username | string ≤ 20 | |
| country | string ≤ 2 | |
| finishMs | i32 | |
| bestLapMs | i32 | |
| lapMs | list<i32> ≤ 99 | |
| kartId | string ≤ 96 | |
| characterId | string ≤ 96 | |
| ghostId | string ≤ 32 | header `id` |
| recorded | time | header date |
| uploaded | time | |
| flags | u32 | distinct reporters |
| setupMismatch | bool | |
| hidden | bool | only ever true for moderators |
| fileSize | u32 | compressed bytes |
| boardId | u64 | |
| trackId | string ≤ 96 | the board's track |
| engineClass | u8 | the board's engine class |

`LeaderboardFilter`:

| Field | Type | |
|-------|------|-|
| username | string ≤ 20 | case-insensitive substring, `""` = any |
| country | string ≤ 2 | `""` = any |
| continent | string ≤ 2 | `AF AN AS EU NA OC SA`, `""` = any |
| kartId | string ≤ 96 | `""` = any |
| characterId | string ≤ 96 | `""` = any |
| includeHidden | bool | honoured for moderators only |

With a kart/character filter, "best per account" means the best run matching the filter.

### ListBoards (request 128)

`trackId: string ≤ 96`, `engineClass: u8` (255 = any), `simVersion: i32` (0 = any) → `list<BoardInfo> ≤ 200`,
most recent upload first. No login needed.

### GetLeaderboard (request 129)

`boardId: u64`, `offset: u32`, `limit: u16` (1–100), `filter: LeaderboardFilter` →
`total: u32` (rows in the filtered list), `entries: list<RunEntry>`, `hasMine: bool`, then `mine: RunEntry` when
`hasMine` (the logged-in account's best matching the filter, with its rank). No login needed.

### UploadRun (request 130)

`file: bytes ≤ 8 MiB` → `run: RunEntry` (rank within the unfiltered board, 0 if not the account's best),
`personalBest: bool` (the run is now the account's best on its board), `duplicate: bool`. Needs login.

### DownloadRun (request 131)

`runId: u64` → `run: RunEntry`, `file: bytes`. No login needed. Hidden runs are `NotFound` except for moderators.
Clients may keep many downloads in flight.

### ReportRun (request 132)

`runId: u64`, `reason: u8`, `atSeconds: f32` (ghost time of the detection), `detail: string ≤ 512` →
`flags: u32` (current count). Needs login. One report per account and run counts; repeating it updates nothing.

Reasons (u8): 1 `Speed` (above the kart's possible speed), 2 `Acceleration`, 3 `Teleport` (moved without a respawn
cut), 4 `WallClip` (inside static geometry), 5 `Flying` (airborne without cause), 6 `Checkpoints` (lap without
all checkpoints, wrong finish), 7 `LapTimes` (lap times disagree with the frames), 8 `Divergence` (re-simulation
from the recorded inputs ends elsewhere), 9 `Data` (inconsistent data), 255 `Other`.

### MyRuns (request 133)

`trackId: string ≤ 96` → `list<RunEntry> ≤ 500`: every run of the logged-in account on that track (all boards,
hidden ones included with `hidden` set), so the client can mark local ghosts as uploaded. Needs login.

## Moderation (phase 2)

All need a moderator account (`Forbidden` otherwise) and write the audit log. Moderators cannot disable moderator
accounts. `reason` is shown to the affected player where applicable.

| Type | Name | Request | Response |
|------|------|---------|----------|
| 64 | ModHideRun | `runId: u64`, `reason: string ≤ 256` | empty |
| 65 | ModUnhideRun | `runId: u64` | empty |
| 66 | ModDisableAccount | `accountId: u64`, `reason: string ≤ 256` | empty (revokes the account's sessions, connected clients get push `Disconnect(AccountDisabled, reason)`) |
| 67 | ModEnableAccount | `accountId: u64` | empty |
| 68 | ModListFlagged | `offset: u32`, `limit: u16` (1–100), `includeHidden: bool` | `total: u32`, `list<RunEntry>` (flags ≥ 1, most flags first, then newest) |
| 69 | ModRunReports | `runId: u64` | `list<RunReport> ≤ 500` |
| 70 | ModGetAccount | `accountId: u64` | `ModAccountInfo` |

`RunReport`: `reporter: string ≤ 20`, `reason: u8`, `atSeconds: f32`, `detail: string ≤ 512`, `created: time`,
`counted: bool` (false when the reporter's reports are ignored).

`ModAccountInfo`: `account: Account`, `disabled: bool`, `disabledReason: string ≤ 256`, `runs: u32`,
`hiddenRuns: u32`, `cheatKicks: u32` (phase 3), `created: time`, `lastLogin: time`.

## Multiplayer (phase 3)

### Overview

- Up to **10 players** per lobby and race. Every client simulates its **own kart** (client authority over its own
  motion) and renders the others from UDP snapshots. Kart-vs-kart contacts are resolved by each client against
  kinematic stand-ins of the others.
- The server decides everything that has a single truth: lobby phases, which track, the GO instant, item box
  pickups and item rolls, which items a player holds, hit arbitration, finish order, points, kicks.
- Clients simulate projectiles locally from the use events; the server keeps them in sync by the **median** of the
  positions clients report and flags outliers.
- **Anticheat** (lobbies with anticheat on): clients check other karts' motion with the plausibility engine (the
  ghost validator's core) and report; the server kicks on reports from enough distinct eligible observers. Server-side
  checks (items held, pickup distance, projectile outliers, ignored hits) add strikes. Accounts that were ever kicked
  for cheating have their reports ignored forever. Nobody is banned by this; moderators can still disable accounts.
- An anticheat-off lobby skips report processing, strikes and kicks; item and hit arbitration still run so the race
  stays consistent.

### Eligibility and thresholds

| Rule | Value |
|------|-------|
| observer and observed RTT (UDP, server-measured) for a check or report to count | ≤ 200 ms each |
| client: report a kart after | 3 strikes within 10 s from its own checks |
| server: kick a racer for cheating when within 15 s | distinct eligible reporters ≥ max(2, ⌈eligible observers / 2⌉) |
| eligible observers | other racers in the race, RTT ≤ 200 ms, never kicked for cheating |
| server strikes (item use without holding it, pickup too far, projectile outlier, ignored forced hit) | kick at 5 within 30 s |
| projectile outlier | > max(4 m, 3 × median absolute deviation) from the component-wise median, ≥ 3 reporters |
| unknown content | ≥ 2 distinct reporters about racer X → X leaves the race; exactly 1 reporter → the reporter leaves the race (desync); neither counts as cheating unless the item is absent from the catalog majority of X's pack version (fabricated) |

### Content agreement

Clients describe the gameplay content of every enabled mod pack as a **pack manifest**: `packId`, `commit`
(`ContentPack.version`) and the sorted list of items `(kind, id, fingerprint)` the pack registers, where the
fingerprint covers the item's gameplay-relevant data (visuals and sounds excluded) of the **effective** definition
(after load-order overrides). Kinds: 1 race track (definition data; geometry is hashed at race load), 2 kart,
3 character, 4 powerup, 5 status effect, 6 cup, 7 battle arena (a track with the arena layout). `manifestHash` = 32 hex
chars, SHA-256/128 over the canonical item list. Votes for Random draw a random race track (race lobbies) or arena
(battle lobbies) of the lobby's packs from the catalog-majority manifests.

For each (`packId`, `commit`, `simVersion`) the server keeps which `manifestHash` each account reported; the
**catalog majority** is the hash reported by the most distinct accounts (a tie means no majority: then each client's
own manifest decides what it may select, without strikes). Nothing is decided by the first report. A
client whose hash differs from a majority backed by ≥ 3 accounts gets a content strike (stored, shown to
moderators) and cannot join anticheat lobbies with that pack ("your copy of <pack> differs from other players'").
The built-in `game` pack is announced like any other pack; lobbies do not list it but its items are lobby content.

A lobby's **content** = the items of its packs (plus the built-in `game` pack). Players may join a lobby if they have
every pack of it enabled (extra packs are fine; their items are not selectable there). A client that has a mod
overriding an id of a lobby pack must disable it for that lobby (the client checks before joining).

### Public lobby categories (u8)

| # | Mode | Packs | Anticheat |
|---|------|-------|-----------|
| 1 | Races | mhk2, mhk3, mhke, mhkt | on |
| 2 | Races | mhk2, mhk3, mhke, mhkt | off |
| 3 | Races | mhk2, mhk3, mhke, mhkt, tincan | on |
| 4 | Races | mhk2, mhk3, mhke, mhkt, tincan | off |
| 5 | Battles | mhk2, mhk3, mhke, mhkt | on |
| 6 | Battles | mhk2, mhk3, mhke, mhkt | off |
| 7 | Battles | mhk2, mhk3, mhke, mhkt, tincan | on |
| 8 | Battles | mhk2, mhk3, mhke, mhkt, tincan | off |
| 9 | Races | mhk2 | on |
| 10 | Races | mhk2 | off |

Instances exist per (category, `simVersion`): there is always one instance with a free slot; when all are full a
new one opens; an empty instance closes while another one of its category has room. Public lobbies race at
**150cc** (engine class 2), track laps, items on with default odds. Flow, while at least **two humans** are in the
lobby: **Voting** (20 s or until everyone voted: each player votes a track/arena or Random; one vote is drawn at
random) → Loading → Countdown → Racing → Results (10 s) → after **4 races** SetResults (15 s, points reset) → Voting.
With one human the lobby waits (phase Waiting, nothing starts by itself). Players joining during a race wait for the
next one. Points per race by position: 15, 12, 10, 8, 7, 6, 5, 4, 3, 2 (DNF 0); battles rank survivors by health,
then eliminated players by reverse elimination order.

**AI** only on request while a player is alone: the lone member of a public lobby, or the owner of a private room who
is alone, may start a race (`StartRace`, own track pick or `""` = random lobby track) against AI their own client
simulates (no item/anticheat arbitration): 6 AI opponents in public lobbies, `maxPlayers − 1` in private rooms with
`aiWhenAlone` (none without). Such solo races award no points and do not count towards a set. If someone joins during
one, the server tells the solo racer (`SoloRaceJoinWaiting`) and they may end it early (`EndRaceEarly`); afterwards a
public lobby starts Voting with the humans only, a private room waits for its owner.

### Private rooms

Created by a player with `RoomSettings`; joined by **code** (6 chars of `ABCDEFGHJKLMNPQRSTUVWXYZ23456789`) or link
`mhkos://join/<code>`. The owner changes settings while waiting, starts races, may kick members. When the owner
leaves, the longest-present member becomes owner; an empty room closes. Joining requires the room's `simVersion` and
packs.

### Common structures

`vec3` = f32 x, y, z. `quat` = f32 x, y, z, w.

`RoomSettings`:

| Field | Type | |
|-------|------|-|
| name | string ≤ 32 | |
| mode | u8 | 0 race set, 1 battle set, 2 championship (cup) |
| packs | list<string ≤ 64> ≤ 32 | content packs of the room |
| anticheat | bool | default true in the client |
| engineClass | u8 | 0–2 |
| laps | u8 | 0 = each track's default, else 1–99 |
| items | bool | |
| itemOdds | list<(powerupId: string ≤ 96, percent: f32)> ≤ 64 | empty = default odds |
| aiWhenAlone | bool | AI opponents while the owner is alone |
| maxPlayers | u8 | 2–10 |
| trackPick | u8 | 0 owner picks, 1 vote (championship rooms are always 0: the owner's client starts each cup track in order) |
| racesPerSet | u8 | 1–16 (championship: the cup's track count) |
| cupId | string ≤ 96 | championship only |

`LobbySummary`:

| Field | Type | |
|-------|------|-|
| lobbyId | u64 | |
| kind | u8 | 0 public, 1 private |
| category | u8 | public category, 0 for private |
| instance | u16 | number within the category (#1, #2, …) |
| name | string ≤ 64 | |
| mode | u8 | as RoomSettings.mode |
| anticheat | bool | |
| packs | list<string ≤ 64> ≤ 32 | |
| players | u8 | |
| maxPlayers | u8 | |
| phase | u8 | 0 Waiting, 1 Voting, 2 Loading, 3 Countdown, 4 Racing, 5 Results, 6 SetResults |
| simVersion | i32 | |
| engineClass | u8 | 0–2 |
| setRace | u8 | as `LobbyState.setRace` |
| setSize | u8 | as `LobbyState.setSize` |

`LobbyMember`:

| Field | Type | |
|-------|------|-|
| accountId | u64 | |
| username | string ≤ 20 | |
| country | string ≤ 2 | |
| characterId | string ≤ 96 | `""` = not chosen |
| kartId | string ≤ 96 | |
| ready | bool | private rooms |
| owner | bool | |
| moderator | bool | |
| inRace | bool | racing in the current race |
| pingMs | u16 | 0 = unknown |
| setPoints | i32 | points in the current set |
| joined | time | |
| platform | u8 | the member's device, from the `platform` of its Hello: 0 unknown, 1 Windows, 2 macOS, 3 Linux, 4 Android, 5 iOS |

`LobbyState` (push 256 and the answer of joins):

| Field | Type | |
|-------|------|-|
| summary | LobbySummary | |
| code | string ≤ 8 | private rooms (members only), else `""` |
| settings | RoomSettings | effective settings (public lobbies too) |
| members | list<LobbyMember> ≤ 10 | |
| phaseEnds | time | 0 = no timer |
| setRace | u8 | 1-based race number in the set, 0 between sets |
| setSize | u8 | |
| votes | list<(accountId: u64, trackId: string ≤ 96)> ≤ 10 | `""` = Random |
| trackId | string ≤ 96 | drawn/current track, `""` before the draw |
| raceId | u64 | current race, 0 = none |
| lastResults | list<RaceResultRow> ≤ 10 | of the previous race, for the results phases |

`RaceResultRow`: `slot: u8`, `accountId: u64`, `username: string ≤ 20`, `position: u8` (1-based),
`finishMs: i32` (0 = DNF), `points: i32`, `setPoints: i32`, `outcome: u8` (0 finished, 1 DNF, 2 left,
3 kicked: cheating, 4 kicked: moderator, 5 desync), `ai: bool`.

### Content messages

| Type | Name | Request | Response |
|------|------|---------|----------|
| 256 | ContentHello | `simVersion: i32`, `packs: list<(packId: string ≤ 64, commit: string ≤ 40, manifestHash: string ≤ 32)> ≤ 64` | `missing: list<string ≤ 32> ≤ 64` (manifest hashes the server wants), `strikes: list<(packId: string ≤ 64, message: string ≤ 256)> ≤ 64` (packs that differ from the catalog majority) |
| 257 | ContentManifests | `list<PackManifest> ≤ 64` | empty |

`PackManifest`: `packId: string ≤ 64`, `commit: string ≤ 40`, `simVersion: i32`, `manifestHash: string ≤ 32`,
`items: list<(kind: u8, id: string ≤ 96, fingerprint: string ≤ 32)> ≤ 4096`. The server recomputes the hash from
the items (canonical form: items sorted by kind ascending, then by the lowercased id (ASCII; ids are 1–96 characters
of `[A-Za-z0-9_.-]`) in ordinal byte order; for each the UTF-8 text `<kind>\n<id lowercase>\n<fingerprint>\n`;
SHA-256, first 16 bytes as lowercase hex; hashes compare case-insensitively) and rejects a mismatch, or two items with
the same kind and lowercased id, with `BadRequest`.

ContentHello must succeed (and any requested manifests be sent) before lobby requests; lobby requests before it
answer `BadRequest` "content not announced". Needs login.

### Lobby messages (requests, need login)

| Type | Name | Request | Response |
|------|------|---------|----------|
| 258 | ListLobbies | empty (uses the connection's simVersion) | `list<LobbySummary> ≤ 500` (public instances of every category, joinable private rooms are not listed) |
| 259 | JoinLobby | `lobbyId: u64`, `code: string ≤ 8` (one of them; code wins when non-empty) | `LobbyState` |
| 260 | QuickJoin | `category: u8` | `LobbyState` (the fullest public instance with room) |
| 261 | CreateRoom | `RoomSettings` | `LobbyState` |
| 262 | UpdateRoom | `RoomSettings` | empty (owner, Waiting only) |
| 263 | LeaveLobby | empty | empty |
| 264 | SetLoadout | `characterId: string ≤ 96`, `kartId: string ≤ 96` | empty (must be lobby content) |
| 265 | SetReady | `ready: bool` | empty |
| 266 | Vote | `trackId: string ≤ 96` (`""` = Random) | empty (Voting phase, lobby content) |
| 267 | StartRace | `trackId: string ≤ 96` (alone: a solo AI race on that track, `""` = random lobby track; with others: the track in owner-pick rooms, `""` in vote rooms) | empty (owner of a private room; the lone member of a public lobby while it waits) |
| 268 | EndRaceEarly | empty | empty (the solo racer of an AI race) |
| 269 | KickMember | `accountId: u64`, `reason: string ≤ 256` | empty (room owner within their private room; moderators anywhere, also out of a race; audit-logged) |

Join errors: `NotFound`, `Forbidden` (full, kicked from this lobby in the last 10 min, or a pack differs from the
catalog majority in an anticheat lobby; message says which), `BadRequest` (missing packs: message lists them,
simVersion differs: message says to update). A player is in at most one lobby; joining another leaves the first
without a `LobbyLeft` push (the join answer replaces the state).

### Lobby pushes

| Type | Name | Body |
|------|------|------|
| 256 | LobbyState | `LobbyState` (after every change) |
| 257 | LobbyLeft | `reason: u8` (0 left, 1 kicked by owner, 2 kicked by moderator, 3 lobby closed), `message: string ≤ 256` |
| 258 | SoloRaceJoinWaiting | `username: string ≤ 20` (someone waits while you race alone against AI) |

### Race messages (TCP)

Server → client pushes:

| Type | Name | Body |
|------|------|------|
| 512 | RaceSetup | `RaceSetup` |
| 513 | RaceGo | `raceId: u64`, `go: time` (server clock instant of GO; the countdown shows the 3 s before it) |
| 514 | ItemGranted | `raceId: u64`, `boxId: u16`, `powerupId: string ≤ 96`, `charges: u8`, `tick: u32` (only to the picker) |
| 515 | ItemBoxTaken | `raceId: u64`, `slot: u8`, `boxId: u16`, `tick: u32` (to everyone else) |
| 516 | ItemDenied | `raceId: u64`, `boxId: u16`, `reason: u8` (1 box not available, 2 too far, 3 cannot receive) |
| 517 | ItemUsed | `raceId: u64`, `slot: u8`, `useId: u32`, `powerupId: string ≤ 96`, `tick: u32`, `pos: vec3`, `rot: quat`, `backwards: bool`, `targetSlot: u8` (255 none) (to everyone else) |
| 518 | ItemUseRejected | `raceId: u64`, `useId: u32`, `reason: u8` (1 not held, 2 cooldown) |
| 519 | ProjectileCorrect | `raceId: u64`, `useId: u32`, `tick: u32`, `pos: vec3`, `vel: vec3` |
| 520 | HitApplied | `raceId: u64`, `victimSlot: u8`, `attackerSlot: u8` (255 none), `useId: u32`, `effectId: string ≤ 96`, `tick: u32` |
| 521 | HitForce | `raceId: u64`, `attackerSlot: u8`, `useId: u32`, `effectId: string ≤ 96` (to the victim: apply it) |
| 522 | PlayerFinished | `raceId: u64`, `slot: u8`, `finishMs: i32`, `lapMs: list<i32> ≤ 99` |
| 523 | RaceResults | `raceId: u64`, `rows: list<RaceResultRow> ≤ 16`, `setDone: bool` |
| 524 | PlayerLeftRace | `raceId: u64`, `slot: u8`, `outcome: u8` (as RaceResultRow.outcome), `message: string ≤ 256` |
| 525 | Eliminated | `raceId: u64`, `slot: u8`, `attackerSlot: u8`, `tick: u32` (battle) |
| 526 | BridgeTriggered | `raceId: u64`, `bridgeId: u16`, `tick: u32` |

`RaceSetup`:

| Field | Type | |
|-------|------|-|
| raceId | u64 | |
| lobbyId | u64 | |
| trackId | string ≤ 96 | |
| mode | u8 | 0 race, 1 battle |
| engineClass | u8 | |
| laps | u8 | 0 = track default |
| items | bool | |
| itemOdds | list<(string ≤ 96, f32)> ≤ 64 | as RoomSettings |
| anticheat | bool | |
| solo | bool | exactly one human: race against AI locally, no arbitration |
| aiCount | u8 | AI opponents in a solo race |
| seed | u64 | race seed for deterministic cosmetic randomness |
| grid | list<RaceSlot> ≤ 10 | start order: grid slot = list index |
| yourSlot | u8 | |
| udpPort | u16 | |
| udpToken | u64 | |
| udpKey | bytes ≤ 32 | 32 bytes HMAC key |
| setRace | u8 | |
| setSize | u8 | |

`RaceSlot`: `slot: u8`, `accountId: u64`, `username: string ≤ 20`, `country: string ≤ 2`,
`characterId: string ≤ 96`, `kartId: string ≤ 96`, `platform: u8` (as `LobbyMember.platform`).

Hello `platform` values the server maps (Unity `RuntimePlatform` names, editors included): `WindowsPlayer`,
`WindowsEditor` → 1; `OSXPlayer`, `OSXEditor` → 2; `LinuxPlayer`, `LinuxEditor` → 3; `Android` → 4; `IPhonePlayer` → 5;
anything else → 0.

Client → server notifies (TCP, kind 3; the server ignores those for a race the sender is not in):

| Type | Name | Body |
|------|------|------|
| 528 | RaceLoaded | `RaceLoaded` |
| 529 | ItemPickup | `raceId: u64`, `boxId: u16`, `tick: u32`, `pos: vec3` |
| 530 | ItemUse | `raceId: u64`, `useId: u32`, `tick: u32`, `pos: vec3`, `rot: quat`, `backwards: bool`, `targetSlot: u8` |
| 531 | HitTaken | `raceId: u64`, `attackerSlot: u8`, `useId: u32` (0 = track hazard/zone), `effectId: string ≤ 96`, `tick: u32` |
| 532 | HitSeen | `raceId: u64`, `victimSlot: u8`, `attackerSlot: u8`, `useId: u32`, `effectId: string ≤ 96`, `tick: u32` |
| 533 | Finished | `raceId: u64`, `finishMs: i32`, `lapMs: list<i32> ≤ 99`, `tick: u32`, `position: u8` (solo races: the racer's place among all karts incl. AI; 0 otherwise) |
| 534 | CheatReport | `raceId: u64`, `suspectSlot: u8`, `reason: u8` (FlagReason), `tick: u32`, `detail: string ≤ 256` |
| 535 | UnknownContent | `raceId: u64`, `slot: u8`, `kind: u8`, `id: string ≤ 96` |
| 536 | EliminatedSelf | `raceId: u64`, `attackerSlot: u8`, `tick: u32` |
| 537 | BridgeTrigger | `raceId: u64`, `bridgeId: u16`, `tick: u32` |
| 538 | ProjectileGone | `raceId: u64`, `useId: u32`, `tick: u32` |

`RaceLoaded`: `raceId: u64`, `trackHash: string ≤ 32`, `laps: u8` (laps of this race, resolved from the track default when `RaceSetup.laps` is 0), `boxes: list<ItemBoxInfo> ≤ 512`,
`pool: list<PoolEntry> ≤ 128`, `specials: list<(slot: u8, powerupId: string ≤ 96)> ≤ 10`,
`rules: ItemRules`, `effects: list<string ≤ 96> ≤ 1024` (status effect ids, the index table of UDP states).

- `ItemBoxInfo`: `pos: vec3`, `respawnTenths: u16` (respawn time in 0.1 s), `fixed: string ≤ 96` (`""` random,
  a powerup id, `@special`, or a status effect id: boxes that apply an effect are not arbitrated). Box ids are the
  index in this list; clients order boxes by their stable hierarchy path.
- `PoolEntry`: `powerupId: string ≤ 96`, `charges: u8`, `battleCharges: u8`, `weights: 11 × f32` (weight at
  position fraction 0, 0.1, … 1; 0 = leader, 1 = last).
- `ItemRules`: `replacesHeld: bool`, `rouletteMs: u16`.

The server takes the majority of `trackHash`, `laps`, `boxes`, `pool`, `specials`, `rules`, `effects` (compared as
canonical hashes) among the racers; racers that differ leave the race (`PlayerLeftRace`, outcome desync) and, when
their pack commit equals the majority's, get a content strike. A racer that did not send `RaceLoaded` within 45 s
leaves the race. The server then sends `RaceGo` with GO = now + 3.5 s.

Item roll: position fraction f = (position − 1) / (racers − 1) from the latest `raceDistance`s (finished racers in
finish order; battles and single racers use 0.5); weight = linear interpolation of the 11 weights at f; a weighted
random draw (server RNG) among entries with weight > 0. `@special` boxes grant the slot's special powerup (random
draw when it has none).

### Realtime UDP

All datagrams (≤ 1200 bytes):

| Field | Type | |
|-------|------|-|
| type | u8 | |
| token | u64 | `udpToken` of the sender's race seat (server → client: the receiver's token) |
| seq | u32 | per-direction, per-token increasing; receivers drop packets not newer than the newest seen (except type 1/2) |
| payload | … | |
| mac | 8 bytes | first 8 bytes of HMAC-SHA256(`udpKey`, all preceding bytes) |

Datagrams with an unknown token or a bad MAC are dropped silently. The server learns the client's address from its
datagrams (latest valid one wins).

| Type | Direction | Payload |
|------|-----------|---------|
| 1 Ping | c → s | `clientTime: time`, `echoServerTime: time` (`serverTime` of the newest Pong received, 0 = none yet), `echoHoldMs: u16` (ms between receiving that Pong and sending this Ping, clamped to 65535). Sent 4×/s during loading and races; doubles as keepalive. The server's RTT sample = receive time − `echoServerTime` − `echoHoldMs` (ignored when the echo is 0, in the future or older than 5 s), smoothed with EWMA α = 1/8; it feeds `World.rttMs` and eligibility |
| 2 Pong | s → c | `clientTime: time`, `serverTime: time` |
| 3 State | c → s | `KartState`, `reports: u8 n` + n × `ProjectileReport` |
| 4 World | s → c | `serverTime: time`, `n: u8` + n × (`slot: u8`, `rttMs: u16`, `KartState`) (30 Hz, latest state of every other racer) |

`KartState` (sent at 30 Hz):

| Field | Type | |
|-------|------|-|
| tick | u32 | race tick (physics steps since GO; 0 before GO) |
| pos | vec3 | |
| rot | 4 × i16 | quaternion components × 32767, renormalised by the receiver |
| vel | 3 × i16 | cm/s |
| keys | u8 | `GhostInputKeys` bits |
| flags | u8 | bit 0 cut (teleport/respawn since the previous state), 1 controls enabled, 2 finished, 3 eliminated, 4 intangible, 5 airborne, 6 look back |
| steer | i8 | steer angle / max × 127 (visual) |
| lap | u8 | |
| raceDistance | f32 | metres along the race (ranking) |
| health | u8 | battle health (0–255 clamp) |
| effects | u8 n + n × u16 | active status effects as indexes into `RaceLoaded.effects` |

`ProjectileReport`: `useId: u32`, `tick: u32`, `pos: vec3`, `vel: vec3` (each live projectile every 0.5 s).

Clocks: clients estimate the server clock offset from Ping/Pong (minimum-RTT samples) and show the countdown and
start the race at GO in server time. Ticks are counted locally from GO at 60 Hz; remote karts are rendered about
100 ms behind the newest received tick.

`useId` = `(slot + 1) << 24 | n` with n the player's own use counter (1-based) in this race: unique without a
round trip. A client must not use an item before its `ItemGranted` arrived.

### Race rules (server)

- **Grid**: racers in the lobby with a loadout, in join order shuffled by the race seed; at most 10. Waiting players
  and players without a loadout (character and kart) sit the race out.
- **Hits**: the victim's client decides hits on its own kart and applies them at once, then sends `HitTaken`; the
  server forwards `HitApplied` to everyone else (once per victim and `useId`). Observers that see a projectile hit
  another kart send `HitSeen` and do not apply it. If, within victim RTT + 300 ms of the first claim, the server holds
  claims from ≥ max(2, ⌈eligible observers / 2⌉) distinct eligible observers but no `HitTaken`, it sends `HitForce` to
  the victim (a legit client applies it and answers with `HitTaken`) and adds an "ignored hit" strike when the victim
  still does not confirm within another RTT + 300 ms (anticheat lobbies only).
- **Projectiles**: positions reported at ticks t_i are compared at the newest tick T after extrapolating
  `pos + vel × (T − t_i) / 60`; `ProjectileCorrect` goes to everyone when the reports spread more than 2 m from the
  median; clients blend towards the corrected state. `ProjectileGone` from the owner or a majority of reporters ends
  tracking.
- **Finish**: a `Finished` whose time disagrees with the server's clock since GO by more than RTT + 1 s, or whose lap
  count differs from the race's, is a server strike and the finish is taken at the server's receive time. The race
  ends when every racer finished, left or was eliminated, 30 s after the first finisher, or after 20 min; unfinished
  racers are DNF. Battles end when ≤ 1 racer is alive or after 4 min.
- **Battles**: the victim reports its own elimination (`EliminatedSelf`); observers' kart states carry health; a racer
  at health 0 in a majority of observers' views for 2 s without `EliminatedSelf` gets a strike and is eliminated.
- **Collapsing bridges**: the first `BridgeTrigger` (lowest tick, then arrival) of a bridge wins and is broadcast.
- **Disconnects**: a racer whose control connection drops leaves the race (outcome left); others remove the kart.
- **Kicks**: a cheating kick writes `cheat_kicks` (account, lobby, race, reason, reporters) and removes the racer from
  the race (outcome kicked: cheating); they stay in the lobby and may race again next race. Moderator kicks remove
  from race and lobby for 10 min and write the audit log.
- **Solo races** (`solo`): nothing is arbitrated; the client sends `Finished` with its `position` among the AI, the
  server records that placement (AI rows are not reported) and ends the race when it arrives (or on `EndRaceEarly`:
  DNF).
