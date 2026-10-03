package draft

import (
	"fmt"

	"github.com/elecnix/gh-monitor/internal/ghcli"
	"github.com/elecnix/gh-monitor/internal/prlookup"
	"github.com/elecnix/gh-monitor/internal/resolver"
)

// pullLookup is the part of prlookup.Service this service depends on: the
// canonical repository coordinates and the pull request's node id. It is an
// interface so the service's GraphQL can be tested without the lookup.
type pullLookup interface {
	Repository(pr resolver.Identity) (prlookup.Repository, error)
	PullRequest(pr resolver.Identity) (prlookup.Ref, error)
}

// Service exposes pull request draft operations.
type Service struct {
	API ghcli.API

	pulls pullLookup
}

// NewService constructs a Service with the provided API client.
func NewService(api ghcli.API) *Service {
	return &Service{API: api, pulls: prlookup.NewService(api)}
}

// Draft marks a pull request as draft when permissions allow it.
func (s *Service) Draft(pr resolver.Identity, opts ActionOptions) (ActionResult, error) {
	return s.changeDraftState(pr, opts, true)
}

// Ready marks a pull request as ready for review when permissions allow it.
func (s *Service) Ready(pr resolver.Identity, opts ActionOptions) (ActionResult, error) {
	return s.changeDraftState(pr, opts, false)
}

// Status returns the current draft status of a pull request.
func (s *Service) Status(pr resolver.Identity, opts ActionOptions) (DraftInfo, error) {
	pull, err := s.lookup(pr, opts)
	if err != nil {
		return DraftInfo{}, err
	}

	return s.statusOf(pull)
}

// List returns all draft pull requests in the repository.
func (s *Service) List(pr resolver.Identity) ([]DraftInfo, error) {
	repo, err := s.pulls.Repository(pr)
	if err != nil {
		return nil, err
	}

	variables := map[string]interface{}{
		"owner": repo.Owner,
		"repo":  repo.Name,
	}

	var resp struct {
		Repository struct {
			PullRequests struct {
				Nodes []struct {
					Number  int    `json:"number"`
					Title   string `json:"title"`
					IsDraft bool   `json:"isDraft"`
				} `json:"nodes"`
			} `json:"pullRequests"`
		} `json:"repository"`
	}

	if err := s.API.GraphQL(draftListQuery, variables, &resp); err != nil {
		return nil, err
	}

	drafts := make([]DraftInfo, 0, len(resp.Repository.PullRequests.Nodes))
	for _, pr := range resp.Repository.PullRequests.Nodes {
		if pr.IsDraft {
			drafts = append(drafts, DraftInfo{
				PRNumber: pr.Number,
				IsDraft:  pr.IsDraft,
				Title:    pr.Title,
			})
		}
	}

	return drafts, nil
}

func (s *Service) changeDraftState(pr resolver.Identity, opts ActionOptions, makeDraft bool) (ActionResult, error) {
	pull, err := s.lookup(pr, opts)
	if err != nil {
		return ActionResult{}, err
	}

	// First check current status
	current, err := s.statusOf(pull)
	if err != nil {
		return ActionResult{}, err
	}

	// If already in desired state, return early
	if current.IsDraft == makeDraft {
		action := "ready for review"
		if makeDraft {
			action = "draft"
		}
		return ActionResult{
			PRNumber: current.PRNumber,
			IsDraft:  current.IsDraft,
			Status:   fmt.Sprintf("already %s", action),
		}, nil
	}

	// The node id comes from the lookup, so the mutation needs no further round trip.
	if makeDraft {
		return s.convertToDraft(pull.NodeID)
	}
	return s.markReadyForReview(pull.NodeID)
}

// lookup resolves the requested pull request to its canonical repository and
// node id, honouring an explicit PR number over the identity's.
func (s *Service) lookup(pr resolver.Identity, opts ActionOptions) (prlookup.Ref, error) {
	target := pr
	if opts.PRNumber != 0 {
		target.Number = opts.PRNumber
	}
	return s.pulls.PullRequest(target)
}

// statusOf reads the draft state from the canonical coordinates the lookup
// resolved, so a renamed repository reports its pull request rather than
// claiming it does not exist.
func (s *Service) statusOf(pull prlookup.Ref) (DraftInfo, error) {
	variables := map[string]interface{}{
		"owner":  pull.Owner,
		"repo":   pull.Name,
		"number": pull.Number,
	}

	var resp struct {
		Repository struct {
			PullRequest *struct {
				Number  int    `json:"number"`
				Title   string `json:"title"`
				IsDraft bool   `json:"isDraft"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}

	if err := s.API.GraphQL(pullRequestStatusQuery, variables, &resp); err != nil {
		return DraftInfo{}, err
	}

	if resp.Repository.PullRequest == nil {
		return DraftInfo{}, fmt.Errorf("pull request %d not found in %s/%s", pull.Number, pull.Owner, pull.Name)
	}

	return DraftInfo{
		PRNumber: resp.Repository.PullRequest.Number,
		IsDraft:  resp.Repository.PullRequest.IsDraft,
		Title:    resp.Repository.PullRequest.Title,
	}, nil
}

func (s *Service) convertToDraft(nodeID string) (ActionResult, error) {
	variables := map[string]interface{}{"pullRequestId": nodeID}
	var resp struct {
		ConvertPullRequestToDraft struct {
			PullRequest struct {
				Number  int  `json:"number"`
				IsDraft bool `json:"isDraft"`
			} `json:"pullRequest"`
		} `json:"convertPullRequestToDraft"`
	}

	if err := s.API.GraphQL(convertToDraftMutation, variables, &resp); err != nil {
		return ActionResult{}, err
	}

	return ActionResult{
		PRNumber: resp.ConvertPullRequestToDraft.PullRequest.Number,
		IsDraft:  resp.ConvertPullRequestToDraft.PullRequest.IsDraft,
		Status:   "marked as draft",
	}, nil
}

func (s *Service) markReadyForReview(nodeID string) (ActionResult, error) {
	variables := map[string]interface{}{"pullRequestId": nodeID}
	var resp struct {
		MarkPullRequestReadyForReview struct {
			PullRequest struct {
				Number  int  `json:"number"`
				IsDraft bool `json:"isDraft"`
			} `json:"pullRequest"`
		} `json:"markPullRequestReadyForReview"`
	}

	if err := s.API.GraphQL(markReadyForReviewMutation, variables, &resp); err != nil {
		return ActionResult{}, err
	}

	return ActionResult{
		PRNumber: resp.MarkPullRequestReadyForReview.PullRequest.Number,
		IsDraft:  resp.MarkPullRequestReadyForReview.PullRequest.IsDraft,
		Status:   "marked as ready for review",
	}, nil
}

const pullRequestStatusQuery = `
query PullRequestStatus($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      number
      title
      isDraft
    }
  }
}
`

const draftListQuery = `
query DraftList($owner: String!, $repo: String!) {
  repository(owner: $owner, name: $repo) {
    pullRequests(states: [OPEN], first: 100, orderBy: {field: CREATED_AT, direction: DESC}) {
      nodes {
        number
        title
        isDraft
      }
    }
  }
}
`

const convertToDraftMutation = `
mutation ConvertToDraft($pullRequestId: ID!) {
  convertPullRequestToDraft(input: {pullRequestId: $pullRequestId}) {
    pullRequest {
      number
      isDraft
    }
  }
}
`

const markReadyForReviewMutation = `
mutation MarkReadyForReview($pullRequestId: ID!) {
  markPullRequestReadyForReview(input: {pullRequestId: $pullRequestId}) {
    pullRequest {
      number
      isDraft
    }
  }
}
`
