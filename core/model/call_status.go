package model

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labring/aiproxy/core/relay/mode"
)

// CallStatus is the outcome customers see for one call (owner decision D1,
// 2026-10-11). It is derived from a log row and never stored, so a late
// settlement turns an unknown row back into succeeded or failed.
type CallStatus string

const (
	CallStatusSucceeded  CallStatus = "succeeded"
	CallStatusFailed     CallStatus = "failed"
	CallStatusRejected   CallStatus = "rejected"
	CallStatusProcessing CallStatus = "processing"
	CallStatusUnknown    CallStatus = "unknown"

	// callStatusSuccessAlias is the pre-P1 filter value for succeeded, still
	// sent by the admin app and by app versions deployed before P1.
	callStatusSuccessAlias = "success"
)

// CallStaleAfter is how long a call may stay processing before it is shown as
// unknown (D1).
const CallStaleAfter = 24 * time.Hour

// CallStatuses lists every status in display order.
var CallStatuses = []CallStatus{
	CallStatusSucceeded,
	CallStatusFailed,
	CallStatusRejected,
	CallStatusProcessing,
	CallStatusUnknown,
}

// ParseCallStatus accepts a status filter. An empty value means no filter;
// "success" is accepted as an alias for succeeded.
func ParseCallStatus(raw string) (CallStatus, error) {
	if raw == "" {
		return "", nil
	}

	if raw == callStatusSuccessAlias {
		return CallStatusSucceeded, nil
	}

	status := CallStatus(raw)
	if !slices.Contains(CallStatuses, status) {
		return "", fmt.Errorf("invalid call status %q", raw)
	}

	return status, nil
}

type relayModeClass int

const (
	// modeClassSyncCall: the HTTP answer is the outcome. An async usage status
	// on such a row is billing state only (the measured-image outbox).
	modeClassSyncCall relayModeClass = iota
	// modeClassRead: polls, downloads, deletes and other reads. They are logged
	// only when they fail and are never calls.
	modeClassRead
	// modeClassAsyncSubmit: the answer accepts a job whose outcome the async
	// usage status reports later.
	modeClassAsyncSubmit
)

// relayModeClasses is the only classification of relay modes. Consume's
// logging rule, the call counters and the guards all read it through
// IsReadMode and IsAsyncSubmitMode. TestEveryRelayModeIsClassified fails for a
// mode that is missing here.
var relayModeClasses = map[mode.Mode]relayModeClass{
	mode.Unknown:                 modeClassSyncCall,
	mode.ChatCompletions:         modeClassSyncCall,
	mode.Completions:             modeClassSyncCall,
	mode.Embeddings:              modeClassSyncCall,
	mode.Moderations:             modeClassSyncCall,
	mode.ImagesGenerations:       modeClassSyncCall,
	mode.ImagesEdits:             modeClassSyncCall,
	mode.AudioSpeech:             modeClassSyncCall,
	mode.AudioTranscription:      modeClassSyncCall,
	mode.AudioTranslation:        modeClassSyncCall,
	mode.Rerank:                  modeClassSyncCall,
	mode.ParsePdf:                modeClassSyncCall,
	mode.Anthropic:               modeClassSyncCall,
	mode.VideoGenerationsJobs:    modeClassAsyncSubmit,
	mode.VideoGenerationsGetJobs: modeClassRead,
	mode.VideoGenerationsContent: modeClassRead,
	// A background response (and a stream that ends before the upstream
	// finishes) is accepted with status queued or in_progress; the async usage
	// poller later records completed or failed (relay/adaptor/openai
	// responseNeedsAsyncUsage, fetchResponseUsage).
	mode.Responses:              modeClassAsyncSubmit,
	mode.ResponsesGet:           modeClassRead,
	mode.ResponsesDelete:        modeClassRead,
	mode.ResponsesCancel:        modeClassRead,
	mode.ResponsesInputItems:    modeClassRead,
	mode.Gemini:                 modeClassSyncCall,
	mode.Videos:                 modeClassAsyncSubmit,
	mode.VideosGet:              modeClassRead,
	mode.VideosContent:          modeClassRead,
	mode.VideosDelete:           modeClassRead,
	mode.VideosRemix:            modeClassAsyncSubmit,
	mode.GeminiVideo:            modeClassAsyncSubmit,
	mode.GeminiVideoOperations:  modeClassRead,
	mode.GeminiTTS:              modeClassSyncCall,
	mode.GeminiImage:            modeClassSyncCall,
	mode.GeminiFiles:            modeClassRead,
	mode.VideosEdits:            modeClassAsyncSubmit,
	mode.VideosExtensions:       modeClassAsyncSubmit,
	mode.AliVideo:               modeClassAsyncSubmit,
	mode.AliVideoTasks:          modeClassRead,
	mode.DoubaoVideo:            modeClassAsyncSubmit,
	mode.DoubaoVideoTasks:       modeClassRead,
	mode.DoubaoVideoTasksDelete: modeClassRead,
	mode.ResponsesCompact:       modeClassSyncCall,
	// AlphaSearch successes are never logged, so counting only its failures
	// would make its success rate 0.
	mode.AlphaSearch: modeClassRead,
	mode.NativeTasks: modeClassAsyncSubmit,
}

