package controller

import (
	"context"
	"errors"
	"github.com/labring/aiproxy/core/common/ownedimage"
	"github.com/labring/aiproxy/core/model"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestTaskMetadataFillsMissingDimensionsWithoutChangingURL(t *testing.T) {
	out := []model.ImageOutput{{URL: "https://result.example/image", ContentType: "image/jpeg"}}
	calls := 0
	read := func(context.Context, string) (ownedimage.Metadata, error) {
		calls++
		return ownedimage.Metadata{Width: 1024, Height: 768, ContentType: "image/png"}, nil
	}
	require.True(t, fillImageMetadata(context.Background(), out, read))
	require.Equal(t, int64(1024), *out[0].Width)
	require.Equal(t, int64(768), *out[0].Height)
	require.Equal(t, "image/png", out[0].ContentType)
	require.Equal(t, "https://result.example/image", out[0].URL)
	require.False(t, fillImageMetadata(context.Background(), out, read))
	require.Equal(t, 1, calls)
}
func TestTaskMetadataFailureKeepsCompletedImage(t *testing.T) {
	out := []model.ImageOutput{{URL: "https://result.example/image"}}
	require.False(t, fillImageMetadata(context.Background(), out, func(context.Context, string) (ownedimage.Metadata, error) {
		return ownedimage.Metadata{}, errors.New("unavailable")
	}))
	require.Nil(t, out[0].Width)
	require.NotEmpty(t, out[0].URL)
}
