package loadtest

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Har103/pixelforge/internal/room"
)

// TestLoadManyRooms pushes past the registry's resident cap.
//
// room.MaxRooms is 64 and room.IdleTTL is 20 minutes, and there are two
// separate paths that take a room out of memory. Registry.sweep, on a one
// minute tick, skips any room that still has clients:
//
//	if rm.Hub.Count() == 0 && rm.Idle() > r.idleTTL {
//
// Registry.evictIfCrowded, which runs on every Get and every Create, does not:
//
//	if idlest == nil || rm.Idle() > idlest.Idle() { idlest = rm }
//
// So the question is not whether a room can be evicted while somebody is
// connected to it - it plainly can - but what happens to that person.
func TestLoadManyRooms(t *testing.T) {
	requireLoadtest(t)
	dsn := requireDSN(t)
	cond := measureConditions()

	t.Run("Residency", func(t *testing.T) { roomsResidency(t, dsn, cond) })
	t.Run("EvictedWithClientConnected", func(t *testing.T) { roomsEvictedWithClient(t, dsn, cond) })
}

// roomsResidency drives traffic into far more rooms than may be resident and
// checks the cap holds, then measures what the churn costs: every eviction
// throws a grid away and every return visit rebuilds it from the log.
func roomsResidency(t *testing.T, dsn string, cond conditions) {
	h := newHarness(t, harnessOpts{dsn: dsn})

	const rooms = 80
	slugs := make([]string, rooms)
	for i := range slugs {
		slugs[i], _ = createRoom(t, h.base, fmt.Sprintf("many-%d", i), 64, 64, 0)
	}

	resident, clients := h.registry.Resident()
	t.Logf("after creating %d rooms: %d resident, %d clients", rooms, resident, clients)
	if resident > room.MaxRooms {
		t.Errorf("resident rooms = %d, which is past the cap of %d", resident, room.MaxRooms)
	}

	// Now paint in all 80, round robin, so every room is repeatedly wanted and
	// the registry is forced to thrash.
	var wg sync.WaitGroup
	deadline := time.Now().Add(scaled(10 * time.Second))
	var placed, failed int64
	var mu sync.Mutex

	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			p := newPainter(h.base)
			defer p.close()
			local, localFail := int64(0), int64(0)
			n := 0
			for time.Now().Before(deadline) {
				slug := slugs[(id+n)%len(slugs)]
				status, err := p.place(slug, n%64, (n/64)%64, 1+(n%7))
				if err != nil || status >= 500 {
					localFail++
				} else if status == 200 {
					local++
				}
				n++
			}
			mu.Lock()
			placed += local
			failed += localFail
			mu.Unlock()
		}(w)
	}
	wg.Wait()

	resident, clients = h.registry.Resident()
	evictions := h.logs.count(msgEvicting)
	materialised := h.logs.count(msgRoomResident)

	t.Logf("\nMANY ROOMS: RESIDENCY AND CHURN\nconditions: %s\n"+
		"%d rooms of 64x64, 32 painters cycling through all of them for %s\n"+
		"  placements accepted ......... %d (%.0f/s)\n"+
		"  requests that failed ........ %d\n"+
		"  rooms resident at the end ... %d (cap %d)\n"+
		"  clients ..................... %d\n"+
		"  evictions ................... %d\n"+
		"  rooms materialised .......... %d (%.1f materialisations per room)\n",
		cond, rooms, scaled(10*time.Second), placed, rate(int(placed), scaled(10*time.Second)),
		failed, resident, room.MaxRooms, clients, evictions, materialised,
		float64(materialised)/float64(rooms))

	if resident > room.MaxRooms {
		t.Errorf("resident rooms = %d, past the cap of %d", resident, room.MaxRooms)
	}
	if failed > 0 {
		t.Errorf("%d requests failed while the registry was thrashing", failed)
	}
}

