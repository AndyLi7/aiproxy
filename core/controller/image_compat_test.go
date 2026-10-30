//nolint:testpackage // Exercises profile selection and signed delivery boundaries.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/common/balance"
	"github.com/labring/aiproxy/core/common/imagecompat"
	"github.com/labring/aiproxy/core/common/nativetask"
	"github.com/labring/aiproxy/core/common/ownedartifact"
	"github.com/labring/aiproxy/core/middleware"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func imageProfileFixture(t *testing.T, edit bool) model.ModelConfig {
	t.Helper()

	capability := "text-to-image"
	if edit {
		capability = "image-to-image"
	}

	id := "openai/gpt-image-1.5/" + capability
	contract := map[string]any{
		"version":       1,
		"model":         id,
		"input_schema":  map[string]any{"type": "object"},
		"output_schema": map[string]any{"type": "object"},
		"artifacts":     []any{map[string]any{"path": []string{"images", "*", "url"}}},
	}
	raw, err := json.Marshal(contract)
	require.NoError(t, err)

	sum := sha256.Sum256(raw)

	return model.ModelConfig{
		Model: id + "::route",
		Type:  mode.NativeTasks,
		Config: map[model.ModelConfigKey]any{
			model.ModelConfigCapabilityContractVersionKey: 1,
			model.ModelConfigPublicModelKey:               "openai/gpt-image-1.5",
			model.ModelConfigCapabilityKey:                capability,
			model.ModelConfigPublicCapabilityModelKey:     id,
			NativeResultConfigKey: map[string]any{
				"version":   1,
				"channelId": 1,
				"contract":  contract,
			},
			ImageCompatConfigKey: imageCompatProfile{
				Version:        1,
				Enabled:        true,
				ContractSHA256: hex.EncodeToString(sum[:]),
				InlineEdit:     edit,
			},
		},
	}
}

func TestImageCompatProfileClosedAndCollisions(t *testing.T) {
	mc := imageProfileFixture(t, false)
	configs := map[string]model.ModelConfig{mc.Model: mc}
	_, ok := selectImageCompatProfile(configs, imagecompat.GenerateModel)
	require.True(t, ok)

	mc.Config[model.ModelConfigPublicCapabilityAliasesKey] = []string{"vendor/old-image"}
	configs[mc.Model] = mc
	configs["old"] = model.ModelConfig{Model: "vendor/old-image", Type: mode.ImagesGenerations}
	_, ok = selectImageCompatProfile(configs, imagecompat.GenerateModel)
	require.False(t, ok, "an alias collision blocks entire activation")
	delete(configs, "old")
	_, ok = selectImageCompatProfile(configs, "vendor/old-image")
	require.True(t, ok)

	mc.Config[ImageCompatConfigKey] = imageCompatProfile{
		Version:        1,
		Enabled:        true,
		ContractSHA256: strings.Repeat("0", 64),
	}
	_, ok = selectImageCompatProfile(configs, imagecompat.GenerateModel)
	require.False(t, ok)
	delete(mc.Config, ImageCompatConfigKey)
	require.False(t, validImageCompatProfile(mc))
}

func signedImageFixture(t *testing.T) (*model.NativeTask, []byte) {
	t.Helper()

	data := []byte("owned-image-content")
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	id := "oi_" + strings.Repeat("a", 64)
	receipt := ownedartifact.Receipt{
		Key:    fmt.Sprintf("native-results/%s/0/%s.bin", id, hash),
		Size:   int64(len(data)),
		SHA256: hash,
		Type:   "image/png",
	}
	raw, err := json.Marshal(map[int]ownedartifact.Receipt{0: receipt})
	require.NoError(t, err)

	return &model.NativeTask{
		ID:               id,
		GroupID:          "owner",
		TokenID:          7,
		Model:            imagecompat.GenerateModel,
		Status:           "completed",
		DeliveredOutput:  `{"images":[{"url":"owned"}]}`,
		ArtifactManifest: string(raw),
		CreatedAt:        time.Now(),
	}, data
}

