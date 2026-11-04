package broker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noAWS removes every credential source the AWS SDK would find, so a test
// proves a code path never asked for one.
func noAWS(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AWS_PROFILE", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/nonexistent")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

// A broker that is not IoT Core (a local relay) is named by URL. The daemon
// connects to it unsigned: it needs no AWS credentials, so none has to sit in
// the environment that every sub-daemon child inherits.
func TestConnectionURL_SchemeEndpointIsUnsignedAndNeedsNoCredentials(t *testing.T) {
	noAWS(t)
	for _, tc := range []struct{ endpoint, want string }{
		{"ws://127.0.0.1:9001", "ws://127.0.0.1:9001/mqtt"},
		{"wss://127.0.0.1:9001", "wss://127.0.0.1:9001/mqtt"},
		{"ws://127.0.0.1:9001/custom", "ws://127.0.0.1:9001/custom"},
	} {
		got, err := connectionURL(context.Background(), Config{Endpoint: tc.endpoint, Region: "us-east-1"})
		require.NoError(t, err, tc.endpoint)
		assert.Equal(t, tc.want, got, tc.endpoint)
	}
}

// A bare host name keeps meaning IoT Core: SigV4-presigned, over wss.
func TestConnectionURL_BareEndpointStaysSigned(t *testing.T) {
	noAWS(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	got, err := connectionURL(context.Background(), Config{Endpoint: "x.iot.us-east-1.amazonaws.com", Region: "us-east-1"})
	require.NoError(t, err)
	assert.Contains(t, got, "wss://x.iot.us-east-1.amazonaws.com/mqtt?")
	assert.Contains(t, got, "X-Amz-Signature=")
}

// An unsupported scheme fails loudly instead of being signed as a host name.
func TestConnectionURL_RejectsUnsupportedScheme(t *testing.T) {
	_, err := connectionURL(context.Background(), Config{Endpoint: "http://127.0.0.1:9001", Region: "us-east-1"})
	assert.Error(t, err)
}
