package static

import (
	"math/bits"
	"slices"
	"sort"
	"sync"
)

// closer computes transitive closures over a dependency function. It condenses the
// graph into strongly connected components once (Tarjan, over the nodes reached from
// the starts asked for so far, resolved lazily), so that a closure is a walk over the
// component DAG instead of over every file edge: the files of a Go package import each
// other and list every file of the packages they import, and test files of one package
// share most of their closure.
//
// The condensation is a snapshot of direct: a Graph never changes once built (deps
// memoizes it), so a closer lives as long as its Graph.
type closer struct {
	mu     sync.Mutex
	direct func(string) []string
	// names and id map nodes to their index and back. The files of the Graph get the
	// first indexes in sorted order (sorted), so that a closure comes out sorted by
	// scanning a bitset; other nodes (a start file the tree does not list) follow.
	names  []string
	id     map[string]int32
	sorted int32

	// Per node: Tarjan's index and lowlink (unvisited: 0), whether it is on Tarjan's
	// stack, its component (-1: none yet) and, while it is visited, its successors.
	index, low []int32
	onStack    []bool
	comp       []int32
	adj        [][]int32
	next       int32 // the next Tarjan index (from 1)

	// Per component: its members and the components it depends on directly.
	members [][]int32
	succ    [][]int32
	// linked holds, per component, the last component that listed it as a successor
	// (to list each one once).
	linked []int32

	// Scratch, reused under mu: the components a walk reached (by stamp) and the
	// bitset of the sorted files of a closure.
	stamp []uint32
	epoch uint32
	bits  []uint64
}

// newCloser returns a closer over direct; files are the nodes indexed first, in
// sorted order.
func newCloser(files map[string]bool, direct func(string) []string) *closer {
	names := make([]string, 0, len(files))
	for f := range files {
		names = append(names, f)
	}
	sort.Strings(names)
	c := &closer{direct: direct, id: make(map[string]int32, len(names)), sorted: int32(len(names))}
	for _, f := range names {
		c.add(f)
	}
	c.bits = make([]uint64, (len(names)+63)/64)
	return c
}

// add indexes a new node.
func (c *closer) add(p string) int32 {
	i := int32(len(c.names))
	c.names = append(c.names, p)
	c.id[p] = i
	c.index = append(c.index, 0)
	c.low = append(c.low, 0)
	c.onStack = append(c.onStack, false)
	c.comp = append(c.comp, -1)
	c.adj = append(c.adj, nil)
	return i
}

// node returns the index of p, indexing it when new.
func (c *closer) node(p string) int32 {
	if i, ok := c.id[p]; ok {
		return i
	}
	return c.add(p)
}

// closure returns the transitive dependencies of the start nodes, including them,
// sorted. The result is the caller's.
func (c *closer) closure(start []string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	roots := make([]int32, 0, len(start))
	for _, p := range start {
		v := c.node(p)
		if c.comp[v] < 0 {
			c.condense(v)
		}
		roots = append(roots, c.comp[v])
	}
	// Walk the component DAG from the start components.
	c.epoch++
	if c.epoch == 0 { // wrapped: forget every stamp
		clear(c.stamp)
		c.epoch = 1
	}
	if n := len(c.members); len(c.stamp) < n {
		c.stamp = append(c.stamp, make([]uint32, n-len(c.stamp))...)
	}
	var extra []string
	count := 0
	stack := roots[:0]
	for _, r := range roots {
		if c.stamp[r] != c.epoch {
			c.stamp[r] = c.epoch
			stack = append(stack, r)
		}
	}
	for len(stack) > 0 {
		k := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, v := range c.members[k] {
			if v < c.sorted {
				c.bits[v>>6] |= 1 << (v & 63)
			} else {
				extra = append(extra, c.names[v])
			}
			count++
		}
		for _, s := range c.succ[k] {
			if c.stamp[s] != c.epoch {
				c.stamp[s] = c.epoch
				stack = append(stack, s)
			}
		}
	}
	// The sorted files come out of the bitset in order (clearing it for the next
	// call); the other nodes are merged in.
	out := make([]string, 0, count)
	sort.Strings(extra)
	for w, word := range c.bits {
		if word == 0 {
			continue
		}
		c.bits[w] = 0
		for word != 0 {
			b := bits.TrailingZeros64(word)
			word &= word - 1
			f := c.names[w<<6+b]
			for len(extra) > 0 && extra[0] < f {
				out = append(out, extra[0])
				extra = extra[1:]
			}
			out = append(out, f)
		}
	}
	return append(out, extra...)
}

// condense runs Tarjan's algorithm from v (iteratively: import chains can be deep),
// assigning a component to every node it reaches that has none yet. Components are
// completed in reverse topological order, so the components a new one depends on
// already exist when it is completed.
func (c *closer) condense(v int32) {
	type frame struct {
		v int32
		i int // the next successor of v to visit
	}
	var stack []int32 // Tarjan's stack
	visit := func(v int32) frame {
		c.next++
		c.index[v], c.low[v] = c.next, c.next
		stack = append(stack, v)
		c.onStack[v] = true
		deps := c.direct(c.names[v])
		adj := make([]int32, len(deps))
		for i, d := range deps {
			adj[i] = c.node(d)
		}
		c.adj[v] = adj
		return frame{v: v}
	}
	calls := []frame{visit(v)}
	for len(calls) > 0 {
		f := &calls[len(calls)-1]
		if f.i < len(c.adj[f.v]) {
			w := c.adj[f.v][f.i]
			f.i++
			switch {
			case c.index[w] == 0:
				calls = append(calls, visit(w))
			case c.onStack[w]:
				c.low[f.v] = min(c.low[f.v], c.index[w])
			}
			continue
		}
		u := f.v
		calls = calls[:len(calls)-1]
		if len(calls) > 0 {
			p := calls[len(calls)-1].v
			c.low[p] = min(c.low[p], c.low[u])
		}
		if c.low[u] != c.index[u] {
			continue
		}
		// u is the root of a component: pop it.
		k := int32(len(c.members))
		i := len(stack) - 1
		for stack[i] != u {
			i--
		}
		members := slices.Clone(stack[i:])
		stack = stack[:i]
		for _, m := range members {
			c.onStack[m] = false
			c.comp[m] = k
		}
		c.linked = append(c.linked, k) // k itself: no self edge
		var succ []int32
		for _, m := range members {
			for _, w := range c.adj[m] {
				if cw := c.comp[w]; c.linked[cw] != k {
					c.linked[cw] = k
					succ = append(succ, cw)
				}
			}
			c.adj[m] = nil // no longer needed: the component holds the edges
		}
		c.members = append(c.members, members)
		c.succ = append(c.succ, succ)
	}
}
