package mcp

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestStateForError_MapsAuthErrors(t *testing.T) {
	t.Parallel()

	state := stateForError(&AuthRequiredError{Name: "notion"})
	require.Equal(t, StateNeedsAuth, state)

	state = stateForError(errors.New(`calling "initialize": sending "initialize": rejected by transport: Post "https://example.com/mcp": Unauthorized`))
	require.Equal(t, StateNeedsAuth, state)

	state = stateForError(errors.New("dial tcp 10.0.4.3:401: connect: connection refused"))
	require.Equal(t, StateError, state)

	state = stateForError(errors.New("boom"))
	require.Equal(t, StateError, state)

	// Network errors that happen to mention auth-like words must not be
	// misclassified as auth errors (would trigger OAuth refresh instead of
	// reconnect).
	state = stateForError(errors.New("Proxy unauthorized: connection refused"))
	require.Equal(t, StateError, state)

	state = stateForError(errors.New("Get \"https://example.com/mcp\": dial tcp 10.0.0.1:443: connect: connection refused"))
	require.Equal(t, StateError, state)

	// Genuine 401 responses should still be treated as auth errors.
	state = stateForError(errors.New("HTTP 401 Unauthorized"))
	require.Equal(t, StateNeedsAuth, state)

	state = stateForError(errors.New("status code 401: Unauthorized"))
	require.Equal(t, StateNeedsAuth, state)
}

func TestCloneMCPOAuthConfig(t *testing.T) {
	t.Parallel()

	in := &config.MCPOAuthConfig{
		Enabled: true,
		Registration: &config.MCPOAuthRegistration{
			ClientID: "client-id",
		},
		AuthServer: &config.MCPOAuthAuthServer{
			Issuer: "https://auth.example.com",
		},
		Scopes: []string{"read", "write"},
	}

	out := cloneMCPOAuthConfig(in)
	require.NotNil(t, out)
	require.NotSame(t, in, out)
	require.NotSame(t, in.Registration, out.Registration)
	require.NotSame(t, in.AuthServer, out.AuthServer)
	require.NotSame(t, &in.Scopes[0], &out.Scopes[0])

	out.Registration.ClientID = "changed"
	out.AuthServer.Issuer = "https://changed.example.com"
	out.Scopes[0] = "changed"

	require.Equal(t, "client-id", in.Registration.ClientID)
	require.Equal(t, "https://auth.example.com", in.AuthServer.Issuer)
	require.Equal(t, []string{"read", "write"}, in.Scopes)
}

func TestOAuthRoundTripperMapsUnauthorizedToAuthRequired(t *testing.T) {
	t.Parallel()

	rt := &oauthRoundTripper{
		base: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header: http.Header{
					"WWW-Authenticate": []string{`Bearer realm="example", resource_metadata="https://example.com/.well-known/oauth-protected-resource"`},
				},
				Body:    io.NopCloser(strings.NewReader("unauthorized")),
				Request: req,
			}, nil
		}),
		authorizer: newMCPOAuthAuthorizer("notion", nil, nil),
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/mcp", nil)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	require.Nil(t, resp)
	require.Error(t, err)
	var authErr *AuthRequiredError
	require.ErrorAs(t, err, &authErr)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}
