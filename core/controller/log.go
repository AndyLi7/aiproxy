package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/controller/utils"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"gorm.io/gorm"
)

const maxExcludedLogModes = 32

func parseExcludedModes(c *gin.Context) ([]int, error) {
	raw, present := c.GetQuery("exclude_modes")
	if !present {
		return nil, nil
	}

	parts := strings.Split(raw, ",")
	if len(parts) == 0 || len(parts) > maxExcludedLogModes {
		return nil, errors.New("invalid excluded mode count")
	}

	modes := make([]int, 0, len(parts))

	seen := make(map[int]struct{}, len(parts))
	for _, part := range parts {
		value, err := strconv.ParseInt(strings.TrimSpace(part), 10, 32)
		if err != nil || value <= 0 {
			return nil, errors.New("invalid excluded mode")
		}

		mode := int(value)
		if _, exists := seen[mode]; exists {
			return nil, errors.New("duplicate excluded mode")
		}

		seen[mode] = struct{}{}
		modes = append(modes, mode)
	}

	return modes, nil
}

// parseOperationalLogFilter reads the filters of the admin request log
// (/api/logs/ and /api/logs/search).
func parseOperationalLogFilter(c *gin.Context) (model.OperationalLogFilter, error) {
	status, err := model.ParseCallStatus(c.Query("status"))
	if err != nil {
		return model.OperationalLogFilter{}, err
	}

	filter := model.OperationalLogFilter{
		Status: status,
		Source: strings.TrimSpace(c.Query("request_source")),
		Kind:   model.AdminLogKind(c.Query("kind")),
		Now:    time.Now(),
	}

	if !model.ValidRequestSource(filter.Source) {
		return model.OperationalLogFilter{}, fmt.Errorf("invalid request source %q", filter.Source)
	}

	if !filter.Kind.Valid() {
		return model.OperationalLogFilter{}, fmt.Errorf("invalid kind %q", filter.Kind)
	}

	if !filter.Kind.AllowsStatus(filter.Status) {
		return model.OperationalLogFilter{}, fmt.Errorf("status %q is not valid with kind %q", filter.Status, filter.Kind)
	}

	rawChannels := strings.TrimSpace(c.Query("channels"))
	if rawChannels == "" {
		return filter, nil
	}

	for rawChannelID := range strings.SplitSeq(rawChannels, ",") {
		channelID, err := strconv.Atoi(strings.TrimSpace(rawChannelID))
		if err != nil || channelID <= 0 {
			return model.OperationalLogFilter{}, fmt.Errorf("invalid channel ID %q", rawChannelID)
		}

		filter.ChannelIDs = append(filter.ChannelIDs, channelID)
	}

	return filter, nil
}

func parseCommonParams(c *gin.Context) (params struct {
	group         string
	tokenName     string
	modelName     string
	channelID     int
	tokenID       int
	order         string
	requestID     string
	upstreamID    string
	codeType      string
	code          int
	includeDetail bool
	ip            string
	user          string
},
) {
	params.group = c.Query("group")
	params.tokenName = c.Query("token_name")
	params.modelName = c.Query("model_name")
	params.channelID, _ = strconv.Atoi(c.Query("channel"))
	params.tokenID, _ = strconv.Atoi(c.Query("token_id"))
	params.order = c.Query("order")
	params.requestID = c.Query("request_id")
	params.upstreamID = c.Query("upstream_id")
	params.codeType = c.Query("code_type")
	params.code, _ = strconv.Atoi(c.Query("code"))
	params.includeDetail, _ = strconv.ParseBool(c.Query("include_detail"))
	params.ip = c.Query("ip")
	params.user = c.Query("user")

	return params
}

