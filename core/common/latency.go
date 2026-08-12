package common

import (
	"encoding/json"
	"io"
	"math"
	"sync"

	"github.com/gin-gonic/gin"
)

var latencyOutputMu sync.Mutex

// LatencyEvent contains only operational metadata that is safe to emit.
// Credentials, cookies, prompts, and request bodies deliberately have no field.
type LatencyEvent struct {
	Event      string
	RequestID  string
	Stage      string
	DurationMS float64
	Outcome    string
	Status     int
	Method     string
	Path       string
	Model      string
	ChannelID  int
	ErrorType  string
}

func FormatLatencyEvent(event LatencyEvent) (string, error) {
	payload := map[string]any{
		"event":       event.Event,
		"request_id":  event.RequestID,
		"duration_ms": math.Round(event.DurationMS*1000) / 1000,
		"outcome":     event.Outcome,
	}
	if event.Stage != "" {
		payload["stage"] = event.Stage
	}
	if event.Status != 0 {
		payload["status"] = event.Status
	}
	if event.Method != "" {
		payload["method"] = event.Method
	}
	if event.Path != "" {
		payload["path"] = event.Path
	}
	if event.Model != "" {
		payload["model"] = event.Model
	}
	if event.ChannelID != 0 {
		payload["channel_id"] = event.ChannelID
	}
	if event.ErrorType != "" {
		payload["error_type"] = event.ErrorType
	}

	encoded, err := json.Marshal(payload)
	return string(encoded), err
}

func WriteLatencyEvent(writer io.Writer, event LatencyEvent) error {
	line, err := FormatLatencyEvent(event)
	if err != nil {
		return err
	}
	_, err = io.WriteString(writer, line+"\n")
	return err
}

func LogLatencyEvent(c *gin.Context, event LatencyEvent) {
	latencyOutputMu.Lock()
	defer latencyOutputMu.Unlock()
	_ = WriteLatencyEvent(GetLogger(c).Logger.Out, event)
}
