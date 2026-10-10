package ownedimage

import (
	"bytes"
	"context"
	"github.com/stretchr/testify/require"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"testing"
)

func TestInspectActualImageDimensions(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 17, 29))
	for _, format := range []string{"png", "jpeg"} {
		var b bytes.Buffer
		if format == "png" {
			require.NoError(t, png.Encode(&b, img))
		} else {
			require.NoError(t, jpeg.Encode(&b, img, nil))
		}
		m := Inspect(b.Bytes())
		require.Equal(t, 17, m.Width)
		require.Equal(t, 29, m.Height)
		require.Equal(t, "image/"+format, m.ContentType)
	}
	require.Zero(t, Inspect([]byte("not an image")).Width)
}

func TestReadMetadataUsesBoundedHeaderAndNoCredentials(t *testing.T) {
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 41, 53))))
	client := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		require.Empty(t, r.Header.Get("Authorization"))
		require.Equal(t, "bytes=0-1048575", r.Header.Get("Range"))
		return &http.Response{StatusCode: 206, Body: io.NopCloser(bytes.NewReader(b.Bytes()))}, nil
	})}
	m, err := readMetadata(context.Background(), "https://images.example/result.png", client)
	require.NoError(t, err)
	require.Equal(t, 41, m.Width)
	require.Equal(t, 53, m.Height)
	_, err = readMetadata(context.Background(), "http://127.0.0.1/private", client)
	require.Error(t, err)
}
