// Package httpapi wires the canvas, the store and the hub to HTTP handlers.
package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Har103/pixelforge/internal/canvas"
	"github.com/Har103/pixelforge/internal/hub"
	"github.com/Har103/pixelforge/internal/ws"
)

// Server holds everything the handlers need.
type Server struct {
	Canvas  *canvas.Canvas
	Store   *canvas.Store
	Hub     *hub.Hub
	Log     *slog.Logger
	Static  fs.FS
	Version string

	// Secret signs the anonymous identity cookie.
	Secret []byte

	// MaxBodyBytes caps request bodies. Placements are tiny; anything larger is
	// either a bug or an attack.
	MaxBodyBytes int64

	started  time.Time
	requests atomic.Uint64
	places   atomic.Uint64
	rejected atomic.Uint64
}

// Routes builds the mux.
func (s *Server) Routes() http.Handler {
	if s.MaxBodyBytes == 0 {
		s.MaxBodyBytes = 4 << 10
	}
	s.started = time.Now()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/snapshot", s.handleSnapshot)
	mux.HandleFunc("POST /api/place", s.handlePlace)
	mux.HandleFunc("GET /api/history", s.handleHistory)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /ws", s.handleWS)
	mux.HandleFunc("GET /sse", s.handleSSE)

	// Static assets and the single page app.
	fileServer := http.FileServer(http.FS(s.Static))
	mux.Handle("GET /assets/", fileServer)
	mux.HandleFunc("GET /", s.handleIndex)

	return s.middleware(mux)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		// The app loads only its own assets and talks only to its own origin.
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; connect-src 'self' ws: wss:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- identity --

const uidCookie = "pf_uid"

// identify returns a stable anonymous id for the caller, minting and setting one
// if needed. The value is HMAC-signed so a client cannot forge a different id
// and dodge its cooldown by editing the cookie.
func (s *Server) identify(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(uidCookie); err == nil {
		if uid, ok := s.verifyUID(c.Value); ok {
			return uid
		}
	}
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		// Falling back to the clock keeps the service up; the only cost is a
		// slightly more predictable id under an already broken CSPRNG.
		binary.BigEndian.PutUint64(append(raw[:0], make([]byte, 8)...), uint64(time.Now().UnixNano()))
	}
	uid := hex.EncodeToString(raw)
	http.SetCookie(w, &http.Cookie{
		Name:     uidCookie,
		Value:    s.signUID(uid),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
		MaxAge:   int((365 * 24 * time.Hour).Seconds()),
	})
	return uid
}

func (s *Server) signUID(uid string) string {
	m := hmac.New(sha256.New, s.Secret)
	io.WriteString(m, uid)
	return uid + "." + hex.EncodeToString(m.Sum(nil)[:10])
}

func (s *Server) verifyUID(v string) (string, bool) {
	uid, sig, ok := strings.Cut(v, ".")
	if !ok || len(uid) == 0 || len(uid) > 32 {
		return "", false
	}
	m := hmac.New(sha256.New, s.Secret)
	io.WriteString(m, uid)
	want := hex.EncodeToString(m.Sum(nil)[:10])
	if subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		return "", false
	}
	return uid, true
}

// isHTTPS reports whether the original client request was over TLS, honouring
// the forwarding header a platform proxy sets.
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// ---------------------------------------------------------------- handlers --

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	s.identify(w, r)
	data, err := fs.ReadFile(s.Static, "index.html")
	if err != nil {
		http.Error(w, "index unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	clients, delivered, dropped := s.Hub.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"ephemeral": s.Store.Ephemeral(),
		"version":   s.Version,
		"uptime":    time.Since(s.started).Round(time.Second).String(),
		"clients":   clients,
		"delivered": delivered,
		"dropped":   dropped,
		"requests":  s.requests.Load(),
		"places":    s.places.Load(),
		"rejected":  s.rejected.Load(),
		"seq":       s.Canvas.Seq(),
	})
}

// handleReady is the deployment probe: it fails while the database is
// unreachable, so a platform can hold traffic until the app can actually serve.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if _, err := s.Store.TotalPlacements(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "degraded", "err": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	uid := s.identify(w, r)
	writeJSON(w, http.StatusOK, map[string]any{
		"width":      s.Canvas.Width(),
		"height":     s.Canvas.Height(),
		"palette":    canvas.Palette,
		"cooldownMs": s.Canvas.Cooldown().Milliseconds(),
		"uid":        uid,
		"version":    s.Version,
		"seq":        s.Canvas.Seq(),
		"ephemeral":  s.Store.Ephemeral(),
	})
}

// handleSnapshot serves the whole grid as one binary blob:
//
//	magic "PXF1" | width u16 | height u16 | seq i64 | one byte per pixel
//
// A 512x512 canvas is 262 KiB raw and compresses to a few KiB, which beats any
// JSON encoding by an order of magnitude.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	pixels, seq := s.Canvas.Snapshot()
	buf := make([]byte, 0, 16+len(pixels))
	buf = append(buf, 'P', 'X', 'F', '1')
	buf = binary.BigEndian.AppendUint16(buf, uint16(s.Canvas.Width()))
	buf = binary.BigEndian.AppendUint16(buf, uint16(s.Canvas.Height()))
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

