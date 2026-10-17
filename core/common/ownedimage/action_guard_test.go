package ownedimage

import (
	"context"
	"errors"
	"github.com/labring/aiproxy/core/common/actionguard"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestArchiveActionGuardBeforeDownloadUploadAndMetadata(t *testing.T) {
	for _, stage := range []string{"download", "upload", "metadata"} {
		t.Run(stage, func(t *testing.T) {
			lost := errors.New("claim lost")
			checks, downloads, uploads := 0, 0, 0
			ctx := actionguard.With(context.Background(), func(context.Context) error {
				checks++
				if stage != "upload" || checks > 1 {
					return lost
				}
				return nil
			})
			download := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
				downloads++
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("\x89PNG\r\n\x1a\n" + strings.Repeat("x", 30)))}, nil
			})}
			upload := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
				uploads++
				t.Error("upload must not start")
				return nil, lost
			})}
			if stage == "metadata" {
				_, err := readMetadata(ctx, "https://cdn.example/image.png", download)
				require.ErrorIs(t, err, lost)
			} else {
				_, _, err := storeWithMetadata(ctx, "https://cdn.example/image.png", "http://app.internal", "private", download, upload, nil)
				require.ErrorIs(t, err, lost)
			}
			require.Zero(t, uploads)
			if stage == "upload" {
				require.Equal(t, 1, downloads)
			} else {
				require.Zero(t, downloads)
			}
		})
	}
}
