package controller

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/nativeresult"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"gorm.io/gorm"
)

// Administrative reads of one customer's native task, for the application's
// customer generation history. The application authenticates the customer and
// passes their gateway group; no routing, provider identity or storage key is
// returned.
type groupNativeTaskReader struct {
	engine func() (*nativetask.Engine, bool)
}

func groupNativeTaskDefaults() groupNativeTaskReader {
	return groupNativeTaskReader{NativeTaskEngine}
}

// publicNativeStatus matches the status customers see from GET /v1/model-tasks/:id.
func publicNativeStatus(status string) string {
	switch status {
	case "reserved", "submitting", "submission_unknown":
		return "queued"
	case "result_received", "delivery_ready":
		return "running"
	}
	return status
}

// customerNativeInput is what the customer sent: platform-fixed parameters are
// configuration, not part of their request.
func customerNativeInput(task *model.NativeTask) string {
	var input map[string]json.RawMessage
	var contract nativeresult.TaskContract
	if json.Unmarshal([]byte(task.NativeInput), &input) != nil ||
		json.Unmarshal([]byte(task.FrozenContract), &contract) != nil {
		return ""
	}
	for key := range contract.FixedParameters {
		delete(input, key)
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return ""
	}
	return string(raw)
}

func (r groupNativeTaskReader) find(c *gin.Context) (*nativetask.Engine, *model.NativeTask, bool) {
	engine, ok := r.engine()
	if !ok {
		middleware.ErrorResponse(c, http.StatusServiceUnavailable, "native execution unavailable")
		return nil, nil, false
	}
	group, id := c.Param("group"), c.Param("id")
	if group == "" || id == "" || len(id) > 128 {
		middleware.ErrorResponse(c, http.StatusNotFound, "task not found")
		return nil, nil, false
	}
	task, err := model.GetNativeTaskInGroup(engine.DB, id, group)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		middleware.ErrorResponse(c, http.StatusNotFound, "task not found")
		return nil, nil, false
	}
	if err != nil {
		middleware.ErrorResponse(c, http.StatusServiceUnavailable, "task temporarily unavailable")
		return nil, nil, false
	}
	return engine, task, true
}

func (r groupNativeTaskReader) detail(c *gin.Context) {
	_, task, ok := r.find(c)
	if !ok {
		return
	}
	artifacts, _ := nativeTrialArtifactList(task)
	// Native JSON stays text so the admin client never rounds large integers.
	data := gin.H{
		"id": task.ID, "model": task.Model, "status": publicNativeStatus(task.Status),
		"createdAt": task.CreatedAt.UnixMilli(), "updatedAt": task.UpdatedAt.UnixMilli(),
		"inputJSON": customerNativeInput(task), "artifacts": artifacts,
	}
	if task.Status == "completed" {
		data["outputJSON"] = task.DeliveredOutput
	}
	if task.Status == "failed" {
		data["errorCode"] = task.ErrorCode
	}
	c.Header("Cache-Control", "private, no-store")
	c.JSON(http.StatusOK, middleware.APIResponse{Success: true, Data: data})
}

func (r groupNativeTaskReader) artifact(c *gin.Context) {
	engine, task, ok := r.find(c)
	if !ok {
		return
	}
	handler := &nativetask.HTTP{Engine: engine, Identity: func(*http.Request) (string, int, error) {
		return task.GroupID, task.TokenID, nil
	}}
	handler.GetArtifact(c.Writer, c.Request, task.ID, c.Param("index"))
}

func GetGroupNativeTask(c *gin.Context)         { groupNativeTaskDefaults().detail(c) }
func GetGroupNativeTaskArtifact(c *gin.Context) { groupNativeTaskDefaults().artifact(c) }
