# Pixelforge

Make a shared pixel canvas in one click. Send the link. Everyone paints on the
same grid, live.

![The Pixelforge home page](docs/home.png)

No signup, nothing to install. Whoever creates a canvas gets moderator control
of it; everyone else just needs the URL. Every room exports as a PNG, replays as
an animated GIF, embeds as a live read-only iframe, and unfurls in Slack or
Discord as a picture of itself.

The engineering constraint underneath all of it: **the entire thing is Go's
standard library.** The PostgreSQL driver, the WebSocket server, the password
hashing and the image encoders are all written in this repository. `go.mod` has
no `require` block and there is no `go.sum`, and CI fails the build if either
stops being true.

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
- [Sharing](#sharing)
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

**Rooms.** Anyone can create a canvas: pick a size, a palette and a cooldown,
and you get a link. Public rooms appear on the browse page with a live
thumbnail; unlisted ones are reachable only by URL.

**Ownership without a signup wall.** Creating a room hands you a moderator key —
stored as a cookie for this browser, and shown once as a recovery link to save.
Accounts exist and keep your rooms together across devices, but painting never
needs one, and neither does making a canvas. A login form in front of "draw with
your friends" is a product bug.

**Moderation, because any public canvas needs it within a day.** The owner can
pause the room, lock rectangles so a finished area stays finished, clear the
grid, block a painter, or undo every pixel one person placed — with whatever
they painted over restored from the log rather than guessed. Per-address rate
limiting backs up the per-painter cooldown, which a script defeats by throwing
its cookie away.

**Live, two ways.** WebSocket by default, with an automatic fallback to
Server-Sent Events for networks that will not carry an upgrade; there is a
toggle in the room so you can watch both paths work. Updates are coalesced on a
50&nbsp;ms tick, so a paint storm costs a handful of frames a second instead of
hundreds.

**It remembers.** Every placement is appended to PostgreSQL and each grid is
snapshotted, so a restart brings every canvas back exactly as it was. Scrub the
whole history in the browser, or export it.

**Five palettes**, from a twenty-colour default to four shades of Game Boy
green, because a constraint is often what makes the picture.

<table>
<tr>
<td><img src="docs/room.png" alt="A canvas mid-paint"></td>
<td><img src="docs/manage.png" alt="The moderation panel"></td>
</tr>
</table>

## Sharing

This is the part a kanban clone has no equivalent of, and it costs nothing
extra: `image/png` and `image/gif` are in the standard library.

| What | Where | Notes |
|---|---|---|
| **Link preview** | `/r/{slug}/card.png` | A 1200×630 Open Graph card rendered from the canvas *as it looks right now*, with the room name drawn in a bitmap font defined in code. Paste a room link into Slack, Discord or a timeline and it unfurls as a picture of itself. |
| **PNG export** | `/r/{slug}/canvas.png?scale=8` | Nearest-neighbour at any integer scale, so pixels stay pixels. |
| **Time-lapse GIF** | `/r/{slug}/timelapse.gif` | The placement log replayed from empty to now, encoded server-side. |
| **Live embed** | `/embed/{slug}` | A read-only canvas that keeps syncing, for a wiki page or a stream overlay. It sets no credentials and can do nothing on a visitor's behalf. |

![The Open Graph card a room link unfurls into](docs/card.png)

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
                    ┌──────────────────────────────────┐
   browser ───────► │ httpapi                          │
                    │  /r/{slug}  routing, ownership,  │
                    │  rate limiting, exports          │
                    └───────────────┬──────────────────┘
                                    │ Get(slug)
                    ┌───────────────▼──────────────────┐
                    │ room.Registry                    │  lazy load,
                    │  resident rooms, single-flight,  │  idle eviction,
                    │  idle sweep, eviction cap        │  64 resident max
                    └───────────────┬──────────────────┘
                     ┌──────────────┴───────────────┐
                     ▼                              ▼
            ┌──────────────────┐          ┌──────────────────┐
            │ room.Room        │   ...    │ room.Room        │
            │  canvas.Canvas   │          │  one per live    │
            │  hub.Hub (50ms)  │          │  canvas          │
            │  write-behind    │          │                  │
            └────────┬─────────┘          └──────────────────┘
                     │
            ┌────────▼─────────┐   ┌──────────────────┐
            │ store            │   │ render           │
            │  every SQL       │   │  PNG, OG card,   │
            │  statement       │   │  GIF time-lapse  │
            └────────┬─────────┘   └──────────────────┘
                     │
            ┌────────▼─────────┐
            │ pg               │  v3 wire protocol, SCRAM-SHA-256
            └────────┬─────────┘
                     ▼
                 PostgreSQL
        rooms · room_placements · room_snapshots · users · bans
```

A room is loaded on first request and released after twenty idle minutes with
nobody connected, so a thousand dormant canvases cost rows in Postgres rather
than a thousand resident grids. Concurrent requests for a cold room collapse
onto one load; without that, a burst of traffic to a shared link would each
build a grid and start a pair of goroutines, and all but one would be discarded.

The write path never blocks on the database. `Place` takes the canvas lock,
validates against the cooldown, the bans and the locks, writes one byte, and
returns; persistence and broadcast both happen off the back of a queue. If the
database falls far enough behind to fill the buffer, history entries are dropped
with a warning rather than stalling anyone's paint — the next snapshot captures
the pixels regardless.

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
| `PORT` | `8080` | HTTP listen port. Dockup injects its own; do not hardcode one |
| `DATABASE_URL` | *(unset)* | `postgres://…` URL or libpq keyword form. `POSTGRES_URL` and `PG_DSN` also work; failing those, the standard `PGHOST`/`PGUSER`/`PGPASSWORD`/`PGDATABASE`/`PGSSLMODE` variables |
| `APP_SECRET` | *(random per boot)* | Signs painter ids, sessions and moderator keys. **Set it.** Without it, every restart invalidates every moderator key, and a room somebody expects to still own tomorrow becomes unowned |
| `BASE_URL` | *(relative)* | Absolute origin, e.g. `https://pixelforge.example`. Link previews and share links need it to be absolute |
| `RATE_LIMIT_PER_MIN` | `600` | Writes per address per minute. See below |
| `DB_POOL_SIZE` | `6` | 1–32 |
| `LOG_FORMAT` | `text` | `json` for structured logs |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

Room size, palette and cooldown are per room, chosen at creation, not
environment variables.

**On the rate limit.** Ten writes a second per address is a real trade rather
than a formality. Lower and it fights the product — a room with no cooldown is
meant to be painted fast, and an office or a school shares one address between
everybody in it. Higher and it stops backing up the per-painter cooldown, which
a script defeats by discarding its cookie. If you run this behind a proxy that
does not set `X-Forwarded-For`, every visitor looks like one address and the
limit will bite; fix the proxy rather than raising the number.

## HTTP API

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/palettes` | Palettes and the limits the creation form should respect |
| `GET` | `/api/rooms` | Public rooms, most recently active first |
| `POST` | `/api/rooms` | Create one. Returns the slug and the moderator key |
| `GET` | `/api/r/{slug}/config` | Room description, your painter id, whether you are the owner |
| `GET` | `/api/r/{slug}/snapshot` | The whole grid as one binary blob |
| `POST` | `/api/r/{slug}/place` | `{"x":1,"y":2,"c":7}` → `200`, or `429` with `retryInMs` |
| `GET` | `/api/r/{slug}/history` | Placement log, oldest first |
| `GET` | `/api/r/{slug}/stats` | Canvas stats, painter leaderboard |
| `GET` | `/api/r/{slug}/ws` | WebSocket: binary pixel batches out, placements in |
| `GET` | `/api/r/{slug}/sse` | Server-Sent Events, JSON only |
| `POST` | `/api/r/{slug}/mod/*` | `pause`, `clear`, `ban`, `undo`, `locks`, `settings` — moderator key required |
| `POST` | `/api/auth/{register,login,logout}` | Optional accounts |
| `GET` | `/healthz` · `/readyz` | Liveness, and readiness that fails while the database is unreachable |

Moderator authority is proved three ways: the per-room cookie set at creation,
a `?key=` on the recovery link, or an account that owns the room. Only the HMAC
of a key is stored, so a database leak does not hand out control of every
canvas.

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
go test -race ./...                                   # everything that needs no database
PIXELFORGE_TEST_DSN=postgres://… go test -race ./...  # plus the API integration tests
```

The API tests need a real PostgreSQL, because rooms are a database concept and
mocking the store would only test the mock. They skip cleanly without a DSN.

Worth singling out:

- `TestSCRAMRFC7677Vector` — the database authentication exchange checked
  against the RFC's own worked example
- `TestConcurrentWritesDoNotInterleave` — 200 WebSocket frames from 8
  goroutines, all arriving intact
- `TestConcurrentPlacesAreSerialised` — 1,600 concurrent placements, no
  duplicate sequence numbers
- `TestRoomsAreIsolated` — a pixel painted in one room does not appear in another
- `TestModerationNeedsTheModeratorKey` — every moderation route refuses a
  stranger, accepts the creator's cookie, and accepts the recovery link's key
  from a browser that has never seen the room
- `TestBanAndUndo` — undoing a painter restores what they painted over, from the log
- `TestZeroCooldownIsHonoured` and `TestRoomPageHasNoInlineScript` — two
  regressions with stories, described where they live
- The WebSocket protocol-violation suite — unmasked frames, reserved bits,
  oversized control frames, invalid UTF-8, each expecting a specific close code

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

- **Horizontal scale.** One process owns each resident room, so a second replica
  would serve a different copy of the same canvas. The fix is to route a slug to
  a single instance, or to move the fan-out to `LISTEN`/`NOTIFY`. Worth doing
  when one container stops being enough, not before.
- **Undo a rectangle, not just a painter.** Undoing a person is the blunt tool;
  most vandalism is one region by several accounts.
- **A gallery of finished canvases**, so a room has somewhere to go when it is
  done rather than sitting on the browse page forever.
- **Cooldown changes taking effect live.** They are recorded immediately but the
  running canvas keeps the value it was loaded with, and the API says so rather
  than pretending otherwise.
- **Argon2id instead of PBKDF2** for passwords, the day `golang.org/x/crypto`
  becomes acceptable — which under this project's rule is never, so the real
  answer is to keep watching whether the standard library grows it.

## Licence

MIT — see [LICENSE](LICENSE).
