package cli

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/dotneet/smates/internal/gitx"
)

// ciInfo describes the CI environment, when there is one.
type ciInfo struct {
	Name string // "github", "gitlab", ... or "" when not on CI
	// Branch being built (for pull requests: the head branch).
	Branch string
	// DefaultBranch of the repository, when the CI exposes it.
	DefaultBranch string
	// PullRequest is set for pull/merge request builds.
	PullRequest bool
	// BaseRef is the target branch of a pull request.
	BaseRef string
}

func detectCI() ciInfo {
	g := os.Getenv
	switch {
	case g("GITHUB_ACTIONS") == "true":
		ci := ciInfo{Name: "github", Branch: g("GITHUB_REF_NAME"), BaseRef: g("GITHUB_BASE_REF")}
		ev := g("GITHUB_EVENT_NAME")
		ci.PullRequest = strings.HasPrefix(ev, "pull_request") || ev == "merge_group"
		if ci.PullRequest && g("GITHUB_HEAD_REF") != "" {
			ci.Branch = g("GITHUB_HEAD_REF")
		}
		if p := g("GITHUB_EVENT_PATH"); p != "" {
			if data, err := os.ReadFile(p); err == nil {
				var ev struct {
					Repository struct {
						DefaultBranch string `json:"default_branch"`
					} `json:"repository"`
				}
				if json.Unmarshal(data, &ev) == nil {
					ci.DefaultBranch = ev.Repository.DefaultBranch
				}
			}
		}
		return ci
	case g("GITLAB_CI") == "true":
		return ciInfo{Name: "gitlab", Branch: first(g("CI_MERGE_REQUEST_SOURCE_BRANCH_NAME"), g("CI_COMMIT_BRANCH")), DefaultBranch: g("CI_DEFAULT_BRANCH"),
			PullRequest: g("CI_MERGE_REQUEST_IID") != "", BaseRef: g("CI_MERGE_REQUEST_TARGET_BRANCH_NAME")}
	case g("CIRCLECI") == "true":
		return ciInfo{Name: "circleci", Branch: g("CIRCLE_BRANCH"), PullRequest: g("CIRCLE_PULL_REQUEST") != ""}
	case g("BUILDKITE") == "true":
		return ciInfo{Name: "buildkite", Branch: g("BUILDKITE_BRANCH"), DefaultBranch: g("BUILDKITE_PIPELINE_DEFAULT_BRANCH"),
			PullRequest: g("BUILDKITE_PULL_REQUEST") != "" && g("BUILDKITE_PULL_REQUEST") != "false", BaseRef: g("BUILDKITE_PULL_REQUEST_BASE_BRANCH")}
	case g("JENKINS_URL") != "":
		return ciInfo{Name: "jenkins", Branch: first(g("CHANGE_BRANCH"), g("BRANCH_NAME")), PullRequest: g("CHANGE_ID") != "", BaseRef: g("CHANGE_TARGET")}
	case g("CI") == "true" || g("CI") == "1":
		return ciInfo{Name: "ci"}
	}
	return ciInfo{}
}

// defaultBranch is the repository's default branch: the CI's when it says (a CI
// checkout often has no origin/HEAD, nor a main or master ref, for git to tell), else
// git's.
func defaultBranch(repo *gitx.Repo) string {
	return first(detectCI().DefaultBranch, repo.DefaultBranch())
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// fullRunBranch reports whether `smates run` should run everything by default: on CI,
// for a push build of a full-run branch (the default branch unless configured).
func fullRunBranch(ci ciInfo, repo *gitx.Repo, configured []string) (bool, string) {
	if ci.Name == "" || ci.PullRequest {
		return false, ""
	}
	branch := ci.Branch
	if branch == "" {
		branch = repo.CurrentBranch()
	}
	targets := configured
	if len(targets) == 0 {
		def := ci.DefaultBranch
		if def == "" {
			def = repo.DefaultBranch()
		}
		targets = []string{def}
	}
	for _, t := range targets {
		if t == branch {
			return true, branch
		}
	}
	return false, branch
}

// autoBase picks a git base for diff mode when there is no recorded state.
func autoBase(ci ciInfo, repo *gitx.Repo) string {
	var cands []string
	if ci.BaseRef != "" {
		cands = append(cands, "origin/"+ci.BaseRef, ci.BaseRef)
	}
	def := ci.DefaultBranch
	if def == "" {
		def = repo.DefaultBranch()
	}
	cands = append(cands, "origin/"+def, def)
	for _, c := range cands {
		if !repo.RefExists(c) {
			continue
		}
		if mb := repo.MergeBase(c); mb != "" {
			if mb == repo.Head() {
				// On the base branch itself: compare with what is not pushed yet, else
				// the previous commit.
				if ci.Name == "" && repo.RefExists("@{upstream}") {
					if up := repo.MergeBase("@{upstream}"); up != "" && up != mb {
						return up
					}
				}
				if repo.RefExists("HEAD~1") {
					return "HEAD~1"
				}
				return ""
			}
			return mb
		}
	}
	// Shallow clone on CI: a pull request's merge commit (two parents) has the base
	// as its first parent. Any other HEAD^1 would only cover the last commit.
	if ci.PullRequest && repo.RefExists("HEAD^2") && repo.RefExists("HEAD^1") {
		return "HEAD^1"
	}
	return ""
}
