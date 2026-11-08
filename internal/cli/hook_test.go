package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/state"
	"gopkg.in/yaml.v3"
)

func hookCmd(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code, err := cmdHook(context.Background(), append(append([]string{}, args[0], "-C", dir), args[1:]...), nil, &out, &errb)
	if err != nil {
		errb.WriteString(err.Error())
	}
	return code, out.String(), errb.String()
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestHookInstallUninstall(t *testing.T) {
	g := newGitRepo(t)
	path := filepath.Join(g.repo.Root, ".git", "hooks", "pre-push")
	if code, _, errs := hookCmd(t, g.dir, "install"); code != 0 {
		t.Fatalf("install: %d %s", code, errs)
	}
	content := readFile(t, path)
	if !strings.HasPrefix(content, "#!/bin/sh\n"+hookBegin) || !strings.Contains(content, "smates hook run pre-push -- --no-agent || exit $?") {
		t.Fatalf("hook:\n%s", content)
	}
	if fi, _ := os.Stat(path); fi.Mode()&0o111 == 0 {
		t.Errorf("hook not executable: %v", fi.Mode())
	}
	// Installing twice changes nothing.
	if code, out, _ := hookCmd(t, g.dir, "install"); code != 0 || !strings.Contains(out, "already installed") {
		t.Errorf("second install: %d %q", code, out)
	}
	if readFile(t, path) != content {
		t.Error("second install rewrote the hook")
	}
	// Different flags update the block in place.
	if code, out, _ := hookCmd(t, g.dir, "install", "--agent", "--", "--explain"); code != 0 || !strings.Contains(out, "updated") {
		t.Errorf("reinstall: %d %q", code, out)
	}
	if c := readFile(t, path); !strings.Contains(c, "smates hook run pre-push -- --explain || exit") || strings.Count(c, hookBegin) != 1 {
		t.Errorf("reinstalled hook:\n%s", c)
	}
	if _, out, _ := hookCmd(t, g.dir, "status"); !strings.HasPrefix(out, "pre-push: installed\n") {
		t.Errorf("status: %q", out)
	}
	if code, _, errs := hookCmd(t, g.dir, "uninstall"); code != 0 {
		t.Fatalf("uninstall: %d %s", code, errs)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("hook still exists: %v", err)
	}
	if _, out, _ := hookCmd(t, g.dir, "status"); !strings.HasPrefix(out, "pre-push: not installed") {
		t.Errorf("status after uninstall: %q", out)
	}
	// pre-commit is a separate hook.
	if code, _, _ := hookCmd(t, g.dir, "install", "--hook", "pre-commit"); code != 0 {
		t.Fatal("install pre-commit")
	}
	if c := readFile(t, filepath.Join(g.repo.Root, ".git", "hooks", "pre-commit")); !strings.Contains(c, "smates hook run pre-commit") || !strings.Contains(c, "git commit --no-verify") {
		t.Errorf("pre-commit hook:\n%s", c)
	}
	if code, _, _ := hookCmd(t, g.dir, "install", "--hook", "post-merge"); code != 2 {
		t.Errorf("unsupported hook: exit %d", code)
	}
}

func TestHookExistingScript(t *testing.T) {
	g := newGitRepo(t)
	path := filepath.Join(g.repo.Root, ".git", "hooks", "pre-push")
	mine := "#!/usr/bin/env bash\nset -e\n./scripts/lint.sh\n"
	if err := os.WriteFile(path, []byte(mine), 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, errs := hookCmd(t, g.dir, "install")
	// A setup problem (exit 4), never "tests failed" (1).
	if code != ExitEnv || !strings.Contains(errs, "--append") || !strings.Contains(errs, "smates hook run pre-push") {
		t.Errorf("install over a foreign hook: %d %s", code, errs)
	}
	if readFile(t, path) != mine {
		t.Fatal("foreign hook modified")
	}
	if _, out, _ := hookCmd(t, g.dir, "status"); !strings.Contains(out, "not written by smates") {
		t.Errorf("status: %q", out)
	}
	// Uninstall leaves hooks smates did not write alone.
	if code, _, _ := hookCmd(t, g.dir, "uninstall"); code != 0 || readFile(t, path) != mine {
		t.Fatal("uninstall touched a foreign hook")
	}
	if code, _, errs := hookCmd(t, g.dir, "install", "--append"); code != 0 {
		t.Fatalf("append: %d %s", code, errs)
	}
	appended := readFile(t, path)
	if !strings.HasPrefix(appended, mine) || !strings.Contains(appended, hookBegin) {
		t.Fatalf("appended hook:\n%s", appended)
	}
	if code, out, _ := hookCmd(t, g.dir, "install"); code != 0 || !strings.Contains(out, "already installed") || readFile(t, path) != appended {
		t.Errorf("install after append: %d %q", code, out)
	}
	if _, out, _ := hookCmd(t, g.dir, "status"); !strings.Contains(out, "appended") {
		t.Errorf("status: %q", out)
	}
	if code, _, _ := hookCmd(t, g.dir, "uninstall"); code != 0 {
		t.Fatal("uninstall")
	}
	if got := readFile(t, path); got != mine {
		t.Errorf("uninstall left:\n%q\nwant\n%q", got, mine)
	}
	// Scripts in other languages are never appended to.
	py := "#!/usr/bin/env python3\nprint('hi')\n"
	os.WriteFile(path, []byte(py), 0o755)
	if code, _, _ := hookCmd(t, g.dir, "install", "--append"); code == 0 || readFile(t, path) != py {
		t.Error("appended to a python hook")
	}
}

func TestHookManagers(t *testing.T) {
	g := newGitRepo(t)
	g.git("config", "core.hooksPath", ".husky/_")
	code, _, errs := hookCmd(t, g.dir, "install")
	if code == 0 || !strings.Contains(errs, "husky") || !strings.Contains(errs, ".husky/pre-push") || !strings.Contains(errs, "smates hook run pre-push -- --no-agent") {
		t.Errorf("husky: %d %s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(g.repo.Root, ".husky")); !os.IsNotExist(err) {
		t.Error("wrote into husky's directory")
	}

	g = newGitRepo(t)
	os.WriteFile(filepath.Join(g.repo.Root, "lefthook.yml"), []byte("pre-commit:\n"), 0o644)
	code, _, errs = hookCmd(t, g.dir, "install")
	if code == 0 || !strings.Contains(errs, "lefthook") {
		t.Errorf("lefthook: %d %s", code, errs)
	}
	// The printed entry is valid YAML that lefthook can load as it is.
	var cfg map[string]struct {
		Commands map[string]struct {
			Run      string `yaml:"run"`
			UseStdin bool   `yaml:"use_stdin"`
		} `yaml:"commands"`
	}
	if err := yaml.Unmarshal([]byte(lefthookSnippet("pre-push", []string{"--no-agent"})), &cfg); err != nil {
		t.Fatalf("lefthook snippet: %v", err)
	}
	cmd := cfg["pre-push"].Commands["smates"]
	if !cmd.UseStdin || !strings.Contains(cmd.Run, "smates hook run pre-push -- --no-agent") || !strings.Contains(cmd.Run, "not found on PATH") {
		t.Errorf("lefthook entry: %+v", cmd)
	}
	if !strings.Contains(errs, lefthookSnippet("pre-push", []string{"--no-agent"})) {
		t.Errorf("lefthook output:\n%s", errs)
	}
	if _, err := os.Stat(filepath.Join(g.repo.Root, ".git", "hooks", "pre-push")); !os.IsNotExist(err) {
		t.Error("installed despite lefthook")
	}
}

func TestHookLocation(t *testing.T) {
	g := newGitRepo(t)
	g.git("config", "core.hooksPath", "githooks")
	if code, _, errs := hookCmd(t, g.dir, "install"); code != 0 {
		t.Fatalf("install: %s", errs)
	}
	if _, err := os.Stat(filepath.Join(g.repo.Root, "githooks", "pre-push")); err != nil {
		t.Errorf("core.hooksPath not honoured: %v", err)
	}
	if _, err := os.Stat(filepath.Join(g.repo.Root, ".git", "hooks", "pre-push")); !os.IsNotExist(err) {
		t.Error("installed into .git/hooks despite core.hooksPath")
	}

	// A linked worktree shares the main repository's hooks.
	g = newGitRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	g.git("worktree", "add", "-q", wt)
	if code, _, errs := hookCmd(t, wt, "install"); code != 0 {
		t.Fatalf("install in worktree: %s", errs)
	}
	if _, err := os.Stat(filepath.Join(g.repo.Root, ".git", "hooks", "pre-push")); err != nil {
		t.Errorf("worktree hook not in the common git directory: %v", err)
	}
	if _, out, _ := hookCmd(t, g.dir, "status"); !strings.HasPrefix(out, "pre-push: installed") {
		t.Errorf("status from the main worktree: %q", out)
	}
}

func TestShellJoin(t *testing.T) {
	got := shellJoin([]string{"--no-agent", "--suite", "web app", "it's", "--base=origin/main"})
	want := `--no-agent --suite 'web app' 'it'\''s' --base=origin/main`
	if got != want {
		t.Errorf("shellJoin = %s, want %s", got, want)
	}
}

func TestHookRunSkip(t *testing.T) {
	g := newGitRepo(t)
	t.Setenv("SMATES_SKIP_HOOK", "1")
	var errb bytes.Buffer
	code, err := cmdHook(context.Background(), []string{"run", "pre-push", "--", "-C", g.dir}, strings.NewReader(""), io.Discard, &errb)
	if code != 0 || err != nil || !strings.Contains(errb.String(), "SMATES_SKIP_HOOK") {
		t.Errorf("skip: %d %v %q", code, err, errb.String())
	}
	t.Setenv("SMATES_SKIP_HOOK", "")
	// Pushing only deletions runs nothing.
	errb.Reset()
	stdin := strings.NewReader("(delete) 0000000000000000000000000000000000000000 refs/heads/x " + g.git("rev-parse", "HEAD") + "\n")
	if code, err := cmdHook(context.Background(), []string{"run", "pre-push", "--", "-C", g.dir}, stdin, io.Discard, &errb); code != 0 || err != nil {
		t.Errorf("delete-only push: %d %v %s", code, err, errb.String())
	}
	if code, _ := cmdHook(context.Background(), []string{"run", "post-merge"}, nil, io.Discard, io.Discard); code != 2 {
		t.Errorf("unsupported hook: exit %d", code)
	}
}

func TestReadPushUpdates(t *testing.T) {
	in := "refs/heads/main 1111 refs/heads/main 2222\nrefs/heads/x 3333 refs/heads/x 0000\nbad line\n"
	got := readPushUpdates(strings.NewReader(in))
	want := []pushUpdate{{"refs/heads/main", "1111"}, {"refs/heads/x", "3333"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("readPushUpdates = %v", got)
	}
}

func TestUncommitted(t *testing.T) {
	g := newGitRepo(t)
	os.WriteFile(filepath.Join(g.dir, "a.txt"), []byte("a"), 0o644)
	g.git("add", "a.txt")
	g.commit("a")
	if n := uncommitted(g.repo, "pre-push"); n != 0 {
		t.Errorf("clean tree: %d", n)
	}
	os.WriteFile(filepath.Join(g.dir, "a.txt"), []byte("b"), 0o644)
	g.git("add", "a.txt")
	// Staged changes are what pre-commit commits, but not what pre-push pushes.
	if pc, pp := uncommitted(g.repo, "pre-commit"), uncommitted(g.repo, "pre-push"); pc != 0 || pp != 1 {
		t.Errorf("staged: pre-commit %d, pre-push %d", pc, pp)
	}
	os.WriteFile(filepath.Join(g.dir, "a.txt"), []byte("c"), 0o644)
	os.WriteFile(filepath.Join(g.dir, "new.txt"), []byte("n"), 0o644)
	if pc, pp := uncommitted(g.repo, "pre-commit"), uncommitted(g.repo, "pre-push"); pc != 2 || pp != 2 {
		t.Errorf("unstaged: pre-commit %d, pre-push %d", pc, pp)
	}
}

func TestRefreshHookState(t *testing.T) {
	clearCI(t)
	t.Setenv("SMATES_BRANCH", "")
	g := newGitRepo(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	g.git("init", "-q", "--bare", remote)
	g.git("remote", "add", "origin", remote)

	shared := state.New()
	shared.Commit = g.git("rev-parse", "HEAD")
	shared.Created = time.Now().UTC().Truncate(time.Second)
	shared.Suite("s").Tests["remote_test.go"] = &state.Entry{Status: "pass", At: shared.Created}
	src := filepath.Join(t.TempDir(), "state.json.gz")
	if err := shared.Save(src); err != nil {
		t.Fatal(err)
	}
	if err := pushState(g.repo, src, "origin", stateRefPrefix+"main", io.Discard); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(g.repo.Root, ".smates", "state.json.gz")
	f := &runFlags{}
	refreshHookState(g.repo, f, io.Discard)
	st, err := state.Load(path)
	if err != nil || st.Suites["s"] == nil || st.Suites["s"].Tests["remote_test.go"] == nil {
		t.Fatalf("missing state not pulled: %v %+v", err, st)
	}

	// A fresh local state on HEAD's history is used as it is.
	local := state.New()
	local.Commit = shared.Commit
	local.Created = time.Now().UTC()
	local.Suite("s").Tests["local_test.go"] = &state.Entry{Status: "pass", At: local.Created}
	if err := local.Save(path); err != nil {
		t.Fatal(err)
	}
	refreshHookState(g.repo, f, io.Discard)
	if st, _ := state.Load(path); st.Suites["s"].Tests["remote_test.go"] != nil {
		t.Error("fresh local state was merged with the remote one")
	}

	// A stale one is merged with the newer remote state, keeping local entries, but
	// not within hookPullEvery of the previous fetch.
	local.Created = shared.Created.Add(-30 * 24 * time.Hour)
	if err := local.Save(path); err != nil {
		t.Fatal(err)
	}
	refreshHookState(g.repo, f, io.Discard)
	if st, _ := state.Load(path); st.Suites["s"].Tests["remote_test.go"] != nil {
		t.Error("fetched again within hookPullEvery")
	}
	stamp := filepath.Join(filepath.Dir(path), ".state.json.gz.pulled")
	old := time.Now().Add(-2 * hookPullEvery)
	if err := os.Chtimes(stamp, old, old); err != nil {
		t.Fatal(err)
	}
	refreshHookState(g.repo, f, io.Discard)
	st, _ = state.Load(path)
	if st.Suites["s"].Tests["remote_test.go"] == nil || st.Suites["s"].Tests["local_test.go"] == nil {
		t.Errorf("stale state not merged: %+v", st.Suites["s"].Tests)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), ".pull-state.json.gz")); !os.IsNotExist(err) {
		t.Error("temporary pull file left behind")
	}
}

// A state this smates cannot read (a newer version's, on the branch's ref) is skipped
// for the next ref; with no readable state on any ref, pulling fails.
func TestPullStateSkipsUnreadableRef(t *testing.T) {
	g := newGitRepo(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	g.git("init", "-q", "--bare", remote)
	g.git("remote", "add", "origin", remote)
	good := state.New()
	good.Suite("s").Tests["main_test.go"] = &state.Entry{Status: "pass", At: time.Now().UTC()}
	src := filepath.Join(t.TempDir(), "good.json.gz")
	if err := good.Save(src); err != nil {
		t.Fatal(err)
	}
	newer := filepath.Join(t.TempDir(), "newer.json")
	if err := os.WriteFile(newer, []byte(`{"version": 999}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := pushState(g.repo, src, "origin", stateRefPrefix+"main", io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := pushState(g.repo, newer, "origin", stateRefPrefix+"feature", io.Discard); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "state.json.gz")
	var log bytes.Buffer
	if err := pullState(g.repo, path, "origin", []string{stateRefPrefix + "feature", stateRefPrefix + "main"}, &log); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if st, err := state.Load(path); err != nil || st.Suites["s"] == nil || st.Suites["s"].Tests["main_test.go"] == nil {
		t.Errorf("main's state not pulled: %v %+v", err, st)
	}
	if !strings.Contains(log.String(), "skipping the state on origin "+stateRefPrefix+"feature") || !strings.Contains(log.String(), "pulled state from origin "+stateRefPrefix+"main") {
		t.Errorf("log:\n%s", log.String())
	}

	other := filepath.Join(t.TempDir(), "state.json.gz")
	err := pullState(g.repo, other, "origin", []string{stateRefPrefix + "feature", stateRefPrefix + "missing"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unsupported state version 999") {
		t.Errorf("no readable state: %v", err)
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Error("a state was saved")
	}
}
