package doubao

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/relay/meta"
)

func TestFetchDoubaoVideoContentWithRetryWaitsForTransientReadiness(t *testing.T) {
	t.Parallel()

	attempts := 0
	fetch := func(context.Context, *meta.Meta, string) (*http.Response, error) {
		attempts++
		status := http.StatusServiceUnavailable
		body := "not ready"
		if attempts == 3 {
			status = http.StatusOK
			body = "video"
		}

		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	}

	response, err := fetchDoubaoVideoContentWithRetry(
		context.Background(),
		nil,
		"https://cdn.example.com/video.mp4",
		fetch,
		[]time.Duration{0, 0, 0},
	)
	if err != nil {
		t.Fatalf("fetch content: %v", err)
	}
	defer response.Body.Close()

	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
}

func TestFetchDoubaoVideoContentWithRetryDoesNotRetryPermanentStatus(t *testing.T) {
	t.Parallel()

	attempts := 0
	fetch := func(context.Context, *meta.Meta, string) (*http.Response, error) {
		attempts++

		return &http.Response{
			StatusCode: http.StatusForbidden,
			Body:       io.NopCloser(strings.NewReader("forbidden")),
			Header:     make(http.Header),
		}, nil
	}

	response, err := fetchDoubaoVideoContentWithRetry(
		context.Background(),
		nil,
		"https://cdn.example.com/video.mp4",
		fetch,
		[]time.Duration{0, 0},
	)
	if err != nil {
		t.Fatalf("fetch content: %v", err)
	}
	defer response.Body.Close()

	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
}

func TestFetchDoubaoVideoContentWithRetryRetriesTemporaryNetworkError(t *testing.T) {
	t.Parallel()

	attempts := 0
	fetch := func(context.Context, *meta.Meta, string) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("temporary connection reset")
		}

		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("video")),
			Header:     make(http.Header),
		}, nil
	}

	response, err := fetchDoubaoVideoContentWithRetry(
		context.Background(),
		nil,
		"https://cdn.example.com/video.mp4",
		fetch,
		[]time.Duration{0},
	)
	if err != nil {
		t.Fatalf("fetch content: %v", err)
	}
	defer response.Body.Close()

	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}