// GetLogs godoc
//
//	@Summary		Get all logs
//	@Description	Returns a paginated list of all logs with optional filters
//	@Tags			logs
//	@Produce		json
//	@Security		ApiKeyAuth
//	@Param			page			query		int		false	"Page number"
//	@Param			per_page		query		int		false	"Items per page"
//	@Param			start_timestamp	query		int		false	"Start timestamp (milliseconds)"
//	@Param			end_timestamp	query		int		false	"End timestamp (milliseconds)"
//	@Param			model_name		query		string	false	"Model name"
//	@Param			channel			query		int		false	"Channel ID"
//	@Param			channels		query		string	false	"Comma-separated channel IDs"
//	@Param			status			query		string	false	"Call status: succeeded (alias success), failed, rejected, processing, unknown"
//	@Param			request_source	query		string	false	"Request source: api, playground, admin_demo"
//	@Param			kind			query		string	false	"calls (recognised key, not a read; trial and admin demo rows included), reads (polls, downloads, other reads and requests whose key was not recognised), tasks (one row per generation task; status may not be rejected), non_task (every other row; no status) or all (default); see model.AdminLogKind. tasks and non_task answer 503 task_view_unavailable until log_task_view_v1 is announced"
//	@Param			order			query		string	false	"Order"
//	@Param			request_id		query		string	false	"Request ID"
//	@Param			upstream_id		query		string	false	"Upstream ID"
//	@Param			code_type		query		string	false	"Status code type"
//	@Param			code			query		int		false	"Status code"
//	@Param			include_detail	query		bool	false	"Include request and response detail"
//	@Param			ip				query		string	false	"IP"
//	@Param			user			query		string	false	"User"
//	@Success		200				{object}	middleware.APIResponse{data=model.GetLogsResult}
//	@Router			/api/logs/ [get]
func GetLogs(c *gin.Context) {
	page, perPage := utils.ParsePageParams(c)
	startTime, endTime := utils.ParseTimeRange(c, 0)
	params := parseCommonParams(c)

	operationalFilter, err := parseOperationalLogFilter(c)
	if err != nil {
		middleware.ErrorResponse(c, http.StatusBadRequest, err.Error())
		return
	}

	if operationalFilter.Kind.TaskView() && !taskViewStorage.ready(model.LogDB) {
		taskViewUnavailable(c)
		return
	}

	result, err := model.GetLogs(
		startTime,
		endTime,
		params.modelName,
		params.requestID,
		params.upstreamID,
		params.channelID,
		operationalFilter,
		params.order,
		model.CodeType(params.codeType),
		params.code,
		params.includeDetail,
		params.ip,
		params.user,
		page,
		perPage,
	)
	if err != nil {
		middleware.ErrorResponse(c, http.StatusInternalServerError, err.Error())
		return
	}

	middleware.SuccessResponse(c, result)
}

const (
	maxLogModelIDLength = 256
	logQueryTimeout     = 10 * time.Second
	// maxWindowAge bounds how far back a window (log list and usage counts)
	// may start. Until the request_at indexes of key usage P2 exist,
	// created_at >= start is the only index bound of a window scan, so an
	// older start would read the group's whole history from that point on,
	// however narrow the window. The website's oldest start is 398 days back.
	maxWindowAge = 400 * 24 * time.Hour
)

var (
	errMixedLogTimeParameters = errors.New(
		"start and end cannot be combined with start_timestamp or end_timestamp",
	)
	errWindowTooOld = errors.New("start may be at most 400 days before now")
)

// windowStartTooOld reports whether a window starting at startMS (Unix ms)
// begins more than maxWindowAge before now.
func windowStartTooOld(startMS int64, now time.Time) bool {
	return startMS < now.Add(-maxWindowAge).UnixMilli()
}

// parseOptionalPositiveInt reads a query value that, when present, must be a
// positive integer. A malformed value is an error rather than "no filter".
func parseOptionalPositiveInt(c *gin.Context, name string) (int, bool, error) {
	raw, present := c.GetQuery(name)
	if !present {
		return 0, false, nil
	}

	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 32)
	if err != nil || value <= 0 {
		return 0, true, fmt.Errorf("invalid %s parameter", name)
	}

	return int(value), true, nil
}

