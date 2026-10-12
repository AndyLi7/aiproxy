package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
)

const (
	usageQueryTimeout = 10 * time.Second
	// maxUsageSpan bounds a usage window. The website's longest custom range
	// is 366 local days, which stays below it even with a DST hour added.
	maxUsageSpan     = 400 * 24 * time.Hour
	maxUsageTokenIDs = 100
)

// Filters that arrive with the P4 dashboard and the P5 API. Until then a
// caller never gets unfiltered totals for a filter it asked for.
var unsupportedUsageCallParameters = []string{"bucket", "tz", "token_id", "source", "model_id"}

// parseUsageWindow reads the required start/end (Unix ms): 0 <= start < end,
// end - start <= 400 days, and start at most 400 days before now (see
// maxWindowAge). The gateway takes absolute instants only; the website
// computes calendar boundaries in the browser's time zone (D9).
func parseUsageWindow(c *gin.Context, now time.Time) (time.Time, time.Time, error) {
	rawStart, hasStart := c.GetQuery("start")
	rawEnd, hasEnd := c.GetQuery("end")

	if !hasStart || !hasEnd {
		return time.Time{}, time.Time{}, errors.New("start and end are required")
	}

	startMS, err := strconv.ParseInt(strings.TrimSpace(rawStart), 10, 64)
	if err != nil || startMS < 0 {
		return time.Time{}, time.Time{}, errors.New("invalid start parameter")
	}

	if windowStartTooOld(startMS, now) {
		return time.Time{}, time.Time{}, errWindowTooOld
	}

	endMS, err := strconv.ParseInt(strings.TrimSpace(rawEnd), 10, 64)
	if err != nil || endMS < 0 {
		return time.Time{}, time.Time{}, errors.New("invalid end parameter")
	}

	if startMS >= endMS {
		return time.Time{}, time.Time{}, errors.New("start must be before end")
	}

	if endMS-startMS > maxUsageSpan.Milliseconds() {
		return time.Time{}, time.Time{}, errors.New("the window may span at most 400 days")
	}

	return time.UnixMilli(startMS), time.UnixMilli(endMS), nil
}

func parseUsageTokenIDs(c *gin.Context) ([]int, error) {
	raw := strings.TrimSpace(c.Query("token_ids"))
	if raw == "" {
		return nil, errors.New("token_ids is required")
	}

	parts := strings.Split(raw, ",")
	if len(parts) > maxUsageTokenIDs {
		return nil, fmt.Errorf("token_ids may list at most %d keys", maxUsageTokenIDs)
	}

	ids := make([]int, 0, len(parts))
	seen := make(map[int]struct{}, len(parts))

	for _, part := range parts {
		value, err := strconv.ParseInt(strings.TrimSpace(part), 10, 32)
		if err != nil || value <= 0 {
			return nil, errors.New("invalid token_ids parameter")
		}

		id := int(value)
		if _, duplicate := seen[id]; duplicate {
			return nil, errors.New("duplicate token id in token_ids")
		}

		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	return ids, nil
}

func usageQueryFailed(ctx context.Context, c *gin.Context, err error) {
	// Drivers do not all wrap the context error, so the context decides.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		middleware.ErrorResponse(c, http.StatusServiceUnavailable, "usage query timed out")
		return
	}

	common.GetLogger(c).Errorf("read usage: %v", err)
	middleware.ErrorResponse(c, http.StatusInternalServerError, "unable to read usage")
}

func logUsageQueryDuration(c *gin.Context, startedAt time.Time) {
	common.GetLogger(c).Data["usage_query_ms"] = time.Since(startedAt).Milliseconds()
}

// GetGroupUsageCalls godoc
//
//	@Summary		Group call totals
//	@Description	Counts the group's calls in [start, end) by status (succeeded, failed, rejected, processing, unknown). Polls, downloads and internal demo requests are not calls.
//	@Tags			usage
//	@Produce		json
//	@Security		ApiKeyAuth
//	@Param			group		path		string	true	"Group name"
//	@Param			start		query		int		true	"Window start, Unix milliseconds; at most 400 days before now"
//	@Param			end			query		int		true	"Window end (exclusive), Unix milliseconds; at most 400 days after start"
//	@Param			group_by	query		string	false	"source: also return totals per request source"
//	@Success		200			{object}	middleware.APIResponse{data=map[string]any}
//	@Router			/api/usage/{group}/calls [get]
func GetGroupUsageCalls(c *gin.Context) {
	group := c.Param("group")
	if group == "" {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid group parameter")
		return
	}

	for _, name := range unsupportedUsageCallParameters {
		if _, present := c.GetQuery(name); present {
			middleware.ErrorResponse(
				c,
				http.StatusBadRequest,
				name+" is not supported in this version",
			)

			return
		}
	}

	groupBy := c.Query("group_by")
	if groupBy != "" && groupBy != "source" {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid group_by parameter")
		return
	}

	now := time.Now()

	start, end, err := parseUsageWindow(c, now)
	if err != nil {
		middleware.ErrorResponse(c, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), usageQueryTimeout)
	defer cancel()

	startedAt := time.Now()
	totals, err := model.GetGroupCallTotals(ctx, group, start, end, now)

	logUsageQueryDuration(c, startedAt)

	if err != nil {
		usageQueryFailed(ctx, c, err)
		return
	}

	response := gin.H{
		"start":  start.UnixMilli(),
		"end":    end.UnixMilli(),
		"as_of":  now.UnixMilli(),
		"totals": totals.Totals,
	}
	if groupBy == "source" {
		response["by_source"] = totals.BySource
	}

	middleware.SuccessResponse(c, response)
}

// GetGroupUsageKeys godoc
//
//	@Summary		Per-key call stats
//	@Description	For each requested key: calls in [start, end) by status, plus the time and key name of its latest call at any time. One entry per requested id, in request order; a key of another group returns zeros. last_request_unavailable marks a key without calls in the window whose older history could not be read in time (its last call is then unknown, not absent).
//	@Tags			usage
//	@Produce		json
//	@Security		ApiKeyAuth
//	@Param			group		path		string	true	"Group name"
//	@Param			token_ids	query		string	true	"Comma-separated key ids (1-100, distinct)"
//	@Param			start		query		int		true	"Window start, Unix milliseconds; at most 400 days before now"
//	@Param			end			query		int		true	"Window end (exclusive), Unix milliseconds; at most 400 days after start"
//	@Success		200			{object}	middleware.APIResponse{data=map[string]any}
//	@Router			/api/usage/{group}/keys [get]
func GetGroupUsageKeys(c *gin.Context) {
	group := c.Param("group")
	if group == "" {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid group parameter")
		return
	}

	tokenIDs, err := parseUsageTokenIDs(c)
	if err != nil {
		middleware.ErrorResponse(c, http.StatusBadRequest, err.Error())
		return
	}

	now := time.Now()

	start, end, err := parseUsageWindow(c, now)
	if err != nil {
		middleware.ErrorResponse(c, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), usageQueryTimeout)
	defer cancel()

	startedAt := time.Now()
	keys, err := model.GetGroupKeyCallStats(ctx, group, tokenIDs, start, end, now)

	logUsageQueryDuration(c, startedAt)

	if err != nil {
		usageQueryFailed(ctx, c, err)
		return
	}

	middleware.SuccessResponse(c, gin.H{
		"start": start.UnixMilli(),
		"end":   end.UnixMilli(),
		"as_of": now.UnixMilli(),
		"keys":  keys,
	})
}
