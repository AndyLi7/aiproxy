//nolint:testpackage
package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequestIDColumnsAcceptGatewayCorrelationIDs(t *testing.T) {
	withTestPostgresStoreDB(t, func() {
		for _, table := range []string{"logs", "retry_logs", "consume_errors", "async_usage_infos"} {
			require.NoError(t, LogDB.Exec(
				"CREATE TABLE "+table+" (id bigserial PRIMARY KEY, request_id char(16))",
			).Error)
		}

		require.NoError(t, LogDB.AutoMigrate(
			&Log{},
			&RetryLog{},
			&ConsumeError{},
			&AsyncUsageInfo{},
		))

		const requestID = "17d3ac03-be43-4108-9de2-9aba41adf3b8"

		require.NoError(t, LogDB.Create(&Log{RequestID: EmptyNullString(requestID)}).Error)
		require.NoError(t, LogDB.Create(&RetryLog{RequestID: EmptyNullString(requestID)}).Error)
		require.NoError(t, LogDB.Create(&ConsumeError{
			RequestID: requestID,
			TokenName: EmptyNullString("playground"),
		}).Error)
		require.NoError(t, LogDB.Create(&AsyncUsageInfo{RequestID: requestID}).Error)
	})
}