func TestImageCompatSignedURLAndTampering(t *testing.T) {
	t.Setenv("PUBLIC_IMAGE_BASE_URL", "https://gateway.example")
	t.Setenv("EXTERNAL_BALANCE_KEY", strings.Repeat("s", 40))
	task, data := signedImageFixture(t)
	target, ok := signedNativeImageURL(task, "owner", 7, time.Now())
	require.True(t, ok)
	_, ok = signedNativeImageURL(task, "other", 7, time.Now())
	require.False(t, ok)
	_, ok = signedNativeImageURL(task, "owner", 8, time.Now())
	require.False(t, ok)
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "content.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}))
	require.NoError(t, db.Create(task).Error)

	oldDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() { model.LogDB = oldDB })

	h := &nativetask.HTTP{Download: func(*http.Request, string) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			ContentLength: int64(len(data)),
			Body:          io.NopCloser(bytes.NewReader(data)),
		}, nil
	}}
	router := gin.New()
	router.GET(
		"/v1/images/native-results/:id/:index",
		func(c *gin.Context) { getNativeImageContent(c, h) },
	)

	response := httptest.NewRecorder()
	router.ServeHTTP(
		response,
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil),
	)
	require.Equal(t, 200, response.Code)
	require.Equal(t, data, response.Body.Bytes())

	u, err := url.Parse(target)
	require.NoError(t, err)

	variants := make([]string, 0, 6)

	variants = append(variants,
		strings.Replace(target, "/0?", "/1?", 1),
		strings.Replace(target, task.ID, "oi_"+strings.Repeat("b", 64), 1),
		u.Path,
	)
	for _, pair := range [][2]string{{"signature", strings.Repeat("0", 64)}, {"expires", "1"}, {"expires", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)}} {
		q := u.Query()
		q.Set(pair[0], pair[1])

		v := *u
		v.RawQuery = q.Encode()
		variants = append(variants, v.String())
	}

	for _, candidate := range variants {
		r := httptest.NewRecorder()
		router.ServeHTTP(
			r,
			httptest.NewRequestWithContext(t.Context(), http.MethodGet, candidate, nil),
		)
		require.Equal(t, 404, r.Code, candidate)
	}
	// The separate native route still requires an owner identity.
	authenticated := &nativetask.HTTP{
		Engine:   &nativetask.Engine{DB: db},
		Identity: func(*http.Request) (string, int, error) { return "", 0, nil },
	}
	denied := httptest.NewRecorder()
	authenticated.GetArtifact(
		denied,
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil),
		task.ID,
		"0",
	)
	require.Equal(t, 401, denied.Code)
}

func TestImageCompatReplayAfterProfileDisabled(t *testing.T) {
	task, _ := signedImageFixture(t)
	headers := http.Header{}
	headers.Set("Idempotency-Key", "disabled-profile")
	id, err := imagecompat.OperationID(headers, "owner", 7)
	require.NoError(t, err)

	task.ID = id
	task.Status = "queued"
	body := `{"model":"` + imagecompat.GenerateModel + `","prompt":"same input"}`
	req := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/",
		strings.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	translated, err := imagecompat.Parse(req, false)
	require.NoError(t, err)

	sum := sha256.Sum256(translated.Body)
	task.Fingerprint = hex.EncodeToString(sum[:])
	db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "replay.db"))
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.NativeTask{}))
	require.NoError(t, db.Create(task).Error)

	oldDB, oldWallet := model.LogDB, balance.Default
	model.LogDB = db
	balance.Default = nativeControllerWallet{}
	t.Cleanup(func() { model.LogDB = oldDB; balance.Default = oldWallet })

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(middleware.Group, model.GroupCache{ID: "owner"})
		c.Set(middleware.Token, model.TokenCache{ID: 7})
		c.Next()
	})
	router.POST(
		"/v1/images/generations",
		selectImageCompat(mode.ImagesGenerations),
		func(c *gin.Context) { t.Error("disabled replay reached new routing") },
	)

	request := httptest.NewRequestWithContext(t.Context(),
		http.MethodPost,
		"/v1/images/generations",
		strings.NewReader(body),
	)
	request.Header = headers
	request.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	// Cancel only after the durable selector lookup succeeds, exercising an
	// accepted replay after disable without waiting for the HTTP deadline.
	require.NoError(
		t,
		db.Callback().
			Query().
			After("gorm:query").
			Register("cancel-after-lookup", func(*gorm.DB) { cancel() }),
	)
	request = request.WithContext(ctx)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, 503, response.Code)
	require.Contains(t, response.Body.String(), "image_operation_pending")
	require.Equal(t, id, response.Header().Get("X-Musespan-Task-Id"))

	request = httptest.NewRequestWithContext(t.Context(),
		http.MethodPost,
		"/v1/images/generations",
		strings.NewReader(strings.Replace(body, "same input", "changed", 1)),
	)
	request.Header = headers
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, 409, response.Code)
}

