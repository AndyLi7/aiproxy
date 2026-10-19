//nolint:testpackage // Usage handlers are exercised with their internal helpers.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func serveUsage(
	t *testing.T,
	handler gin.HandlerFunc,
	path string,
	query url.Values,
) (int, []byte) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "group", Value: "g"}}
	c.Request = httptest.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		path+"?"+query.Encode(),
		nil,
	)

	handler(c)

	return w.Code, w.Body.Bytes()
}

func TestUsageEndpointsRejectInvalidParameters(t *testing.T) {
	newLogTestDB(t)

	const day = int64(24 * 3600 * 1000)

	// Windows inside the last 400 days, where every window must start.
	recent := time.Now().Add(-30 * 24 * time.Hour).UnixMilli()
	at := func(offset int64) string { return strconv.FormatInt(recent+offset, 10) }
	window := func(extra string) string {
		return "start=" + at(0) + "&end=" + at(30*day) + extra
	}
	// 400 days ago, plus a minute so the request still lands inside the limit.
	oldest := time.Now().Add(-400*24*time.Hour + time.Minute).UnixMilli()
	tooLong := "start=" + strconv.FormatInt(
		oldest,
		10,
	) + "&end=" + strconv.FormatInt(
		oldest+400*day+1,
		10,
	)
	// A one-day window from 1970: narrow, but it would scan the whole history.
	tooOld := "start=0&end=86400000"
	hundredOne := make([]string, 0, 101)

	for id := 1; id <= 101; id++ {
		hundredOne = append(hundredOne, strconv.Itoa(id))
	}

	calls := []string{
		"",
		"start=" + at(0),
		"end=" + at(30*day),
		"start=abc&end=" + at(30*day),
		"start=-1&end=" + at(30*day),
		"start=" + at(30*day) + "&end=" + at(30*day),
		"start=" + at(30*day+1) + "&end=" + at(30*day),
		tooLong,
		tooOld,
		"start=" + strconv.FormatInt(
			time.Now().Add(-401*24*time.Hour).UnixMilli(),
			10,
		) + "&end=" + at(
			0,
		),
		window("&group_by=model"),
		window("&bucket=day"),
		window("&tz=Asia/Shanghai"),
		window("&token_id=3"),
		window("&source=api"),
		window("&model_id=gpt-4o"),
	}
	for _, rawQuery := range calls {
		t.Run("calls "+rawQuery, func(t *testing.T) {
			query, err := url.ParseQuery(rawQuery)
			require.NoError(t, err)

			code, body := serveUsage(t, GetGroupUsageCalls, "/api/usage/g/calls", query)
			require.Equal(t, http.StatusBadRequest, code, string(body))
		})
	}

	keys := []string{
		window(""),
		window("&token_ids="),
		window("&token_ids=0"),
		window("&token_ids=3,0"),
		window("&token_ids=3,abc"),
		window("&token_ids=3,3"),
		window("&token_ids=" + strings.Join(hundredOne, ",")),
		"token_ids=3",
		"token_ids=3&" + tooLong,
		"token_ids=3&" + tooOld,
	}
	for _, rawQuery := range keys {
		t.Run("keys "+rawQuery, func(t *testing.T) {
			query, err := url.ParseQuery(rawQuery)
			require.NoError(t, err)

			code, body := serveUsage(t, GetGroupUsageKeys, "/api/usage/g/keys", query)
			require.Equal(t, http.StatusBadRequest, code, string(body))
		})
	}

	exactly400Days := "start=" + strconv.FormatInt(
		oldest,
		10,
	) + "&end=" + strconv.FormatInt(
		oldest+400*day,
		10,
	)

	code, body := serveUsage(
		t,
		GetGroupUsageCalls,
		"/api/usage/g/calls",
		mustQuery(t, exactly400Days+"&group_by=source"),
	)
	require.Equal(t, http.StatusOK, code, string(body))

	code, body = serveUsage(
		t,
		GetGroupUsageKeys,
		"/api/usage/g/keys",
		mustQuery(t, exactly400Days+"&token_ids="+strings.Join(hundredOne[:100], ",")),
	)
	require.Equal(t, http.StatusOK, code, string(body))
}

