package model_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
	"gorm.io/gorm"
)

func openLastAPIRequestLogDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "last-api-request.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}

	closeTestSQLite(t, db)

	prevLogDB := model.LogDB
	model.LogDB = db

	t.Cleanup(func() { model.LogDB = prevLogDB })

	if err := db.AutoMigrate(&model.Log{}, &model.RequestDetail{}); err != nil {
		t.Fatalf("migrate log db: %v", err)
	}

	// The index the production lookup walks (CreateLogIndexes picks the dialect
	// from a process-wide flag, so create the SQLite form directly).
	if err := db.Exec(
		"CREATE INDEX IF NOT EXISTS idx_group_creat ON logs (group_id, created_at DESC)",
	).Error; err != nil {
		t.Fatalf("create group index: %v", err)
	}

	return db
}

// seedLastAPIRequestRow inserts a raw logs row so request_source can be NULL.
func seedLastAPIRequestRow(
	t *testing.T,
	db *gorm.DB,
	group, tokenName string,
	source any,
	code int,
	createdAt time.Time,
) {
	t.Helper()

	row := map[string]any{
		"group_id":       group,
		"token_name":     tokenName,
		"request_source": source,
		"model":          "test-model",
		"code":           code,
		"created_at":     createdAt,
		"request_at":     createdAt,
	}
	if err := db.Table("logs").Create(row).Error; err != nil {
		t.Fatalf("seed log row for %s: %v", group, err)
	}
}

func TestGetGroupsLastAPIRequestAtUsesNewestRowFromCustomerKeys(t *testing.T) {
	db := openLastAPIRequestLogDB(t)
	base := time.Unix(1_790_000_000, 0)
	at := func(seconds int) time.Time { return base.Add(time.Duration(seconds) * time.Second) }

	const (
		api  = model.RequestSourceAPI
		pg   = model.RequestSourcePlayground
		demo = model.RequestSourceAdminDemo
	)

	seed := func(group, tokenName string, source any, code, seconds int) {
		seedLastAPIRequestRow(t, db, group, tokenName, source, code, at(seconds))
	}

	// Newest qualifying row wins regardless of insert order.
	seed("u_newest", "key-a", api, 200, 1)
	seed("u_newest", "key-a", api, 200, 30)
	seed("u_newest", "key-b", api, 200, 20)

	// Playground and admin demo rows never count, even when newer.
	seed("u_excluded", "key-a", api, 200, 2)
	seed("u_excluded", "playground", pg, 200, 50)
	seed("u_excluded", "key-a", demo, 200, 60)

	// Legacy rows without a source count: NULL and empty.
	seed("u_legacy_null", "key-a", nil, 200, 4)
	seed("u_legacy_null", "playground", pg, 200, 70)
	seed("u_legacy_empty", "key-a", "", 200, 5)

	// A key the customer named "playground" is an ordinary API key when its
	// requests arrive with source "api".
	seed("u_playground_key", "playground", api, 200, 8)
	seed("u_playground_key", "playground", pg, 200, 9)

	// Native task acceptance (202) and rejected requests are API use too.
	seed("u_native", "key-a", api, 202, 10)
	seed("u_rejected", "key-a", api, 429, 11)

	// Only excluded traffic, and no rows at all, are absent from the result.
	seed("u_only_excluded", "playground", pg, 200, 12)
	seed("u_only_excluded", "key-a", demo, 200, 13)

	// A group that was not asked for never leaks into the result.
	seed("u_other", "key-a", api, 200, 99)

	found, unchecked, err := model.GetGroupsLastAPIRequestAt(t.Context(), []string{
		"u_newest",
		"u_excluded",
		"u_legacy_null",
		"u_legacy_empty",
		"u_playground_key",
		"u_native",
		"u_rejected",
		"u_only_excluded",
		"u_no_rows",
		"u_newest",
		"",
	})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}

	if unchecked != nil {
		t.Fatalf("unchecked = %v, want nil", unchecked)
	}

	want := map[string]time.Time{
		"u_newest":         at(30),
		"u_excluded":       at(2),
		"u_legacy_null":    at(4),
		"u_legacy_empty":   at(5),
		"u_playground_key": at(8),
		"u_native":         at(10),
		"u_rejected":       at(11),
	}
	if len(found) != len(want) {
		t.Fatalf("found %d groups (%v), want %d (%v)", len(found), found, len(want), want)
	}

	for group, wantAt := range want {
		gotAt, ok := found[group]
		if !ok {
			t.Fatalf("group %s missing from result %v", group, found)
		}

		if gotAt.UnixMilli() != wantAt.UnixMilli() {
			t.Fatalf("group %s = %s, want %s", group, gotAt, wantAt)
		}
	}
}