// parseLogWindow reads start/end (Unix ms). Window mode starts with start,
// at most 400 days before now; end defaults to now and must be after start.
// There is no other limit on the span.
func parseLogWindow(c *gin.Context, now time.Time) (bool, time.Time, time.Time, error) {
	rawStart, hasStart := c.GetQuery("start")
	rawEnd, hasEnd := c.GetQuery("end")
	_, hasLegacyStart := c.GetQuery("start_timestamp")
	_, hasLegacyEnd := c.GetQuery("end_timestamp")

	if !hasStart && !hasEnd {
		return false, time.Time{}, time.Time{}, nil
	}

	if hasLegacyStart || hasLegacyEnd {
		return false, time.Time{}, time.Time{}, errMixedLogTimeParameters
	}

	if !hasStart {
		return false, time.Time{}, time.Time{}, errors.New("end requires start")
	}

	startMS, err := strconv.ParseInt(strings.TrimSpace(rawStart), 10, 64)
	if err != nil || startMS < 0 {
		return false, time.Time{}, time.Time{}, errors.New("invalid start parameter")
	}

	if windowStartTooOld(startMS, now) {
		return false, time.Time{}, time.Time{}, errWindowTooOld
	}

	end := now
	if hasEnd {
		endMS, err := strconv.ParseInt(strings.TrimSpace(rawEnd), 10, 64)
		if err != nil || endMS < 0 {
			return false, time.Time{}, time.Time{}, errors.New("invalid end parameter")
		}

		end = time.UnixMilli(endMS)
	}

	start := time.UnixMilli(startMS)
	if !start.Before(end) {
		return false, time.Time{}, time.Time{}, errors.New("start must be before end")
	}

	return true, start, end, nil
}

func parseGroupLogQuery(c *gin.Context, group string, now time.Time) (model.GroupLogQuery, error) {
	page, perPage := utils.ParsePageParams(c)
	params := parseCommonParams(c)

	excludedModes, err := parseExcludedModes(c)
	if err != nil {
		return model.GroupLogQuery{}, errors.New("invalid exclude_modes parameter")
	}

	windowed, start, end, err := parseLogWindow(c, now)
	if err != nil {
		return model.GroupLogQuery{}, err
	}

	logID, _, err := parseOptionalPositiveInt(c, "log_id")
	if err != nil {
		return model.GroupLogQuery{}, err
	}

	tokenID, _, err := parseOptionalPositiveInt(c, "token_id")
	if err != nil {
		return model.GroupLogQuery{}, err
	}

	status, err := model.ParseCallStatus(c.Query("status"))
	if err != nil {
		return model.GroupLogQuery{}, errors.New("invalid status parameter")
	}

	kind := model.GroupLogKind(c.Query("kind"))
	if !kind.Valid() {
		return model.GroupLogQuery{}, errors.New("invalid kind parameter")
	}

	if !kind.AllowsStatus(status) {
		return model.GroupLogQuery{}, errors.New("invalid status parameter for this kind")
	}

	modelID := c.Query("model_id")
	if utf8.RuneCountInString(modelID) > maxLogModelIDLength {
		return model.GroupLogQuery{}, errors.New("invalid model_id parameter")
	}

	query := model.GroupLogQuery{
		Group:         group,
		Windowed:      windowed,
		Start:         start,
		End:           end,
		LogID:         logID,
		ModelName:     params.modelName,
		ModelID:       modelID,
		RequestID:     params.requestID,
		UpstreamID:    params.upstreamID,
		TokenID:       tokenID,
		TokenName:     params.tokenName,
		ExcludedModes: excludedModes,
		Status:        status,
		Kind:          kind,
		Order:         params.order,
		CodeType:      model.CodeType(params.codeType),
		Code:          params.code,
		WithBody:      params.includeDetail,
		IP:            params.ip,
		User:          params.user,
		Page:          page,
		PerPage:       perPage,
		Now:           now,
	}

	if !windowed {
		// Legacy created_at range with the 7-day default and cap; with log_id
		// alone it only bounds the model and key-name summaries.
		query.StartTimestamp, query.EndTimestamp = utils.ParseTimeRange(c, 0)
	}

	return query, nil
}