func mustQuery(t *testing.T, rawQuery string) url.Values {
	t.Helper()

	query, err := url.ParseQuery(rawQuery)
	require.NoError(t, err)

	return query
}

type usageCallsResponse struct {
	Data struct {
		Start    int64            `json:"start"`
		End      int64            `json:"end"`
		AsOf     int64            `json:"as_of"`
		Totals   model.CallCounts `json:"totals"`
		BySource []struct {
			Source string `json:"source"`
			model.CallCounts
		} `json:"by_source"`
	} `json:"data"`
}

type usageKeysResponse struct {
	Data struct {
		Start int64                `json:"start"`
		End   int64                `json:"end"`
		AsOf  int64                `json:"as_of"`
		Keys  []model.KeyCallStats `json:"keys"`
	} `json:"data"`
}

// The customer list, the key page and the account overview read the same
// calls: for one key and one window, list total = keys requests, and the
// group's calls total sums every key.
func TestUsageCountsAgreeWithTheCallList(t *testing.T) {
	db := newLogTestDB(t)
	now := time.Now()
	seedGroupLogs(t, db, now)

	start := now.Add(-30 * 24 * time.Hour)
	window := url.Values{"start": {ms(start)}, "end": {ms(now.Add(time.Minute))}}

	keys := url.Values{"token_ids": {"3,4,9"}}
	maps.Copy(keys, window)

	code, body := serveUsage(t, GetGroupUsageKeys, "/api/usage/g/keys", keys)
	require.Equal(t, http.StatusOK, code, string(body))

	var keyStats usageKeysResponse
	require.NoError(t, json.Unmarshal(body, &keyStats))
	require.Len(t, keyStats.Data.Keys, 3)
	require.Equal(t, []int{3, 4, 9}, []int{
		keyStats.Data.Keys[0].TokenID, keyStats.Data.Keys[1].TokenID, keyStats.Data.Keys[2].TokenID,
	})
	// Key 9 belongs to another group: zeros, no last use.
	require.Zero(t, keyStats.Data.Keys[2].Requests)
	require.Nil(t, keyStats.Data.Keys[2].LastRequestAt)
	require.Equal(t, "prod", keyStats.Data.Keys[0].LastTokenName)

	var sum int64

	for _, key := range keyStats.Data.Keys[:2] {
		list := url.Values{"kind": {"calls"}, "token_id": {strconv.Itoa(key.TokenID)}}
		maps.Copy(list, window)

		code, page, listBody := serveGroupLogs(t, list)
		require.Equal(t, http.StatusOK, code, listBody)
		require.Equal(t, page.Total, key.Requests, "key %d", key.TokenID)
		require.Equal(
			t,
			key.Requests,
			key.Succeeded+key.Failed+key.Rejected+key.Processing+key.Unknown,
		)

		sum += key.Requests
	}

	callsQuery := url.Values{"group_by": {"source"}}
	maps.Copy(callsQuery, window)

	code, body = serveUsage(t, GetGroupUsageCalls, "/api/usage/g/calls", callsQuery)
	require.Equal(t, http.StatusOK, code, string(body))

	var calls usageCallsResponse
	require.NoError(t, json.Unmarshal(body, &calls))
	require.Equal(t, sum, calls.Data.Totals.Requests)
	require.EqualValues(t, 6, calls.Data.Totals.Requests)
	require.EqualValues(t, 3, calls.Data.Totals.Succeeded)
	require.EqualValues(t, 1, calls.Data.Totals.Failed)
	require.EqualValues(t, 1, calls.Data.Totals.Rejected)
	require.EqualValues(t, 1, calls.Data.Totals.Processing)
	require.Equal(t, ms(start), strconv.FormatInt(calls.Data.Start, 10))
	require.GreaterOrEqual(t, calls.Data.AsOf, now.UnixMilli())
	require.Len(t, calls.Data.BySource, 1)
	require.Equal(t, "api", calls.Data.BySource[0].Source)

	// Without group_by the per-source rows are not returned.
	code, body = serveUsage(t, GetGroupUsageCalls, "/api/usage/g/calls", window)
	require.Equal(t, http.StatusOK, code, string(body))
	require.NotContains(t, string(body), "by_source")
}

