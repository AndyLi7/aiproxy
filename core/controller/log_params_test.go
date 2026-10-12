//nolint:testpackage // Group log handlers are exercised with their internal helpers.
package controller

import (
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labring/aiproxy/core/model"
)

// Malformed filters used to be dropped silently, so a caller got unfiltered
// rows for a filter it asked for.
func TestGroupLogsRejectInvalidParameters(t *testing.T) {
	newLogTestDB(t)

	// A day ago: inside the 400 days a window may reach back.
	recent := time.Now().Add(-24 * time.Hour).UnixMilli()
	at := func(offset int64) string { return strconv.FormatInt(recent+offset, 10) }

	for _, rawQuery := range []string{
		"start=abc",
		"start=-1",
		"end=" + at(0),
		"start=" + at(0) + "&end=abc",
		"start=" + at(0) + "&end=" + at(0),
		"start=" + at(1) + "&end=" + at(0),
		"start=" + at(0) + "&start_timestamp=" + at(0),
		"end=" + at(0) + "&end_timestamp=" + at(0),
		"start=" + at(0) + "&end_timestamp=" + at(0),
		// Narrow windows that start too far back would scan the group's whole
		// history (created_at >= start is the only index bound).
		"start=0",
		"start=0&end=86400000",
		"start=" + strconv.FormatInt(time.Now().Add(-401*24*time.Hour).UnixMilli(), 10),
		"log_id=0",
		"log_id=-3",
		"log_id=abc",
		"log_id=",
		"token_id=0",
		"token_id=abc",
		"token_id=",
		"status=pending",
		"status=Succeeded",
		"kind=everything",
		"model_id=" + strings.Repeat("m", 257),
		"exclude_modes=0",
	} {
		t.Run(rawQuery, func(t *testing.T) {
			query, err := url.ParseQuery(rawQuery)
			if err != nil {
				t.Fatalf("parse query: %v", err)
			}

			code, _, body := serveGroupLogs(t, "g", query)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", code, body)
			}
		})
	}

	for _, rawQuery := range []string{
		"start=" + at(0),
		"start=" + at(0) + "&end=" + at(1),
		"start=" + strconv.FormatInt(time.Now().Add(-399*24*time.Hour).UnixMilli(), 10),
		"start_timestamp=" + at(0) + "&end_timestamp=" + at(1),
		"status=success",
		"status=unknown",
		"kind=all",
		"kind=reads",
		"model_id=" + strings.Repeat("m", 256),
		"token_id=7&log_id=9",
	} {
		t.Run("valid "+rawQuery, func(t *testing.T) {
			query, err := url.ParseQuery(rawQuery)
			if err != nil {
				t.Fatalf("parse query: %v", err)
			}

			code, _, body := serveGroupLogs(t, "g", query)
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", code, body)
			}
		})
	}
}

// A database failure answers with a fixed message; the driver text used to be
// echoed to the caller.
func TestGroupLogsHideDatabaseErrors(t *testing.T) {
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}

	old := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = old })

	code, _, body := serveGroupLogs(t, "g", url.Values{"kind": {"calls"}})
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", code, body)
	}

	if !strings.Contains(body, "unable to read logs") || strings.Contains(body, "no such table") {
		t.Fatalf("body = %s, want the fixed message only", body)
	}
}
