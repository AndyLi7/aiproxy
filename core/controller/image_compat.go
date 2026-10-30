package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/common/imagecompat"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"gorm.io/gorm"
)

const (
	imageCompatContext = "openai_images_compat_request"
	imageCompatWait    = 30 * time.Second
)

type imageCompatCall struct {
	request imagecompat.Request
	id      string
	until   time.Time
}

func imageCompatError(c *gin.Context, status int, code, message, id string) {
	c.Header("x-should-retry", "false")
	c.Header("Cache-Control", "no-store")

	detail := gin.H{"code": code, "type": "invalid_request_error", "message": message}
	if status >= 500 {
		detail["type"] = "api_error"
	}

	if id != "" {
		detail["task_id"] = id
		detail["status_url"] = "/v1/model-tasks/" + id
		c.Header("X-MuseSpan-Task-ID", id)
		c.Header("Location", "/v1/model-tasks/"+id)
	}

	recordNativeFailure(c, status, code, message)
	c.AbortWithStatusJSON(status, gin.H{"error": detail})
}

func imageCompatHandlers(legacy mode.Mode) []gin.HandlerFunc {
	return []gin.HandlerFunc{
		selectImageCompat(legacy),
		func(c *gin.Context) {
			if _, ok := c.Get(imageCompatContext); ok {
				middleware.NewDistribute(mode.NativeTasks)(c)
			} else {
				middleware.NewDistribute(legacy)(c)
			}
		},
		func(c *gin.Context) {
			if value, ok := c.Get(imageCompatContext); ok {
				call, valid := value.(*imageCompatCall)
				if !valid {
					c.AbortWithStatus(http.StatusInternalServerError)
					return
				}

				submitImageCompat(c, call)
			} else {
				NewRelay(legacy)(c)
			}
		},
	}
}

type imageCompatBodyCounter struct {
	io.ReadCloser
	bytes int64
}

func (r *imageCompatBodyCounter) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.bytes += int64(n)
	return n, err
}

func selectImageCompat(legacy mode.Mode) gin.HandlerFunc {
	return func(c *gin.Context) {
		var (
			requested   string
			bodyCounter *imageCompatBodyCounter
		)
		if legacy == mode.ImagesEdits {
			// Existing multipart limits and cleanup apply also when the legacy route wins.
			bodyCounter = &imageCompatBodyCounter{ReadCloser: c.Request.Body}

			c.Request.Body = bodyCounter
			if err := common.ParseMultipartFormWithLimit(c.Request); err != nil {
				c.Next()
				return
			}

			if c.Request.MultipartForm != nil {
				form := c.Request.MultipartForm
				defer func() { _ = form.RemoveAll() }()

				if values := c.Request.MultipartForm.Value["model"]; len(values) > 0 {
					requested = values[0]
				}
			}
		} else {
			raw, err := common.GetRequestBodyReusable(c.Request)
			if err != nil {
				c.Next()
				return
			}

			var body struct {
				Model string `json:"model"`
			}
			if json.Unmarshal(raw, &body) != nil {
				c.Next()
				return
			}

			requested = body.Model
		}
		// Durable accepted operations are looked up before today's profile. A profile
		// can be disabled/upgraded without stranding a previously accepted task.
		group, token := middleware.GetGroup(c).ID, middleware.GetToken(c).ID

		var existing *model.NativeTask

		id, idErr := imagecompat.OperationID(c.Request.Header, group, token)
		if idErr == nil && c.GetHeader("Idempotency-Key") != "" && model.LogDB != nil {
			task, err := model.GetNativeTask(model.LogDB, id, group, token)
			if err == nil {
				existing = task
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				// A matching active profile below still fails through the native store gate.
				existing = nil
			}
		}

		value, _ := c.Get(middleware.ModelCaches)
		caches, _ := value.(*model.ModelCaches)

		var selected model.ModelConfig

		enabled := false
		if caches != nil {
			selected, enabled = selectImageCompatProfile(caches.EnabledModelConfigsMap, requested)
		}

		if existing == nil && !enabled {
			c.Next()
			return
		}

		c.Header("x-should-retry", "false")

		if idErr != nil {
			imageCompatError(c, 400, "invalid_idempotency", idErr.Error(), "")
			return
		}

		if bodyCounter != nil &&
			(bodyCounter.bytes > imagecompat.MaxBodyBytes || c.Request.ContentLength > imagecompat.MaxBodyBytes || (bodyCounter.bytes == 0 && c.Request.ContentLength < 0)) {
			imageCompatError(
				c,
				413,
				"image_request_too_large",
				"The complete image request must fit 3 MiB.",
				"",
			)

			return
		}

		parsed, err := imagecompat.Parse(c.Request, legacy == mode.ImagesEdits)
		if errors.Is(err, imagecompat.ErrBusy) {
			imageCompatError(c, 429, "image_validation_busy", err.Error(), "")
			return
		}

		if err != nil {
			imageCompatError(
				c,
				400,
				"unsupported_image_parameters",
				"Use the documented single-image parameters and supported image file limits.",
				"",
			)

			return
		}

		if existing != nil {
			sum := sha256.Sum256(parsed.Body)
			if existing.Fingerprint != hex.EncodeToString(sum[:]) {
				imageCompatError(
					c,
					409,
					"request_id_conflict",
					"This Idempotency-Key belongs to a different model or generation input.",
					id,
				)

				return
			}
		}

		if enabled {
			identity, _ := model.PublicCapabilityIdentityFromConfig(selected)
			if (legacy == mode.ImagesEdits) != (identity.CapabilityModel == imagecompat.EditModel) {
				imageCompatError(
					c,
					400,
					"wrong_image_endpoint",
					"Use this capability's generation or edit endpoint.",
					"",
				)

				return
			}
		}

		call := &imageCompatCall{request: parsed, id: id, until: time.Now().Add(imageCompatWait)}
		c.Header("X-MuseSpan-Task-ID", id)

		if existing != nil {
			// Reserved replays still go through today's native route checks. Accepted
			// replays need only the original fingerprint and owner, even after disable.
			if existing.Status != "reserved" {
				submitImageCompat(c, call)
				c.Abort()
				return
			}

			if !enabled {
				imageCompatError(
					c,
					503,
					"image_operation_pending",
					"The original operation is reserved; query its task before retrying.",
					id,
				)

				return
			}
		}

		common.SetRequestBody(c.Request, parsed.Body)
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.MultipartForm = nil
		c.Set(imageCompatContext, call)
		c.Next()
	}
}