// asyncUsageBillingOnlyModes are sync-call modes whose adaptors may still
// return AsyncUsage: chat, completions, Anthropic and Gemini requests served
// through an OpenAI Responses-only model, when the stream ends before the
// upstream reports usage. The customer already has the answer and only the
// bill is fetched later, so the row keeps the status of its HTTP outcome.
var asyncUsageBillingOnlyModes = map[mode.Mode]struct{}{
	mode.Completions:     {},
	mode.ChatCompletions: {},
	mode.Anthropic:       {},
	mode.Gemini:          {},
}

func relayModeClassOf(m mode.Mode) relayModeClass {
	if class, ok := relayModeClasses[m]; ok {
		return class
	}

	// Historical rows may carry a value no longer defined; they count as calls.
	return modeClassSyncCall
}

// IsReadMode reports polls, downloads, deletes and other reads. They are
// logged only on failure and never count as calls.
func IsReadMode(m mode.Mode) bool {
	return relayModeClassOf(m) == modeClassRead
}

// IsAsyncSubmitMode reports modes whose success answer accepts a job; the row's
// async usage status then carries the job's outcome.
func IsAsyncSubmitMode(m mode.Mode) bool {
	return relayModeClassOf(m) == modeClassAsyncSubmit
}

// AsyncUsageExpected reports whether a relay result on this mode may carry
// AsyncUsage without misreporting the call: async-submit modes, reads (never
// calls) and the billing-only sync modes above. Any other producer would show
// unfinished jobs as succeeded and never as unknown.
func AsyncUsageExpected(m mode.Mode) bool {
	if _, ok := asyncUsageBillingOnlyModes[m]; ok {
		return true
	}

	return IsReadMode(m) || IsAsyncSubmitMode(m)
}

func relayModesOfClass(class relayModeClass) []int {
	modes := make([]int, 0, len(relayModeClasses))
	for relayMode, modeClass := range relayModeClasses {
		if modeClass == class {
			modes = append(modes, int(relayMode))
		}
	}

	slices.Sort(modes)

	return modes
}

var (
	readModeIDs        = relayModesOfClass(modeClassRead)
	asyncSubmitModeIDs = relayModesOfClass(modeClassAsyncSubmit)
)

// IsCall reports whether the row counts as a customer call: the key was
// recognised, it is not an internal demo, and it is not a read (a relay read
// mode, or a middleware rejection of a GET or DELETE such as a failed poll).
func (l *Log) IsCall() bool {
	endpoint := string(l.Endpoint)

	return l.GroupID != "" &&
		l.TokenID > 0 &&
		l.RequestSource != RequestSourceAdminDemo &&
		!IsReadMode(mode.Mode(l.Mode)) &&
		!strings.HasPrefix(endpoint, http.MethodGet+" ") &&
		!strings.HasPrefix(endpoint, http.MethodDelete+" ")
}

