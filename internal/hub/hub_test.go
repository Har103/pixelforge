package hub

import (
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Har103/pixelforge/internal/canvas"
)

func testHub(t *testing.T) *Hub {
	t.Helper()
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.tick = time.Millisecond
	return h
}

// drain reads a subscriber's channel until it closes, so a test can churn
// clients without any of them looking slow.
func drain(s *Subscriber, wg *sync.WaitGroup) {
	defer wg.Done()
	for range s.C {
	}
}

// TestBroadcastSurvivesClientsLeavingMidSend is the test this package should
// have had from the start, and it is not hypothetical: the race detector caught
// it in a full run, and what it catches is a panic rather than a lost message.
//
// broadcast copies the subscriber set and then releases the hub lock before
// delivering, so that one slow client cannot stall everyone else. That leaves a
// window between the copy and the send, and in that window another goroutine
// can close the very channel about to be written to - Unsubscribe when a socket
// drops, or closeAll when the room shuts down. A send on a closed channel takes
// the process with it.
//
// Every broadcaster the server has is represented here, because they all reach
// the same code: the coalescing loop flushing pixels and announcing presence, a
// request goroutine sending a control message, and the cursor tick.
func TestBroadcastSurvivesClientsLeavingMidSend(t *testing.T) {
	h := testHub(t)
	done := make(chan struct{})
	go h.Run(done)

	var readers sync.WaitGroup
	var actors sync.WaitGroup
	stop := make(chan struct{})

	// Clients arriving and leaving constantly, which is what a real room looks
	// like and what opens the window.
	for i := 0; i < 6; i++ {
		actors.Add(1)
		go func() {
			defer actors.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				name := "ws"
				if i%2 == 0 {
					name = "sse"
				}
				s := h.Subscribe(name)
				readers.Add(1)
				go drain(s, &readers)
				h.Unsubscribe(s)
			}
		}()
	}

	// Pixels, control messages and presence, all broadcasting at once.
	for i := 0; i < 3; i++ {
		actors.Add(1)
		go func() {
			defer actors.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				h.Publish(canvas.Pixel{X: 1, Y: 2, Color: 3})
				h.BroadcastJSON(map[string]any{"t": "cursors", "c": []int{1, 2}})
				h.flush()
			}
		}()
	}

	time.Sleep(250 * time.Millisecond)
	close(stop)
	actors.Wait()

	// And shutdown, which closes every remaining channel while the last
	// broadcasts may still be in flight.
	close(done)
	readers.Wait()
}

// TestSubscribersGetTheFormTheirTransportCanCarry pins the split that exists
// because SSE cannot carry a binary frame at all: a WebSocket client that
// received the JSON form would decode it as a pixel batch and paint nothing.
func TestSubscribersGetTheFormTheirTransportCanCarry(t *testing.T) {
	h := testHub(t)
	ws := h.Subscribe("ws")
	sse := h.Subscribe("sse")

	h.Publish(canvas.Pixel{X: 9, Y: 8, Color: 7})
	h.flush()

	binFrame, ok := nextFrame(t, ws)
	if !ok {
		t.Fatal("the WebSocket client received nothing")
	}
	if !binFrame.Binary {
		t.Fatalf("the WebSocket client got a text frame: %s", binFrame.Data)
	}
	if len(binFrame.Data) != 8 || binFrame.Data[0] != KindPixelBatch {
		t.Fatalf("binary frame = % x, want an 8-byte batch of one pixel starting %#x", binFrame.Data, KindPixelBatch)
	}
	if x, y, c := int(binFrame.Data[3])<<8|int(binFrame.Data[4]),
		int(binFrame.Data[5])<<8|int(binFrame.Data[6]), binFrame.Data[7]; x != 9 || y != 8 || c != 7 {
		t.Errorf("binary frame carries (%d,%d) colour %d, want (9,8) colour 7", x, y, c)
	}

	txtFrame, ok := nextFrame(t, sse)
	if !ok {
		t.Fatal("the SSE client received nothing")
	}
	if txtFrame.Binary {
		t.Fatal("the SSE client got a binary frame, which that transport cannot carry")
	}
	var msg struct {
		T string         `json:"t"`
		P []canvas.Pixel `json:"p"`
	}
	if err := json.Unmarshal(txtFrame.Data, &msg); err != nil {
		t.Fatalf("decoding the SSE frame %s: %v", txtFrame.Data, err)
	}
	if msg.T != "px" || len(msg.P) != 1 || msg.P[0].X != 9 || msg.P[0].Y != 8 {
		t.Errorf("SSE frame = %s, want one px entry at (9,8)", txtFrame.Data)
	}
}

