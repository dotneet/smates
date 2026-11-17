package cli

// ---- what became of each unit a cycle planned ----
//
// Every unit the cycle plans (cycle.plan) is in exactly one state at a time and ends in
// one of the final states: ran, notRun, unstarted or unmet. The log, the summaries, the
// GitHub outputs, --report, the run record and what is recorded into the state all read
// them from here, so they cannot disagree about a unit.

// unitState is where a planned unit stands.
type unitState uint8

const (
	unitPlanned unitState = iota // in a batch that has not started (yet)
	unitRunning                  // handed to the runner, no outcome yet
	// unitRan: its batch ran; unitRun.status is the outcome observed or inferred ("" when
	// nothing was: a passing batch that left no trace of it).
	unitRan
	// unitNotRun: the run stopped before its batch started, or cut the batch short
	// (cycle.stop says why).
	unitNotRun
	// unitUnstarted: its batch's runner could not start, or exited 126/127 before any
	// test reported a result (notStarted).
	unitUnstarted
	// unitUnmet: a requirement it needs (requires) is not met, or one a unit of a suite
	// that must run whole needs.
	unitUnmet
)

// final reports whether s is a state a unit ends the cycle in.
func (s unitState) final() bool {
	return s >= unitRan
}

// notStarted reports whether s is a final state of a unit that did not run: it stays
// selected.
func (s unitState) notStarted() bool {
	return s == unitNotRun || s == unitUnstarted || s == unitUnmet
}

// stopReason is what stopped a run before every batch started.
type stopReason uint8

const (
	stopNone      stopReason = iota
	stopFailFast             // --fail-fast: a batch failed
	stopMaxTime              // --max-time: the next batch would end too late
	stopInterrupt            // Ctrl-C, SIGTERM or SIGHUP
	stopConfig               // a suite command that cannot be parsed
)

// String names what stopped the run, for the log and the summaries.
func (r stopReason) String() string {
	switch r {
	case stopFailFast:
		return "--fail-fast"
	case stopMaxTime:
		return "--max-time"
	case stopInterrupt:
		return "an interrupt"
	case stopConfig:
		return "a configuration error"
	}
	return ""
}

// unitRun is what became of one planned unit.
type unitRun struct {
	state unitState
	// started: its batch's runner process started (it may still not have run: a runner
	// exiting 126/127, an interrupt).
	started bool
	// status is the outcome the run observed or inferred for it (state.Pass, ...; ""
	// when none); inferred: it comes from the batch's exit code.
	status   string
	inferred bool
}

// unitRuns tracks the planned units of a cycle, per suite, in the order they were
// planned.
type unitRuns struct {
	by    map[string]map[string]*unitRun
	order map[string][]string
}

// get returns the run of unit u of suite, adding it (planned) when it is not tracked yet.
func (r *unitRuns) get(suite, u string) *unitRun {
	if r.by == nil {
		r.by, r.order = map[string]map[string]*unitRun{}, map[string][]string{}
	}
	m := r.by[suite]
	if m == nil {
		m = map[string]*unitRun{}
		r.by[suite] = m
	}
	ur := m[u]
	if ur == nil {
		ur = &unitRun{}
		m[u] = ur
		r.order[suite] = append(r.order[suite], u)
	}
	return ur
}

// lookup returns the run of unit u of suite (nil when it is not tracked).
func (r *unitRuns) lookup(suite, u string) *unitRun {
	return r.by[suite][u]
}

// plan tracks the units of batches, in their order.
func (r *unitRuns) plan(batches []*batch) {
	for _, b := range batches {
		for _, u := range b.units {
			r.get(b.sr.Name, u)
		}
	}
}

// set puts units of suite in state st.
func (r *unitRuns) set(suite string, units []string, st unitState) {
	for _, u := range units {
		r.get(suite, u).state = st
	}
}

// start notes that the runner of a batch of units of suite starts.
func (r *unitRuns) start(suite string, units []string) {
	for _, u := range units {
		ur := r.get(suite, u)
		ur.state, ur.started = unitRunning, true
	}
}

// in returns the units of suite in any of the states sts, in planned order.
func (r *unitRuns) in(suite string, sts ...unitState) []string {
	var out []string
	for _, u := range r.order[suite] {
		if ur := r.by[suite][u]; inStates(ur.state, sts) {
			out = append(out, u)
		}
	}
	return out
}

// bySuite returns the units in any of the states sts, per suite (suites without any
// left out), in planned order.
func (r *unitRuns) bySuite(sts ...unitState) map[string][]string {
	out := map[string][]string{}
	for suite := range r.order {
		if units := r.in(suite, sts...); len(units) > 0 {
			out[suite] = units
		}
	}
	return out
}

// count counts the units in any of the states sts.
func (r *unitRuns) count(sts ...unitState) int {
	n := 0
	for suite := range r.order {
		n += r.countIn(suite, sts...)
	}
	return n
}

// countIn counts the units of suite in any of the states sts.
func (r *unitRuns) countIn(suite string, sts ...unitState) int {
	n := 0
	for _, ur := range r.by[suite] {
		if inStates(ur.state, sts) {
			n++
		}
	}
	return n
}

// startedUnits returns the units whose batch's runner started, per suite.
func (r *unitRuns) startedUnits() map[string][]string {
	out := map[string][]string{}
	for suite, units := range r.order {
		for _, u := range units {
			if r.by[suite][u].started {
				out[suite] = append(out[suite], u)
			}
		}
	}
	return out
}

// anyStarted reports whether the runner of any batch started.
func (r *unitRuns) anyStarted() bool {
	for _, m := range r.by {
		for _, ur := range m {
			if ur.started {
				return true
			}
		}
	}
	return false
}

// statuses returns the outcomes the run observed or inferred (suite -> unit ->
// status), for --report. A suite of which a batch ran is listed even when it observed
// nothing.
func (r *unitRuns) statuses() map[string]map[string]string {
	out := map[string]map[string]string{}
	for suite, m := range r.by {
		for u, ur := range m {
			if ur.status == "" && ur.state != unitRan {
				continue
			}
			if out[suite] == nil {
				out[suite] = map[string]string{}
			}
			if ur.status != "" {
				out[suite][u] = ur.status
			}
		}
	}
	return out
}

// countStatus counts the units whose outcome is status.
func (r *unitRuns) countStatus(status string) int {
	n := 0
	for _, m := range r.by {
		for _, ur := range m {
			if ur.status == status {
				n++
			}
		}
	}
	return n
}

// status is the outcome of unit u of suite ("" when none).
func (r *unitRuns) status(suite, u string) string {
	if ur := r.lookup(suite, u); ur != nil {
		return ur.status
	}
	return ""
}

func inStates(s unitState, sts []unitState) bool {
	for _, x := range sts {
		if s == x {
			return true
		}
	}
	return false
}
