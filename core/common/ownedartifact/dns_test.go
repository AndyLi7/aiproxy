//nolint:testpackage // White-box regression tests exercise unexported production boundaries.
package ownedartifact

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type artifactDNSTransport func(*http.Request) (*http.Response, error)

func (f artifactDNSTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestArtifactDNSRejectsUnsafeAnswersWithoutFallback(t *testing.T) {
	for _, body := range []string{
		`{"Status":0,"Answer":[{"type":1,"data":"198.18.2.159"}]}`,
		`{"Status":0,"Answer":[{"type":1,"data":"127.0.0.1"}]}`,
		`{"Status":0,"Answer":[{"type":1,"data":"10.0.0.1"}]}`,
		`{"Status":0,"Answer":[{"type":1,"data":"1.1.1.1"},{"type":1,"data":"169.254.169.254"}]}`,
		`{"Status":3,"Answer":[{"type":1,"data":"1.1.1.1"}]}`,
		`{"Status":0,"Answer":[{"type":1,"data":"invalid"}]}`,
		`{"Status":0,"Answer":[]}`,
		`{}`,
		strings.Repeat(" ", 65537),
	} {
		calls := 0
		client := &http.Client{
			Transport: artifactDNSTransport(func(r *http.Request) (*http.Response, error) {
				calls++

				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(body)),
				}, nil
			}),
		}
		_, err := cloudflareArtifactIPs(context.Background(), "fal.media", client)
		require.ErrorIs(t, err, errArtifactDNS)
		require.Equal(t, 1, calls)
	}
}

func TestArtifactDNSOnlySendsHostnameToFixedHTTPSResolver(t *testing.T) {
	client := &http.Client{
		Transport: artifactDNSTransport(func(r *http.Request) (*http.Response, error) {
			require.Equal(t, "https", r.URL.Scheme)
			require.Equal(t, "cloudflare-dns.com", r.URL.Host)
			require.Equal(t, "/dns-query", r.URL.Path)
			require.Equal(t, "fal.media", r.URL.Query().Get("name"))
			require.Equal(t, "A", r.URL.Query().Get("type"))
			require.Empty(t, r.Header.Get("Authorization"))
			require.Nil(t, r.Body)
			_, deadline := r.Context().Deadline()
			require.True(t, deadline)

			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(
				`{"Status":0,"Answer":[{"type":5,"data":"alias.fal.media"},{"type":1,"data":"1.1.1.1"}]}`,
			))}, nil
		}),
	}
	ips, err := cloudflareArtifactIPs(context.Background(), "fal.media", client)
	require.NoError(t, err)
	require.Equal(t, "1.1.1.1", ips[0].IP.String())
}

func TestArtifactDNSFailureAndUnsupportedModeFailClosed(t *testing.T) {
	t.Setenv("NATIVE_ARTIFACT_DNS_RESOLVER", "typo")

	_, err := lookupArtifactIPs(context.Background(), "fal.media")
	require.ErrorIs(t, err, errArtifactDNS)

	client := &http.Client{
		Transport: artifactDNSTransport(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("network unavailable")
		}),
	}
	_, err = cloudflareArtifactIPs(context.Background(), "fal.media", client)
	require.ErrorIs(t, err, errArtifactDNS)
	_, err = cloudflareArtifactIPs(context.Background(), "bad/path", client)
	require.ErrorIs(t, err, errArtifactDNS)
}

func TestArtifactDNSSystemModeUnchanged(t *testing.T) {
	t.Setenv("NATIVE_ARTIFACT_DNS_RESOLVER", "")

	ips, err := lookupArtifactIPs(context.Background(), "127.0.0.1")
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", ips[0].IP.String())
	require.False(t, publicIP(ips[0].IP))
}

// Opt-in local transport preflight: only DNS and an unauthenticated HEAD to
// the public artifact host, never a generation request or a provider credential.
func TestArtifactDNSLocalReadOnlyPreflight(t *testing.T) {
	if os.Getenv("NATIVE_ARTIFACT_DNS_PREFLIGHT") != "true" {
		t.Skip("opt-in public DNS/TLS check")
	}

	t.Setenv("NATIVE_ARTIFACT_DNS_RESOLVER", "cloudflare")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client := downloadClient()
	defer client.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://v3.fal.media/", nil)
	require.NoError(t, err)
	response, err := client.Do(req)
	require.NoError(t, err)

	defer response.Body.Close()

	require.Less(t, response.StatusCode, 500)
}
