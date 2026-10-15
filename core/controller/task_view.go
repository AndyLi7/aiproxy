package controller

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

const (
	// taskViewRecheck bounds how often an unready LogDB is probed.
	taskViewRecheck = 10 * time.Second
	// taskViewCheckTimeout bounds one probe: /api/status is the health check
	// and must answer even when the probe hangs.
	taskViewCheckTimeout = 2 * time.Second

	errTaskViewUnavailable = "task_view_unavailable"
)

// featureReadiness caches a storage check for an optional feature (C step 2
// task view). Like storageReadiness it stays ready once ready and probes at
// most every taskViewRecheck while not; unlike it, a failure never turns
// /api/status into a 503: the feature is only left out, and its endpoints
// answer 503 task_view_unavailable.
type featureReadiness struct {
	mu        sync.Mutex
	db        *gorm.DB
	readyOnce bool
	checkedAt time.Time
	name      string
	check     func(context.Context, *gorm.DB) (bool, error)
	now       func() time.Time
}

func (f *featureReadiness) ready(db *gorm.DB) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.db != db {
		// Another database handle (tests swap LogDB): forget the old answer.
		f.db, f.readyOnce, f.checkedAt = db, false, time.Time{}
	}

	if f.readyOnce {
		return true
	}

	now := f.now()
	if !f.checkedAt.IsZero() && now.Sub(f.checkedAt) < taskViewRecheck {
		return false
	}

	f.checkedAt = now

	ctx, cancel := context.WithTimeout(context.Background(), taskViewCheckTimeout)
	defer cancel()

	ready, err := f.check(ctx, db)
	if err != nil {
		log.Warnf("%s storage check: %v", f.name, err)
		return false
	}

	if !ready {
		log.Warnf("%s unavailable: LogDB lacks logs.billing_operation_id, the usage attribution indexes"+
			" (migrations/usage-attribution-v1 step 2) or the task tables", f.name)
		return false
	}

	f.readyOnce = true

	return true
}

var (
	// taskViewStorage gates kind=tasks, kind=non_task and unit=tasks, and the
	// log_task_view_v1 feature.
	taskViewStorage = &featureReadiness{
		name:  model.LogTaskViewFeature,
		check: model.TaskViewStorageReady,
		now:   time.Now,
	}
	// taskRequestsStorage gates /api/log/:group/task-requests, which also
	// probes native_tasks and image_tasks.
	taskRequestsStorage = &featureReadiness{
		name:  "log task requests",
		check: model.TaskRequestsStorageReady,
		now:   time.Now,
	}
)

func taskViewUnavailable(c *gin.Context) {
	middleware.ErrorResponse(c, http.StatusServiceUnavailable, errTaskViewUnavailable)
}

// GetGroupLogTaskRequests godoc
//
//	@Summary		Requests related to one task
//	@Description	For a native or image task row of the group: the rejected submits with the task's ID, group and key (a 409 request_id_conflict is a different generation and is never included), and the other rows carrying the task's billing operation (submits that failed after the hold). Each lookup reads at most 100 rows within 2 s; truncated marks a list cut by either limit. 400 when the row is not a native or image task row; 503 task_view_unavailable before the task view storage is ready.
//	@Tags			log
//	@Produce		json
//	@Security		ApiKeyAuth
//	@Param			group	path		string	true	"Group name"
//	@Param			log_id	query		int		true	"ID of the task's row (kind=tasks)"
//	@Success		200		{object}	middleware.APIResponse{data=model.TaskRequests}
//	@Router			/api/log/{group}/task-requests [get]
func GetGroupLogTaskRequests(c *gin.Context) {
	group := c.Param("group")
	if group == "" {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid group parameter")
		return
	}

	logID, present, err := parseOptionalPositiveInt(c, "log_id")
	if err != nil || !present {
		middleware.ErrorResponse(c, http.StatusBadRequest, "invalid log_id parameter")
		return
	}

	if !taskRequestsStorage.ready(model.LogDB) {
		taskViewUnavailable(c)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), logQueryTimeout)
	defer cancel()

	result, err := model.GetTaskRequests(ctx, group, logID, time.Now())

	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		middleware.ErrorResponse(c, http.StatusNotFound, "record not found")
	case errors.Is(err, model.ErrNotTaskRow):
		middleware.ErrorResponse(c, http.StatusBadRequest, model.ErrNotTaskRow.Error())
	case err != nil:
		common.GetLogger(c).Errorf("read task requests: %v", err)
		middleware.ErrorResponse(c, http.StatusInternalServerError, "unable to read task requests")
	default:
		middleware.SuccessResponse(c, result)
	}
}
