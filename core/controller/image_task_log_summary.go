package controller

import "encoding/json"

// The async task log is written at reservation, outside the normal relay
// detail recorder. Keep user-authored text while excluding uploaded media and
// arbitrary request fields from the persisted detail.
func imageTaskLogRequestSummary(body []byte) string {
	var input struct {
		Prompt         string `json:"prompt"`
		NegativePrompt string `json:"negative_prompt"`
	}
	if json.Unmarshal(body, &input) != nil {
		return ""
	}
	summary := make(map[string]string)
	if input.Prompt != "" && len(input.Prompt) <= 8192 {
		summary["prompt"] = input.Prompt
	}
	if input.NegativePrompt != "" && len(input.NegativePrompt) <= 8192 {
		summary["negative_prompt"] = input.NegativePrompt
	}
	if len(summary) == 0 {
		return ""
	}
	encoded, err := json.Marshal(summary)
	if err != nil {
		return ""
	}
	return string(encoded)
}