func TestGetGroupsLastAPIRequestAtReturnsNothingForUnknownGroups(t *testing.T) {
	openLastAPIRequestLogDB(t)

	found, unchecked, err := model.GetGroupsLastAPIRequestAt(
		t.Context(),
		[]string{"u_missing"},
	)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}

	if len(found) != 0 || unchecked != nil {
		t.Fatalf("found = %v, unchecked = %v, want both empty", found, unchecked)
	}
}

func TestGetGroupsLastAPIRequestAtStopsWhenContextIsCancelled(t *testing.T) {
	db := openLastAPIRequestLogDB(t)
	base := time.Unix(1_790_000_000, 0)

	seedLastAPIRequestRow(t, db, "u_first", "key-a", model.RequestSourceAPI, 200, base)
	seedLastAPIRequestRow(t, db, "u_second", "key-a", model.RequestSourceAPI, 200, base)

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	found, unchecked, err := model.GetGroupsLastAPIRequestAt(
		cancelled,
		[]string{"u_first", "u_second", "u_first"},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	if len(found) != 0 || !reflect.DeepEqual(unchecked, []string{"u_first", "u_second"}) {
		t.Fatalf(
			"found = %v, unchecked = %v, want nothing found and both unchecked",
			found,
			unchecked,
		)
	}
}

func TestGetGroupsLastAPIRequestAtReturnsGroupsFinishedBeforeCancellation(t *testing.T) {
	db := openLastAPIRequestLogDB(t)
	base := time.Unix(1_790_000_000, 0)

	seedLastAPIRequestRow(t, db, "u_first", "key-a", model.RequestSourceAPI, 200, base)
	seedLastAPIRequestRow(t, db, "u_second", "key-a", model.RequestSourceAPI, 200, base)
	seedLastAPIRequestRow(t, db, "u_third", "key-a", model.RequestSourceAPI, 200, base)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	// Cancel as soon as the first group's query has finished.
	if err := db.Callback().Query().After("gorm:query").
		Register("test:cancel_after_first_query", func(*gorm.DB) { cancel() }); err != nil {
		t.Fatalf("register callback: %v", err)
	}

	found, unchecked, err := model.GetGroupsLastAPIRequestAt(
		ctx,
		[]string{"u_first", "u_second", "u_third"},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	if len(found) != 1 || found["u_first"].UnixMilli() != base.UnixMilli() {
		t.Fatalf("found = %v, want only u_first", found)
	}

	if !reflect.DeepEqual(unchecked, []string{"u_second", "u_third"}) {
		t.Fatalf("unchecked = %v, want [u_second u_third]", unchecked)
	}
}

func TestGetGroupsLastAPIRequestAtReportsQueryFailure(t *testing.T) {
	db := openLastAPIRequestLogDB(t)

	if err := db.Migrator().DropTable(&model.RequestDetail{}, &model.Log{}); err != nil {
		t.Fatalf("drop logs: %v", err)
	}

	found, unchecked, err := model.GetGroupsLastAPIRequestAt(
		t.Context(),
		[]string{"u_first"},
	)
	if err == nil {
		t.Fatal("expected an error when the logs table is missing")
	}

	if found != nil || unchecked != nil {
		t.Fatalf("found = %v, unchecked = %v, want nil on failure", found, unchecked)
	}
}
