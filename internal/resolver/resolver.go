package resolver

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var (
	pullURLRE = regexp.MustCompile(`^/([^/]+)/([^/]+)/pull/([0-9]+)(?:/.*)?$`)
	// shorthandRE matches the OWNER/REPO#NUMBER cross-reference form — the
	// shape GitHub's own UI, its cross-references, and this tool's pr_label
	// all use to name a pull request. Neither an owner nor a repository name
	// can contain '#', so the separator is unambiguous. The character classes
	// are GitHub's own rules for the two names, so a typo is rejected here
	// instead of becoming a 404 from the API.
	shorthandRE = regexp.MustCompile(`^([A-Za-z0-9-]+)/([A-Za-z0-9._-]+)#([0-9]+)$`)
)

// selectorHint tells the reader which selector forms are accepted, and where
// the host comes from — the shorthand names a repository, not a host.
const selectorHint = "expected owner/repo#number, a number with -R owner/repo, or a pull request URL; the host comes from GH_HOST"

// Identity represents a fully-resolved reference target.
// Target is one of: "pr", "ref", "commit", "issue".
type Identity struct {
	Owner     string
	Repo      string
	Host      string
	Number    int    // PR number or issue number
	Target    string // "pr" | "ref" | "commit" | "issue" | "run"
	Ref       string // branch ref for "ref" target
	CommitSHA string // commit SHA for "commit" target
	RunID     int    // workflow run id for "run" target
}

// NormalizeSelector ensures that either an explicit selector or --pr flag is present and mutually consistent.
func NormalizeSelector(selector string, prFlag int) (string, error) {
	selector = strings.TrimSpace(selector)

	switch {
	case selector != "" && prFlag > 0:
		if !matchesNumber(selector, prFlag) {
			return "", fmt.Errorf("pull request argument %q does not match --pr=%d", selector, prFlag)
		}
	case selector == "" && prFlag > 0:
		selector = strconv.Itoa(prFlag)
	}

	if selector == "" {
		return "", errors.New("must specify a pull request via --pr or selector")
	}

	if isNumeric(selector) {
		return selector, nil
	}

	if _, err := parsePullURL(selector); err == nil {
		return selector, nil
	}

	if _, err := parseShorthand(selector); err == nil {
		return selector, nil
	}

	return "", fmt.Errorf("invalid pull request selector %q: %s", selector, selectorHint)
}

// Resolve interprets a selector, optional repo flag, and host (GH_HOST) into a concrete pull request identity.
func Resolve(selector, repoFlag, host string) (Identity, error) {
	selector = strings.TrimSpace(selector)
	repoFlag = strings.TrimSpace(repoFlag)
	host = SanitizeHost(host)

	if selector == "" {
		return Identity{}, errors.New("empty selector")
	}

	if id, err := parsePullURL(selector); err == nil {
		id.Target = "pr"
		return id, nil
	}

	// The OWNER/REPO#NUMBER shorthand names its own repository, so it wins
	// over --repo exactly the way a pull request URL's repository does: a
	// selector that carries the identity is not second-guessed by a flag
	// that only fills in a missing part.
	if id, err := parseShorthand(selector); err == nil {
		id.Host = host
		id.Target = "pr"
		return id, nil
	}

	if n, err := strconv.Atoi(selector); err == nil && n > 0 {
		owner, repo, err := splitRepo(repoFlag)
		if err != nil {
			return Identity{}, fmt.Errorf("--repo: %w", err)
		}
		return Identity{Owner: owner, Repo: repo, Host: host, Number: n, Target: "pr"}, nil
	}

	return Identity{}, fmt.Errorf("invalid pull request selector %q: %s", selector, selectorHint)
}

// ResolveRef resolves a ref (branch) target for monitoring.
func ResolveRef(ref, repoFlag, host string) (Identity, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Identity{}, errors.New("ref must be non-empty")
	}
	owner, repo, err := splitRepo(repoFlag)
	if err != nil {
		return Identity{}, fmt.Errorf("--repo: %w", err)
	}
	return Identity{Owner: owner, Repo: repo, Host: SanitizeHost(host), Ref: ref, Target: "ref"}, nil
}

// ResolveCommit resolves a commit SHA target for monitoring.
func ResolveCommit(sha, repoFlag, host string) (Identity, error) {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return Identity{}, errors.New("commit SHA must be non-empty")
	}
	owner, repo, err := splitRepo(repoFlag)
	if err != nil {
		return Identity{}, fmt.Errorf("--repo: %w", err)
	}
	return Identity{Owner: owner, Repo: repo, Host: SanitizeHost(host), CommitSHA: sha, Target: "commit"}, nil
}

