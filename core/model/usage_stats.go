package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// CallCounts are one group's call totals by status (owner decision D1).
// Requests always equals the sum of the five statuses.
type CallCounts struct {
	Requests      int64  `json:"requests"`
	Succeeded     int64  `json:"succeeded"`
	Failed        int64  `json:"failed"`
	Rejected      int64  `json:"rejected"`
	Processing    int64  `json:"processing"`
	Unknown       int64  `json:"unknown"`
	LastRequestAt *int64 `json:"last_request_at"`
}

func (c *CallCounts) add(other CallCounts) {
	c.Requests += other.Requests
	c.Succeeded += other.Succeeded
	c.Failed += other.Failed
	c.Rejected += other.Rejected
	c.Processing += other.Processing
	c.Unknown += other.Unknown

	if other.LastRequestAt != nil &&
		(c.LastRequestAt == nil || *other.LastRequestAt > *c.LastRequestAt) {
		last := *other.LastRequestAt
		c.LastRequestAt = &last
	}
}

// SourceCallCounts are the calls of one request source (api or playground).
type SourceCallCounts struct {
	Source string `json:"source"`
	CallCounts
}

// GroupCallTotals are the calls of a group in [Start, End) as of AsOf.
type GroupCallTotals struct {
	Totals   CallCounts
	BySource []SourceCallCounts
}

// KeyCallStats are one key's calls in the window plus its latest call ever.
type KeyCallStats struct {
	TokenID int `json:"token_id"`
	CallCounts
	LastTokenName string `json:"last_token_name"`
	// LastRequestUnavailable: the key has no call in the window and its older
	// history could not be read in time, so LastRequestAt is unknown rather
	// than "never used". The counts are still exact.
	LastRequestUnavailable bool `json:"last_request_unavailable,omitempty"`
}

// A key without a call in the window looks for its newest older call. P1 has
// only the token_id index for that, so the statement reads and sorts every
// row the key ever wrote: slow for an idle key that was once busy. Each such
// search gets keyFallbackTimeout and all of them together keyFallbackBudget;
// a key that runs out reports LastRequestUnavailable instead of failing every
// key of the response. Remove with P2's (group_id, token_id, request_at DESC,
// id DESC) index.
const (
	keyFallbackTimeout = 2 * time.Second
	keyFallbackBudget  = 4 * time.Second
)

// callCountColumns aggregates the status column st of the call subquery.
const callCountColumns = "COUNT(*) AS requests," +
	" SUM(CASE WHEN st = 'succeeded' THEN 1 ELSE 0 END) AS succeeded," +
	" SUM(CASE WHEN st = 'failed' THEN 1 ELSE 0 END) AS failed," +
	" SUM(CASE WHEN st = 'rejected' THEN 1 ELSE 0 END) AS rejected," +
	" SUM(CASE WHEN st = 'processing' THEN 1 ELSE 0 END) AS processing," +
	" SUM(CASE WHEN st = 'unknown' THEN 1 ELSE 0 END) AS unknown," +
	" MAX(request_at) AS last_request_at"

// groupCallsInWindow selects the group's calls with request_at in [start, end)
// as rows of (columns..., request_at, st). created_at >= start is the index
// lower bound (every writer sets created_at at or after request_at), so the
// scan uses idx_group_creat. With unit tasks, SUPERSEDED rows are left out
// (see CallUnit); the status columns are unchanged, so requests is still the
// sum of the five.
func groupCallsInWindow(
	db *gorm.DB,
	group string,
	start, end, now time.Time,
	unit CallUnit,
	columns string,
	columnArgs ...any,
) *gorm.DB {
	statusSQL, statusArgs := callStatusExpr(now)
	callSQL, callArgs := isCallExpr()

	args := make([]any, 0, len(columnArgs)+len(statusArgs))
	args = append(args, columnArgs...)
	args = append(args, statusArgs...)

	tx := db.Model(&Log{}).
		Select(columns+", request_at, ("+statusSQL+") AS st", args...).
		Where("group_id = ?", group).
		Where("created_at >= ?", start).
		Where("request_at >= ? AND request_at < ?", start, end).
		Where("("+callSQL+")", callArgs...)

	if unit == CallUnitTasks {
		supersededSQL, supersededArgs := supersededExpr()
		tx = tx.Where("NOT "+supersededSQL, supersededArgs...)
	}

	return tx
}

