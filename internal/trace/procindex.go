package trace

import (
	"slices"
	"sort"
	"strconv"
	"strings"
)

// procIndex answers "which traced process held pid P (thread T) at time t". Pids are
// reused: a trace may hold several processes with one pid and thread id, which never ran
// at the same time (each started after the one before it ended). It is the one place
// that decides this, for references inherited through the environment (Anc, SpawnKey),
// coverage files, parent pids and the main thread of a worker thread.
type procIndex struct {
	procs map[[2]int][]*Proc // {pid, tid} -> processes in order of start
	tids  map[int][]int      // pid -> its thread ids, ascending
}

func newProcIndex(procs []*Proc) *procIndex {
	ix := &procIndex{procs: map[[2]int][]*Proc{}, tids: map[int][]int{}}
	for _, p := range procs {
		ix.add(p)
	}
	ix.sort()
	return ix
}

func (ix *procIndex) add(p *Proc) {
	k := [2]int{p.Pid, p.Tid}
	if len(ix.procs[k]) == 0 {
		tids := ix.tids[p.Pid]
		i, _ := slices.BinarySearch(tids, p.Tid)
		ix.tids[p.Pid] = slices.Insert(tids, i, p.Tid)
	}
	ix.procs[k] = append(ix.procs[k], p)
}

// sort orders the processes of each pid and thread id by start (call it after add).
func (ix *procIndex) sort() {
	for _, ps := range ix.procs {
		sort.SliceStable(ps, func(i, j int) bool { return ps[i].Start < ps[j].Start })
	}
}

// instances returns the processes with pid and tid, in order of start.
func (ix *procIndex) instances(pid, tid int) []*Proc { return ix.procs[[2]int{pid, tid}] }

// named returns the process with pid and tid that a reference made at time t names: the
// last one started by then, also when it has ended since (a process started through a
// shell outlives the traced ancestor named in its environment, and a coverage file is
// written after the exit event), else the first one. self is never returned.
func (ix *procIndex) named(pid, tid int, t int64, self *Proc) *Proc {
	var out *Proc
	for _, p := range ix.instances(pid, tid) {
		if p == self {
			continue
		}
		if out == nil || p.Start <= t {
			out = p
		}
	}
	return out
}

// running returns the process with pid and tid that was running at time t (a parent pid
// read when a process started): the last one started by then, unless it had ended more
// than startSlack before (End 0: unknown, taken as running). nil when none was. A time
// of 0 (a process that left no start event) tells nothing: named answers.
func (ix *procIndex) running(pid, tid int, t int64, self *Proc) *Proc {
	if t == 0 {
		return ix.named(pid, tid, t, self)
	}
	var out *Proc
	for _, p := range ix.instances(pid, tid) {
		if p != self && p.Start <= t {
			out = p
		}
	}
	if out != nil && out.End > 0 && out.End+startSlack < t {
		return nil // the pid was free, or held by a process that left no trace
	}
	return out
}

// main returns the main thread (the lowest thread id traced) of the process with pid
// that was running at time t, other than self.
func (ix *procIndex) main(pid int, t int64, self *Proc) *Proc {
	for _, tid := range ix.tids[pid] {
		if p := ix.running(pid, tid, t, self); p != nil {
			return p
		}
	}
	return nil
}

// resolve rewrites a "pid-tid" reference made by a process started at t to the key of
// the instance it names (see Proc.Inst).
func (ix *procIndex) resolve(ref string, t int64, self *Proc) string {
	pid, tid, ok := parseRef(ref)
	if !ok || len(ix.instances(pid, tid)) < 2 {
		return ref
	}
	if p := ix.named(pid, tid, t, self); p != nil {
		return p.Key()
	}
	return ref
}

// parseRef splits a "pid-tid" reference.
func parseRef(ref string) (pid, tid int, ok bool) {
	a, b, found := strings.Cut(ref, "-")
	if !found {
		return 0, 0, false
	}
	pid, err1 := strconv.Atoi(a)
	tid, err2 := strconv.Atoi(b)
	return pid, tid, err1 == nil && err2 == nil
}

// parentOf returns the traced process that started p (byKey maps keys to processes),
// nil when none is known. A worker thread's is its process's main thread. Otherwise it
// is the process that held p's parent pid when p started (ppid is read live, so it must
// have been running then), as long as that agrees with the traced ancestor p names
// (Anc, inherited through the environment): a traced parent passes on its own key, or
// (Bun.spawn without an env option) the environment Bun started with, which names Bun's
// own ancestor. A parent pid that disagrees was reused (an untraced shell got the pid of
// a traced process that had ended, say), or belongs to a process whose worker thread
// started p: p belongs to the traced ancestor it names.
func parentOf(p *Proc, ix *procIndex, byKey map[string]*Proc) *Proc {
	if p.Tid != 0 {
		if m := ix.main(p.Pid, p.Start, p); m != nil && m.Tid < p.Tid {
			return m
		}
	}
	anc := byKey[p.Anc]
	if anc == p {
		anc = nil
	}
	if p.Ppid == 0 || p.Ppid == p.Pid {
		return anc
	}
	q := ix.main(p.Ppid, p.Start, p)
	switch {
	case q == nil:
		return anc
	case anc == nil, q == anc, q.Lang == "bun" && q.Anc != "" && q.Anc == p.Anc:
		return q
	}
	return anc
}
