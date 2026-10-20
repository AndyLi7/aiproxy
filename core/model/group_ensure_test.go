package model_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func groupEnsureDB(t *testing.T, path string) *gorm.DB {
	t.Helper()
	db, err := model.OpenSQLite(path)
	require.NoError(t, err)
	old := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = old; sqlDB, _ := db.DB(); _ = sqlDB.Close() })
	return db
}

func TestEnsureGroupProcess(t *testing.T) {
	path := os.Getenv("GROUP_ENSURE_TEST_DB")
	if path == "" {
		t.Skip("subprocess helper")
	}
	groupEnsureDB(t, path)
	g, err := model.EnsureGroup(&model.Group{ID: "concurrent", Status: 3, AvailableSets: []string{"private"}})
	require.NoError(t, err)
	require.Equal(t, 3, g.Status)
	require.Equal(t, []string{"private"}, g.AvailableSets)
}

func TestEnsureGroupIndependentProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "groups.db")
	db := groupEnsureDB(t, path)
	require.NoError(t, db.AutoMigrate(&model.Group{}))
	var commands []*exec.Cmd
	for range 8 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestEnsureGroupProcess$", "-test.count=1")
		cmd.Env = append(os.Environ(), "GROUP_ENSURE_TEST_DB="+path)
		commands = append(commands, cmd)
		require.NoError(t, cmd.Start())
	}
	for _, cmd := range commands {
		require.NoError(t, cmd.Wait())
	}
	var count int64
	require.NoError(t, db.Model(&model.Group{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
	// A new process reinitializing after restart reuses the same row.
	cmd := exec.Command(os.Args[0], "-test.run=^TestEnsureGroupProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), "GROUP_ENSURE_TEST_DB="+path)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

func TestEnsureGroupPreservesStateAndPropagatesFailures(t *testing.T) {
	db := groupEnsureDB(t, filepath.Join(t.TempDir(), "groups.db"))
	require.NoError(t, db.AutoMigrate(&model.Group{}, &model.Token{}, &model.GroupModelConfig{}))
	initial := &model.Group{ID: "existing", Status: 2, RPMRatio: 2.5, TPMRatio: 3, UsedAmount: 9.25, RequestCount: 27, AvailableSets: []string{"restricted"}, BalanceAlertEnabled: true, BalanceAlertThreshold: 17}
	require.NoError(t, model.CreateGroup(initial))
	token := &model.Token{GroupID: "existing", Name: "preserve", Key: "local-test-only", Status: 2, Quota: 12, UsedAmount: 3}
	require.NoError(t, db.Create(token).Error)
	cached, err := model.CacheGetGroup("existing")
	require.NoError(t, err)
	require.Equal(t, 2, cached.Status)

	var before model.Group
	require.NoError(t, db.First(&before, "id = ?", "existing").Error)
	reused, err := model.EnsureGroup(&model.Group{ID: "existing", Status: 3, AvailableSets: []string{"other"}, Tokens: []model.Token{{Name: "must-not-create"}}})
	require.NoError(t, err)
	require.Equal(t, before, *reused)
	cachedAgain, err := model.CacheGetGroup("existing")
	require.NoError(t, err)
	require.Equal(t, cached, cachedAgain)
	// Negative cache must not hide a newly created group in this process.
	_, err = model.CacheGetGroup("cached-missing")
	require.Error(t, err)
	_, err = model.EnsureGroup(&model.Group{ID: "cached-missing", Status: 3})
	require.NoError(t, err)
	fresh, err := model.CacheGetGroup("cached-missing")
	require.NoError(t, err)
	require.Equal(t, 3, fresh.Status)
	var after model.Token
	require.NoError(t, db.First(&after, token.ID).Error)
	require.True(t, token.CreatedAt.Equal(after.CreatedAt))
	token.CreatedAt = after.CreatedAt
	require.Equal(t, *token, after)
	var count int64
	require.NoError(t, db.Model(&model.Token{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
	// Generic create remains strict; no global duplicate-error suppression.
	require.Error(t, model.CreateGroup(&model.Group{ID: "existing"}))
	// An unrelated unique constraint must not be swallowed by the PK conflict clause.
	require.NoError(t, db.Exec("CREATE UNIQUE INDEX test_unique_rpm ON groups(rpm_ratio)").Error)
	_, err = model.EnsureGroup(&model.Group{ID: "new", RPMRatio: 2.5})
	require.Error(t, err)
	require.NoError(t, db.Migrator().DropTable(&model.Group{}))
	_, err = model.EnsureGroup(&model.Group{ID: "existing"})
	require.Error(t, err)
}
