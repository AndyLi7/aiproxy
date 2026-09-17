//nolint:testpackage // These fixtures verify internal metering and persistence boundaries.
package model

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMeasuredImageAdditiveMigrationPreservesLegacyRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := OpenSQLite(path)
	require.NoError(t, err)

	types := []any{
		&ModelConfig{},
		&GroupModelConfig{},
		&Log{},
		&AsyncUsageInfo{},
		&Summary{},
		&GroupSummary{},
		&SummaryMinute{},
		&GroupSummaryMinute{},
	}
	require.NoError(t, db.AutoMigrate(types...))
	// Recreate the immediately preceding schema by dropping only new nullable fields.
	for _, v := range types {
		for _, col := range []string{"image_billing", "image_usage", "image_billing_result", "measured_image", "service_tier_flex_image_billing_result", "service_tier_priority_image_billing_result", "claude_long_context_image_billing_result"} {
			if db.Migrator().HasColumn(v, col) {
				require.NoError(t, db.Migrator().DropColumn(v, col))
			}
		}
	}

	require.NoError(
		t,
		db.Exec(
			"INSERT INTO logs (request_id, used_amount, output_price, pricing_version) VALUES (?, ?, ?, ?)",
			"legacy",
			1.5,
			.5,
			"old",
		).Error,
	)
	require.NoError(
		t,
		db.Exec(
			"INSERT INTO async_usage_infos (request_id, status, used_amount, output_price) VALUES (?, ?, ?, ?)",
			"legacy",
			2,
			1.5,
			.5,
		).Error,
	)
	require.NoError(
		t,
		db.Exec(
			"INSERT INTO model_configs (model, output_price) VALUES (?, ?)",
			"legacy",
			.5,
		).Error,
	)
	sql, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sql.Close())

	db, err = OpenSQLite(path)
	require.NoError(t, err)
	t.Cleanup(func() { sql, _ := db.DB(); _ = sql.Close() })
	require.NoError(t, db.AutoMigrate(types...))
	require.NoError(t, db.AutoMigrate(types...))

	var entry Log
	require.NoError(t, db.First(&entry).Error)
	require.Equal(t, 1.5, entry.Amount.UsedAmount)
	require.Equal(t, "old", entry.PricingVersion)
	require.Nil(t, entry.Price.ImageBilling)
	require.Nil(t, entry.UsageContext.ImageUsage)
	require.Nil(t, entry.Amount.ImageBillingResult)

	var info AsyncUsageInfo
	require.NoError(t, db.First(&info).Error)
	require.Equal(t, AsyncUsageStatusCompleted, info.Status)
	require.False(t, info.MeasuredImage)

	var mc ModelConfig
	require.NoError(t, db.First(&mc).Error)
	require.Equal(t, ZeroNullFloat64(.5), mc.Price.OutputPrice)
	mc.Type = 5
	mc.Price.ImageBilling = &ImageBillingPolicy{Version: 1, Scenario: "generation"}
	require.NoError(t, db.Save(&mc).Error)

	var reread ModelConfig
	require.NoError(t, db.First(&reread).Error)
	require.Equal(t, "generation", reread.Price.ImageBilling.Scenario)
}
