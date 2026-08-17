package ghcli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/elecnix/gh-monitor/backend"
)

// Client executes GitHub API requests through the `gh` CLI to reuse
// the authenticated context and host configuration provided by the user.
type Client struct {
	Host string
}

// API defines the subset of GitHub API interactions required by the command logic.
type API interface {
	REST(method, path string, params map[string]string, body interface{}, result interface{}) error
	GraphQL(query string, variables map[string]interface{}, result interface{}) error
}

// GitHubErrorBody is a parsed GitHub REST API error response — a valid JSON body
// whose top-level shape matches {"message":"...","documentation_url":"..."}.
// These are the bodies returned by non-2xx GitHub responses (403 rate limit,
// 404 not found, 422 validation for private repos, etc.). They unmarshal
// silently into any target struct, including GraphQL envelope wrappers — the
// fields do not collide and json.Unmarshal ignores unknowns.
type GitHubErrorBody struct {
	Message string `json:"message"`
	Status  string `json:"status"`
}

// GitHubRateLimitBody is a parsed rate-limit error response.
type GitHubRateLimitBody struct {
	Message          string `json:"message"`
	DocumentationURL string `json:"documentation_url"`
	Status           string `json:"status"`
}

// IsGitHubErrorBody reports whether body is a GitHub REST API error document.
func IsGitHubErrorBody(body []byte) (*GitHubErrorBody, bool) {
	var eb GitHubErrorBody
	if err := json.Unmarshal(body, &eb); err != nil {
		return nil, false
	}
	if eb.Message != "" {
		// A status field like "403" strengthens the match to avoid false
		// positives on legitimate payloads that happen to have a "message" key.
		if eb.Status != "" {
			return &eb, true
		}
		// Rate-limit bodies specifically carry documentation_url.
		var rlb GitHubRateLimitBody
		if err := json.Unmarshal(body, &rlb); err == nil && rlb.DocumentationURL != "" {
			return &GitHubErrorBody{Message: rlb.Message, Status: rlb.Status}, true
		}
	}
	return nil, false
}

// GraphQLErrorEntry captures a single GraphQL error payload. It is the
// backend-facing APIErrorEntry: a review submission reports these to callers,
// so the type has to be one an out-of-tree backend can construct.
type GraphQLErrorEntry = backend.APIErrorEntry

// GraphQLError represents GraphQL-level errors returned alongside a response.
type GraphQLError struct {
	Errors []GraphQLErrorEntry
}

func (e *GraphQLError) Error() string {
	if len(e.Errors) == 0 {
		return "graphql returned errors"
	}
	if len(e.Errors) == 1 {
		return fmt.Sprintf("graphql error: %s", e.Errors[0].Message)
	}
	parts := make([]string, 0, len(e.Errors))
	for _, err := range e.Errors {
		parts = append(parts, err.Message)
	}
	return fmt.Sprintf("graphql errors: %s", strings.Join(parts, "; "))
}

// APIError wraps errors returned by the `gh api` command, exposing the HTTP status code when detected.
type APIError struct {
	StatusCode int
	Message    string
	Stderr     string
	Body       string
	Err        error
}