type imageCompatConfigCache map[string]model.ModelConfig

func (m imageCompatConfigCache) GetModelConfig(id string) (model.ModelConfig, bool) {
	value, ok := m[id]
	return value, ok
}

func TestImageCompatUsesNativeDistributorAndLegacyFallsThrough(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			mc := imageProfileFixture(t, false)
			if !enabled {
				delete(mc.Config, ImageCompatConfigKey)
				mc.Model = imagecompat.GenerateModel
				mc.Type = mode.ImagesGenerations
				mc.Config = map[model.ModelConfigKey]any{}
			}

			configs := imageCompatConfigCache{mc.Model: mc}
			sets := map[string][]string{"default": {mc.Model}}
			token := model.TokenCache{ID: 7}
			token.SetAvailableSets([]string{"default"})
			token.SetModelsBySet(sets)
			caches := &model.ModelCaches{
				ModelConfig:            configs,
				EnabledModelConfigsMap: configs,
				EnabledModelsBySet:     sets,
			}
			router := gin.New()
			router.Use(func(c *gin.Context) {
				c.Set(
					middleware.Group,
					model.GroupCache{ID: "internal-fixture", Status: model.GroupStatusInternal},
				)
				c.Set(middleware.Token, token)
				c.Set(middleware.ModelCaches, caches)
				c.Next()
			})

			handlers := imageCompatHandlers(mode.ImagesGenerations)
			router.POST("/v1/images/generations", handlers[0], handlers[1], func(c *gin.Context) {
				expected := mode.ImagesGenerations
				if enabled {
					expected = mode.NativeTasks
				}

				require.Equal(t, expected, middleware.GetModelConfig(c).Type)

				if enabled {
					_, ok := c.Get(imageCompatContext)
					require.True(t, ok)
				}

				c.Status(204)
			})

			request := httptest.NewRequestWithContext(t.Context(),
				http.MethodPost,
				"/v1/images/generations",
				strings.NewReader(
					`{"model":"`+imagecompat.GenerateModel+`","prompt":"route fixture"}`,
				),
			)
			request.Header.Set("Content-Type", "application/json")

			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			require.Equal(t, 204, response.Code, response.Body.String())
		})
	}
}