// nextFrame takes the first pixel frame off a subscriber, skipping any presence
// announcement the loop has made in the meantime.
func nextFrame(t *testing.T, s *Subscriber) (Frame, bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case f, ok := <-s.C:
			if !ok {
				return Frame{}, false
			}
			if !f.Binary {
				var probe struct {
					T string `json:"t"`
				}
				if json.Unmarshal(f.Data, &probe) == nil && probe.T == "presence" {
					continue
				}
			}
			return f, true
		case <-deadline:
			return Frame{}, false
		}
	}
}

// TestASlowClientIsDroppedRatherThanStallingEveryoneElse pins the bounded
// buffer. A client that cannot keep up with the canvas is better off
// reconnecting and refetching the snapshot than holding up every other painter.
func TestASlowClientIsDroppedRatherThanStallingEveryoneElse(t *testing.T) {
	h := testHub(t)
	slow := h.Subscribe("ws")   // never read from
	quick := h.Subscribe("sse") // drained throughout

	read := make(chan struct{})
	go func() {
		defer close(read)
		for range quick.C {
		}
	}()

	// Paced, not blasted. The point is a client that never reads at all, not one
	// that momentarily loses a scheduling race with a tight loop - and without
	// the pause the drained client can fall behind too, which would make this
	// test about Go's scheduler rather than about the hub.
	for i := 0; i < h.bufferSize*3; i++ {
		h.Publish(canvas.Pixel{X: i % 16, Y: 1, Color: 2})
		h.flush()
		time.Sleep(time.Millisecond)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, dropped := h.Stats(); dropped > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, _, dropped := h.Stats(); dropped == 0 {
		t.Error("a client that never reads was never dropped, so its buffer would grow without bound")
	}

	// The one keeping up is untouched, which is the whole point of dropping the
	// other one.
	for time.Now().Before(deadline) {
		if h.Count() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := h.Count(); n != 1 {
		t.Errorf("%d clients remain, want only the one that keeps up", n)
	}
	select {
	case <-read:
		t.Error("the client that was keeping up got disconnected along with the slow one")
	default:
	}

	h.Unsubscribe(quick)
	<-read
	_ = slow
}

// TestUnsubscribingTwiceIsHarmless matters because two paths reach it at once:
// the reader goroutine when a socket closes, and broadcast's slow-client
// handler. Closing an already-closed channel is a panic, not a no-op.
func TestUnsubscribingTwiceIsHarmless(t *testing.T) {
	h := testHub(t)
	s := h.Subscribe("ws")

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.Unsubscribe(s)
		}()
	}
	wg.Wait()

	if n := h.Count(); n != 0 {
		t.Errorf("%d clients remain after unsubscribing the only one", n)
	}

	// Drain whatever was buffered before confirming the channel really is
	// closed, because a receive on a channel that still holds frames returns one
	// of them rather than the closed signal. A channel left open leaves the
	// writer goroutine parked on it forever.
	closed := false
	for i := 0; i < h.bufferSize+2; i++ {
		if _, ok := <-s.C; !ok {
			closed = true
			break
		}
	}
	if !closed {
		t.Error("the channel is still open after Unsubscribe, so the writer goroutine never returns")
	}
}

