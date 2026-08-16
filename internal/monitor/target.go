package monitor

import (
	"encoding/json"

	"github.com/elecnix/gh-monitor/backend"
	"github.com/elecnix/gh-monitor/internal/resolver"
)

// TargetOf converts a resolved identity into the backend-facing Target. An
// empty Identity.Target means a pull request: resolver.Resolve leaves it
// empty when it parses a pull request URL, and every caller already reads it
// that way.
func TargetOf(id resolver.Identity) backend.Target {
	kind := backend.Kind(id.Target)
	if id.Target == "" {
		kind = backend.KindPR
	}
	return backend.Target{
		Kind:   kind,
		Host:   id.Host,
		Owner:  id.Owner,
		Repo:   id.Repo,
		Number: id.Number,
		Ref:    id.Ref,
		SHA:    id.CommitSHA,
		RunID:  id.RunID,
	}
}

// IdentityOf converts a Target back into a resolver.Identity, for the parts
// of the engine that still take one.
func IdentityOf(t backend.Target) resolver.Identity {
	return resolver.Identity{
		Owner:     t.Owner,
		Repo:      t.Repo,
		Host:      t.Host,
		Number:    t.Number,
		Target:    string(t.Kind),
		Ref:       t.Ref,
		CommitSHA: t.SHA,
		RunID:     t.RunID,
	}
}

// ---------------------------------------------------------------------------
// backend.Status implementations
// ---------------------------------------------------------------------------

// TargetKind reports which kind of target this status describes.
func (s *PRStatus) TargetKind() backend.Kind { return backend.KindPR }

// TargetKind reports which kind of target this status describes.
func (s *IssueStatus) TargetKind() backend.Kind { return backend.KindIssue }

// TargetKind reports which kind of target this status describes.
func (s *RunStatus) TargetKind() backend.Kind { return backend.KindRun }

// TargetKind reports which kind of target this status describes. RefStatus
// backs both ref and commit targets — they differ in how the commit is named,
// not in what is observed about it.
func (s *RefStatus) TargetKind() backend.Kind { return backend.KindRef }

// TargetKind reports which kind of target this status describes.
func (s *RepoStatus) TargetKind() backend.Kind { return backend.KindRepo }

// decodeStatus builds a decoder for a concrete status type.
func decodeStatus[T any, PT interface {
	*T
	backend.Status
}]() func([]byte) (backend.Status, error) {
	return func(raw []byte) (backend.Status, error) {
		var v T
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		return PT(&v), nil
	}
}

func init() {
	// Register the built-in status types so an Update that crossed a process
	// boundary arrives with a typed Status rather than opaque bytes.
	backend.RegisterStatusDecoder(backend.KindPR, decodeStatus[PRStatus]())
	backend.RegisterStatusDecoder(backend.KindIssue, decodeStatus[IssueStatus]())
	backend.RegisterStatusDecoder(backend.KindRun, decodeStatus[RunStatus]())
	backend.RegisterStatusDecoder(backend.KindRef, decodeStatus[RefStatus]())
	backend.RegisterStatusDecoder(backend.KindCommit, decodeStatus[RefStatus]())
	backend.RegisterStatusDecoder(backend.KindRepo, decodeStatus[RepoStatus]())
}
