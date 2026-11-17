package selector

import (
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// gitIn runs git in the fixture's directory (a repository made by gitFixture).
func gitIn(t *testing.T, f *fixture, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = f.dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestSelectNoSnapshotInState: a state with entries but no snapshot and no base
// revision (started in diff mode, or written by --suite runs only, with no base branch
// to be found) takes the changes since the oldest commit its entries ran at, so a
// change no test depends on is still an orphan; when none of those commits is in the
// clone it runs everything, with a note, rather than select nothing silently. The
// per-test hashes still decide the tests in state mode.
func TestSelectNoSnapshotInState(t *testing.T) {
	const missing = "0123456789abcdef0123456789abcdef01234567"
	cases := []struct {
		name string
		// commits returns the commits the entries ran at, given the commits of the
		// fixture's history: base (the first), mid (a commit adding Makefile, which no
		// test depends on) and side (a commit on a branch HEAD does not contain).
		commits func(base, mid, side string) []string
		repo    bool // the selection has the repository
		// committed: Dockerfile changed in a commit after base (mid); otherwise it is
		// edited in the working tree after HEAD.
		committed bool
		wantAll   bool // ModeAll (must keep: no base can be found)
		// sinceBase: the changes are taken since base, so Makefile is an orphan too
		// (must keep: the oldest commit, not the newest).
		sinceBase bool
	}{
		{name: "entries at HEAD, working-tree change", commits: func(b, m, s string) []string { return []string{m} }, repo: true},
		{name: "entries at HEAD, committed change", commits: func(b, m, s string) []string { return []string{m} }, repo: true, committed: true},
		{name: "entries at an older commit", commits: func(b, m, s string) []string { return []string{b} }, repo: true, committed: true, sinceBase: true},
		{name: "oldest of several commits", commits: func(b, m, s string) []string { return []string{m, b, m} }, repo: true, sinceBase: true},
		{name: "a commit not in the clone next to one that is", commits: func(b, m, s string) []string { return []string{missing, m} }, repo: true},
		{name: "a commit on a branch HEAD does not contain", commits: func(b, m, s string) []string { return []string{s, m} }, repo: true, committed: true, sinceBase: true},
		{name: "no commit is in the clone", commits: func(b, m, s string) []string { return []string{missing} }, repo: true, wantAll: true},
		{name: "entries without a commit", commits: func(b, m, s string) []string { return []string{""} }, repo: true, wantAll: true},
		{name: "no repository", commits: func(b, m, s string) []string { return []string{b} }, wantAll: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := webFixture(t)
			repo, base := gitFixture(t, f)
			gitIn(t, f, "checkout", "-q", "-b", "side")
			f.write("web/fixtures/users.json", `["side"]`)
			gitIn(t, f, "commit", "-q", "-am", "side")
			side := gitIn(t, f, "rev-parse", "HEAD")
			gitIn(t, f, "checkout", "-q", "main")
			f.write("web/fixtures/users.json", `[]`)
			f.write("migrations/001.sql", "create table u()")
			f.write("Makefile", "all:")
			gitIn(t, f, "add", "-A")
			gitIn(t, f, "commit", "-q", "-m", "mid")
			mid := gitIn(t, f, "rev-parse", "HEAD")

			st := f.record(nil, nil)
			st.Files = map[string]string{}
			commits := tc.commits(base, mid, side)
			i := 0
			for _, e := range st.Suites["web"].Tests {
				e.Commit = commits[i%len(commits)]
				i++
			}
			if tc.committed {
				f.write("Dockerfile", "FROM y")
				gitIn(t, f, "commit", "-q", "-am", "dockerfile")
			} else {
				f.write("Dockerfile", "FROM z")
			}
			f.write("web/src/a.ts", "x")
			res := f.sel(st, func(in *Input) {
				if tc.repo {
					in.Repo = repo
				}
			})
			if tc.wantAll {
				if res.Mode != ModeAll || !slices.ContainsFunc(res.Notes, func(n string) bool { return strings.Contains(n, "no snapshot") }) {
					t.Fatalf("mode %s notes %v: want every test, with a note", res.Mode, res.Notes)
				}
				if got := decisions(res)["web/src/b.test.ts"]; got != ReasonAll {
					t.Errorf("b.test.ts: %s", got)
				}
				return
			}
			if res.Mode != ModeState {
				t.Fatalf("mode %s notes %v", res.Mode, res.Notes)
			}
			wantOrphans := []string{"Dockerfile"}
			if tc.sinceBase {
				wantOrphans = append(wantOrphans, "Makefile")
			}
			if !reflect.DeepEqual(res.Orphans, wantOrphans) {
				t.Errorf("orphans %v (changed %v)", res.Orphans, res.Changed)
			}
			if !slices.ContainsFunc(res.Notes, func(n string) bool { return strings.Contains(n, "changes are taken since") }) {
				t.Errorf("notes %v", res.Notes)
			}
			want := map[string]string{"web/src/a.test.ts": ReasonChanged, "web/src/b.test.ts": "-unaffected", "web/src/c.test.ts": "-unaffected"}
			for id, w := range want {
				if got := decisions(res)[id]; got != w {
					t.Errorf("%s: %s, want %s", id, got, w)
				}
			}
			// A run that handles the orphans starts the snapshot from that commit's
			// tree and marks them seen (cli saveState): the next selection has a
			// snapshot, and does not take every change since that commit again.
			if res.ChangesSince == "" {
				t.Fatal("ChangesSince is empty")
			}
			snap, err := repo.TreeSnapshot(res.ChangesSince)
			if err != nil {
				t.Fatal(err)
			}
			cur, err := repo.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			st.Files = map[string]string(snap)
			for _, o := range res.Orphans {
				st.Files[o] = cur[o]
			}
			next := f.sel(st, func(in *Input) { in.Repo = repo })
			if next.ChangesSince != "" || len(next.Orphans) != 0 {
				t.Errorf("after the snapshot: since %q orphans %v notes %v", next.ChangesSince, next.Orphans, next.Notes)
			}
			// Must keep: a later change no test depends on is an orphan again.
			f.write("Makefile", "other:")
			if again := f.sel(st, func(in *Input) { in.Repo = repo }); !slices.Contains(again.Orphans, "Makefile") {
				t.Errorf("a later change: orphans %v", again.Orphans)
			}
		})
	}
	// A state with a snapshot does not need a base.
	f := webFixture(t)
	st := f.record(nil, nil)
	f.write("Dockerfile", "y")
	if res := f.sel(st, nil); res.Mode != ModeState || !reflect.DeepEqual(res.Orphans, []string{"Dockerfile"}) {
		t.Errorf("with a snapshot: mode %s orphans %v", res.Mode, res.Orphans)
	}
}
