// Package httpapi wires rooms, storage and the realtime hub to HTTP handlers.
package httpapi

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Har103/pixelforge/internal/auth"
	"github.com/Har103/pixelforge/internal/canvas"
	"github.com/Har103/pixelforge/internal/room"
	"github.com/Har103/pixelforge/internal/store"
)

// Server holds everything the handlers need.
type Server struct {
	Rooms   *room.Registry
	Store   *store.Store
	Signer  *auth.Signer
	Secret  []byte
	Log     *slog.Logger
	Static  fs.FS
	Version string
	BaseURL string // absolute origin, used for link previews and share text

	// MaxBodyBytes caps request bodies. Everything this API accepts is tiny.
	MaxBodyBytes int64

	// RateLimitPerMin bounds writes from one address per minute.
	//
	// Picking this is a real trade rather than a formality. Too low and it
	// fights the product: a room with no cooldown is meant to be painted fast,
	// and an office or a school shares one address between everybody in it.
	// Too high and it stops being the backstop for the per-painter cooldown,
	// which a script defeats simply by throwing its cookie away. 600 a minute
	// is ten a second - comfortably above a human clicking flat out and well
	// below what makes scripted defacement worth the trouble.
	RateLimitPerMin int

	started  time.Time
	requests atomic.Uint64
	places   atomic.Uint64
	rejected atomic.Uint64

	limiter *ipLimiter
}

// Routes builds the mux.
func (s *Server) Routes() http.Handler {
	if s.MaxBodyBytes == 0 {
		s.MaxBodyBytes = 8 << 10
	}
	s.started = time.Now()
	if s.RateLimitPerMin <= 0 {
		s.RateLimitPerMin = 600
	}
	s.limiter = newIPLimiter(s.RateLimitPerMin, time.Minute)

	mux := http.NewServeMux()

	// Health
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)

	// Rooms
	mux.HandleFunc("GET /api/palettes", s.handlePalettes)
	mux.HandleFunc("GET /api/rooms", s.handleListRooms)
	mux.HandleFunc("POST /api/rooms", s.handleCreateRoom)

	// Per-room
	mux.HandleFunc("GET /api/r/{slug}/config", s.withRoom(s.handleConfig))
	mux.HandleFunc("GET /api/r/{slug}/snapshot", s.withRoom(s.handleSnapshot))
	mux.HandleFunc("POST /api/r/{slug}/place", s.withRoom(s.handlePlace))
	mux.HandleFunc("GET /api/r/{slug}/history", s.withRoom(s.handleHistory))
	mux.HandleFunc("GET /api/r/{slug}/stats", s.withRoom(s.handleStats))
	mux.HandleFunc("GET /api/r/{slug}/pixel", s.withRoom(s.handlePixel))
	mux.HandleFunc("POST /api/r/{slug}/undo", s.withRoom(s.handleUndoOwn))
	mux.HandleFunc("GET /api/r/{slug}/ws", s.withRoom(s.handleWS))
	mux.HandleFunc("GET /api/r/{slug}/sse", s.withRoom(s.handleSSE))

	// Moderation
	mux.HandleFunc("POST /api/r/{slug}/mod/settings", s.withOwner(s.handleModSettings))
	mux.HandleFunc("POST /api/r/{slug}/mod/pause", s.withOwner(s.handleModPause))
	mux.HandleFunc("POST /api/r/{slug}/mod/clear", s.withOwner(s.handleModClear))
	mux.HandleFunc("POST /api/r/{slug}/mod/ban", s.withOwner(s.handleModBan))
	mux.HandleFunc("POST /api/r/{slug}/mod/undo", s.withOwner(s.handleModUndo))
	mux.HandleFunc("POST /api/r/{slug}/mod/locks", s.withOwner(s.handleModLocks))
	mux.HandleFunc("POST /api/r/{slug}/mod/delete", s.withOwner(s.handleModDelete))

	// Accounts
	mux.HandleFunc("POST /api/auth/register", s.handleRegister)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	mux.HandleFunc("GET /api/auth/me", s.handleMe)
	mux.HandleFunc("GET /api/me/rooms", s.handleMyRooms)
	mux.HandleFunc("POST /api/r/{slug}/claim", s.withOwner(s.handleClaim))

	// Exports and previews
	mux.HandleFunc("GET /r/{slug}/canvas.png", s.withRoom(s.handlePNG))
	mux.HandleFunc("GET /r/{slug}/timelapse.gif", s.withRoom(s.handleGIF))
	mux.HandleFunc("GET /r/{slug}/card.png", s.withRoom(s.handleCard))

	// Pages
	mux.HandleFunc("GET /r/{slug}", s.handleRoomPage)
	mux.HandleFunc("GET /embed/{slug}", s.handleEmbedPage)
	mux.Handle("GET /assets/", http.FileServer(http.FS(s.Static)))
	mux.HandleFunc("GET /", s.handleHome)

	return s.middleware(mux)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		// The app loads only its own assets and talks only to its own origin.
		// frame-ancestors stays open because embedding a canvas is a feature;
		// the embed view is read-only and carries no credentials.
		// script-src is spelled out rather than left to default-src: an inline
		// <script> used to carry the room slug and was silently refused by this
		// policy, which broke the page in a way only a browser could show. The
		// slug now rides on a data attribute and no inline script exists.
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; img-src 'self' data:; "+
				"style-src 'self' 'unsafe-inline'; connect-src 'self' ws: wss:; "+
				"base-uri 'none'; form-action 'none'")
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- routing --

