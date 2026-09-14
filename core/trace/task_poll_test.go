package trace

import (
	"context"
	"github.com/labring/aiproxy/core/common/requesttrace"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestBackgroundPollUsesSavedAssociationAndActualPollStage(t *testing.T) {
	db := openRuntimeSQLite(t)
	r := Start(context.Background(), db, Options{Enabled: true})
	t.Cleanup(func() { require.NoError(t, r.Close(context.Background())) })
	require.NoError(t, db.AutoMigrate(&model.AsyncUsageInfo{}))
	info := model.AsyncUsageInfo{GroupID: "group-a", TokenID: 7, ChannelID: 9, UpstreamID: "task-a", RequestID: "submit-a"}
	require.NoError(t, db.Create(&info).Error)
	require.NoError(t, r.store.SaveTaskTrace(context.Background(), info.ID, info.GroupID, "11111111111111111111111111111111", "22222222222222222222222222222222"))
	finish := r.BeginTaskPoll(context.Background(), &info)
	finish(true, nil)
	require.NoError(t, r.Close(context.Background()))
	var spans []model.RequestTraceSpan
	require.NoError(t, db.Find(&spans).Error)
	require.Len(t, spans, 2)
	for _, span := range spans {
		require.Equal(t, "11111111111111111111111111111111", span.TraceID)
		require.Equal(t, "group-a", span.GroupID)
		require.Contains(t, []requesttrace.Stage{requesttrace.StageAsyncStatusPoll, requesttrace.StageAsyncObservedResult}, span.Stage)
		require.Equal(t, requesttrace.StatusSuccess, span.Status)
	}
}