// roomsEvictedWithClient is the experiment this whole file exists for: hold a
// WebSocket open on a room, then make the registry evict it, and watch what the
// person on the other end of that socket experiences.
func roomsEvictedWithClient(t *testing.T, dsn string, cond conditions) {
	h := newHarness(t, harnessOpts{dsn: dsn})

	victim, _ := createRoom(t, h.base, "eviction victim", 64, 64, 0)

	// Watchers, not painters. A room with people painting in it keeps being
	// touched and is never the idlest; a room where people are watching someone
	// else's canvas is idle from the moment they arrive. That is an ordinary
	// thing for a shared canvas to be.
	const watchers = 16
	type watcher struct {
		p  *painter
		ws *wsClient
	}
	ws := make([]watcher, 0, watchers)
	for i := 0; i < watchers; i++ {
		p := newPainter(h.base)
		if _, err := p.bootstrap(victim); err != nil {
			t.Fatalf("watcher %d bootstrap: %v", i, err)
		}
		c, err := wsDial(wsURL(h.hostPort(), victim), p.cookieHeader())
		if err != nil {
			t.Fatalf("watcher %d dial: %v", i, err)
		}
		ws = append(ws, watcher{p: p, ws: c})
	}
	defer func() {
		for _, w := range ws {
			w.ws.close()
			w.p.close()
		}
	}()

	// Paint one pixel so the room has a known, checkable state, then leave it
	// alone so it becomes the idlest.
	painter0 := ws[0].p
	if _, err := painter0.place(victim, 1, 1, 3); err != nil {
		t.Fatalf("seeding the victim room: %v", err)
	}

	before, ok := h.registry.Lookup(victim)
	if !ok {
		t.Fatalf("victim room is not resident before the test even starts")
	}
	seqBefore := before.Canvas.Seq()
	clientsBefore := before.Hub.Count()
	t.Logf("victim room %s: seq %d, %d clients connected", victim, seqBefore, clientsBefore)

	heapBefore := sampleMem(time.Now())

	// Fill the registry. Every Create runs evictIfCrowded, which picks the
	// idlest room - and that is the one with sixteen people watching it.
	for i := 0; i < room.MaxRooms+4; i++ {
		createRoom(t, h.base, fmt.Sprintf("filler-%d", i), 16, 16, 0)
	}

	// This is now the assertion rather than the setup. The test was written to
	// demonstrate a bug: evictIfCrowded chose the idlest resident room without
	// asking whether anybody was in it, and a room full of watchers is idle by
	// definition - nobody watching somebody else's canvas is touching it. What
	// followed was four failures at once: placements accepted and never written
	// down, two live rooms for one slug, sockets left open with nothing ever
	// coming down them, and a pending buffer nothing would drain.
	//
	// Eviction now skips occupied rooms, so the room survives and the rest of
	// this test has nothing to observe. Keeping it as a skip would have quietly
	// stopped testing anything, so it asserts instead. The paragraphs below are
	// left in place as the record of what was actually measured.
	resident, _ := h.registry.Resident()
	if _, still := h.registry.Lookup(victim); !still {
		t.Fatalf("a room with %d people connected to it was evicted to stay under the "+
			"cap (%d resident): their pixels are accepted, broadcast to each other and "+
			"never written down, and the next visitor gets a second room that has never "+
			"heard of any of it", clientsBefore, resident)
	}
	if before.Closed() {
		t.Fatal("the occupied room was stopped, so its write-behind loop has returned " +
			"while its sockets are still painting into it")
	}
	t.Logf("the occupied room survived %d creations; %d rooms resident, cap %d - "+
		"the cap gives way to people rather than disconnecting them",
		room.MaxRooms+4, resident, room.MaxRooms)

	// Everything below here only runs if the room was evicted after all, which
	// is now a failure above. It stays because it is the measurement that made
	// the bug undeniable, and because it is the shape of the test somebody would
	// want if this ever regresses.
	if _, still := h.registry.Lookup(victim); still {
		return
	}
	t.Logf("victim room evicted (registry logged %d evictions); it still has %d "+
		"subscribers attached to its now-stopped hub",
		h.logs.count(msgEvicting), before.Hub.Count())

	// ---- 1. What does the person on the socket see?
	//
	// The hub's closeAll closes every subscriber channel, which ends the
	// handler's writer goroutine - including its 25 second ping. The reader
	// loop is still blocked on the socket, so the connection is not closed.
	sockAlive := 0
	sockClosed := 0
	for _, w := range ws {
		if _, err := w.ws.read(time.Now().Add(1500 * time.Millisecond)); err != nil {
			if isTimeout(err) {
				sockAlive++ // open, but nothing is coming
			} else {
				sockClosed++
			}
			continue
		}
		sockAlive++
	}

	// ---- 2. Can they still paint, and where does it go?
	const ghostX, ghostY, ghostColour = 5, 5, 4
	placesBefore := h.health().Places
	if err := ws[0].ws.sendPlace(ghostX, ghostY, ghostColour); err != nil {
		t.Fatalf("sending a placement on the evicted room's socket: %v", err)
	}
	// The socket is stale, so nothing will be echoed; give the server time to
	// have processed the message either way.
	time.Sleep(1500 * time.Millisecond)
	placesAfter := h.health().Places
	accepted := placesAfter - placesBefore

	ghostSeq := before.Canvas.Seq()

	// ---- 3. Is there now a second live room for the same slug?
	fresh := newPainter(h.base)
	defer fresh.close()
	cfg, err := fresh.bootstrap(victim)
	if err != nil {
		t.Fatalf("revisiting the evicted room: %v", err)
	}
	after, ok := h.registry.Lookup(victim)
	if !ok {
		t.Fatalf("the room did not come back after a visit")
	}
	splitBrain := after != before

	// ---- 4. Does the pixel exist anywhere durable?
	pixels, _, err := snapshotGrid(h.base, victim)
	if err != nil {
		t.Fatalf("reading the reloaded grid: %v", err)
	}
	ghostOnDisk := pixels[ghostY*64+ghostX] == ghostColour

	// ---- 5. Does the orphaned hub keep accumulating?
	//
	// Hub.Publish appends to h.pending and the coalescing loop is what empties
	// it. Once Run has returned, nothing empties it again.
	// Two floods rather than one. The absolute heap includes everything the
	// generator allocated too; the difference between two floods of known size,
	// each measured after a collection, isolates what the orphaned hub retained.
	const ghostFlood = 20000
	flood := func(n int) {
		for i := 0; i < n; i++ {
			if err := ws[0].ws.sendPlace(i%64, (i/64)%64, 1+(i%7)); err != nil {
				return
			}
		}
		time.Sleep(1500 * time.Millisecond)
		settle()
	}
	flood(ghostFlood)
	heapMid := sampleMem(time.Now())
	flood(4 * ghostFlood)
	heapAfter := sampleMem(time.Now())
	marginal := float64(heapAfter.HeapAlloc-heapMid.HeapAlloc) / float64(4*ghostFlood)
	orphanDrops := h.logs.count(msgBufferFull)

	t.Logf("\nMANY ROOMS: A CLIENT CONNECTED TO A ROOM THAT GETS EVICTED\nconditions: %s\n"+
		"room %s 64x64, %d WebSocket watchers, then %d rooms created to force eviction\n"+
		"  sockets still open after eviction ... %d of %d (closed: %d)\n"+
		"  a placement sent on a stale socket .. server counted %d as accepted\n"+
		"  evicted room's canvas seq ........... %d before, %d after that placement\n"+
		"  a second live room for the same slug  %v\n"+
		"  the pixel is in the reloaded grid ... %v\n"+
		"  reloaded room reports seq ........... %d\n"+
		"  heap after %d then %d more ghost placements: %s -> %s -> %s\n"+
		"  retained per ghost placement ........ %.0f bytes\n"+
		"  history entries dropped by the orphaned room: %d\n",
		cond, victim, watchers, room.MaxRooms+4,
		sockAlive, watchers, sockClosed, accepted,
		seqBefore, ghostSeq, splitBrain, ghostOnDisk, cfg.Room.Seq,
		ghostFlood, 4*ghostFlood, mib(heapBefore.HeapAlloc), mib(heapMid.HeapAlloc),
		mib(heapAfter.HeapAlloc), marginal, orphanDrops)

	if accepted > 0 && !ghostOnDisk {
		t.Errorf("DATA LOSS: a placement sent over a WebSocket attached to an evicted room "+
			"was accepted by the server (places counter +%d, canvas seq %d -> %d) and does "+
			"not exist in the room a new visitor sees. The painter was told nothing.",
			accepted, seqBefore, ghostSeq)
	}
	if splitBrain {
		t.Errorf("SPLIT BRAIN: two live room.Room values exist for slug %q at once. The "+
			"WebSocket clients hold the evicted one, whose canvas is at seq %d and whose "+
			"write-behind loop and hub have both stopped; every new visitor gets the other, "+
			"at seq %d.", victim, ghostSeq, cfg.Room.Seq)
	}
	if marginal > 16 {
		t.Errorf("UNBOUNDED GROWTH: the evicted room's hub retained %.0f bytes per "+
			"placement across a flood of %d. Hub.Publish appends to h.pending and only "+
			"Hub.Run's coalescing loop ever empties it; Run returned when the room was "+
			"stopped, so nothing empties it again for as long as one socket stays open.",
			marginal, 4*ghostFlood)
	}
	if sockAlive > 0 {
		t.Errorf("SILENT DEAD SOCKET: %d of %d WebSocket connections are still open after "+
			"their room was evicted. Their writer goroutine has returned, so they receive "+
			"no pixels, no presence and no pings, and nothing tells the client to "+
			"reconnect.", sockAlive, watchers)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if rm, ok := h.registry.Lookup(victim); ok {
		if n, err := h.store.CountPlacements(ctx, rm.Meta.ID); err == nil {
			t.Logf("rows in room_placements for %s: %d", victim, n)
		}
	}
}
