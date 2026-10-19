//nolint:testpackage
package model

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func useRetentionTestDB(t *testing.T, db *gorm.DB) {
	t.Helper()

	oldDB, oldLog, oldCaches := DB, LogDB, LoadModelCaches()
	DB, LogDB = db, db
	t.Cleanup(func() {
		DB, LogDB = oldDB, oldLog

		modelCaches.Store(oldCaches)
	})
}

// Recovery polls and settles native tasks with the channel's stored key, so a
// channel keeps its credentials until every task naming it is finished and
// settled (the ClaimNativeRecovery predicate).
func TestNativeTasksProtectChannelCredentials(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "channel.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&NativeTask{}, &Channel{}, &ChannelTest{}, &ModelConfig{}))

	useRetentionTestDB(t, db)

	ch := &Channel{Type: ChannelTypeFal, Key: "old"}
	other := &Channel{Type: ChannelTypeFal, Key: "other"}

	require.NoError(t, db.Create(ch).Error)
	require.NoError(t, db.Create(other).Error)

	task := &NativeTask{
		ID: "native-in-flight", GroupID: "g", TokenID: 1, Model: "m", Fingerprint: "fp",
		OutputSchema: "{}", OutputSchemaHash: "sha", Status: "queued", ChannelID: ch.ID,
	}
	require.NoError(t, db.Create(task).Error)
	// A task on another channel never retains this one.
	require.NoError(t, db.Create(&NativeTask{
		ID: "native-other", GroupID: "g", TokenID: 1, Model: "m", Fingerprint: "fp",
		OutputSchema: "{}", OutputSchemaHash: "sha", Status: "queued", ChannelID: other.ID,
	}).Error)

	replacement, same := "replacement", "old"
	refused := func() {
		t.Helper()
		require.ErrorContains(t, DeleteChannelByID(ch.ID), "native tasks")
		require.ErrorContains(t, DeleteChannelsByIDs([]int{ch.ID}), "native tasks")
		require.ErrorContains(
			t,
			UpdateChannel(ch, &ChannelPatch{Key: &replacement}),
			"native tasks",
		)
		current, err := GetChannelByID(ch.ID)
		require.NoError(t, err)
		require.Equal(t, "old", current.Key)
	}

	for _, state := range []struct {
		status  string
		settled bool
	}{{"reserved", false}, {"queued", false}, {"running", true}, {"completed", false}, {"failed", false}} {
		require.NoError(
			t,
			db.Model(task).
				Updates(map[string]any{"status": state.status, "billing_settled": state.settled}).
				Error,
		)
		refused()
	}

	// Writes that keep the credentials stay allowed.
	priority := int32(5)
	require.NoError(t, UpdateChannel(ch, &ChannelPatch{Priority: &priority}))
	require.NoError(t, UpdateChannel(ch, &ChannelPatch{Key: &same}))

	// Finished and settled: the credentials may change.
	require.NoError(
		t,
		db.Model(task).
			Updates(map[string]any{"status": "completed", "billing_settled": true}).
			Error,
	)
	require.NoError(t, UpdateChannel(ch, &ChannelPatch{Key: &replacement}))
	current, err := GetChannelByID(ch.ID)
	require.NoError(t, err)
	require.Equal(t, "replacement", current.Key)
	require.NoError(t, DeleteChannelByID(ch.ID))
}

// The handler passes UpdateChannel a copy that already holds the new key; the
// guard compares with the stored row, so it still fires.
func TestChannelRetentionComparesWithStoredRow(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "channel.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&NativeTask{}, &Channel{}, &ChannelTest{}, &ModelConfig{}))

	useRetentionTestDB(t, db)

	ch := &Channel{Type: ChannelTypeFal, Key: "old"}
	require.NoError(t, db.Create(ch).Error)
	require.NoError(t, db.Create(&NativeTask{
		ID: "native-in-flight", GroupID: "g", TokenID: 1, Model: "m", Fingerprint: "fp",
		OutputSchema: "{}", OutputSchemaHash: "sha", Status: "queued", ChannelID: ch.ID,
	}).Error)

	patched := *ch
	patched.Key = "replacement"
	require.ErrorContains(
		t,
		UpdateChannel(&patched, &ChannelPatch{Key: &patched.Key}),
		"native tasks",
	)

	current, err := GetChannelByID(ch.ID)
	require.NoError(t, err)
	require.Equal(t, "old", current.Key)
}