// GetGroupCallTotals counts the group's calls by status in [start, end), with
// one row per request source; an empty request source counts as api. unit
// selects requests (every call row) or tasks (see CallUnit).
func GetGroupCallTotals(
	ctx context.Context,
	group string,
	start, end, now time.Time,
	unit CallUnit,
) (*GroupCallTotals, error) {
	if group == "" {
		return nil, errors.New("group is required")
	}

	db := LogDB.WithContext(ctx)
	calls := groupCallsInWindow(
		db,
		group,
		start,
		end,
		now,
		unit,
		"COALESCE(NULLIF(request_source, ''), ?) AS src",
		RequestSourceAPI,
	)

	rows, err := db.Table("(?) AS t", calls).
		Select("src, " + callCountColumns).
		Group("src").
		Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := &GroupCallTotals{BySource: []SourceCallCounts{}}

	for rows.Next() {
		var (
			source string
			counts CallCounts
		)

		if err := scanCallCounts(rows, &source, &counts); err != nil {
			return nil, err
		}

		result.BySource = append(
			result.BySource,
			SourceCallCounts{Source: source, CallCounts: counts},
		)
		result.Totals.add(counts)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	slices.SortFunc(result.BySource, func(a, b SourceCallCounts) int {
		return strings.Compare(a.Source, b.Source)
	})

	return result, nil
}

// GetGroupKeyCallStats returns one entry per requested key, in request order:
// its calls in [start, end) and its latest call at any time. A key of another
// group gets zeros, because every statement is scoped to the group. unit
// applies to the window counts only; the latest call stays per row.
func GetGroupKeyCallStats(
	ctx context.Context,
	group string,
	tokenIDs []int,
	start, end, now time.Time,
	unit CallUnit,
) ([]KeyCallStats, error) {
	if group == "" {
		return nil, errors.New("group is required")
	}

	db := LogDB.WithContext(ctx)
	stats := make(map[int]*KeyCallStats, len(tokenIDs))

	for _, tokenID := range tokenIDs {
		stats[tokenID] = &KeyCallStats{TokenID: tokenID}
	}

	if len(tokenIDs) > 0 {
		if err := readKeyWindowCounts(
			db,
			group,
			tokenIDs,
			start,
			end,
			now,
			unit,
			stats,
		); err != nil {
			return nil, err
		}
	}

	callSQL, callArgs := isCallExpr()
	latest := func(tx *gorm.DB, tokenID int, recent bool) (*Log, error) {
		query := tx.Model(&Log{}).
			Select("id", "request_at", "token_name").
			Where("group_id = ? AND token_id = ?", group, tokenID).
			Where("("+callSQL+")", callArgs...)
		if recent {
			query = query.Where("created_at >= ? AND request_at >= ?", start, start)
		} else {
			query = query.Where("request_at < ?", start)
		}

		var rows []Log
		if err := query.Order("request_at DESC, id DESC").Limit(1).Find(&rows).Error; err != nil {
			return nil, err
		}

		if len(rows) == 0 {
			return nil, nil
		}

		return &rows[0], nil
	}

	// olderLatest reads a key's newest call before the window under its own
	// deadline (see keyFallbackTimeout).
	fallbackDeadline := time.Now().Add(keyFallbackBudget)
	olderLatest := func(tokenID int) (*Log, error) {
		remaining := time.Until(fallbackDeadline)
		if remaining <= 0 {
			return nil, context.DeadlineExceeded
		}

		fallbackCtx, cancel := context.WithTimeout(ctx, min(keyFallbackTimeout, remaining))
		defer cancel()

		return latest(db.WithContext(fallbackCtx), tokenID, false)
	}

	result := make([]KeyCallStats, 0, len(tokenIDs))

	for _, tokenID := range tokenIDs {
		entry := stats[tokenID]

		// Latest call since the window start (bounded by the index); only a key
		// without one reads its older history.
		last, err := latest(db, tokenID, true)
		if err != nil {
			return nil, err
		}

		if last == nil {
			last, err = olderLatest(tokenID)
			if err != nil {
				// The request itself ran out: no partial answer.
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, ctxErr
				}

				log.Warnf(
					"usage: last call of key %d before the window is unavailable: %v",
					tokenID,
					err,
				)

				entry.LastRequestUnavailable = true
			}
		}

		if last != nil {
			lastMS := last.RequestAt.UnixMilli()
			entry.LastRequestAt = &lastMS
			entry.LastTokenName = last.TokenName
		}

		result = append(result, *entry)
	}

	return result, nil
}

// readKeyWindowCounts reads and closes the window counts before the per-key
// latest-call statements run, so a single-connection pool cannot deadlock.
func readKeyWindowCounts(
	db *gorm.DB,
	group string,
	tokenIDs []int,
	start, end, now time.Time,
	unit CallUnit,
	stats map[int]*KeyCallStats,
) error {
	calls := groupCallsInWindow(db, group, start, end, now, unit, "token_id").
		Where("token_id IN ?", tokenIDs)

	rows, err := db.Table("(?) AS t", calls).
		Select("token_id, " + callCountColumns).
		Group("token_id").
		Rows()
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			tokenID int
			counts  CallCounts
		)

		if err := scanCallCounts(rows, &tokenID, &counts); err != nil {
			return err
		}

		if entry, ok := stats[tokenID]; ok {
			entry.CallCounts = counts
		}
	}

	return rows.Err()
}

func scanCallCounts(rows *sql.Rows, key any, counts *CallCounts) error {
	var (
		succeeded, failed, rejected, processing, unknown sql.NullInt64
		last                                             scannedTime
	)

	if err := rows.Scan(
		key,
		&counts.Requests,
		&succeeded,
		&failed,
		&rejected,
		&processing,
		&unknown,
		&last,
	); err != nil {
		return err
	}

	counts.Succeeded = succeeded.Int64
	counts.Failed = failed.Int64
	counts.Rejected = rejected.Int64
	counts.Processing = processing.Int64
	counts.Unknown = unknown.Int64

	if last.Valid {
		lastMS := last.Time.UnixMilli()
		counts.LastRequestAt = &lastMS
	}

	return nil
}

// scannedTime reads an aggregate timestamp. Postgres returns time.Time;
// SQLite returns the stored text, which carries the writer's UTC offset.
type scannedTime struct {
	Time  time.Time
	Valid bool
}

var scannedTimeLayouts = []string{
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02T15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999 -0700 MST",
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
}

func (t *scannedTime) Scan(value any) error {
	switch v := value.(type) {
	case nil:
		t.Time, t.Valid = time.Time{}, false
		return nil
	case time.Time:
		t.Time, t.Valid = v, true
		return nil
	case string:
		return t.parse(v)
	case []byte:
		return t.parse(string(v))
	default:
		return fmt.Errorf("unsupported timestamp value %T", value)
	}
}

func (t *scannedTime) parse(value string) error {
	for _, layout := range scannedTimeLayouts {
		parsed, err := time.ParseInLocation(layout, value, time.Local)
		if err == nil {
			t.Time, t.Valid = parsed, true
			return nil
		}
	}

	return fmt.Errorf("unsupported timestamp %q", value)
}
