package controller

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"net"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestImageContentSignatureIsolation(t *testing.T) {
	task := &model.ImageTask{ID: "task-one", KeyFingerprint: strings.Repeat("a", 64), Data: []model.ImageOutput{{URL: "https://upstream.example/a.png"}}}
	sig := imageContentSignature(task, 0, 1100)
	if !validImageContentSignature(task, 0, 1100, sig, 1000) {
		t.Fatal("valid signature rejected")
	}
	for _, changed := range []model.ImageTask{{ID: "task-two", KeyFingerprint: task.KeyFingerprint, Data: task.Data}, {ID: task.ID, KeyFingerprint: strings.Repeat("b", 64), Data: task.Data}} {
		if validImageContentSignature(&changed, 0, 1100, sig, 1000) {
			t.Fatal("cross-task/key signature accepted")
		}
	}
	if validImageContentSignature(task, 1, 1100, sig, 1000) || validImageContentSignature(task, 0, 1100, sig, 1101) || validImageContentSignature(task, 0, 4700, imageContentSignature(task, 0, 4700), 1000) {
		t.Fatal("invalid index/expiry accepted")
	}
}
func TestImageContentBlocksPrivateAndReservedAddresses(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "::1", "fc00::1", "100.64.0.1", "198.18.0.1", "192.0.2.1"} {
		if publicImageIP(net.ParseIP(ip)) {
			t.Fatalf("accepted %s", ip)
		}
	}
	if !publicImageIP(net.ParseIP("8.8.8.8")) {
		t.Fatal("public address rejected")
	}
}

func TestPublicImageTaskHidesProviderAndPreservesStoredTask(t *testing.T) {
	t.Setenv("PUBLIC_IMAGE_BASE_URL", "https://gateway.example.com")
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	count := int64(1)
	task := &model.ImageTask{NumImages: &count, ID: "task-one", KeyFingerprint: strings.Repeat("a", 64), Status: "completed", Data: []model.ImageOutput{{URL: "https://fal.media/private.png"}}}
	result := publicImageTask(ctx, task)
	body, _ := json.Marshal(result)
	require.Contains(t, string(body), `"num_images":1`)
	if strings.Contains(string(body), "fal.media") || result.Status != "completed" {
		t.Fatalf("unsafe response: %s", body)
	}
	target, err := url.Parse(result.Data[0].URL)
	if err != nil || target.Host != "gateway.example.com" {
		t.Fatal("missing platform URL")
	}
	expires, _ := strconv.ParseInt(target.Query().Get("expires"), 10, 64)
	if !validImageContentSignature(task, 0, expires, target.Query().Get("signature"), time.Now().Unix()) {
		t.Fatal("invalid generated signature")
	}
	if task.Data[0].URL != "https://fal.media/private.png" {
		t.Fatal("stored task mutated")
	}
	task.Error = &model.ImageTaskError{Code: "provider_error", Message: "fal-ai upstream error"}
	body, _ = json.Marshal(publicImageTask(ctx, task))
	if strings.Contains(string(body), "fal") {
		t.Fatal("provider error leaked")
	}
	t.Setenv("PUBLIC_IMAGE_BASE_URL", "")
	result = publicImageTask(ctx, task)
	if len(result.Data) != 0 || result.Status != "failed" {
		t.Fatal("missing origin did not fail closed")
	}
}

func TestPublicImageTaskRetentionAndProcessing(t *testing.T) {
	t.Setenv("PUBLIC_IMAGE_BASE_URL", "https://api.example.com")
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	task := &model.ImageTask{ID: "stored", KeyFingerprint: strings.Repeat("a", 64), Status: "result_processing", ArchiveRequired: true, Data: []model.ImageOutput{{URL: "https://source/image"}}}
	task.ProviderMetadata = map[string]json.RawMessage{"caption": json.RawMessage(`"caption"`)}
	pending := publicImageTask(c, task)
	require.Equal(t, "in_progress", pending.Status)
	require.Equal(t, "result_processing", pending.Phase)
	require.Empty(t, pending.Data)
	require.Empty(t, pending.ProviderMetadata)
	require.NotEmpty(t, task.ProviderMetadata)
	task.Status = "completed"
	expiry := time.Now().UTC().Add(20 * time.Minute)
	task.ResultExpiresAt = &expiry
	task.Data[0].Stored = true
	out := publicImageTask(c, task)
	require.Equal(t, "stored", out.ResultAvailability)
	require.Equal(t, task.ProviderMetadata, out.ProviderMetadata)
	require.Len(t, out.Data, 1)
	require.False(t, out.Data[0].Stored)
	require.NotNil(t, out.Data[0].URLExpiresAt)
	require.True(t, out.Data[0].URLExpiresAt.Equal(expiry))
	require.NotContains(t, out.Data[0].URL, "source")
	expired := time.Now().Add(-time.Second)
	task.ResultExpiresAt = &expired
	out = publicImageTask(c, task)
	require.Equal(t, "completed", out.Status)
	require.Equal(t, "expired", out.ResultAvailability)
	require.Empty(t, out.Data)
}