// roomHandler is a handler that has already had its room resolved.
type roomHandler func(w http.ResponseWriter, r *http.Request, rm *room.Room)

// withRoom resolves {slug} and hands the live room to the wrapped handler.
func (s *Server) withRoom(h roomHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := strings.ToLower(r.PathValue("slug"))
		if !room.ValidSlug(slug) {
			s.notFound(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		rm, err := s.Rooms.Get(ctx, slug)
		if err != nil {
			if errors.Is(err, room.ErrNotFound) {
				s.notFound(w, r)
				return
			}
			s.Log.Error("loading room", "slug", slug, "err", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "room unavailable"})
			return
		}
		h(w, r, rm)
	}
}

// withOwner is withRoom plus a proof-of-ownership check.
func (s *Server) withOwner(h roomHandler) http.HandlerFunc {
	return s.withRoom(func(w http.ResponseWriter, r *http.Request, rm *room.Room) {
		if !s.isOwner(r, rm) {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": "this needs the moderator key for this room",
			})
			return
		}
		h(w, r, rm)
	})
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	page, err := s.page("404.html", nil)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write(page)
}

// --------------------------------------------------------------- identity --

const (
	uidCookie  = "pf_uid"
	sessCookie = "pf_session"
)

// modCookie is the per-room moderator cookie name.
func modCookie(slug string) string { return "pf_mod_" + slug }

// painterID returns a stable anonymous id for the caller, minting and setting
// one if needed. It is HMAC-signed so a client cannot pick its own id and dodge
// a cooldown or a ban by editing the cookie.
func (s *Server) painterID(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(uidCookie); err == nil {
		if uid, ok := s.Signer.Verify(c.Value); ok && uid != "" {
			return uid
		}
	}
	uid := auth.NewID()
	http.SetCookie(w, &http.Cookie{
		Name:     uidCookie,
		Value:    s.Signer.Sign(uid, 0),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
		MaxAge:   int((365 * 24 * time.Hour).Seconds()),
	})
	return uid
}