func (s *Server) handlePlace(w http.ResponseWriter, r *http.Request) {
	uid := s.identify(w, r)

	var req placeRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.MaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.rejected.Add(1)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "malformed request body"})
		return
	}

	px, err := s.place(req, uid)
	if err != nil {
		s.rejected.Add(1)
		status, msg := placeError(err)
		body := map[string]any{"error": msg}
		if errors.Is(err, canvas.ErrCooldown) {
			body["retryInMs"] = s.Canvas.CooldownRemaining(uid, time.Now()).Milliseconds()
		}
		writeJSON(w, status, body)
		return
	}

	s.places.Add(1)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"seq":        px.Seq,
		"cooldownMs": s.Canvas.Cooldown().Milliseconds(),
	})
}

// place applies one placement and pushes it to history and to every client.
func (s *Server) place(req placeRequest, uid string) (canvas.Pixel, error) {
	if req.C < 0 || req.C > 255 {
		return canvas.Pixel{}, canvas.ErrBadColour
	}
	px, err := s.Canvas.Place(req.X, req.Y, uint8(req.C), uid, time.Now())
	if err != nil {
		return canvas.Pixel{}, err
	}
	s.Store.Record(px)
	s.Hub.Publish(px)
	return px, nil
}

func placeError(err error) (int, string) {
	switch {
	case errors.Is(err, canvas.ErrCooldown):
		return http.StatusTooManyRequests, "still cooling down"
	case errors.Is(err, canvas.ErrOutOfBounds):
		return http.StatusBadRequest, "coordinates outside the canvas"
	case errors.Is(err, canvas.ErrBadColour):
		return http.StatusBadRequest, "colour not in the palette"
	case errors.Is(err, canvas.ErrSameColour):
		return http.StatusConflict, "pixel is already that colour"
	default:
		return http.StatusInternalServerError, "could not place pixel"
	}
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit == 0 {
		limit = 20000
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	entries, err := s.Store.History(ctx, after, limit)
	if err != nil {
		s.Log.Error("history query failed", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "history unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": entries,
		"count":   len(entries),
		"more":    len(entries) == limit,
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	stats := s.Canvas.Stats()
	total, err := s.Store.TotalPlacements(ctx)
	if err != nil {
		s.Log.Warn("placement count failed", "err", err)
	}
	leaders, err := s.Store.Leaderboard(ctx, 10)
	if err != nil {
		s.Log.Warn("leaderboard query failed", "err", err)
	}
	clients, _, _ := s.Hub.Stats()

	writeJSON(w, http.StatusOK, map[string]any{
		"canvas":      stats,
		"placements":  total,
		"leaderboard": leaders,
		"clients":     clients,
		"uptime":      time.Since(s.started).Round(time.Second).String(),
	})
}

// -------------------------------------------------------------- transports --

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	uid := s.identify(w, r)

	conn, err := ws.Upgrade(w, r, &ws.Options{MaxMessageSize: 8 << 10})
	if err != nil {
		s.Log.Debug("websocket upgrade refused", "err", err, "remote", r.RemoteAddr)
		return
	}
	sub := s.Hub.Subscribe("ws")

	done := make(chan struct{})

	// Writer: drains the hub into frames, and keeps the connection alive with
	// pings so an idle proxy does not silently drop it.
	go func() {
		defer close(done)
		ping := time.NewTicker(25 * time.Second)
		defer ping.Stop()
		for {
			select {
			case f, ok := <-sub.C:
				if !ok {
					return
				}
				op := ws.OpText
				if f.Binary {
					op = ws.OpBinary
				}
				if err := conn.WriteMessage(byte(op), f.Data); err != nil {
					return
				}
			case <-ping.C:
				if err := conn.Ping(); err != nil {
					return
				}
			}
		}
	}()

	// Greet the client with the current sequence so it can tell whether its
	// snapshot is stale.
	hello, _ := json.Marshal(map[string]any{
		"t": "hello", "uid": uid, "seq": s.Canvas.Seq(), "clients": s.Hub.Count(),
	})
	_ = conn.WriteText(hello)

	// Reader: placements may arrive over the socket, which saves a round trip
	// compared with POSTing each one.
	for {
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		op, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if op != ws.OpText {
			continue
		}
		var msg struct {
			T string `json:"t"`
			X int    `json:"x"`
			Y int    `json:"y"`
			C int    `json:"c"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		switch msg.T {
		case "place":
			if _, err := s.place(placeRequest{X: msg.X, Y: msg.Y, C: msg.C}, uid); err != nil {
				s.rejected.Add(1)
				reply, _ := json.Marshal(map[string]any{
					"t":         "denied",
					"reason":    err.Error(),
					"x":         msg.X,
					"y":         msg.Y,
					"retryInMs": s.Canvas.CooldownRemaining(uid, time.Now()).Milliseconds(),
				})
				_ = conn.WriteText(reply)
				continue
			}
			s.places.Add(1)
		case "ping":
			_ = conn.WriteText([]byte(`{"t":"pong"}`))
		}
	}

	s.Hub.Unsubscribe(sub)
	<-done
	_ = conn.Close()
}

// handleSSE is the fallback for networks or proxies that will not carry a
// WebSocket. It is one-way, so the client POSTs placements instead.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	s.identify(w, r)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	// Tell nginx-style proxies not to buffer, or events arrive in clumps.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	fmt.Fprintf(w, "retry: 2000\n\n")
	hello, _ := json.Marshal(map[string]any{"t": "hello", "seq": s.Canvas.Seq(), "clients": s.Hub.Count()})
	fmt.Fprintf(w, "data: %s\n\n", hello)
	flusher.Flush()

	sub := s.Hub.Subscribe("sse")
	defer s.Hub.Unsubscribe(sub)

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case f, ok := <-sub.C:
			if !ok {
				return
			}
			if f.Binary {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", f.Data); err != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
