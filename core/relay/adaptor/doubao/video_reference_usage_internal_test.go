package doubao

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	coremodel "github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/adaptor"
	"github.com/labring/aiproxy/core/relay/mode"
	relaymodel "github.com/labring/aiproxy/core/relay/model"
	"github.com/stretchr/testify/require"
)

func TestReferenceResponsePreservesFractionalSeconds(t *testing.T) {
	var response relaymodel.DoubaoVideoTaskResponse
	require.NoError(
		t,
		json.Unmarshal(
			[]byte(
				`{"id":"task-1","status":"succeeded","duration":4.6,"usage":{"completion_tokens":1234}}`,
			),
			&response,
		),
	)
	video := buildDoubaoVideo(referenceTaskMeta(), "task-1", &response)
	body, err := json.Marshal(video)
	require.NoError(t, err)

	var output map[string]any
	require.NoError(t, json.Unmarshal(body, &output))
	require.Equal(t, 4.6, output["seconds"])
	require.Equal(
		t,
		coremodel.ZeroNullInt64(1234),
		doubaoVideoUsageToModelUsage(response.Usage).OutputTokens,
	)
}

func TestReferenceMissingUsageRemainsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"task-1","status":"succeeded","duration":5}`))
	}))
	defer server.Close()

	store := &doubaoTestStore{
		saved: []adaptor.StoreCache{
			{
				ID:       coremodel.VideoGenerationStoreID("task-1"),
				Metadata: `{"reference_task_type":"edit","duration":-1}`,
			},
		},
	}
	request := doubaoAsyncUsageRequestWithMode(mode.Videos, server.URL, "task-1", store)
	_, _, completed, err := (&Adaptor{}).FetchAsyncUsage(context.Background(), request)
	require.False(t, completed)
	require.Error(t, err)
}

func TestReferenceAutoDurationNeverBecomesUsageQuantity(t *testing.T) {
	meta := referenceTaskMeta()
	setDoubaoVideoMetadata(meta, doubaoVideoStoreMetadata{Duration: -1})
	require.GreaterOrEqual(t, doubaoVideoRequestUsageContext(meta).VideoSeconds, int64(0))
}
