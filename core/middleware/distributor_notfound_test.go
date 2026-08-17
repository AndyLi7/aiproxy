package middleware

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	coremodel "github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// distribute() answers 404 instead of 500 by testing
// errors.Is(err, gorm.ErrRecordNotFound) on whatever getRequestModel returns.
// That only holds because model.NotFoundError wraps with %w, several frames
// below. Nothing else pins it, so a plain errors.New() anywhere in that chain
// would silently turn every unknown id back into an opaque 500 — which is the
// state that made a routine "unknown id" indistinguishable from an outage.
func TestGetRequestModelUnknownStoredIDIsRecordNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name  string
		mode  mode.Mode
		param string
		path  string
	}{
		{name: "videos get", mode: mode.VideosGet, param: "video_id", path: "/v1/videos/missing-1"},
		{
			name:  "videos content",
			mode:  mode.VideosContent,
			param: "video_id",
			path:  "/v1/videos/missing-2/content",
		},
		{
			name:  "videos delete",
			mode:  mode.VideosDelete,
			param: "video_id",
			path:  "/v1/videos/missing-3",
		},
		{
			name:  "video generations job",
			mode:  mode.VideoGenerationsGetJobs,
			param: "id",
			path:  "/v1/video/generations/jobs/missing-4",
		},
		{
			name:  "video generations content",
			mode:  mode.VideoGenerationsContent,
			param: "id",
			path:  "/v1/video/generations/missing-5/content/video",
		},
	}

	withTestStoreDB(t, func() {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tc.path, nil)

				ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx.Request = req
				ctx.Set(Mode, tc.mode)
				ctx.Params = gin.Params{{Key: tc.param, Value: "never-stored-" + tc.name}}

				_, err := getRequestModel(ctx, tc.mode, "group-1", 7)
				require.Error(t, err)
				assert.True(
					t,
					errors.Is(err, gorm.ErrRecordNotFound),
					"unknown id must stay classifiable as not-found so distribute() can answer 404, got %v",
					err,
				)
			})
		}
	})
}

// A task whose retention window lapsed is gone, not broken. It reached
// customers as a 500 for days: they had been billed, held a valid id, and the
// gateway reported an internal error rather than telling them the result had
// expired.
func TestGetRequestModelExpiredStoredIDIsRecordNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)

	withTestStoreDB(t, func() {
		const videoID = "expired-video-901"

		_, err := coremodel.SaveStore(&coremodel.StoreV2{
			ID:        coremodel.VideoGenerationStoreID(videoID),
			GroupID:   "group-1",
			TokenID:   7,
			ChannelID: 42,
			Model:     "doubao-seedance-2-0-260128",
			ExpiresAt: time.Now().Add(-time.Hour),
		})
		require.NoError(t, err)

		req := httptest.NewRequestWithContext(
			t.Context(),
			http.MethodGet,
			"/v1/videos/"+videoID,
			nil,
		)

		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = req
		ctx.Set(Mode, mode.VideosGet)
		ctx.Params = gin.Params{{Key: "video_id", Value: videoID}}

		_, err = getRequestModel(ctx, mode.VideosGet, "group-1", 7)
		require.Error(t, err)
		assert.True(
			t,
			errors.Is(err, gorm.ErrRecordNotFound),
			"an expired task must resolve to not-found (404), not an internal error, got %v",
			err,
		)
	})
}