func TestImageCompatEditNativeDistributor(t *testing.T) {
	mc := imageProfileFixture(t, true)
	configs := imageCompatConfigCache{mc.Model: mc}
	sets := map[string][]string{"default": {mc.Model}}
	token := model.TokenCache{ID: 7}
	token.SetAvailableSets([]string{"default"})
	token.SetModelsBySet(sets)
	caches := &model.ModelCaches{
		ModelConfig:            configs,
		EnabledModelConfigsMap: configs,
		EnabledModelsBySet:     sets,
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(
			middleware.Group,
			model.GroupCache{ID: "internal-fixture", Status: model.GroupStatusInternal},
		)
		c.Set(middleware.Token, token)
		c.Set(middleware.ModelCaches, caches)
		c.Next()
	})

	handlers := imageCompatHandlers(mode.ImagesEdits)
	router.POST("/v1/images/edits", handlers[0], handlers[1], func(c *gin.Context) {
		require.Equal(t, mode.NativeTasks, middleware.GetModelConfig(c).Type)
		require.Nil(t, c.Request.MultipartForm)
		c.Status(204)
	})

	var body, picture bytes.Buffer
	require.NoError(t, png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 2, 2))))

	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("model", imagecompat.EditModel))
	require.NoError(t, writer.WriteField("prompt", "edit route fixture"))
	file, err := writer.CreateFormFile("image", "input.png")
	require.NoError(t, err)
	_, err = file.Write(picture.Bytes())
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	request := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/v1/images/edits",
		&body,
	)
	request.Header.Set("Content-Type", writer.FormDataContentType())

	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, 204, response.Code, response.Body.String())
}

func TestImageCompatUnknownIdempotencyFailsClosed(t *testing.T) {
	for _, scenario := range []string{"read-failure", "missing-store", "confirmed-absent", "unkeyed-legacy"} {
		t.Run(scenario, func(t *testing.T) {
			db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "fault.db"))
			require.NoError(t, err)
			require.NoError(t, db.AutoMigrate(&model.NativeTask{}))

			headers := http.Header{"Idempotency-Key": {"operation-before-disable"}}
			group := "fault-" + scenario
			id, err := imagecompat.OperationID(headers, group, 7)
			require.NoError(t, err)

			if scenario == "read-failure" {
				task, _ := signedImageFixture(t)
				task.ID, task.GroupID, task.Status = id, group, "queued"
				require.NoError(t, db.Create(task).Error)
				require.NoError(
					t,
					db.Callback().
						Query().
						Before("gorm:query").
						Register("injected-store-failure", func(tx *gorm.DB) {
							_ = tx.AddError(errors.New("synthetic store outage"))
						}),
				)
			}

			old := model.LogDB

			model.LogDB = db
			if scenario == "missing-store" || scenario == "unkeyed-legacy" {
				model.LogDB = nil
			}

			t.Cleanup(func() { model.LogDB = old })

			if scenario == "unkeyed-legacy" {
				headers.Del("Idempotency-Key")
			}

			providerCalls, walletCalls := 0, 0
			router := gin.New()
			router.Use(func(c *gin.Context) {
				c.Set(middleware.Group, model.GroupCache{ID: group})
				c.Set(middleware.Token, model.TokenCache{ID: 7})
				// No active native profile, with a successful legacy route available.
				c.Next()
			})
			router.POST(
				"/v1/images/generations",
				selectImageCompat(mode.ImagesGenerations),
				func(c *gin.Context) {
					providerCalls++
					walletCalls++

					c.Status(http.StatusNoContent)
				},
			)

			request := httptest.NewRequestWithContext(
				t.Context(),
				http.MethodPost,
				"/v1/images/generations",
				strings.NewReader(
					`{"model":"`+imagecompat.GenerateModel+`","prompt":"same operation"}`,
				),
			)
			request.Header = headers
			request.Header.Set("Content-Type", "application/json")

			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			if scenario == "confirmed-absent" || scenario == "unkeyed-legacy" {
				require.Equal(t, http.StatusNoContent, response.Code)
				require.Equal(t, 1, providerCalls)
				return
			}

			require.Equal(t, http.StatusServiceUnavailable, response.Code)
			require.Equal(t, "false", response.Header().Get("X-Should-Retry"))
			require.Equal(t, id, response.Header().Get("X-Musespan-Task-Id"))
			require.Equal(t, "/v1/model-tasks/"+id, response.Header().Get("Location"))
			require.Contains(t, response.Body.String(), "task_store_unavailable")
			require.Contains(t, response.Body.String(), id)
			require.Zero(t, providerCalls)
			require.Zero(t, walletCalls)
		})
	}
}
