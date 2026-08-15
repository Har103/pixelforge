package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Har103/pixelforge/internal/auth"
	"github.com/Har103/pixelforge/internal/render"
	"github.com/Har103/pixelforge/internal/room"
	"github.com/Har103/pixelforge/internal/store"
	"github.com/Har103/pixelforge/internal/ws"
)

// ------------------------------------------------------------- transports --

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request, rm *room.Room) {
	uid := s.painterID(w, r)

	conn, err := ws.Upgrade(w, r, &ws.Options{MaxMessageSize: 8 << 10})
	if err != nil {
		s.Log.Debug("websocket upgrade refused", "err", err, "remote", clientIP(r))
		return
	}
	sub := rm.Hub.Subscribe("ws")
	rm.Touch()

	done := make(chan struct{})

	// Writer: drains the hub, and pings so an idle proxy does not quietly drop
	// a connection that is merely waiting for someone else to paint.
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

	hello, _ := json.Marshal(map[string]any{
		"t": "hello", "uid": uid, "seq": rm.Canvas.Seq(),
		"clients": rm.Hub.Count(), "paused": rm.Paused(),
	})
	_ = conn.WriteText(hello)

	// Reader: placements may arrive over the socket, which saves a round trip
	// per pixel compared with POSTing each one.
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
			if !s.limiter.allow(clientIP(r)) {
				s.rejected.Add(1)
				reply, _ := json.Marshal(map[string]any{
					"t": "denied", "reason": "too many requests from this address",
					"x": msg.X, "y": msg.Y, "retryInMs": 5000,
				})
				_ = conn.WriteText(reply)
				continue
			}
			if _, err := s.place(rm, placeRequest{X: msg.X, Y: msg.Y, C: msg.C}, uid); err != nil {
				s.rejected.Add(1)
				_, reason := placeError(err)
				reply, _ := json.Marshal(map[string]any{
					"t": "denied", "reason": reason, "x": msg.X, "y": msg.Y,
					"retryInMs": rm.Canvas.CooldownRemaining(uid, time.Now()).Milliseconds(),
				})
				_ = conn.WriteText(reply)
				continue
			}
			s.places.Add(1)
		case "ping":
			_ = conn.WriteText([]byte(`{"t":"pong"}`))
		}
	}

	rm.Hub.Unsubscribe(sub)
	<-done
	_ = conn.Close()
}

// handleSSE is the fallback for networks or proxies that will not carry an
// upgrade. It is one-way, so the client POSTs placements instead.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request, rm *room.Room) {
	s.painterID(w, r)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	fmt.Fprint(w, "retry: 2000\n\n")
	hello, _ := json.Marshal(map[string]any{
		"t": "hello", "seq": rm.Canvas.Seq(), "clients": rm.Hub.Count(), "paused": rm.Paused(),
	})
	fmt.Fprintf(w, "data: %s\n\n", hello)
	flusher.Flush()

	sub := rm.Hub.Subscribe("sse")
	defer rm.Hub.Unsubscribe(sub)
	rm.Touch()

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

// ------------------------------------------------------------ moderation ---

func (s *Server) handleModSettings(w http.ResponseWriter, r *http.Request, rm *room.Room) {
	var req struct {
		Name       *string `json:"name"`
		Visibility *string `json:"visibility"`
		CooldownMs *int    `json:"cooldownMs"`
	}
	if err := readJSON(w, r, s.MaxBodyBytes, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "malformed request body"})
		return
	}

	meta := rm.Meta
	if req.Name != nil {
		spec := room.Spec{Name: *req.Name}
		spec.Normalise()
		meta.Name = spec.Name
	}
	if req.Visibility != nil && (*req.Visibility == "public" || *req.Visibility == "unlisted") {
		meta.Visibility = *req.Visibility
	}
	if req.CooldownMs != nil {
		// The live canvas holds its cooldown at construction, so a change here
		// is recorded and takes effect when the room is next materialised.
		// Saying so beats pretending it applied instantly.
		v := *req.CooldownMs
		if v < 0 {
			v = 0
		}
		if v > room.MaxCooldownMs {
			v = room.MaxCooldownMs
		}
		meta.CooldownMs = v
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.Store.UpdateRoom(ctx, meta); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "could not save"})
		return
	}
	rm.Meta = meta
	rm.Hub.BroadcastJSON(map[string]any{"t": "room", "name": meta.Name})

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"room": rm.Info(),
		"note": "a cooldown change applies the next time this room is loaded from cold",
	})
}