// ResolveRun resolves a GitHub Actions workflow-run target for monitoring.
// The run id is the numeric id from a run's URL (e.g. .../actions/runs/<id>).
func ResolveRun(runID int, repoFlag, host string) (Identity, error) {
	if runID <= 0 {
		return Identity{}, errors.New("run id must be positive")
	}
	owner, repo, err := splitRepo(repoFlag)
	if err != nil {
		return Identity{}, fmt.Errorf("--repo: %w", err)
	}
	return Identity{Owner: owner, Repo: repo, Host: SanitizeHost(host), RunID: runID, Target: "run"}, nil
}

// ResolveRepo resolves a repository target for monitoring (watches for new PRs
// and issues).
func ResolveRepo(repoFlag, host string) (Identity, error) {
	owner, repo, err := splitRepo(repoFlag)
	if err != nil {
		return Identity{}, fmt.Errorf("--repo: %w", err)
	}
	return Identity{Owner: owner, Repo: repo, Host: SanitizeHost(host), Target: "repo"}, nil
}

// ResolveIssue resolves an issue target for monitoring.
func ResolveIssue(number int, repoFlag, host string) (Identity, error) {
	if number <= 0 {
		return Identity{}, errors.New("issue number must be positive")
	}
	owner, repo, err := splitRepo(repoFlag)
	if err != nil {
		return Identity{}, fmt.Errorf("--repo: %w", err)
	}
	return Identity{Owner: owner, Repo: repo, Host: SanitizeHost(host), Number: number, Target: "issue"}, nil
}

func parsePullURL(raw string) (Identity, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Identity{}, err
	}
	if u.Host == "" {
		return Identity{}, errors.New("missing host")
	}
	matches := pullURLRE.FindStringSubmatch(u.Path)
	if matches == nil {
		return Identity{}, errors.New("not a pull request url")
	}
	number, _ := strconv.Atoi(matches[3])
	return Identity{
		Owner:  matches[1],
		Repo:   matches[2],
		Host:   SanitizeHost(u.Host),
		Number: number,
	}, nil
}

// parseShorthand parses the OWNER/REPO#NUMBER cross-reference form. The host
// is not part of it: the shorthand names a repository on the host the
// caller is already talking to (GH_HOST, github.com by default).
func parseShorthand(raw string) (Identity, error) {
	matches := shorthandRE.FindStringSubmatch(raw)
	if matches == nil {
		return Identity{}, errors.New("not an owner/repo#number shorthand")
	}
	number, err := strconv.Atoi(matches[3])
	if err != nil {
		return Identity{}, fmt.Errorf("invalid pull request number %q", matches[3])
	}
	if number <= 0 {
		return Identity{}, errors.New("pull request number must be positive")
	}
	return Identity{
		Owner:  matches[1],
		Repo:   matches[2],
		Number: number,
	}, nil
}

func matchesNumber(selector string, target int) bool {
	if id, err := parsePullURL(selector); err == nil {
		return id.Number == target
	}
	if id, err := parseShorthand(selector); err == nil {
		return id.Number == target
	}
	if n, err := strconv.Atoi(selector); err == nil {
		return n == target
	}
	return false
}

func isNumeric(selector string) bool {
	if selector == "" {
		return false
	}
	for _, r := range selector {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func splitRepo(repoFlag string) (string, string, error) {
	if repoFlag == "" {
		return "", "", errors.New("no repository specified: use --repo owner/repo, or run from a git repository to infer automatically")
	}
	parts := strings.Split(repoFlag, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", errors.New("expected owner/repo")
	}
	return parts[0], parts[1], nil
}

// SanitizeHost normalizes a host string by stripping scheme, path, and port.
// Returns "github.com" if the result is empty.
func SanitizeHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "github.com"
	}

	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			raw = u.Host
		} else {
			raw = strings.TrimPrefix(strings.TrimPrefix(lower, "http://"), "https://")
		}
	}

	if strings.Contains(raw, "/") {
		raw = strings.SplitN(raw, "/", 2)[0]
	}

	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	} else if idx := strings.Index(raw, ":"); idx >= 0 {
		raw = raw[:idx]
	}

	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "github.com"
	}

	return strings.ToLower(raw)
}
