package ddtxn

import (
	"container/heap"
	"flag"
	"fmt"
	"log"
	"runtime/debug"
)

var WRRatio = flag.Float64("wr", 2.0, "Ratio of sampled write conflicts and sampled writes to sampled reads at which to move a piece of data to split.  Default 3")

var ConflictWeight = flag.Float64("cw", 2.0, "Weight given to conflicts over writes\n")
var ReadWeight = flag.Float64("rw", 0.5, "Weight given to reads over stashes\n")

type OneStat struct {
	k         Key
	op        KeyType
	reads     float64
	writes    float64
	conflicts float64
	stash     float64
	index     int
}

func (o *OneStat) ratio() float64 {
	return float64((*ConflictWeight)*o.conflicts+o.writes) / (float64((*ReadWeight)*o.reads) + float64(o.stash))
}

// m is very big; it should have every key the worker sampled.  h is a
// heap of all keys we deemed interesting enough to add to the heap.
// This includes keys where the ratio is high enough to consider
// moving the key to dd, but also keys that are already dd.  We add
// their statistics changes to the heap to be merged in on the next
// stats computation.
//
// Since we limit what we add to h, it doesn't really have to be a
// heap.  But one could imagine eliminating m and only looking at the
// top set of things in the heap instead.
type Candidates struct {
	m map[Key]*OneStat
	h *StatsHeap
}

func (c *Candidates) Merge(c2 *Candidates) {
	for i := 0; i < len(*c2.h); i++ {
		o2 := heap.Pop(c2.h).(*OneStat)
		o, ok := c.m[o2.k]
		if !ok {
			c.m[o2.k] = &OneStat{k: o2.k, op: o2.op, reads: 0, writes: 0, conflicts: 0, stash: 0, index: -1}
			o = c.m[o2.k]
		}
		o.reads += o2.reads
		o.writes += o2.writes
		o.conflicts += o2.conflicts
		o.stash += o2.stash
		c.h.update(o)
	}
}

func (c *Candidates) Read(k Key, br *BRecord) {
	o, ok := c.m[k]
	if !ok {
		c.m[k] = &OneStat{k: k, op: -1, reads: 1, writes: 0, conflicts: 0, stash: 0, index: -1}
		o = c.m[k]
	} else {
		o.reads++
	}
	if o.ratio() > *WRRatio || (br != nil && br.dd) {
		c.h.update(o)
	}
}

// This is only used when a key is in split mode (can't count
// conflicts anymore because they don't happen).  Make it count for
// more.
func (c *Candidates) Write(k Key, br *BRecord, op KeyType) {
	o, ok := c.m[k]
	if !ok {
		c.m[k] = &OneStat{k: k, op: op, reads: 1, writes: 1, conflicts: 0, stash: 0, index: -1}
		o = c.m[k]
	} else {
		if o.op == -1 {
			o.op = op
		}
		if op != o.op {
			debug.PrintStack()
			log.Fatalf("Do not support multiple types of writes right now key %v, op write: %v op was: %v\n", k, op, o.op)
		}
		o.writes++
	}
	if (o.ratio() > *WRRatio && o.conflicts > 1) || (br != nil && br.dd) {
		c.h.update(o)
	}
}

func (c *Candidates) Conflict(k Key, br *BRecord, op KeyType) {
	o, ok := c.m[k]
	if !ok {
		c.m[k] = &OneStat{k: k, op: op, reads: 1, writes: 0, conflicts: 1, stash: 0, index: -1}
		o = c.m[k]
	} else {
		if o.op == -1 {
			o.op = op
		}
		if op != o.op {
			debug.PrintStack()
			log.Fatalf("Do not support multiple types of writes right now key %v, op conflict: %v op was: %v\n", k, op, o.op)
		}
		o.conflicts++
	}
	if o.ratio() > *WRRatio || (br != nil && br.dd) {
		c.h.update(o)
	}
}

func (c *Candidates) Stash(k Key) {
	o, ok := c.m[k]
	if !ok {
		c.m[k] = &OneStat{k: k, op: -1, reads: 0, writes: 0, conflicts: 0, stash: 1, index: -1}
		o = c.m[k]
	} else {
		o.stash++
	}
	c.h.update(o)
}

func (c *Candidates) ReadWrite(k Key, br *BRecord) {
	o, ok := c.m[k]
	if !ok {
		c.m[k] = &OneStat{k: k, op: -1, reads: 5, writes: 0, conflicts: 0, stash: 0, index: -1}
		o = c.m[k]
	} else {
		o.reads = o.reads + 10
		o.conflicts = o.conflicts - 1
	}
	if o.ratio() > *WRRatio || o.index > -1 || br.dd {
		c.h.update(o)
	}
}

func (c *Candidates) Print() {
	for i := 0; i < len(*c.h); i++ {
		if i > 20 {
			return
		}
		x := (*c.h)[i]
		z, y := UndoCKey(x.k)
		fmt.Printf("k: %v %v, r: %v, w: %v, conflicts: %v, stash: %v\n", z, y, x.reads, x.writes, x.conflicts, x.stash)
	}
}

type StatsHeap []*OneStat

func (h StatsHeap) Len() int           { return len(h) }
func (h StatsHeap) Less(i, j int) bool { return h[i].ratio() > h[j].ratio() }
func (h StatsHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *StatsHeap) Push(x interface{}) {
	// Push and Pop use pointer receivers because they modify the slice's length,
	// not just its contents.
	n := len(*h)
	*h = append(*h, x.(*OneStat))
	(*h)[n].index = n
}

func (h *StatsHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	x.index = -1
	*h = old[0 : n-1]
	return x
}

// after updating reads and writes in item, re-set
func (h *StatsHeap) update(o *OneStat) {
	if o.index != -1 {
		heap.Remove(h, o.index)
	}
	heap.Push(h, o)
}