func (s *Server) handleModPause(w http.ResponseWriter, r *http.Request, rm *room.Room) {
	var req struct {
		Paused bool `json:"paused"`
	}
	if err := readJSON(w, r, s.MaxBodyBytes, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "malformed request body"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := rm.SetPaused(ctx, req.Paused); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "could not save"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "paused": req.Paused})
}

func (s *Server) handleModClear(w http.ResponseWriter, r *http.Request, rm *room.Room) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := rm.Clear(ctx); err != nil {
		s.Log.Error("clearing room", "room", rm.Slug(), "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "could not clear"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleModBan(w http.ResponseWriter, r *http.Request, rm *room.Room) {
	var req struct {
		UID  string `json:"uid"`
		Lift bool   `json:"lift"`
	}
	if err := readJSON(w, r, s.MaxBodyBytes, &req); err != nil || req.UID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "which painter?"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var err error
	if req.Lift {
		err = rm.Unban(ctx, req.UID)
	} else {
		err = rm.Ban(ctx, req.UID)
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "could not save"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": req.UID, "banned": !req.Lift})
}

func (s *Server) handleModUndo(w http.ResponseWriter, r *http.Request, rm *room.Room) {
	var req struct {
		UID string `json:"uid"`
	}
	if err := readJSON(w, r, s.MaxBodyBytes, &req); err != nil || req.UID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "which painter?"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	n, err := rm.UndoPainter(ctx, req.UID)
	if err != nil {
		s.Log.Error("undoing painter", "room", rm.Slug(), "uid", req.UID, "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "could not undo"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "undone": n})
}

func (s *Server) handleModLocks(w http.ResponseWriter, r *http.Request, rm *room.Room) {
	var req struct {
		Locks []room.Lock `json:"locks"`
	}
	if err := readJSON(w, r, s.MaxBodyBytes, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "malformed request body"})
		return
	}
	if len(req.Locks) > 32 {
		req.Locks = req.Locks[:32]
	}
	clean := make([]room.Lock, 0, len(req.Locks))
	for _, l := range req.Locks {
		if l.X1 > l.X2 {
			l.X1, l.X2 = l.X2, l.X1
		}
		if l.Y1 > l.Y2 {
			l.Y1, l.Y2 = l.Y2, l.Y1
		}
		clean = append(clean, l)
	}
	rm.SetLocks(clean)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "locks": rm.Locks()})
}

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request, rm *room.Room) {
	uid := s.accountID(r)
	if uid == 0 {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in first"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.Store.ClaimRoom(ctx, rm.Meta.ID, uid); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "could not claim"})
		return
	}
	rm.Meta.OwnerUser = uid
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// -------------------------------------------------------------- accounts ---

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// validUsername keeps names to something that can be shown, said and looked up
// without surprises.
func validUsername(u string) bool {
	if len(u) < 3 || len(u) > 24 {
		return false
	}
	for _, r := range u {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '_' || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if s.Store.Ephemeral() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "accounts need a database"})
		return
	}
	if !s.limiter.allow(clientIP(r)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "slow down"})
		return
	}
	var c credentials
	if err := readJSON(w, r, s.MaxBodyBytes, &c); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "malformed request body"})
		return
	}
	c.Username = strings.TrimSpace(c.Username)
	if !validUsername(c.Username) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "usernames are 3 to 24 characters: letters, digits, dot, dash, underscore",
		})
		return
	}
	if len(c.Password) < 10 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "passwords need at least 10 characters",
		})
		return
	}

	hash, err := auth.HashPassword(c.Password)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not register"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	u, err := s.Store.CreateUser(ctx, c.Username, hash)
	if err != nil {
		if errors.Is(err, store.ErrUsernameTaken) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "that username is taken"})
			return
		}
		s.Log.Error("registering user", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not register"})
		return
	}
	s.setSession(w, r, u.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": u.Username})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.Store.Ephemeral() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "accounts need a database"})
		return
	}
	if !s.limiter.allow(clientIP(r)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "slow down"})
		return
	}
	var c credentials
	if err := readJSON(w, r, s.MaxBodyBytes, &c); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "malformed request body"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	u, err := s.Store.UserByName(ctx, strings.TrimSpace(c.Username))
	if err != nil || !auth.VerifyPassword(u.PwHash, c.Password) {
		// One message for both cases: telling an attacker which half was wrong
		// is free reconnaissance.
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "wrong username or password"})
		return
	}
	s.setSession(w, r, u.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": u.Username})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessCookie, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: isHTTPS(r), MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	id := s.accountID(r)
	if id == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"signedIn": false})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	u, err := s.Store.UserByID(ctx, id)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"signedIn": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"signedIn": true, "username": u.Username})
}