func (e *APIError) Error() string {
	if e.StatusCode > 0 {
		return fmt.Sprintf("gh api error (status %d): %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("gh api error: %s", e.Message)
}

func (e *APIError) Unwrap() error {
	return e.Err
}

// ContainsLower reports whether any captured message fields contain the target substring (case-insensitive).
func (e *APIError) ContainsLower(target string) bool {
	if target == "" {
		return false
	}
	needle := strings.ToLower(target)
	if strings.Contains(strings.ToLower(e.Message), needle) {
		return true
	}
	if strings.Contains(strings.ToLower(e.Body), needle) {
		return true
	}
	if strings.Contains(strings.ToLower(e.Stderr), needle) {
		return true
	}
	return false
}

var statusRE = regexp.MustCompile(`HTTP\s+(\d{3})\b`)

func wrapError(err error, stdout []byte, stderr string) error {
	message := strings.TrimSpace(stderr)
	if message == "" {
		message = err.Error()
	}

	apiErr := &APIError{Message: message, Stderr: stderr, Err: err}
	if len(stdout) > 0 {
		apiErr.Body = strings.TrimSpace(string(stdout))
		if apiErr.Message == "" {
			apiErr.Message = apiErr.Body
		}
	}
	if matches := statusRE.FindStringSubmatch(stderr); len(matches) == 2 {
		if code, convErr := strconv.Atoi(matches[1]); convErr == nil {
			apiErr.StatusCode = code
		}
	}
	return apiErr
}

// REST invokes the REST API using `gh api`.
// The result parameter must be a pointer and will be unmarshaled from JSON.
func (c *Client) REST(method, path string, params map[string]string, body interface{}, result interface{}) error {
	args := []string{"api"}
	if host := strings.TrimSpace(c.Host); host != "" {
		args = append(args, "--hostname", host)
	}

	args = append(args, "--header", "X-GitHub-Api-Version: 2022-11-28")
	args = append(args, path, "-X", method)

	for key, value := range params {
		args = append(args, "-f", fmt.Sprintf("%s=%s", key, value))
	}

	var stdinData []byte
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		stdinData = data
		args = append(args, "--input", "-")
	}

	stdout, stderr, err := runGh(args, stdinData)
	if err != nil {
		return wrapError(err, stdout, stderr)
	}

	if result == nil {
		return nil
	}

	// Detect GitHub error bodies (rate limit, 403, etc.) that are valid JSON but
	// not the expected data shape. These silently unmarshal into any target struct
	// because json.Unmarshal ignores unknown fields — the caller gets a zero-value
	// struct and may interpret it as valid (empty) data rather than as a failure.
	if errBody, ok := IsGitHubErrorBody(stdout); ok {
		return &APIError{
			StatusCode: 403, // best-effort guess; Status may be "403" or empty
			Message:    errBody.Message,
			Body:       strings.TrimSpace(string(stdout)),
			Stderr:     stderr,
		}
	}

	if result != nil {
		if err := json.Unmarshal(stdout, result); err != nil {
			return fmt.Errorf("unmarshal response: %w", err)
		}
	}

	return nil
}

// GraphQL issues a GraphQL operation through `gh api graphql`.
func (c *Client) GraphQL(query string, variables map[string]interface{}, result interface{}) error {
	payload := map[string]interface{}{
		"query": query,
	}
	if len(variables) > 0 {
		payload["variables"] = variables
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal graphql payload: %w", err)
	}

	args := []string{"api", "graphql"}
	if host := strings.TrimSpace(c.Host); host != "" {
		args = append(args, "--hostname", host)
	}
	args = append(args, "--input", "-")

	stdout, stderr, err := runGh(args, data)
	if err != nil {
		return wrapError(err, stdout, stderr)
	}

	if result == nil {
		return nil
	}

	// Before parsing as a GraphQL envelope, check for a REST-style error body.
	// When the GraphQL endpoint is rate-limited, GitHub returns a REST error
	// body that json.Unmarshal into the envelope struct would treat as valid
	// (both Data and Errors nil/omitted).
	if errBody, ok := IsGitHubErrorBody(stdout); ok {
		return &APIError{
			StatusCode: 403,
			Message:    errBody.Message,
			Body:       strings.TrimSpace(string(stdout)),
			Stderr:     stderr,
		}
	}

	var envelope struct {
		Data   json.RawMessage   `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(stdout, &envelope); err != nil {
		return fmt.Errorf("unmarshal graphql response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		errs := make([]GraphQLErrorEntry, 0, len(envelope.Errors))
		for _, raw := range envelope.Errors {
			var entry GraphQLErrorEntry
			if err := json.Unmarshal(raw, &entry); err != nil {
				entry.Message = strings.TrimSpace(string(raw))
			}
			errs = append(errs, entry)
		}
		return &GraphQLError{Errors: errs}
	}

	if len(envelope.Data) > 0 && result != nil {
		if err := json.Unmarshal(envelope.Data, result); err != nil {
			return fmt.Errorf("unmarshal graphql data: %w", err)
		}
	}

	if len(envelope.Data) == 0 && result != nil {
		return json.Unmarshal(stdout, result)
	}

	return nil
}

// CurrentRepo returns the "owner/repo" for the current working directory
// by delegating to `gh repo view`. This respects `gh repo set-default`
// and the git remote configuration.
func CurrentRepo() (string, error) {
	stdout, stderr, err := runGh([]string{"repo", "view", "--json", "nameWithOwner", "-q", ".nameWithOwner"}, nil)
	if err != nil {
		return "", wrapError(err, stdout, stderr)
	}
	result := strings.TrimSpace(string(stdout))
	if result == "" {
		return "", fmt.Errorf("gh repo view returned empty nameWithOwner")
	}
	return result, nil
}

// CurrentPR returns the pull request number for the current branch
// by delegating to `gh pr view`. Returns 0 and an error if the current
// branch has no associated pull request.
func CurrentPR() (int, error) {
	stdout, stderr, err := runGh([]string{"pr", "view", "--json", "number", "-q", ".number"}, nil)
	if err != nil {
		return 0, wrapError(err, stdout, stderr)
	}
	result := strings.TrimSpace(string(stdout))
	if result == "" {
		return 0, fmt.Errorf("gh pr view returned empty number")
	}
	n, err := strconv.Atoi(result)
	if err != nil {
		return 0, fmt.Errorf("gh pr view returned non-numeric number: %q", result)
	}
	return n, nil
}

// CurrentUser returns the authenticated user's login by delegating to
// `gh api user -q '.login'`.
func CurrentUser() (string, error) {
	stdout, stderr, err := runGh([]string{"api", "user", "-q", ".login"}, nil)
	if err != nil {
		return "", wrapError(err, stdout, stderr)
	}
	result := strings.TrimSpace(string(stdout))
	if result == "" {
		return "", fmt.Errorf("gh api user returned empty login")
	}
	return result, nil
}

// FailedRunLogs returns the failed-job log output for a workflow run by
// delegating to `gh run view <runID> --log-failed`. The output combines each
// failing job's name with its error log lines, so a consumer can diagnose a
// failed run without an extra API call. Returns an empty string when there is
// no failed output (e.g. the run succeeded).
//
// The repository is passed as `[HOST/]OWNER/REPO` so enterprise hosts are
// honored; github.com is left bare to match `gh`'s default resolution.
func (c *Client) FailedRunLogs(owner, repo string, runID int) (string, error) {
	repoArg := owner + "/" + repo
	if host := strings.TrimSpace(c.Host); host != "" && host != "github.com" {
		repoArg = host + "/" + repoArg
	}
	args := []string{"run", "view", strconv.Itoa(runID), "--repo", repoArg, "--log-failed"}
	stdout, stderr, err := runGh(args, nil)
	if err != nil {
		return "", wrapError(err, stdout, stderr)
	}
	return string(stdout), nil
}

// RunGh executes the `gh` CLI command with provided arguments and optional stdin data.
// Exported for use by tests and other packages.
func RunGh(args []string, stdin []byte) ([]byte, string, error) {
	return runGh(args, stdin)
}

// runGh executes the `gh` CLI command with provided arguments and optional stdin data.
func runGh(args []string, stdin []byte) ([]byte, string, error) {
	cmd := exec.Command("gh", args...)
	// DEBUG LOG
	// fmt.Fprintf(os.Stderr, "running gh %s\n", strings.Join(args, " "))
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return stdout.Bytes(), stderr.String(), err
	}

	return stdout.Bytes(), stderr.String(), nil
}
