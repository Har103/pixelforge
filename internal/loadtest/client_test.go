package loadtest

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func jsonDecode(r io.Reader, dst any) error { return json.NewDecoder(r).Decode(dst) }

// painter is one virtual person: an HTTP client with its own cookie jar, so the
// server mints it a distinct pf_uid and the per-painter cooldown applies to it
// the way it would to a real browser. Sharing one jar across the whole load
// would collapse every painter onto one identity and one cooldown.
type painter struct {
	http *http.Client
	base string
	uid  string
}

// jar is the same minimal union-keeping jar the httpapi suite uses: keeping the
// last response's cookies alone loses pf_uid the moment a handler sets only the
// moderator cookie.
type jar struct{ cookies []*http.Cookie }

func (j *jar) SetCookies(_ *url.URL, c []*http.Cookie) {
	byName := map[string]*http.Cookie{}
	for _, existing := range j.cookies {
		byName[existing.Name] = existing
	}
	for _, fresh := range c {
		byName[fresh.Name] = fresh
	}
	j.cookies = j.cookies[:0]
	for _, v := range byName {
		j.cookies = append(j.cookies, v)
	}
}

func (j *jar) Cookies(_ *url.URL) []*http.Cookie { return j.cookies }

// newPainter builds a client with its own identity and a connection of its own.
// MaxConnsPerHost is 1 on purpose: a painter is a browser, and a browser does
// not open sixty sockets to send sixty pixels. It also makes the concurrency
// number in a result mean what it says.
func newPainter(base string) *painter {
	tr := &http.Transport{
		MaxIdleConns:        4,
		MaxIdleConnsPerHost: 4,
		MaxConnsPerHost:     4,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
	return &painter{
		http: &http.Client{
			Jar:       &jar{},
			Transport: tr,
			Timeout:   30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		base: base,
	}
}

func (p *painter) close() {
	if tr, ok := p.http.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

func (p *painter) postJSON(path string, body any) (int, map[string]any, error) {
	raw, _ := json.Marshal(body)
	res, err := p.http.Post(p.base+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out, nil
}

// place posts one pixel and reports the status. It is the request/response
// paint path: the client waits for the server's acknowledgement, so the latency
// it measures is the one a POSTing client feels.
func (p *painter) place(slug string, x, y, colour int) (int, error) {
	raw, _ := json.Marshal(map[string]int{"x": x, "y": y, "c": colour})
	req, err := http.NewRequest("POST", p.base+"/api/r/"+slug+"/place", bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := p.http.Do(req)
	if err != nil {
		return 0, err
	}
	// Drain and close, or the connection is not reusable and every placement
	// pays for a fresh TCP handshake - which would measure the generator.
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return res.StatusCode, nil
}

// bootstrap fetches the room config, which is also how the painter learns the
// uid the server has given it.
func (p *painter) bootstrap(slug string) (roomConfig, error) {
	var out roomConfig
	res, err := p.http.Get(p.base + "/api/r/" + slug + "/config")
	if err != nil {
		return out, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return out, fmt.Errorf("GET config: status %d", res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return out, err
	}
	p.uid = out.UID
	return out, nil
}

type roomConfig struct {
	UID  string `json:"uid"`
	Room struct {
		Slug       string   `json:"slug"`
		Width      int      `json:"width"`
		Height     int      `json:"height"`
		Palette    []string `json:"palette"`
		CooldownMs int64    `json:"cooldownMs"`
		Seq        int64    `json:"seq"`
		Clients    int      `json:"clients"`
	} `json:"room"`
}

// cookieHeader renders this painter's cookies for a raw WebSocket handshake,
// which does not go through net/http's jar.
func (p *painter) cookieHeader() string {
	u, _ := url.Parse(p.base)
	cs := p.http.Jar.Cookies(u)
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

// ----------------------------------------------------------- room creation --

// createRoom makes a room and returns its slug. Cooldown 0 is the usual choice
// here: a load test of the paint path should be limited by the paint path, not
// by a per-painter timer the product applies on purpose.
func createRoom(t *testing.T, base string, name string, w, h, cooldownMs int) (string, *painter) {
	t.Helper()
	p := newPainter(base)
	status, out, err := p.postJSON("/api/rooms", map[string]any{
		"name": name, "width": w, "height": h, "cooldownMs": cooldownMs,
	})
	if err != nil {
		t.Fatalf("creating room %q: %v", name, err)
	}
	if status != http.StatusOK {
		t.Fatalf("creating room %q: status %d body %v", name, status, out)
	}
	slug, _ := out["slug"].(string)
	if slug == "" {
		t.Fatalf("creating room %q: no slug in %v", name, out)
	}
	return slug, p
}

// snapshotGrid fetches the authoritative grid: "PXF1" | w u16 | h u16 | seq i64
// | one byte per cell. This is the ground truth a restart has to reproduce.
func snapshotGrid(base, slug string) (pixels []byte, seq int64, err error) {
	res, err := http.Get(base + "/api/r/" + slug + "/snapshot")
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("GET snapshot: status %d", res.StatusCode)
	}
	buf, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, 0, err
	}
	if len(buf) < 16 || string(buf[:4]) != "PXF1" {
		return nil, 0, fmt.Errorf("snapshot: bad header %q (%d bytes)", buf[:min(4, len(buf))], len(buf))
	}
	seq = int64(binary.BigEndian.Uint64(buf[8:16]))
	return buf[16:], seq, nil
}
