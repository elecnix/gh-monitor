// Package prlookup resolves the identity every capability needs before it can
// act on a pull request: the canonical owner/repository, the GraphQL node id
// and the head SHA.
//
// The repository step is not optional decoration. GitHub's REST API answers a
// renamed owner/repository with a redirect and reports the current full_name,
// while GraphQL's `repository(owner:,name:)` resolves the coordinates as
// given. A command that skips the repository step therefore works on a renamed
// repository only by accident of which API it happens to use. This package
// performs the canonicalisation once, for every caller.
package prlookup

import (
	"fmt"
	"strings"

	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/elecnix/gh-monitor/internal/resolver"
)

// Repository names a repository on a host, following a rename or transfer when
// the host reports one.
type Repository struct {
	Host  string
	Owner string
	Name  string
}

// Identity converts the repository to the identity the command services take.
func (r Repository) Identity() resolver.Identity {
	return resolver.Identity{Owner: r.Owner, Repo: r.Name, Host: r.Host}
}

// Ref is a resolved pull request: the canonical repository it lives in, the
// number it was asked for, and the two identifiers the API hands out under two
// different names depending on which endpoint asked for them.
type Ref struct {
	Repository
	Number  int
	NodeID  string
	HeadSHA string
}

// Identity converts the pull request to the identity the command services take.
func (r Ref) Identity() resolver.Identity {
	identity := r.Repository.Identity()
	identity.Number = r.Number
	identity.Target = "pr"
	return identity
}

// Service resolves repositories and pull requests through the gh CLI.
type Service struct {
	api ghcli.API
}

// NewService constructs a Service backed by api.
func NewService(api ghcli.API) *Service {
	return &Service{api: api}
}

// Repository resolves the canonical owner and name of the repository pr names.
// The host reports the current full_name, which differs from the requested one
// after a rename or transfer; a full_name that is absent or not exactly
// `owner/name` leaves the requested coordinates untouched.
func (s *Service) Repository(pr resolver.Identity) (Repository, error) {
	var repo struct {
		FullName string `json:"full_name"`
	}
	path := fmt.Sprintf("repos/%s/%s", pr.Owner, pr.Repo)
	if err := s.api.REST("GET", path, nil, nil, &repo); err != nil {
		return Repository{}, fmt.Errorf("repository %s/%s not found on %s: %w", pr.Owner, pr.Repo, pr.Host, err)
	}

	canonical := Repository{Host: pr.Host, Owner: pr.Owner, Name: pr.Repo}
	if owner, name, ok := strings.Cut(repo.FullName, "/"); ok && owner != "" && name != "" && !strings.Contains(name, "/") {
		canonical.Owner = owner
		canonical.Name = name
	}

	return canonical, nil
}

// PullRequest resolves the pull request pr names to its canonical repository,
// node id and head SHA.
//
// Both identifiers are required. A response missing either means the client
// and the API disagree about what a pull request is, and passing a half-filled
// identifier on to a mutation turns that disagreement into a write against the
// wrong node.
func (s *Service) PullRequest(pr resolver.Identity) (Ref, error) {
	repo, err := s.Repository(pr)
	if err != nil {
		return Ref{}, err
	}

	var pull struct {
		NodeID string `json:"node_id"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	path := fmt.Sprintf("repos/%s/%s/pulls/%d", repo.Owner, repo.Name, pr.Number)
	if err := s.api.REST("GET", path, nil, nil, &pull); err != nil {
		return Ref{}, fmt.Errorf("pull request %d not found in %s/%s: %w", pr.Number, repo.Owner, repo.Name, err)
	}

	ref := Ref{
		Repository: repo,
		Number:     pr.Number,
		NodeID:     strings.TrimSpace(pull.NodeID),
		HeadSHA:    strings.TrimSpace(pull.Head.SHA),
	}
	if ref.NodeID == "" || ref.HeadSHA == "" {
		return Ref{}, fmt.Errorf("pull request %d missing node identifier or head sha in %s/%s", pr.Number, repo.Owner, repo.Name)
	}

	return ref, nil
}
