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
// the same code: the coalescing loop flushing pixels, a request goroutine
// announcing presence, and the cursor tick.
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

// nextFrame takes the first pixel frame off a subscriber, skipping the presence
// announcements that subscribing itself produces.
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

	// Drain whatever was buffered - subscribing announces presence, so the
	// channel is not empty - and then confirm it really is closed. A channel
	// left open leaves the writer goroutine parked on it forever.
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
