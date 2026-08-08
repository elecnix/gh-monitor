package ghcli

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsGitHubErrorBody(t *testing.T) {
	t.Run("rate limit body is detected", func(t *testing.T) {
		body := []byte(`{"message":"API rate limit exceeded for user ID 17873.","documentation_url":"https://docs.github.com/rest/overview/resources-in-the-rest-api#rate-limiting","status":"403"}`)
		eb, ok := IsGitHubErrorBody(body)
		assert.True(t, ok)
		assert.Contains(t, eb.Message, "API rate limit exceeded")
		assert.Equal(t, "403", eb.Status)
	})

	t.Run("403 error body is detected", func(t *testing.T) {
		body := []byte(`{"message":"Resource not accessible by integration","documentation_url":"https://docs.github.com/rest","status":"403"}`)
		eb, ok := IsGitHubErrorBody(body)
		assert.True(t, ok)
		assert.Contains(t, eb.Message, "not accessible")
	})

	t.Run("404 body without documentation_url still detected", func(t *testing.T) {
		body := []byte(`{"message":"Not Found","status":"404"}`)
		eb, ok := IsGitHubErrorBody(body)
		assert.True(t, ok)
		assert.Equal(t, "Not Found", eb.Message)
		assert.Equal(t, "404", eb.Status)
	})

	t.Run("valid GraphQL response is not detected", func(t *testing.T) {
		body := []byte(`{"data":{"repository":{"pullRequest":{"state":"OPEN"}}}}`)
		_, ok := IsGitHubErrorBody(body)
		assert.False(t, ok)
	})

	t.Run("valid REST array response is not detected", func(t *testing.T) {
		body := []byte(`[{"id":1,"name":"ruleset"}]`)
		_, ok := IsGitHubErrorBody(body)
		assert.False(t, ok)
	})

	t.Run("valid REST object with message key is not detected", func(t *testing.T) {
		// A legitimate response with a "message" field but no "status" or
		// "documentation_url" should not be flagged as an error.
		body := []byte(`{"message":"some info","other_field":"value"}`)
		_, ok := IsGitHubErrorBody(body)
		assert.False(t, ok)
	})

	t.Run("empty body is not detected", func(t *testing.T) {
		_, ok := IsGitHubErrorBody([]byte(``))
		assert.False(t, ok)
	})

	t.Run("non-JSON body is not detected", func(t *testing.T) {
		_, ok := IsGitHubErrorBody([]byte(`not json`))
		assert.False(t, ok)
	})
}