// presenceCounts takes whatever is already sitting in a subscriber's channel and
// returns the headcount out of each presence frame, so a test can talk about
// what one client was shown rather than about what was broadcast. Nothing here
// blocks: everything it reads was put there by the test's own goroutine.
func presenceCounts(t *testing.T, s *Subscriber) []int {
	t.Helper()
	var out []int
	for {
		select {
		case f, ok := <-s.C:
			if !ok {
				return out
			}
			if n, isPresence := presenceOf(t, f); isPresence {
				out = append(out, n)
			}
		default:
			return out
		}
	}
}

// awaitPresence blocks until a presence frame arrives or the deadline passes.
// The arrival of the frame is the event being waited for, so this is a
// synchronisation point and not a sleep standing in for one.
func awaitPresence(t *testing.T, s *Subscriber, within time.Duration) (int, bool) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case f, ok := <-s.C:
			if !ok {
				return 0, false
			}
			if n, isPresence := presenceOf(t, f); isPresence {
				return n, true
			}
		case <-deadline:
			return 0, false
		}
	}
}

func presenceOf(t *testing.T, f Frame) (int, bool) {
	t.Helper()
	if f.Binary {
		return 0, false
	}
	var msg struct {
		T string `json:"t"`
		N int    `json:"n"`
	}
	if err := json.Unmarshal(f.Data, &msg); err != nil || msg.T != "presence" {
		return 0, false
	}
	return msg.N, true
}

// TestAJoinStormDoesNotCostAFramePerJoiner is the one that matters. Presence
// used to be announced by whoever joined or left, which meant one arrival cost a
// frame to every client already in the room: filling a room with N people cost
// N^2/2 frames, and the load test watched frames per joiner climb 14, 27, 52,
// 102 as the room grew.
//
// The frame count is not the injury, though. Those frames arrive faster than a
// client can be scheduled to read them, so its 32-frame buffer fills and
// broadcast disconnects it as too slow - and that disconnection is itself
// another presence change, which broadcasts another N frames. 500 watchers
// joining an idle room produced thirteen thousand disconnections. People were
// being kicked off the canvas by other people arriving.
//
// Nobody reads their channel here, deliberately. A client that has connected but
// has not yet been scheduled is not a slow client, it is an ordinary one.
func TestAJoinStormDoesNotCostAFramePerJoiner(t *testing.T) {
	h := testHub(t)
	const joiners = 200

	subs := make([]*Subscriber, 0, joiners)
	for i := 0; i < joiners; i++ {
		subs = append(subs, h.Subscribe("ws"))
	}

	clients, delivered, dropped := h.Stats()
	if dropped != 0 {
		t.Errorf("%d frames could not be delivered while %d clients joined an idle room; "+
			"each one is a client the hub then disconnects as too slow, so people are "+
			"being kicked off the canvas by other people arriving", dropped, joiners)
	}
	if total := delivered + dropped; total > joiners {
		t.Errorf("%d joins produced %d frames (%.1f per joiner); a join is broadcasting to "+
			"everyone already in the room, which makes filling a room quadratic",
			joiners, total, float64(total)/float64(joiners))
	}
	if clients != joiners {
		t.Errorf("the hub holds %d subscribers after %d joins", clients, joiners)
	}

	// Bounded is only half of it: the number still has to arrive. Once the loop
	// has had its chance to act, every one of those clients must have been told
	// how many people are in the room.
	h.presenceStep(time.Now())
	for i, s := range subs {
		got := presenceCounts(t, s)
		if len(got) == 0 {
			t.Fatalf("client %d was never told how many people are in the room, so its "+
				"headcount stays at whatever it saw when it connected", i)
		}
		if last := got[len(got)-1]; last != joiners {
			t.Fatalf("client %d was last shown %d people in a room of %d", i, last, joiners)
		}
	}
}