func submitImageCompat(c *gin.Context, call *imageCompatCall) {
	h, ok := nativeHTTP(c)
	if !ok {
		return
	}

	h.ResolvePlan = func(_ *http.Request, _ []byte) (nativetask.Plan, nativetask.Provider, error) {
		plan, provider, err := resolveNativePlan(c)
		if plan.Log != nil {
			plan.Log.Endpoint = "POST " + c.Request.URL.Path
		}

		return plan, provider, err
	}
	serveImageCompat(c, call, h)
}

// serveImageCompat uses the native execution service; tests inject only provider
// and archive transports, not a second implementation of the image protocol.
func serveImageCompat(c *gin.Context, call *imageCompatCall, h *nativetask.HTTP) {
	h.ErrorIdentityHeader = "Idempotency-Key"

	task := h.CreateTask(c.Writer, c.Request, call.id, call.request.Body)
	if task == nil {
		return
	}

	task, err := h.WaitImage(c.Request.Context(), task, call.until)
	if err != nil {
		imageCompatError(
			c,
			503,
			"image_operation_pending",
			"Task state is temporarily unavailable; query the existing operation.",
			call.id,
		)

		return
	}

	if task.Status == "failed" {
		imageCompatError(
			c,
			422,
			"image_generation_failed",
			"The original generation failed; inspect its task for details.",
			task.ID,
		)

		return
	}

	if task.Status != "completed" {
		imageCompatError(
			c,
			503,
			"image_operation_pending",
			"Generation continues. Query the existing task; this HTTP deadline does not cancel or refund it.",
			task.ID,
		)

		return
	}

	if _, ok := nativetask.SingleOwnedImage(task); !ok {
		imageCompatError(
			c,
			502,
			"image_delivery_unavailable",
			"The archived result is not exactly one supported image.",
			task.ID,
		)

		return
	}

	item := gin.H{}
	if call.request.Format == "url" {
		target, ok := signedNativeImageURL(
			task,
			middleware.GetGroup(c).ID,
			middleware.GetToken(c).ID,
			time.Now(),
		)
		if !ok {
			imageCompatError(
				c,
				503,
				"image_delivery_unavailable",
				"A signed image download is temporarily unavailable.",
				task.ID,
			)

			return
		}

		item["url"] = target
	} else {
		value, err := h.ReadOwnedImage(c.Request, task)
		if err != nil {
			imageCompatError(
				c,
				503,
				"image_delivery_unavailable",
				"The archived image could not be verified; retry delivery with the same Idempotency-Key.",
				task.ID,
			)

			return
		}

		item["b64_json"] = value
	}

	c.Header("Cache-Control", "private, no-store")
	c.JSON(200, gin.H{"created": task.CreatedAt.Unix(), "data": []gin.H{item}})
}
