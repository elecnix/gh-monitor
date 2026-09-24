package ghcli

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// includedGraphQL is what `gh api --include graphql` prints: the status line
// ends in \n, header lines end in \r\n, and a blank line separates the body.
const includedGraphQL = "HTTP/2.0 200 OK\n" +
	"Content-Type: application/json; charset=utf-8\r\n" +
	"X-Ratelimit-Limit: 5000\r\n" +
	"X-Ratelimit-Remaining: 0\r\n" +
	"X-Ratelimit-Reset: 1790259267\r\n" +
	"X-Ratelimit-Resource: graphql\r\n" +
	"X-Ratelimit-Used: 5000\r\n" +
	"\r\n" +
	`{"data":{"viewer":{"login":"octocat"}}}`

func TestSplitIncluded_SeparatesHeadersFromBody(t *testing.T) {
	headers, body := splitIncluded([]byte(includedGraphQL))
	assert.Equal(t, `{"data":{"viewer":{"login":"octocat"}}}`, string(body))
	assert.Equal(t, "graphql", headers.Get("X-RateLimit-Resource"))
	assert.Equal(t, "0", headers.Get("X-RateLimit-Remaining"))
}

func TestSplitIncluded_PassesPlainOutputThrough(t *testing.T) {
	headers, body := splitIncluded([]byte(`{"a":1}`))
	assert.Empty(t, headers)
	assert.Equal(t, `{"a":1}`, string(body))
}

func TestRateLimitStore_RecordsReadingPerHostAndResource(t *testing.T) {
	store := NewRateLimitStore()
	now := time.Unix(1790258000, 0)
	headers, _ := splitIncluded([]byte(includedGraphQL))
	store.Observe("", headers, now)

	r, ok := store.Latest("github.com", "graphql")
	require.True(t, ok, "an empty host is github.com")
	assert.Equal(t, 5000, r.Limit)
	assert.Equal(t, 0, r.Remaining)
	assert.Equal(t, 5000, r.Used)
	assert.Equal(t, time.Unix(1790259267, 0), r.Reset)
	assert.Equal(t, now, r.ObservedAt)

	_, ok = store.Latest("github.com", "core")
	assert.False(t, ok, "a graphql response says nothing about core")
	_, ok = store.Latest("ghe.example.com", "graphql")
	assert.False(t, ok, "readings from one host never answer for another")
}

func TestRateLimitStore_IgnoresResponsesWithoutRateLimitHeaders(t *testing.T) {
	store := NewRateLimitStore()
	headers, _ := splitIncluded([]byte("HTTP/2.0 200 OK\nContent-Type: text/plain\r\n\r\nok"))
	store.Observe("", headers, time.Now())
	_, ok := store.Latest("github.com", "core")
	assert.False(t, ok)
}

// TestClient_RecordsHeadersOfRealCalls checks that the client asks gh for the
// response headers and records the rate-limit reading, on success and on a
// failed call alike (a 403 still carries the headers).
func TestClient_RecordsHeadersOfRealCalls(t *testing.T) {
	store := NewRateLimitStore()
	var gotArgs []string
	orig := runGh
	t.Cleanup(func() { runGh = orig })
	runGh = func(args []string, _ []byte) ([]byte, string, error) {
		gotArgs = args
		out := "HTTP/2.0 403 Forbidden\n" +
			"X-Ratelimit-Limit: 5000\r\nX-Ratelimit-Remaining: 0\r\nX-Ratelimit-Used: 5000\r\n" +
			"X-Ratelimit-Reset: 1790259267\r\nX-Ratelimit-Resource: graphql\r\n\r\n" +
			`{"message":"API rate limit exceeded","documentation_url":"https://docs.github.com/rest","status":"403"}`
		return []byte(out), "gh: API rate limit exceeded (HTTP 403)", errors.New("exit status 1")
	}

	c := &Client{Limits: store}
	var result map[string]any
	err := c.GraphQL("{viewer{login}}", nil, &result)
	require.Error(t, err)
	assert.Contains(t, gotArgs, "--include")
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.NotContains(t, apiErr.Body, "X-Ratelimit", "the error body must not carry the headers")

	r, ok := store.Latest("github.com", "graphql")
	require.True(t, ok)
	assert.Equal(t, 0, r.Remaining)
}
