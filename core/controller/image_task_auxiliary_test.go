package controller

import (
	"bytes"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAuxiliarySignedDownloadIsScopedAndDoesNotMutateStoredURLs(t *testing.T) {
	for _, name := range []string{"mask_image", "transparent_overlay"} {
		t.Run(name, func(t *testing.T) {
			db, err := model.OpenSQLite(filepath.Join(t.TempDir(), "aux-content.db"))
			require.NoError(t, err)
			require.NoError(t, db.AutoMigrate(&model.ImageTask{}))
			old := model.LogDB
			model.LogDB = db
			t.Cleanup(func() { model.LogDB = old })
			t.Setenv("PUBLIC_IMAGE_BASE_URL", "https://gateway.test")
			expiry := time.Now().Add(30 * time.Minute)
			source := strings.ReplaceAll("https://owned.test/generated-results/images/aux-task/0-mask_image.png", "mask_image", name)
			task := model.ImageTask{ID: "aux-task", Status: "completed", KeyFingerprint: strings.Repeat("a", 64), ArchiveRequired: true, ResultExpiresAt: &expiry, Data: []model.ImageOutput{{URL: "https://owned.test/main.png", Layer: json.RawMessage(`{"z_index":0,"name":null,"bounding_box":null}`), Stored: true, AuxiliaryImages: map[string]*model.ImageOutput{name: {URL: source, Stored: true, ContentType: "image/png"}}}}}
			require.NoError(t, db.Create(&task).Error)
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			other := "mask_image"
			if name == other {
				other = "transparent_overlay"
			}
			task.Data[0].AuxiliaryImages[other] = &model.ImageOutput{URL: source, Stored: true, ContentType: "image/png"}
			public := publicImageTask(ctx, &task)
			require.Len(t, public.Data, 1)
			require.JSONEq(t, string(task.Data[0].Layer), string(public.Data[0].Layer))
			public.Data[0].Layer[0] = '!'
			require.Equal(t, byte('{'), task.Data[0].Layer[0])
			mask := public.Data[0].AuxiliaryImages[name]
			require.NotNil(t, mask)
			require.NotContains(t, mask.URL, "owned.test")
			require.False(t, mask.Stored)
			require.NotNil(t, mask.URLExpiresAt)
			require.Equal(t, source, task.Data[0].AuxiliaryImages[name].URL)
			require.True(t, task.Data[0].AuxiliaryImages[name].Stored)
			var buf bytes.Buffer
			require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))))
			calls := 0
			client := func() *http.Client {
				return &http.Client{Transport: gifTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					require.Equal(t, source, r.URL.String())
					require.Empty(t, r.Header.Get("Authorization"))
					return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(buf.Bytes())), Header: http.Header{}}, nil
				})}
			}
			router := gin.New()
			router.GET("/v1/images/tasks/:id/content/:index", func(c *gin.Context) { getImageTaskContent(c, client) })
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, httptest.NewRequest("GET", mask.URL, nil))
			require.Equal(t, 200, rr.Code)
			require.Equal(t, buf.Bytes(), rr.Body.Bytes())
			for _, target := range []string{mask.URL, public.Data[0].URL} {
				original, err := url.Parse(target)
				require.NoError(t, err)
				for _, role := range []string{"", "mask_image", "transparent_overlay"} {
					if role == original.Query().Get("auxiliary") {
						continue
					}
					u := *original
					q := u.Query()
					if role == "" {
						q.Del("auxiliary")
					} else {
						q.Set("auxiliary", role)
					}
					u.RawQuery = q.Encode()
					rr = httptest.NewRecorder()
					router.ServeHTTP(rr, httptest.NewRequest("GET", u.String(), nil))
					require.Equal(t, 404, rr.Code)
				}
			}

			require.Equal(t, 1, calls)
			task.Status = "result_processing"
			require.Empty(t, publicImageTask(ctx, &task).Data)
			task.Status = "completed"
			past := time.Now().Add(-time.Second)
			task.ResultExpiresAt = &past
			require.Empty(t, publicImageTask(ctx, &task).Data)
			task.ResultExpiresAt = &expiry
			task.Data[0].AuxiliaryImages[name] = nil
			require.Contains(t, publicImageTask(ctx, &task).Data[0].AuxiliaryImages, name)
			require.Nil(t, publicImageTask(ctx, &task).Data[0].AuxiliaryImages[name])

		})
	}
}