// TestPresenceIsThrottledAndStillTrailsTheBurst pins the throttle from both
// ends. Rate limiting presence is easy to get wrong in the direction that
// matters: the old guard read "announce unless it is too soon and the count has
// not changed", and since a join always changes the count it never held anything
// back at all.
//
// Times are passed in rather than slept through, so this says exactly what the
// throttle does without waiting for a real second to go by.
func TestPresenceIsThrottledAndStillTrailsTheBurst(t *testing.T) {
	h := testHub(t)
	h.presenceEvery = time.Second
	watcher := h.Subscribe("sse")

	t0 := time.Now()
	if !h.presenceStep(t0) {
		t.Fatal("presenceStep sat on the first change to a room that had been quiet; a " +
			"throttle that delays the leading edge makes every join feel laggy")
	}
	h.Subscribe("ws")
	if h.presenceStep(t0.Add(200 * time.Millisecond)) {
		t.Error("presenceStep announced again 200ms after the last one, so a burst of joins " +
			"is not being held back and the room still pays a frame per arrival")
	}
	if !h.presenceStep(t0.Add(time.Second)) {
		t.Error("presenceStep never announced the change it had held back, so the count " +
			"every client is showing stays wrong until somebody else happens to join")
	}
	if h.presenceStep(t0.Add(10 * time.Second)) {
		t.Error("presenceStep announced although nobody had joined or left; an idle room " +
			"must cost nothing")
	}

	if got, want := presenceCounts(t, watcher), []int{1, 2}; !sameInts(got, want) {
		t.Errorf("the watcher was shown %v, want %v: one frame for the room it joined and "+
			"one for the person who arrived after it", got, want)
	}
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPresenceConvergesOnTheCountTheRoomSettledAt is the other half of the
// bargain the throttle makes: collapsing a burst is only acceptable if what
// comes out the far side is the number that is true at the end of it. A count
// that is a second stale is fine; a count that is permanently wrong is not.
func TestPresenceConvergesOnTheCountTheRoomSettledAt(t *testing.T) {
	h := testHub(t)
	h.presenceEvery = time.Second
	watcher := h.Subscribe("sse")

	t0 := time.Now()
	h.presenceStep(t0)
	presenceCounts(t, watcher) // the watcher's own arrival, already accounted for

	// Five people arrive and three of them leave again, all inside one interval:
	// a lobby emptying into a room, or a proxy dropping a handful of sockets.
	joined := make([]*Subscriber, 0, 5)
	for i := 0; i < 5; i++ {
		joined = append(joined, h.Subscribe("ws"))
	}
	for _, s := range joined[:3] {
		h.Unsubscribe(s)
	}

	h.presenceStep(t0.Add(time.Second))

	got := presenceCounts(t, watcher)
	if len(got) != 1 {
		t.Errorf("the watcher was shown %v: eight joins and leaves must collapse into the "+
			"one figure the room settled at, not be replayed one frame at a time", got)
	}
	if len(got) == 0 || got[len(got)-1] != h.Count() {
		t.Errorf("the watcher was last shown %v for a room of %d, so its headcount is "+
			"permanently wrong", got, h.Count())
	}
}

// TestRunAnnouncesPresenceSoNobodyElseHasTo pins where the work belongs. The
// loop already owns coalescing placements, and presence is the same problem: a
// number that changes far faster than anybody needs to be told about it. Leaving
// it to the joining goroutine is what made it quadratic.
func TestRunAnnouncesPresenceSoNobodyElseHasTo(t *testing.T) {
	h := testHub(t)
	h.presenceEvery = 5 * time.Millisecond
	done := make(chan struct{})
	defer close(done)
	go h.Run(done)

	watcher := h.Subscribe("sse")
	n, ok := awaitPresence(t, watcher, 10*time.Second)
	if !ok {
		t.Fatal("no presence frame arrived although a client joined; nothing announces " +
			"presence at all, so the number on the page never moves again")
	}
	if n != 1 {
		t.Errorf("the first announcement said %d people, want 1", n)
	}

	h.Subscribe("ws")
	deadline := time.Now().Add(10 * time.Second)
	for {
		n, ok = awaitPresence(t, watcher, time.Until(deadline))
		if !ok {
			t.Fatalf("the room went to 2 people and the watcher was never told; it was "+
				"last shown %d", n)
		}
		if n == 2 {
			break
		}
	}
}