// accountID returns the logged-in account, or 0.
func (s *Server) accountID(r *http.Request) int64 {
	c, err := r.Cookie(sessCookie)
	if err != nil {
		return 0
	}
	v, ok := s.Signer.Verify(c.Value)
	if !ok {
		return 0
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// isOwner reports whether this request may administer the room. Three things
// count: the per-room moderator cookie, a key in the query string (which is
// what the recovery link carries), or an account that owns the room.
func (s *Server) isOwner(r *http.Request, rm *room.Room) bool {
	if uid := s.accountID(r); uid != 0 && rm.Meta.OwnerUser == uid {
		return true
	}
	if c, err := r.Cookie(modCookie(rm.Slug())); err == nil {
		if auth.CheckModerator(s.Secret, rm.Meta.OwnerHash, c.Value) {
			return true
		}
	}
	if key := r.URL.Query().Get("key"); key != "" {
		return auth.CheckModerator(s.Secret, rm.Meta.OwnerHash, key)
	}
	return false
}

func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// clientIP prefers the platform's forwarding header, because behind Dockup's
// proxy RemoteAddr is the proxy for everybody.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if ip := r.Header.Get("X-Real-Ip"); ip != "" {
		return strings.TrimSpace(ip)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ------------------------------------------------------------ rate limits --

// ipLimiter is a fixed-window counter per address. It is the blunt companion to
// the per-painter cooldown: the cooldown is honest but a script that throws its
// cookie away gets a fresh identity every request, and this is what stops that.
type ipLimiter struct {
	mu     sync.Mutex
	counts map[string]*windowCount
	limit  int
	window time.Duration
}

type windowCount struct {
	n     int
	until time.Time
}

func newIPLimiter(limit int, window time.Duration) *ipLimiter {
	return &ipLimiter{counts: map[string]*windowCount{}, limit: limit, window: window}
}

// allow records a request and reports whether it is within the limit.
func (l *ipLimiter) allow(ip string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.counts) > 20000 {
		// Bound the map rather than let a spray of addresses grow it forever.
		for k, v := range l.counts {
			if now.After(v.until) {
				delete(l.counts, k)
			}
		}
	}

	c, ok := l.counts[ip]
	if !ok || now.After(c.until) {
		l.counts[ip] = &windowCount{n: 1, until: now.Add(l.window)}
		return true
	}
	c.n++
	return c.n <= l.limit
}

// ---------------------------------------------------------------- handlers -

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	rooms, clients := s.Rooms.Resident()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"version":   s.Version,
		"ephemeral": s.Store.Ephemeral(),
		"uptime":    time.Since(s.started).Round(time.Second).String(),
		"rooms":     rooms,
		"clients":   clients,
		"requests":  s.requests.Load(),
		"places":    s.places.Load(),
		"rejected":  s.rejected.Load(),
	})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.Store.Ready(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "degraded", "err": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

func (s *Server) handlePalettes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"palettes": canvas.Palettes(),
		"limits": map[string]any{
			"minDim": room.MinDim, "maxDim": room.MaxDim,
			"maxCooldownMs": room.MaxCooldownMs, "maxNameLen": room.MaxNameLen,
		},
	})
}

