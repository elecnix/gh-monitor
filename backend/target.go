// Package backend defines the pluggable surface behind `gh monitor`.
//
// A backend supplies one or both of two independent capabilities:
//
//   - a Source, which delivers Updates describing what changed on a Target.
//     The built-in Source polls the GitHub API; an external backend that can
//     be told about changes rather than discovering them may replace it.
//   - a Reader, which returns the current distilled Status of a Target. This
//     is the query surface behind `--once`.
//
// The two are separate on purpose. A backend registers only what it actually
// implements, for only the Target kinds it covers, and a Registry resolves
// each Target to the most specific registration available — so replacing
// notifications for pull requests does not disturb reads, nor the kinds the
// backend left alone.
//
// This package is deliberately free of GitHub API types so an out-of-tree Go
// module can implement a backend against it. See docs/BACKENDS.md.
package backend

import (
	"fmt"
	"strings"
)

// Kind identifies what sort of thing a Target names. It is the unit of
// partial registration: a backend declares the kinds it covers, and the
// Registry leaves every other kind to the backends that do cover it.
type Kind string

const (
	// KindPR is a pull request.
	KindPR Kind = "pr"
	// KindIssue is an issue.
	KindIssue Kind = "issue"
	// KindRun is a single GitHub Actions workflow run.
	KindRun Kind = "run"
	// KindRef is a branch or ref, watched for CI outcomes only.
	KindRef Kind = "ref"
	// KindCommit is a single commit SHA, watched for CI outcomes only.
	KindCommit Kind = "commit"
	// KindRepo is a whole repository, watched for new pull requests and issues.
	KindRepo Kind = "repo"
)

// AllKinds returns every kind in canonical order. The order is stable so
// `gh monitor backends` and error messages read the same way every time.
func AllKinds() []Kind {
	return []Kind{KindPR, KindIssue, KindRun, KindRef, KindCommit, KindRepo}
}

// ParseKind converts a caller-supplied string into a Kind, rejecting anything
// unrecognised so a typo fails loudly instead of silently matching nothing.
func ParseKind(s string) (Kind, error) {
	norm := Kind(strings.ToLower(strings.TrimSpace(s)))
	for _, k := range AllKinds() {
		if k == norm {
			return k, nil
		}
	}
	return "", fmt.Errorf("unknown target kind %q (expected one of %s)", s, joinKinds(AllKinds()))
}

func joinKinds(kinds []Kind) string {
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		parts = append(parts, string(k))
	}
	return strings.Join(parts, ", ")
}

// Target names the thing being watched or read. Only the fields relevant to
// Kind are populated.
type Target struct {
	Kind Kind `json:"kind"`
	// Host is the GitHub hostname; empty means github.com.
	Host  string `json:"host,omitempty"`
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	// Number is the pull request or issue number.
	Number int `json:"number,omitempty"`
	// Ref is the branch name for KindRef.
	Ref string `json:"ref,omitempty"`
	// SHA is the commit for KindCommit.
	SHA string `json:"sha,omitempty"`
	// RunID is the workflow run id for KindRun.
	RunID int `json:"run_id,omitempty"`
}

// Hostname returns the target's host, defaulting to github.com.
func (t Target) Hostname() string {
	if t.Host == "" {
		return "github.com"
	}
	return t.Host
}

// String renders the target the way notifications label it.
func (t Target) String() string {
	repo := t.Owner + "/" + t.Repo
	switch t.Kind {
	case KindRef:
		return repo + "@" + t.Ref
	case KindCommit:
		sha := t.SHA
		if len(sha) > 7 {
			sha = sha[:7]
		}
		return repo + "@" + sha
	case KindRun:
		return fmt.Sprintf("%s run #%d", repo, t.RunID)
	case KindRepo:
		return repo
	default:
		return fmt.Sprintf("%s#%d", repo, t.Number)
	}
}
