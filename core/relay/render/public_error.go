package render

import "encoding/json"

// Public serialization only: never mutate the event used for usage, audit or retries.
// Keep successful content byte-for-byte, even when generated text discusses errors.
func publicStreamData(data []byte, event string, gemini bool) []byte {
	return publicErrorData(data, event, gemini, false)
}

// PublicResponseData applies the same failure boundary to HTTP-200 Responses
// objects. The adapter keeps its original parsed object for usage and storage.
func PublicResponseData(data []byte) []byte {
	return publicErrorData(data, "", false, true)
}

func publicErrorData(data []byte, event string, gemini, responseObject bool) []byte {
	var body map[string]json.RawMessage
	if json.Unmarshal(data, &body) != nil || body == nil {
		if event != "error" && event != "response.failed" {
			return data
		}
	}

	var kind string

	_ = json.Unmarshal(body["type"], &kind)

	var status string
	_ = json.Unmarshal(body["status"], &status)
	failedResponse := responseObject && status == "failed"

	hasError := len(body["error"]) > 0 && string(body["error"]) != "null"
	if !hasError && !failedResponse && kind != "error" && kind != "response.failed" && event != "error" &&
		event != "response.failed" {
		return data
	}

	const message = "The service is temporarily unavailable. Please try again later."

	detail := map[string]any{
		"message": message,
		"type":    "api_error",
		"code":    "upstream_unavailable",
	}

	safe := map[string]any{"error": detail}
	switch {
	case responseObject && (failedResponse || hasError):
		safe = publicFailedResponse(body, detail)
		safe["object"] = "response"
	case gemini:
		safe["error"] = map[string]any{"message": message, "code": 503, "status": "UNAVAILABLE"}
	case kind == "response.failed" || event == "response.failed":
		var original map[string]json.RawMessage
		_ = json.Unmarshal(body["response"], &original)
		response := publicFailedResponse(original, detail)

		safe = map[string]any{"type": "response.failed", "response": response}
	case kind == "error" || event == "error":
		safe["type"] = "error"
		// Responses also defines flat error events; preserve that envelope.
		if len(body["message"]) > 0 || len(body["code"]) > 0 {
			safe = map[string]any{
				"type":    "error",
				"code":    "upstream_unavailable",
				"message": message,
				"param":   nil,
			}
		}
	}

	encoded, err := json.Marshal(safe)
	if err != nil {
		return []byte(
			`{"error":{"message":"The service is temporarily unavailable. Please try again later.","type":"api_error","code":"upstream_unavailable"}}`,
		)
	}

	return encoded
}

// Failure envelopes omit arbitrary provider diagnostics, as stream errors do.
func publicFailedResponse(original map[string]json.RawMessage, detail map[string]any) map[string]any {
	response := map[string]any{"status": "failed", "error": detail}
	var id string
	if json.Unmarshal(original["id"], &id) == nil && id != "" {
		response["id"] = id
	}
	return response
}
