# Pixelforge

A shared pixel canvas. Everyone who has it open is painting on the same grid, and
every placement lands on every other screen in about a tenth of a second.

![The Pixelforge canvas](docs/screenshot.png)

The interesting part is not the canvas. It is that **the entire thing is Go's
standard library** — the PostgreSQL driver and the WebSocket server included.
`go.mod` has no `require` block and there is no `go.sum`. CI fails the build if
either of those stops being true.

```
$ go list -deps ./... | grep -v '^github.com/Har103/pixelforge' \
                      | grep -v '^vendor/' | grep -c '\.'
0
```

(The `vendor/` entries that filter removes are Go's own vendored copies of
`golang.org/x/net` and friends, which `crypto/tls` and `net/http` pull in. They
ship inside the toolchain — nothing is fetched to build this.)

---

## Contents

- [What it does](#what-it-does)
- [Why it is built this way](#why-it-is-built-this-way)
- [Architecture](#architecture)
- [Running it](#running-it)
- [Deploying on Dockup](#deploying-on-dockup)
- [Configuration](#configuration)
- [HTTP API](#http-api)
- [Wire formats](#wire-formats)
- [Tests](#tests)
- [Things I would do next](#things-i-would-do-next)

---

## What it does

- **A live shared grid.** Pan, zoom, and click a cell to paint it. Every other
  connected client sees it immediately.
- **Two realtime transports.** WebSocket by default, Server-Sent Events as an
  automatic fallback when a proxy will not carry an upgrade. There is a toggle in
  the top right so you can watch both paths work against the same server.
- **A per-user cooldown**, enforced server-side against an HMAC-signed identity
  cookie, so clearing the cookie is the only way to dodge it and forging one is
  not.
- **Full history and a time-lapse.** Every placement is appended to PostgreSQL.
  The scrubber replays the canvas from empty to now.
- **Statistics** — colour histogram, top painters, how much of the grid is
  painted, all straight out of SQL.
- **Survives restarts.** The canvas is snapshotted to the database periodically
  and on shutdown; on boot it loads the snapshot and replays anything newer.

<table>
<tr>
<td><img src="docs/timelapse.png" alt="Time-lapse scrubber replaying the canvas"></td>
<td><img src="docs/stats.png" alt="Statistics panel"></td>
</tr>
</table>

## Why it is built this way

Two constraints drove every decision, and they happen to point the same way.

**No third-party modules.** Not asceticism — a dependency you did not write is a
dependency you cannot debug at 3am, and the two things this app leans on hardest
(a database driver and a WebSocket server) are exactly the two places where a
library's opinions leak into yours. Writing them meant reading RFC 6455, RFC
5802 and the PostgreSQL protocol documentation rather than a wrapper's README.
Roughly 2,200 lines bought complete control of both.

**It is billed by vCPU-minute and GB-minute.** So: a `FROM scratch` image with a
single static binary, a ~7 MB image that cold-starts in milliseconds, an
in-memory canvas that never asks the database for a read on the hot path, and
broadcasts coalesced onto a 50 ms tick so a paint storm costs a handful of frames
per second instead of hundreds.

## Architecture

```
   browser ──── WebSocket (binary deltas) ────┐
   browser ──── SSE (JSON deltas) ────────────┤
   browser ──── POST /api/place ──────────┐   │
                                          ▼   ▼
                            ┌──────────────────────────────┐
                            │  canvas.Canvas               │  authoritative
                            │  one byte per cell, RWMutex  │  grid, in memory
                            │  cooldown per identity       │
                            └───────┬──────────────┬───────┘
                        placement   │              │  placement
                                    ▼              ▼
                         ┌────────────────┐  ┌──────────────────┐
                         │ hub.Hub        │  │ canvas.Store     │
                         │ 50 ms coalesce │  │ write-behind     │
                         │ fan out        │  │ queue + batching │
                         └────────────────┘  └────────┬─────────┘
                                                      │
                                             ┌────────▼─────────┐
                                             │ internal/pg      │
                                             │ v3 wire protocol │
                                             │ SCRAM-SHA-256    │
                                             └────────┬─────────┘
                                                      ▼
                                                 PostgreSQL
                                         placements (append-only log)
                                         snapshots  (one bytea row)
```

The write path never blocks on the database. `Place` takes the canvas lock,
validates, writes one byte, and returns; persistence and broadcast both happen
off the back of a queue. If the database falls far enough behind to fill the
4,096-entry buffer, history entries are dropped with a warning rather than
stalling anyone's paint — the next snapshot captures the pixels regardless.

### `internal/pg` — a PostgreSQL driver, ~1,700 lines

Speaks the v3 frontend/backend protocol over a raw `net.Conn`:

- SSL negotiation and TLS upgrade in place, with the five `sslmode` values
- Authentication: trust, cleartext, MD5, and **SCRAM-SHA-256** (RFC 5802/7677),
  including PBKDF2-HMAC-SHA256 and verification of the server's signature so a
  man in the middle cannot complete the handshake
- The simple query protocol for migrations, and the extended protocol
  (Parse/Bind/Describe/Execute/Sync) with bound parameters for everything else
- Text-format values throughout, which sidesteps type-OID differences between
  versions and extensions; `bytea` is decoded from both the hex and the legacy
  escape format
- A connection pool that revalidates on checkout and retires connections past a
  maximum lifetime, because managed databases idle-close without warning

The SCRAM implementation is checked against the worked example in RFC 7677, so
the test suite fails loudly if any step of the exchange drifts.

Not implemented, deliberately: `COPY`, `LISTEN`/`NOTIFY`, prepared-statement
caching, binary result formats, and `database/sql` integration. None of them are
needed here and each is a meaningful amount of surface area.

### `internal/ws` — a WebSocket server, ~470 lines

`http.Hijacker` to take the socket, then the frame codec by hand:

- Handshake validation, including that `Sec-WebSocket-Key` decodes to 16 bytes,
  and the `base64(SHA-1(key + GUID))` accept value
- All three payload-length encodings, client-frame unmasking, fragmentation
  reassembly, and ping/pong/close handled transparently below the read loop
- The protocol rules that libraries usually get right and hand-rolled code
  usually gets wrong: reserved bits must be clear, client frames must be masked,
  control frames must be ≤125 bytes and unfragmented, text must be valid UTF-8.
  Each one closes with the correct RFC 6455 status code, and each has a test.
- Writes are serialised and each frame is a single `Write`, so concurrent writers
  cannot interleave

Go's standard library has `crypto/sha1` and `encoding/base64`, so those are used
rather than reimplemented — the rule is no third-party modules, not no standard
library.

## Running it

**With Docker Compose** (brings up PostgreSQL too):

```sh
docker compose up --build
# http://localhost:8080
```

**Locally against your own PostgreSQL:**

```sh
export DATABASE_URL='postgres://user:pass@localhost:5432/pixelforge?sslmode=disable'
go run ./cmd/pixelforge
```

**With no database at all:**

```sh
go run ./cmd/pixelforge
```

It starts, logs a warning, shows an `ephemeral` badge in the UI, and works
normally — the canvas just does not survive a restart. That is deliberate: a
demo that comes up degraded beats one that crash-loops in front of whoever is
looking at it.

Open a second browser window next to the first and paint. That is the whole
demo.

## Deploying on Dockup

This repository is built to be imported as a **GitHub Repo** service on
[dockup.ai](https://app.dockup.ai) with a managed **PostgreSQL** database
alongside it.

1. On the Projects canvas, press <kbd>Space</kbd> → **Database** → PostgreSQL.
2. Press <kbd>Space</kbd> again → **GitHub Repo** → this repository.
3. Give the service `DATABASE_URL` from the database node, and set `APP_SECRET`
   to any long random string.
4. Deploy. The `Dockerfile` at the repository root is a standard multi-stage
   build with no build arguments required.

The container listens on whatever `$PORT` says (8080 if unset; Dockup injects
its own, which is why no port configuration is needed). It answers `GET /healthz`
for liveness and `GET /readyz` for readiness — `/readyz` fails while the database
is unreachable, so traffic can be held back until the app can actually serve.
Point the service's health check at `/readyz`.

If the database is attached after the app is already running, restart the
service; the driver only reads `DATABASE_URL` at boot.

## Configuration

Every value has a working default. Nothing is required.

| Variable | Default | Notes |
|---|---|---|
| `PORT` | `8080` | HTTP listen port |
| `DATABASE_URL` | *(unset)* | `postgres://…` URL or libpq keyword form. `POSTGRES_URL` and `PG_DSN` are also accepted; failing those, the standard `PGHOST`/`PGUSER`/`PGPASSWORD`/`PGDATABASE`/`PGSSLMODE` variables |
| `APP_SECRET` | *(random per boot)* | Signs the identity cookie. Set it, or every restart resets everyone's cooldown |
| `CANVAS_WIDTH` | `256` | 16–1024 |
| `CANVAS_HEIGHT` | `256` | 16–1024 |
| `COOLDOWN_MS` | `750` | Per-identity delay between placements. `0` disables it |
| `DB_POOL_SIZE` | `4` | 1–32 |
| `LOG_FORMAT` | `text` | `json` for structured logs |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

Changing the canvas dimensions between deploys is handled: the old snapshot is
discarded and the placement log is replayed into the new grid.

## HTTP API

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/config` | Dimensions, palette, cooldown, your identity |
| `GET` | `/api/snapshot` | The whole grid as one binary blob |
| `POST` | `/api/place` | `{"x":1,"y":2,"c":7}` → `200`, or `429` with `retryInMs` |
| `GET` | `/api/history?after=&limit=` | Placement log, oldest first |
| `GET` | `/api/stats` | Canvas stats, leaderboard, uptime |
| `GET` | `/ws` | WebSocket: binary pixel batches, JSON control messages, and placements in the other direction |
| `GET` | `/sse` | Server-Sent Events, JSON only |
| `GET` | `/healthz` | Liveness plus counters |
| `GET` | `/readyz` | Readiness — fails when the database is unreachable |

## Wire formats

**Snapshot** (`GET /api/snapshot`) — 16-byte header then one byte per cell:

```
"PXF1" | width u16 | height u16 | seq u64 | pixels[width*height]
```

A 256×256 canvas is 64 KiB raw and a few KiB over the wire once compressed,
against roughly 1.5 MB for the equivalent JSON.

**Pixel batch** (WebSocket binary frame) — 3-byte header then 5 bytes per pixel:

```
0x01 | count u16 | { x u16, y u16, colour u8 } * count
```

**Control messages** (WebSocket text frames and every SSE event) are JSON:
`{"t":"hello"|"presence"|"px"|"denied", …}`.

Colours are palette indices, never RGB. The palette lives in `internal/canvas`
and is served to the client at startup, which is why a cell is one byte and why
a client cannot smuggle in an arbitrary colour.

## Tests

```sh
go test -race ./...
```

Sixty-plus tests, no external services required — the HTTP layer runs against the
ephemeral (no-database) store, and the WebSocket tests speak real frames over a
real socket from a hand-written test client.

Worth singling out:

- `TestSCRAMRFC7677Vector` — the authentication exchange against the RFC's
  worked example
- `TestConcurrentWritesDoNotInterleave` — 200 frames from 8 goroutines, all
  arriving intact
- `TestConcurrentPlacesAreSerialised` — 1,600 concurrent placements, no
  duplicate sequence numbers
- `TestForgedIdentityCookieIsIgnored` — a hand-crafted cookie does not buy you a
  chosen identity
- The protocol-violation suite — unmasked frames, reserved bits, oversized
  control frames, invalid UTF-8, each expecting a specific close code

CI additionally runs an integration job against a real PostgreSQL 16 with
`scram-sha-256` forced, paints a pixel, restarts the server, and asserts the
canvas came back.

## Security surface

With no OS packages in the image and no third-party modules in the binary, the
Go toolchain is the *entire* dependency surface. A container scanner run against
this image finds exactly one component: `stdlib`. That is a real benefit — and a
real obligation, because it means the toolchain pin in the `Dockerfile` is the
only lever there is.

It is worth being concrete about this, because it is easy to mistake "zero
dependencies" for "zero CVEs". The first deploy of this repository was built on
`golang:1.23-alpine` and scanned at grade **F: 1 critical, 21 high, 23 medium**.
Every one of those 43 findings was `stdlib@v1.23.12` — Go 1.23 went end-of-life
when Go 1.25 shipped, so it stopped receiving the patches. Not one finding came
from application code or from a library, because there are none. Bumping the
pin to `golang:1.26.6-alpine` clears them.

So: **pin the builder to an exact patch release and bump it deliberately.** A
floating `golang:1-alpine` would have hidden the problem behind whatever the
registry happened to serve that day; an EOL pin freezes you on unpatched
cryptography. Neither is a policy. Watching one line is.

The scanner also flags `FROM scratch` as an unpinned base image. That one is
noise — scratch is the empty image and has nothing to pin.

## Things I would do next

Honest list, in the order I would do them:

- **Horizontal scale.** One process owns the canvas today, so a second replica
  would diverge. The fix is `LISTEN`/`NOTIFY` or Redis for cross-instance fanout
  and optimistic concurrency on the snapshot row — worth doing when one container
  stops being enough, not before.
- **Rate limiting by IP** as well as by identity. The cookie cooldown is honest
  but a script that discards cookies can still outrun it.
- **Region locking / moderation.** Any public canvas needs it within a day.
- **`database/sql` compatibility** for `internal/pg`, so it is useful outside
  this repository.
- **Time-lapse export to GIF**, server-side, which is the part people actually
  want to share.

## Licence

MIT — see [LICENSE](LICENSE).