func (s *Server) handleMyRooms(w http.ResponseWriter, r *http.Request) {
	id := s.accountID(r)
	if id == 0 {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in first"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	rooms, err := s.Store.RoomsForUser(ctx, id)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "could not list rooms"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rooms": s.summarise(rooms)})
}

func (s *Server) setSession(w http.ResponseWriter, r *http.Request, userID int64) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessCookie,
		Value:    s.Signer.Sign(strconv.FormatInt(userID, 10), 30*24*time.Hour),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
		MaxAge:   int((30 * 24 * time.Hour).Seconds()),
	})
}

// --------------------------------------------------------------- exports ---

func (s *Server) handlePNG(w http.ResponseWriter, r *http.Request, rm *room.Room) {
	scale, _ := strconv.Atoi(r.URL.Query().Get("scale"))
	pixels, _ := rm.Canvas.Snapshot()

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=15")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("inline; filename=%q", rm.Slug()+".png"))

	if err := render.EncodePNG(w, pixels, rm.Canvas.Width(), rm.Canvas.Height(),
		rm.Canvas.Palette(), scale); err != nil {
		s.Log.Error("rendering png", "room", rm.Slug(), "err", err)
	}
}

// handleCard renders the Open Graph image. It is what a link to a room turns
// into in Slack, Discord or a timeline, so it shows the canvas as it is now.
func (s *Server) handleCard(w http.ResponseWriter, r *http.Request, rm *room.Room) {
	pixels, _ := rm.Canvas.Snapshot()
	stats := rm.Canvas.Stats()

	subtitle := fmt.Sprintf("%d x %d  -  %d pixels painted  -  paint one at %s",
		stats.Width, stats.Height, stats.Painted, hostOf(s.absolute("/r/"+rm.Slug())))

	w.Header().Set("Content-Type", "image/png")
	// Short cache: the preview should look current, but a burst of unfurls
	// from one paste should not each re-render.
	w.Header().Set("Cache-Control", "public, max-age=60")

	if err := render.SocialCard(w, pixels, stats.Width, stats.Height,
		rm.Canvas.Palette(), rm.Meta.Name, subtitle); err != nil {
		s.Log.Error("rendering social card", "room", rm.Slug(), "err", err)
	}
}

func hostOf(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	return u
}

func (s *Server) handleGIF(w http.ResponseWriter, r *http.Request, rm *room.Room) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// A time-lapse of a very long history is neither interesting nor cheap, so
	// take the most recent window rather than the whole log.
	const maxHistory = 60000
	entries, err := s.Store.History(ctx, rm.Meta.ID, 0, maxHistory)
	if err != nil {
		s.Log.Error("history for timelapse", "room", rm.Slug(), "err", err)
		http.Error(w, "timelapse unavailable", http.StatusServiceUnavailable)
		return
	}

	placements := make([]render.Placement, 0, len(entries))
	for _, e := range entries {
		placements = append(placements, render.Placement{X: e.X, Y: e.Y, Color: e.Color})
	}

	frames, _ := strconv.Atoi(r.URL.Query().Get("frames"))

	w.Header().Set("Content-Type", "image/gif")
	w.Header().Set("Cache-Control", "public, max-age=30")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("inline; filename=%q", rm.Slug()+"-timelapse.gif"))

	if err := render.EncodeTimelapse(w, placements, render.TimelapseOptions{
		Width:   rm.Canvas.Width(),
		Height:  rm.Canvas.Height(),
		Palette: rm.Canvas.Palette(),
		Frames:  frames,
	}); err != nil {
		s.Log.Error("encoding timelapse", "room", rm.Slug(), "err", err)
	}
}
