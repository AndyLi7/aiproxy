package task

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// A task never checked (NULL billing_next_check_at) must be reachable when a
// full batch of scheduled tasks is due. PostgreSQL sorts NULLs last in
// ascending order, so the scan orders by the check time or, before the first
// check, the creation time. SQLite (this test database) sorts NULLs first and
// would pass the old query too, so the generated SQL is asserted as well.
func TestImagePrepaymentScanReachesNeverCheckedTasks(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "scan.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ImageTask{}))

	now := time.Now().UTC()

	sql := db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		return dueImagePrepayments(tx, now).Find(&[]model.ImageTask{})
	})
	require.Contains(t, sql, "ORDER BY COALESCE(billing_next_check_at, created_at) ASC")
	require.Contains(t, sql, "LIMIT 10")

	quote := `{"version":1}`
	for i := range imagePrepaymentBatch {
		due := now.Add(-time.Duration(imagePrepaymentBatch-i) * time.Minute)
		require.NoError(t, db.Create(&model.ImageTask{
			ID: fmt.Sprintf("scheduled-%02d", i), GroupID: "g", TokenID: 1, Status: "completed",
			PrepaymentQuoteJSON: quote, BillingNextCheckAt: &due, CreatedAt: now.Add(-time.Minute),
		}).Error)
	}

	require.NoError(t, db.Create(&model.ImageTask{
		ID: "never-checked", GroupID: "g", TokenID: 1, Status: "failed",
		PrepaymentQuoteJSON: quote, CreatedAt: now.Add(-time.Hour),
	}).Error)
	later := now.Add(time.Minute)
	require.NoError(t, db.Create(&model.ImageTask{
		ID: "leased", GroupID: "g", TokenID: 1, Status: "completed",
		PrepaymentQuoteJSON: quote, BillingNextCheckAt: &later, CreatedAt: now.Add(-2 * time.Hour),
	}).Error)
	require.NoError(t, db.Create(&model.ImageTask{
		ID: "settled", GroupID: "g", TokenID: 1, Status: "completed", BillingSettled: true,
		PrepaymentQuoteJSON: quote, CreatedAt: now.Add(-3 * time.Hour),
	}).Error)

	var tasks []model.ImageTask
	require.NoError(t, dueImagePrepayments(db, now).Find(&tasks).Error)
	require.Len(t, tasks, imagePrepaymentBatch)
	require.Equal(
		t,
		"never-checked",
		tasks[0].ID,
		"the oldest never-checked task is in the first page",
	)

	for _, task := range tasks {
		require.NotEqual(t, "leased", task.ID)
		require.NotEqual(t, "settled", task.ID)
	}
}