// failOlderHistoryOf makes the search for one key's newest call before the
// window fail, as a timeout of that slow statement would.
func failOlderHistoryOf(t *testing.T, db *gorm.DB, tokenID int) {
	t.Helper()

	name := "test:fail_older_history:" + t.Name()
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(name, func(tx *gorm.DB) {
		where, ok := tx.Statement.Clauses["WHERE"].Expression.(clause.Where)
		if !ok {
			return
		}

		older, forKey := false, false

		for _, expression := range where.Exprs {
			expr, ok := expression.(clause.Expr)
			if !ok {
				continue
			}

			if expr.SQL == "request_at < ?" {
				older = true
			}

			if expr.SQL == "group_id = ? AND token_id = ?" && len(expr.Vars) == 2 &&
				expr.Vars[1] == tokenID {
				forKey = true
			}
		}

		if older && forKey {
			_ = tx.AddError(errors.New("injected: older history unavailable"))
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Query().Remove(name) })
}

// One idle key whose older history cannot be read used to fail the whole
// response, blanking calls, success rate and last use of every key.
func TestUsageKeysSurviveOneKeyWithoutOlderHistory(t *testing.T) {
	db := newLogTestDB(t)
	now := time.Now()

	seed := func(tokenID int, name string, code int, stage model.FailureStage, age time.Duration) {
		at := now.Add(-age)
		require.NoError(t, db.Create(&model.Log{
			GroupID: "g", TokenID: tokenID, TokenName: name, Code: code, FailureStage: stage,
			Mode: int(mode.ChatCompletions), Endpoint: "/v1/chat/completions",
			RequestAt: at, CreatedAt: at.Add(time.Second),
		}).Error)
	}
	seed(3, "busy", 200, model.FailureStageNone, time.Hour)
	seed(3, "busy", 502, model.FailureStageUpstream, 2*time.Hour)
	seed(5, "formerly-busy", 200, model.FailureStageNone, 90*24*time.Hour)
	seed(6, "idle", 200, model.FailureStageNone, 60*24*time.Hour)

	failOlderHistoryOf(t, db, 5)

	code, body := serveUsage(t, GetGroupUsageKeys, "/api/usage/g/keys", url.Values{
		"token_ids": {"3,5,6"},
		"start":     {ms(now.Add(-30 * 24 * time.Hour))},
		"end":       {ms(now)},
	})
	require.Equal(t, http.StatusOK, code, string(body))

	var page usageKeysResponse
	require.NoError(t, json.Unmarshal(body, &page))
	require.Len(t, page.Data.Keys, 3)

	busy, formerlyBusy, idle := page.Data.Keys[0], page.Data.Keys[1], page.Data.Keys[2]
	require.EqualValues(t, 2, busy.Requests)
	require.EqualValues(t, 1, busy.Succeeded)
	require.EqualValues(t, 1, busy.Failed)
	require.Equal(t, "busy", busy.LastTokenName)
	require.False(t, busy.LastRequestUnavailable)

	// Unknown, not "never used"; the window counts are still exact.
	require.Zero(t, formerlyBusy.Requests)
	require.Nil(t, formerlyBusy.LastRequestAt)
	require.Empty(t, formerlyBusy.LastTokenName)
	require.True(t, formerlyBusy.LastRequestUnavailable)
	require.Contains(t, string(body), `"last_request_unavailable":true`)

	require.NotNil(t, idle.LastRequestAt)
	require.Equal(t, now.Add(-60*24*time.Hour).UnixMilli(), *idle.LastRequestAt)
	require.Equal(t, "idle", idle.LastTokenName)
	require.False(t, idle.LastRequestUnavailable)
}