func (l *Log) isTaskRow() bool {
	return l.Code == http.StatusAccepted || IsAsyncSubmitMode(mode.Mode(l.Mode))
}

// CallStatus classifies the row at now. An error is rejected only when a gate
// refused it (a failure stage other than none or upstream) and the row carries
// no billing operation: a row with an op is one where money may be held (key
// usage P2, spec §4.3), so its error is ours and the call failed.
func (l *Log) CallStatus(now time.Time) CallStatus {
	if l.Code >= http.StatusBadRequest {
		if l.FailureStage == FailureStageNone || l.FailureStage == FailureStageUpstream ||
			l.BillingOperationID != "" {
			return CallStatusFailed
		}

		return CallStatusRejected
	}

	if l.isTaskRow() {
		switch l.AsyncUsageStatus {
		case AsyncUsageStatusFailed:
			return CallStatusFailed
		case AsyncUsageStatusPending:
			if !l.RequestAt.IsZero() && l.RequestAt.Before(now.Add(-CallStaleAfter)) {
				return CallStatusUnknown
			}

			return CallStatusProcessing
		}
	}

	return CallStatusSucceeded
}

// SetCallStatus fixes the status this row reports in JSON, so every row of one
// response is classified at the same instant.
func (l *Log) SetCallStatus(now time.Time) {
	l.callStatus = l.CallStatus(now)
}

// SetLogsCallStatus classifies every row of one response at now.
func SetLogsCallStatus(logs []*Log, now time.Time) {
	for _, entry := range logs {
		if entry != nil {
			entry.SetCallStatus(now)
		}
	}
}

// callStatusExpr is the SQL form of Log.CallStatus. It runs unchanged on
// Postgres, SQLite and MySQL; TestCallStatusSQLMatchesGo keeps the two equal.
func callStatusExpr(now time.Time) (string, []any) {
	taskRow := "(code = ? OR mode IN ?)"
	sql := "CASE" +
		" WHEN code >= ? THEN CASE WHEN COALESCE(failure_stage, '') IN ? OR COALESCE(billing_operation_id, '') <> ''" +
		" THEN 'failed' ELSE 'rejected' END" +
		" WHEN " + taskRow + " AND async_usage_status = ? THEN 'failed'" +
		" WHEN " + taskRow + " AND async_usage_status = ? AND request_at < ? THEN 'unknown'" +
		" WHEN " + taskRow + " AND async_usage_status = ? THEN 'processing'" +
		" ELSE 'succeeded' END"

	args := []any{
		http.StatusBadRequest,
		[]string{string(FailureStageNone), string(FailureStageUpstream)},
		http.StatusAccepted, asyncSubmitModeIDs, AsyncUsageStatusFailed,
		http.StatusAccepted, asyncSubmitModeIDs, AsyncUsageStatusPending, now.Add(-CallStaleAfter),
		http.StatusAccepted, asyncSubmitModeIDs, AsyncUsageStatusPending,
	}

	return sql, args
}

// isCallExpr is the SQL form of Log.IsCall. It never evaluates to NULL, so
// NOT (isCallExpr) selects exactly the other rows.
func isCallExpr() (string, []any) {
	sql := "COALESCE(group_id, '') <> '' AND COALESCE(token_id, 0) > 0" +
		" AND COALESCE(request_source, '') <> ?" +
		" AND (mode IS NULL OR mode NOT IN ?)" +
		" AND (endpoint IS NULL OR (endpoint NOT LIKE ? AND endpoint NOT LIKE ?))"

	return sql, []any{
		RequestSourceAdminDemo,
		readModeIDs,
		http.MethodGet + " %",
		http.MethodDelete + " %",
	}
}
