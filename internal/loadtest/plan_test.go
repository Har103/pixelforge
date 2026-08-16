package loadtest

// paintPlan decides which cell a virtual painter touches next.
//
// Getting this right matters more than it looks. The obvious workload - every
// painter sweeps rows and cycles colours - quietly aliases: two painters end up
// offering the same colour to the same cell and the server correctly answers
// ErrSameColour, so three quarters of the "load" becomes a cheap 409 and the
// throughput number measures the rejection path. A first draft of this suite
// did exactly that and reported 4,662 placements a second when the real figure
// was 12,492.
//
// So each painter owns a disjoint, contiguous run of cells and walks it in
// order, changing colour once per complete pass. Every request is then a real
// placement: a different cell from the last one, and a different colour from
// the last time this painter was here.
//
// The cost of that choice is honesty about what it does not measure: real
// painters cluster and overwrite each other, so a real room has more
// same-colour rejections and more contention on adjacent cells than this. It
// does not have less.
type paintPlan struct {
	w, h    int
	lo, hi  int // flat cell indices, [lo, hi)
	i       int
	pass    int
	colours int // usable palette entries, excluding background 0
}

// newPaintPlan carves the grid into as many equal runs as there are painters.
func newPaintPlan(w, h, painters, id, colours int) *paintPlan {
	if painters < 1 {
		painters = 1
	}
	if colours < 2 {
		colours = 2
	}
	total := w * h
	per := total / painters
	if per < 1 {
		per = 1
	}
	lo := (id % painters) * per
	hi := lo + per
	if hi > total {
		hi = total
	}
	if lo >= total {
		lo, hi = 0, per
	}
	return &paintPlan{w: w, h: h, lo: lo, hi: hi, i: lo, colours: colours}
}

// next returns the next cell and the colour to put in it.
func (p *paintPlan) next() (x, y, colour int) {
	idx := p.i
	x, y = idx%p.w, idx/p.w
	// Colour 0 is the background; using it would make a "painted" cell
	// indistinguishable from an untouched one when the grid is compared after a
	// restart.
	colour = 1 + (p.pass % (p.colours - 1))
	p.i++
	if p.i >= p.hi {
		p.i = p.lo
		p.pass++
	}
	return x, y, colour
}
