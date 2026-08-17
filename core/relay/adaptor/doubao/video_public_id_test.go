//nolint:testpackage
package doubao

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coremodel "github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/mode"
	relaymodel "github.com/labring/aiproxy/core/relay/model"
)

// The gateway's public handle for a video is the id it returned at create time,
// which is also the store key. Aggregator channels return their own handle on
// create and then echo the origin provider's task id in the status body — if we
// pass that through, the client polls an id no store row exists for and every
// later request fails. Nothing else in the suite pins this, and the OpenAI video
// convention is to keep polling the id in the response body, so a client doing
// the conventional thing is exactly the client that breaks.
func TestVideosStatusHandlerEchoesPublicIDNotUpstreamID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		publicID   = "task_public_abc"
		upstreamID = "cgt-20260817142809-j8r4m"
	)

	store := &doubaoTestStore{saved: []adaptor.StoreCache{{
		ID:       coremodel.VideoGenerationStoreID(publicID),
		Metadata: `{"prompt":"A stored prompt","resolution":"480p","ratio":"16:9","duration":5}`,
	}}}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	m := meta.NewMeta(
		&coremodel.Channel{ID: 4},
		mode.VideosGet,
		"doubao-seedance-2-0-260128",
		coremodel.ModelConfig{},
		meta.WithVideoID(publicID),
	)
	m.Group.ID = "group-1"
	m.Token.ID = 7

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body: io.NopCloser(strings.NewReader(`{
			"id":"` + upstreamID + `",
			"status":"succeeded",
			"content":{"video_url":"https://example.com/video.mp4"}
		}`)),
	}

	result, adaptorErr := VideosStatusHandler(m, store, ctx, resp)
	if adaptorErr != nil {
		t.Fatalf("VideosStatusHandler returned error: %v", adaptorErr)
	}

	var video struct {
		ID     string `json:"id"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &video); err != nil {
		t.Fatalf("unmarshal video response %s: %v", recorder.Body.String(), err)
	}

	if video.ID != publicID {
		t.Fatalf(
			"status response must echo the requested id %q, got %q — a client polling this id gets a store miss",
			publicID,
			video.ID,
		)
	}

	// Proves the stored metadata was read under the public key, not the
	// upstream one. Read it under the wrong key and the prompt silently
	// disappears from every status response.
	if video.Prompt != "A stored prompt" {
		t.Fatalf("expected stored metadata resolved under the public id, got prompt %q", video.Prompt)
	}

	if result.UpstreamID != publicID {
		t.Fatalf(
			"async usage rows are keyed on the public handle at create time, got %q",
			result.UpstreamID,
		)
	}

	// The completion re-save must refresh the row the client actually holds.
	// Writing it under the upstream id leaves the real row unrefreshed and
	// litters the store with a key nobody can ever look up.
	wantKey := coremodel.VideoGenerationStoreID(publicID)
	strayKey := coremodel.VideoGenerationStoreID(upstreamID)

	var refreshed bool
	for _, cache := range store.saved {
		if cache.ID == strayKey {
			t.Fatalf("store row written under the upstream id %q, which no client holds", strayKey)
		}

		if cache.ID == wantKey {
			refreshed = true
		}
	}

	if !refreshed {
		t.Fatalf("expected the completed task to refresh the store row %q", wantKey)
	}
}

// The store row is the only key to a video the customer already paid for, and
// the reaper hard-deletes rows past expires_at. Ark reports
// execution_expires_after=172800 (48h), which describes how long ARK keeps
// executing the task — adopting it verbatim silently shortened our retention to
// 48h and destroyed paid results on a timer.
func TestDoubaoVideoExpiresAtNeverFallsBelowRetentionFloor(t *testing.T) {
	t.Parallel()

	now := time.Now()

	cases := []struct {
		name                  string
		createdAt             int64
		executionExpiresAfter int64
		wantAtLeast           time.Duration
	}{
		{
			name:                  "ark 48h execution window does not shorten retention",
			createdAt:             now.Unix(),
			executionExpiresAfter: 48 * 60 * 60,
			wantAtLeast:           doubaoVideoTTL,
		},
		{
			name:                  "upstream reports nothing",
			createdAt:             0,
			executionExpiresAfter: 0,
			wantAtLeast:           doubaoVideoTTL,
		},
		{
			name:                  "already elapsed upstream window does not expire the row immediately",
			createdAt:             now.Add(-72 * time.Hour).Unix(),
			executionExpiresAfter: 48 * 60 * 60,
			wantAtLeast:           doubaoVideoTTL,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := doubaoVideoExpiresAt(relaymodel.DoubaoVideoTaskResponse{
				CreatedAt:             tc.createdAt,
				ExecutionExpiresAfter: tc.executionExpiresAfter,
			})

			if got.Before(now.Add(tc.wantAtLeast)) {
				t.Fatalf(
					"retention floor breached: expires at %s, less than %s from now",
					got.Format(time.RFC3339),
					tc.wantAtLeast,
				)
			}
		})
	}
}

// A longer upstream window is real information and should extend retention.
func TestDoubaoVideoExpiresAtHonoursLongerUpstreamWindow(t *testing.T) {
	t.Parallel()

	createdAt := time.Now()
	upstreamWindow := doubaoVideoTTL + 72*time.Hour

	got := doubaoVideoExpiresAt(relaymodel.DoubaoVideoTaskResponse{
		CreatedAt:             createdAt.Unix(),
		ExecutionExpiresAfter: int64(upstreamWindow / time.Second),
	})

	if got.Before(createdAt.Add(doubaoVideoTTL + 71*time.Hour)) {
		t.Fatalf("expected the longer upstream window to win, got %s", got.Format(time.RFC3339))
	}
}
