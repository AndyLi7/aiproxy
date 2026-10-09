package nativeresult

import (
	"encoding/json"
	"regexp"
)

// PollResult is transport-neutral. Raw output remains private until archival.
type PollResult struct {
	Status    string
	Output    json.RawMessage
	ErrorCode string
	// Issues name the input fields a provider rejected after acceptance; set
	// only with ErrorCode invalid_parameters.
	Issues []ParameterIssue
	// ProviderStatus and ProviderReason are operator evidence for server logs
	// only (a sanitized type@field summary): never persisted or returned.
	ProviderStatus int
	ProviderReason string
}

// ParameterIssue names one input field a provider rejected and the kind of
// problem (owner decision 2026-10-09). Only a field path and a rule code cross
// the public boundary: never provider text, URLs or the customer's value.
type ParameterIssue struct {
	Field string `json:"field"`
	Rule  string `json:"rule"`
}

// MaxParameterIssues bounds the issues kept for one task.
const MaxParameterIssues = 8

var (
	issueField = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\[[0-9]+\])*(\.[A-Za-z_][A-Za-z0-9_]*(\[[0-9]+\])*)*$`)
	issueRule  = regexp.MustCompile(`^[a-z][a-z_]{0,31}$`)
)

// ValidParameterIssues reports 1..MaxParameterIssues issues whose fields are
// dotted parameter paths with optional [index] parts and whose rules are
// lower-case codes. Storage refuses anything else.
func ValidParameterIssues(issues []ParameterIssue) bool {
	if len(issues) == 0 || len(issues) > MaxParameterIssues {
		return false
	}
	for _, issue := range issues {
		if len(issue.Field) > 256 || !issueField.MatchString(issue.Field) || !issueRule.MatchString(issue.Rule) {
			return false
		}
	}
	return true
}