func (s *Server) handleListRooms(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	rooms, err := s.Store.ListRooms(ctx, 60)
	if err != nil {
		s.Log.Error("listing rooms", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "could not list rooms"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rooms": s.summarise(rooms)})
}

func (s *Server) summarise(rooms []store.RoomSummary) []map[string]any {
	out := make([]map[string]any, 0, len(rooms))
	for _, rm := range rooms {
		live := 0
		if resident, ok := s.Rooms.Lookup(rm.Slug); ok {
			live = resident.Hub.Count()
		}
		out = append(out, map[string]any{
			"slug":       rm.Slug,
			"name":       rm.Name,
			"width":      rm.Width,
			"height":     rm.Height,
			"palette":    rm.Palette,
			"colors":     canvas.PaletteFor(rm.Palette),
			"placements": rm.Placements,
			"paused":     rm.Paused,
			"visibility": rm.Visibility,
			"clients":    live,
			"createdAt":  rm.CreatedAt.UTC().Format(time.RFC3339),
			"lastActive": rm.LastActive.UTC().Format(time.RFC3339),
		})
	}
	return out
}

type createRoomRequest struct {
	Name    string `json:"name"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	Palette string `json:"palette"`
	// A pointer so an explicit 0 ("no cooldown") is distinguishable from the
	// field being absent, which is the difference between what the user chose
	// and what the default happens to be.
	CooldownMs *int `json:"cooldownMs"`
	Unlisted   bool `json:"unlisted"`
}

func (s *Server) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	if s.Store.Ephemeral() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "rooms need a database; this instance is running without one",
		})
		return
	}
	if !s.limiter.allow(clientIP(r)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "slow down"})
		return
	}

	var req createRoomRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.MaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "malformed request body"})
		return
	}

	key, err := auth.NewModeratorKey()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not create room"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	cooldown := -1 // "not specified"
	if req.CooldownMs != nil {
		cooldown = *req.CooldownMs
	}

	rm, err := s.Rooms.Create(ctx, room.Spec{
		Name:       req.Name,
		Width:      req.Width,
		Height:     req.Height,
		Palette:    req.Palette,
		CooldownMs: cooldown,
		Unlisted:   req.Unlisted,
		OwnerHash:  auth.ModeratorHash(s.Secret, key),
		OwnerUser:  s.accountID(r),
	})
	if err != nil {
		s.Log.Error("creating room", "err", err)
		// 503, to match every other handler that fails because the database is
		// unreachable. A 500 here told a monitor the application was broken when
		// what was actually happening was an outage it would recover from.
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "could not create room"})
		return
	}

	// The creator gets the key as a cookie so moderation just works in this
	// browser, and as a value they can save for when the cookie is gone.
	http.SetCookie(w, &http.Cookie{
		Name:     modCookie(rm.Slug()),
		Value:    key,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
		MaxAge:   int((365 * 24 * time.Hour).Seconds()),
	})
	s.painterID(w, r)

	writeJSON(w, http.StatusOK, map[string]any{
		"slug":         rm.Slug(),
		"url":          s.absolute("/r/" + rm.Slug()),
		"moderatorKey": key,
		"moderatorUrl": s.absolute("/r/" + rm.Slug() + "?key=" + key),
		"room":         rm.Info(),
	})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request, rm *room.Room) {
	uid := s.painterID(w, r)
	info := rm.Info()
	writeJSON(w, http.StatusOK, map[string]any{
		"room":      info,
		"uid":       uid,
		"owner":     s.isOwner(r, rm),
		"version":   s.Version,
		"ephemeral": s.Store.Ephemeral(),
		"shareUrl":  s.absolute("/r/" + rm.Slug()),
		"embedUrl":  s.absolute("/embed/" + rm.Slug()),
	})
}

// handleSnapshot serves the whole grid as one binary blob:
//
//	"PXF1" | width u16 | height u16 | seq i64 | one byte per cell
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request, rm *room.Room) {
	pixels, seq := rm.Canvas.Snapshot()
	buf := make([]byte, 0, 16+len(pixels))
	buf = append(buf, 'P', 'X', 'F', '1')
	buf = binary.BigEndian.AppendUint16(buf, uint16(rm.Canvas.Width()))
	buf = binary.BigEndian.AppendUint16(buf, uint16(rm.Canvas.Height()))
	buf = binary.BigEndian.AppendUint64(buf, uint64(seq))
	buf = append(buf, pixels...)

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Canvas-Seq", strconv.FormatInt(seq, 10))
	_, _ = w.Write(buf)
}

type placeRequest struct {
	X int `json:"x"`
	Y int `json:"y"`
	C int `json:"c"`
}

func (s *Server) handlePlace(w http.ResponseWriter, r *http.Request, rm *room.Room) {
	uid := s.painterID(w, r)

	if !s.limiter.allow(clientIP(r)) {
		s.rejected.Add(1)
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": "too many requests from this address", "retryInMs": 5000,
		})
		return
	}

	var req placeRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.MaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.rejected.Add(1)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "malformed request body"})
		return
	}

	px, err := s.place(rm, req, uid)
	if err != nil {
		s.rejected.Add(1)
		status, msg := placeError(err)
		body := map[string]any{"error": msg}
		if errors.Is(err, canvas.ErrCooldown) {
			body["retryInMs"] = rm.Canvas.CooldownRemaining(uid, time.Now()).Milliseconds()
		}
		writeJSON(w, status, body)
		return
	}

	s.places.Add(1)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "seq": px.Seq, "cooldownMs": rm.Canvas.Cooldown().Milliseconds(),
	})
}

func (s *Server) place(rm *room.Room, req placeRequest, uid string) (canvas.Pixel, error) {
	if req.C < 0 || req.C > 255 {
		return canvas.Pixel{}, canvas.ErrBadColour
	}
	return rm.Place(req.X, req.Y, uint8(req.C), uid)
}

func placeError(err error) (int, string) {
	switch {
	case errors.Is(err, canvas.ErrCooldown):
		return http.StatusTooManyRequests, "still cooling down"
	case errors.Is(err, canvas.ErrOutOfBounds):
		return http.StatusBadRequest, "coordinates outside the canvas"
	case errors.Is(err, canvas.ErrBadColour):
		return http.StatusBadRequest, "colour not in this room's palette"
	case errors.Is(err, canvas.ErrSameColour):
		return http.StatusConflict, "pixel is already that colour"
	case errors.Is(err, room.ErrPaused):
		return http.StatusForbidden, "the owner has paused this canvas"
	case errors.Is(err, room.ErrBanned):
		return http.StatusForbidden, "you are blocked from this canvas"
	case errors.Is(err, room.ErrLocked):
		return http.StatusForbidden, "that area is locked by the owner"
	case errors.Is(err, room.ErrRoomClosed):
		// 503 rather than 500: nothing is wrong with the request, the canvas was
		// released from memory between resolving it and painting on it. Sending
		// the same pixel again resolves the slug afresh and lands it.
		return http.StatusServiceUnavailable, "this canvas was just reloaded, try that pixel again"
	default:
		return http.StatusInternalServerError, "could not place pixel"
	}
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request, rm *room.Room) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit == 0 {
		limit = 30000
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	entries, err := s.Store.History(ctx, rm.Meta.ID, after, limit)
	if err != nil {
		s.Log.Error("history query", "room", rm.Slug(), "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "history unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": entries, "count": len(entries), "more": len(entries) == limit,
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request, rm *room.Room) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	stats := rm.Canvas.Stats()
	total, err := s.Store.CountPlacements(ctx, rm.Meta.ID)
	if err != nil {
		s.Log.Warn("placement count", "room", rm.Slug(), "err", err)
	}
	leaders, err := s.Store.Leaderboard(ctx, rm.Meta.ID, 10)
	if err != nil {
		s.Log.Warn("leaderboard", "room", rm.Slug(), "err", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"canvas":      stats,
		"placements":  total,
		"leaderboard": leaders,
		"clients":     rm.Hub.Count(),
		"room":        rm.Info(),
		"uptime":      time.Since(s.started).Round(time.Second).String(),
	})
}

// ------------------------------------------------------------------ pages --

func (s *Server) absolute(path string) string {
	if s.BaseURL == "" {
		return path
	}
	return strings.TrimRight(s.BaseURL, "/") + path
}

// page loads an embedded HTML file and substitutes {{KEY}} placeholders. Values
// are HTML-escaped, so a room name cannot inject markup into its own page.
func (s *Server) page(name string, vars map[string]string) ([]byte, error) {
	raw, err := fs.ReadFile(s.Static, name)
	if err != nil {
		return nil, err
	}
	out := string(raw)
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{{"+k+"}}", html.EscapeString(v))
	}
	// Any placeholder the caller did not supply becomes empty rather than
	// leaking braces into the page.
	for {
		i := strings.Index(out, "{{")
		if i < 0 {
			break
		}
		j := strings.Index(out[i:], "}}")
		if j < 0 {
			break
		}
		out = out[:i] + out[i+j+2:]
	}
	return []byte(out), nil
}

func (s *Server) servePage(w http.ResponseWriter, name string, vars map[string]string) {
	body, err := s.page(name, vars)
	if err != nil {
		http.Error(w, "page unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(body)
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		s.notFound(w, r)
		return
	}
	s.painterID(w, r)
	s.servePage(w, "index.html", map[string]string{
		"OG_URL":   s.absolute("/"),
		"OG_IMAGE": s.absolute("/assets/og-home.png"),
	})
}

func (s *Server) handleRoomPage(w http.ResponseWriter, r *http.Request) {
	slug := strings.ToLower(r.PathValue("slug"))
	if !room.ValidSlug(slug) {
		s.notFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	rm, err := s.Rooms.Get(ctx, slug)
	if err != nil {
		s.notFound(w, r)
		return
	}
	s.painterID(w, r)

	// If the visitor arrived on a moderator link, turn the key into a cookie so
	// the secret leaves the address bar and survives a reload.
	if key := r.URL.Query().Get("key"); key != "" && auth.CheckModerator(s.Secret, rm.Meta.OwnerHash, key) {
		http.SetCookie(w, &http.Cookie{
			Name: modCookie(slug), Value: key, Path: "/", HttpOnly: true,
			SameSite: http.SameSiteLaxMode, Secure: isHTTPS(r),
			MaxAge: int((365 * 24 * time.Hour).Seconds()),
		})
		http.Redirect(w, r, "/r/"+slug, http.StatusSeeOther)
		return
	}

	info := rm.Info()
	s.servePage(w, "room.html", map[string]string{
		"ROOM_NAME": info.Name,
		"ROOM_SLUG": slug,
		"ROOM_DESC": fmt.Sprintf("A %d×%d shared canvas. Paint a pixel and everyone watching sees it.",
			info.Width, info.Height),
		"OG_URL":   s.absolute("/r/" + slug),
		"OG_IMAGE": s.absolute("/r/" + slug + "/card.png"),
	})
}

func (s *Server) handleEmbedPage(w http.ResponseWriter, r *http.Request) {
	slug := strings.ToLower(r.PathValue("slug"))
	if !room.ValidSlug(slug) {
		s.notFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	rm, err := s.Rooms.Get(ctx, slug)
	if err != nil {
		s.notFound(w, r)
		return
	}
	// The embed is the one page that may be framed anywhere; it is read-only
	// and never sees the moderator cookie because it sets no credentials.
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self'; img-src 'self' data:; "+
			"style-src 'self' 'unsafe-inline'; connect-src 'self' ws: wss:")
	s.servePage(w, "embed.html", map[string]string{
		"ROOM_NAME": rm.Meta.Name,
		"ROOM_SLUG": slug,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func readJSON(w http.ResponseWriter, r *http.Request, max int64, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, max))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

var _ = io.Discard
