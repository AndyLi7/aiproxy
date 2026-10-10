package render

import "encoding/json"

// Public serialization only: never mutate the event used for usage, audit or retries.
// Keep successful content byte-for-byte, even when generated text discusses errors.
func publicStreamData(data []byte, event string, gemini bool) []byte {
	var body map[string]json.RawMessage
	if json.Unmarshal(data, &body) != nil || body == nil {
		if event != "error" && event != "response.failed" {
			return data
		}
	}
	var kind string
	_ = json.Unmarshal(body["type"], &kind)
	hasError := len(body["error"]) > 0 && string(body["error"]) != "null"
	if !hasError && kind != "error" && kind != "response.failed" && event != "error" && event != "response.failed" {
		return data
	}
	const message = "The service is temporarily unavailable. Please try again later."
	detail := map[string]any{"message": message, "type": "api_error", "code": "upstream_unavailable"}
	safe := map[string]any{"error": detail}
	if gemini {
		safe["error"] = map[string]any{"message": message, "code": 503, "status": "UNAVAILABLE"}
	} else if kind == "response.failed" || event == "response.failed" {
		response := map[string]any{"status": "failed", "error": detail}
		var original map[string]json.RawMessage
		if json.Unmarshal(body["response"], &original) == nil {
			var id string
			if json.Unmarshal(original["id"], &id) == nil && id != "" {
				response["id"] = id
			}
		}
		safe = map[string]any{"type": "response.failed", "response": response}
	} else if kind == "error" || event == "error" {
		safe["type"] = "error"
		// Responses also defines flat error events; preserve that envelope.
		if len(body["message"]) > 0 || len(body["code"]) > 0 {
			safe = map[string]any{"type": "error", "code": "upstream_unavailable", "message": message, "param": nil}
		}
	}
	encoded, _ := json.Marshal(safe)
	return encoded
}
