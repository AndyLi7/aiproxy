package ownedimage

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/gif"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAnimatedGIFDeliveryPreservesFramesAndMetadata(t *testing.T) {
	p := color.Palette{color.Black, color.White}
	a := image.NewPaletted(image.Rect(0, 0, 2, 3), p)
	b := image.NewPaletted(a.Rect, p)
	b.SetColorIndex(0, 0, 1)
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, &gif.GIF{Image: []*image.Paletted{a, b}, Delay: []int{7, 13}, LoopCount: 3}); err != nil {
		t.Fatal(err)
	}
	original := buf.Bytes()
	download := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" {
			t.Fatal("leaked credential")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(original))}, nil
	})}
	upload := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		data, _ := io.ReadAll(r.Body)
		if !bytes.Equal(data, original) || r.Header.Get("Content-Type") != "image/gif" {
			t.Fatal("GIF changed")
		}
		g, e := gif.DecodeAll(bytes.NewReader(data))
		if e != nil || len(g.Image) != 2 || g.Delay[1] != 13 || g.LoopCount != 3 {
			t.Fatal("animation lost")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"url":"https://media.test/result.gif"}}`))}, nil
	})}
	var meta Metadata
	url, mime, err := storeWithMetadata(context.Background(), "https://provider.test/output.gif", "http://app.internal", "internal", download, upload, &meta)
	if err != nil || url == "" || mime != "image/gif" || meta.Width != 2 || meta.Height != 3 {
		t.Fatalf("bad GIF delivery: %v %v", meta, err)
	}
	got, err := readMetadata(context.Background(), "https://provider.test/output.gif", download)
	if err != nil || got != meta {
		t.Fatalf("bad metadata: %v %v", got, err)
	}
}