// GetGroupLogs godoc
//
//	@Summary		Get group logs
//	@Description	Get logs for a specific group. With start (and optional end) the window is request_at in [start, end); start may be at most 400 days before now and the span has no other limit. Otherwise start_timestamp/end_timestamp filter created_at within 7 days. Every log carries its call status.
//	@Tags			log
//	@Produce		json
//	@Security		ApiKeyAuth
//	@Param			group			path		string	true	"Group name"
//	@Param			page			query		int		false	"Page number"
//	@Param			per_page		query		int		false	"Items per page (1-100)"
//	@Param			start			query		int		false	"Window start, Unix milliseconds (request_at >= start); at most 400 days before now"
//	@Param			end				query		int		false	"Window end, Unix milliseconds (request_at < end); defaults to now and requires start"
//	@Param			start_timestamp	query		int		false	"Legacy created_at start (milliseconds); not combinable with start/end"
//	@Param			end_timestamp	query		int		false	"Legacy created_at end (milliseconds); not combinable with start/end"
//	@Param			log_id			query		int		false	"Log ID; without start it is looked up at any age"
//	@Param			status			query		string	false	"Call status: succeeded (alias success), failed, rejected, processing, unknown"
//	@Param			kind			query		string	false	"calls, reads, tasks (one row per generation task; status may not be rejected), non_task (every other row except internal demos; no status) or all (default). tasks and non_task answer 503 task_view_unavailable until log_task_view_v1 is announced"
//	@Param			model_id		query		string	false	"Callable model ID; matches model, or model plus capability split at the last slash"
//	@Param			token_name		query		string	false	"Token name"
//	@Param			model_name		query		string	false	"Model name"
//	@Param			token_id		query		int		false	"Token ID"
//	@Param			exclude_modes	query		string	false	"Comma-separated relay modes to exclude"
//	@Param			order			query		string	false	"Order"
//	@Param			request_id		query		string	false	"Request ID"
//	@Param			upstream_id		query		string	false	"Upstream ID"
//	@Param			code_type		query		string	false	"Status code type"
//	@Param			code			query		int		false	"Status code"
//	@Param			include_detail	query		bool	false	"Include request and response detail"
//	@Param			ip				query		string	false	"IP"
//	@Param			user			query		string	false	"User"
//	@Success		200				{object}	middleware.APIResponse{data=model.GetGroupLogsResult}
//	@Router			/api/log/{group} [get]
func GetGroupLogs(c *gin.Context) {
	group := c.Param("group")
	if group == "" {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid group parameter")
		return
	}

	query, err := parseGroupLogQuery(c, group, time.Now())
	if err != nil {
		middleware.ErrorResponse(c, http.StatusBadRequest, err.Error())
		return
	}

	if query.Kind.TaskView() && !taskViewStorage.ready(model.LogDB) {
		taskViewUnavailable(c)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), logQueryTimeout)
	defer cancel()

	result, err := model.GetGroupLogs(ctx, query)
	if err != nil {
		common.GetLogger(c).Errorf("read group logs: %v", err)
		middleware.ErrorResponse(c, http.StatusInternalServerError, "unable to read logs")

		return
	}

	middleware.SuccessResponse(c, result)
}

// SearchLogs godoc
//
//	@Summary		Search logs
//	@Description	Search logs with various filters
//	@Tags			logs
//	@Produce		json
//	@Security		ApiKeyAuth
//	@Param			keyword			query		string	false	"Keyword"
//	@Param			page			query		int		false	"Page number"
//	@Param			per_page		query		int		false	"Items per page"
//	@Param			start_timestamp	query		int		false	"Start timestamp (milliseconds)"
//	@Param			end_timestamp	query		int		false	"End timestamp (milliseconds)"
//	@Param			model_name		query		string	false	"Filter by model name"
//	@Param			channel			query		int		false	"Filter by channel"
//	@Param			channels		query		string	false	"Comma-separated channel IDs"
//	@Param			status			query		string	false	"Call status: succeeded (alias success), failed, rejected, processing, unknown"
//	@Param			request_source	query		string	false	"Request source: api, playground, admin_demo"
//	@Param			kind			query		string	false	"calls (recognised key, not a read; trial and admin demo rows included), reads (polls, downloads, other reads and requests whose key was not recognised), tasks (one row per generation task; status may not be rejected), non_task (every other row; no status) or all (default); see model.AdminLogKind. tasks and non_task answer 503 task_view_unavailable until log_task_view_v1 is announced"
//	@Param			group			query		string	true	"Group name"
//	@Param			token_id		query		int		false	"Filter by token id"
//	@Param			token_name		query		string	false	"Filter by token name"
//	@Param			order			query		string	false	"Order"
//	@Param			request_id		query		string	false	"Request ID"
//	@Param			upstream_id		query		string	false	"Upstream ID"
//	@Param			code_type		query		string	false	"Status code type"
//	@Param			code			query		int		false	"Status code"
//	@Param			include_detail	query		bool	false	"Include request and response detail"
//	@Param			ip				query		string	false	"IP"
//	@Param			user			query		string	false	"User"
//	@Success		200				{object}	middleware.APIResponse{data=model.GetLogsResult}
//	@Router			/api/logs/search [get]
func SearchLogs(c *gin.Context) {
	page, perPage := utils.ParsePageParams(c)
	startTime, endTime := utils.ParseTimeRange(c, 0)
	params := parseCommonParams(c)

	operationalFilter, err := parseOperationalLogFilter(c)
	if err != nil {
		middleware.ErrorResponse(c, http.StatusBadRequest, err.Error())
		return
	}

	if operationalFilter.Kind.TaskView() && !taskViewStorage.ready(model.LogDB) {
		taskViewUnavailable(c)
		return
	}

	keyword := c.Query("keyword")

	result, err := model.SearchLogs(
		keyword,
		params.requestID,
		params.upstreamID,
		params.group,
		params.tokenID,
		params.tokenName,
		params.modelName,
		startTime,
		endTime,
		params.channelID,
		operationalFilter,
		params.order,
		model.CodeType(params.codeType),
		params.code,
		params.includeDetail,
		params.ip,
		params.user,
		page,
		perPage,
	)
	if err != nil {
		middleware.ErrorResponse(c, http.StatusInternalServerError, err.Error())
		return
	}

	middleware.SuccessResponse(c, result)
}

