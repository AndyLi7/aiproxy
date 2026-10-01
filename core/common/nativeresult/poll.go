package nativeresult

import "encoding/json"

// PollResult is transport-neutral. Raw output remains private until archival.
type PollResult struct {
	Status    string
	Output    json.RawMessage
	ErrorCode string
}