// SearchGroupLogs godoc
//
//	@Summary		Search group logs
//	@Description	Search logs for a specific group with filters
//	@Tags			log
//	@Produce		json
//	@Security		ApiKeyAuth
//	@Param			group			path		string	true	"Group name"
//	@Param			keyword			query		string	false	"Keyword"
//	@Param			page			query		int		false	"Page number"
//	@Param			per_page		query		int		false	"Items per page"
//	@Param			start_timestamp	query		int		false	"Start timestamp (milliseconds)"
//	@Param			end_timestamp	query		int		false	"End timestamp (milliseconds)"
//	@Param			token_name		query		string	false	"Filter by token name"
//	@Param			model_name		query		string	false	"Filter by model name"
//	@Param			token_id		query		int		false	"Filter by token id"
//	@Param			order			query		string	false	"Order"
//	@Param			request_id		query		string	false	"Request ID"
//	@Param			upstream_id		query		string	false	"Upstream ID"
//	@Param			code_type		query		string	false	"Status code type"
//	@Param			code			query		int		false	"Status code"
//	@Param			include_detail	query		bool	false	"Include request and response detail"
//	@Param			ip				query		string	false	"IP"
//	@Param			user			query		string	false	"User"
//	@Success		200				{object}	middleware.APIResponse{data=model.GetGroupLogsResult}
//	@Router			/api/log/{group}/search [get]
func SearchGroupLogs(c *gin.Context) {
	group := c.Param("group")
	if group == "" {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid group parameter")
		return
	}

	page, perPage := utils.ParsePageParams(c)
	startTime, endTime := utils.ParseTimeRange(c, 0)
	params := parseCommonParams(c)
	keyword := c.Query("keyword")

	result, err := model.SearchGroupLogs(
		group,
		keyword,
		params.requestID,
		params.upstreamID,
		params.tokenID,
		params.tokenName,
		params.modelName,
		startTime,
		endTime,
		params.order,
		model.CodeType(params.codeType),
		params.code,
		params.includeDetail,
		params.ip,
		params.user,
		page,
		perPage,
	)
	if err != nil {
		middleware.ErrorResponse(c, http.StatusInternalServerError, err.Error())
		return
	}

	middleware.SuccessResponse(c, result)
}

// GetLogDetail godoc
//
//	@Summary		Get log detail
//	@Description	Get detailed information about a specific log entry
//	@Tags			logs
//	@Produce		json
//	@Security		ApiKeyAuth
//	@Param			log_id	path		string	true	"Log ID"
//	@Success		200		{object}	middleware.APIResponse{data=model.RequestDetail}
//	@Router			/api/logs/detail/{log_id} [get]
func GetLogDetail(c *gin.Context) {
	logID, parseErr := strconv.Atoi(c.Param("log_id"))
	if parseErr != nil || logID <= 0 {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid log ID")
		return
	}

	log, err := model.GetLogDetail(logID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		middleware.ErrorResponse(c, http.StatusNotFound, "record not found")
		return
	}
	if err != nil {
		middleware.ErrorResponse(c, http.StatusInternalServerError, "unable to read log detail")
		return
	}

	middleware.SuccessResponse(c, log)
}

// GetGroupLogDetail godoc
//
//	@Summary		Get group log detail
//	@Description	Get detailed information about a specific log entry in a group
//	@Tags			log
//	@Produce		json
//	@Security		ApiKeyAuth
//	@Param			group	path		string	true	"Group name"
//	@Param			log_id	path		string	true	"Log ID"
//	@Success		200		{object}	middleware.APIResponse{data=model.RequestDetail}
//	@Router			/api/log/{group}/detail/{log_id} [get]
func GetGroupLogDetail(c *gin.Context) {
	group := c.Param("group")
	if group == "" {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid group parameter")
		return
	}

	logID, parseErr := strconv.Atoi(c.Param("log_id"))
	if parseErr != nil || logID <= 0 {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid log ID")
		return
	}

	log, err := model.GetGroupLogDetail(logID, group)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		middleware.ErrorResponse(c, http.StatusNotFound, "record not found")
		return
	}
	if err != nil {
		middleware.ErrorResponse(c, http.StatusInternalServerError, "unable to read log detail")
		return
	}

	middleware.SuccessResponse(c, log)
}

// DeleteHistoryLogs godoc
//
//	@Summary		Delete historical logs
//	@Description	Deletes logs older than the specified retention period
//	@Tags			logs
//	@Produce		json
//	@Security		ApiKeyAuth
//	@Param			timestamp	query		int	true	"Timestamp (milliseconds)"
//	@Success		200			{object}	middleware.APIResponse{data=int}
//	@Router			/api/logs/ [delete]
func DeleteHistoryLogs(c *gin.Context) {
	timestamp, _ := strconv.ParseInt(c.Query("timestamp"), 10, 64)
	if timestamp == 0 {
		middleware.ErrorResponse(c, http.StatusBadRequest, "timestamp is required")
		return
	}

	count, err := model.DeleteOldLog(time.UnixMilli(timestamp))
	if err != nil {
		middleware.ErrorResponse(c, http.StatusInternalServerError, err.Error())
		return
	}

	middleware.SuccessResponse(c, count)
}

// SearchConsumeError godoc
//
//	@Summary		Search consumption errors
//	@Description	Search for logs with consumption errors
//	@Tags			logs
//	@Produce		json
//	@Security		ApiKeyAuth
//	@Param			page			query		int		false	"Page number"
//	@Param			per_page		query		int		false	"Items per page"
//	@Param			start_timestamp	query		int		false	"Start timestamp (milliseconds)"
//	@Param			end_timestamp	query		int		false	"End timestamp (milliseconds)"
//	@Param			keyword			query		string	false	"Keyword"
//	@Param			group			query		string	false	"Group"
//	@Param			token_name		query		string	false	"Token name"
//	@Param			model_name		query		string	false	"Model name"
//	@Param			content			query		string	false	"Content"
//	@Param			token_id		query		int		false	"Token ID"
//	@Param			order			query		string	false	"Order"
//	@Param			request_id		query		string	false	"Request ID"
//	@Success		200				{object}	middleware.APIResponse{data=map[string]any{logs=[]model.RequestDetail,total=int}}
//	@Router			/api/logs/consume_error [get]
func SearchConsumeError(c *gin.Context) {
	keyword := c.Query("keyword")
	group := c.Query("group")
	tokenName := c.Query("token_name")
	modelName := c.Query("model_name")
	tokenID, _ := strconv.Atoi(c.Query("token_id"))
	page, perPage := utils.ParsePageParams(c)
	order := c.Query("order")
	requestID := c.Query("request_id")

	logs, total, err := model.SearchConsumeError(
		keyword,
		requestID,
		group,
		tokenName,
		modelName,
		tokenID,
		page,
		perPage,
		order,
	)
	if err != nil {
		middleware.ErrorResponse(c, http.StatusInternalServerError, err.Error())
		return
	}

	middleware.SuccessResponse(c, gin.H{
		"logs":  logs,
		"total": total,
	})
}
