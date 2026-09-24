package ownedimage

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestStoreTransfersBytesAndNeverExposesSource(t *testing.T) {
	png := "\x89PNG\r\n\x1a\n" + strings.Repeat("x", 30)
	download := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" {
			t.Fatal("credential sent upstream")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(png))}, nil
	})}
	upload := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		if string(b) != png || r.Header.Get("Authorization") != "Bearer internal" || r.URL.Path != "/api/internal/media/images" || r.Header.Get("X-Musespan-Image-Task-ID") != "request-123" || r.Header.Get("X-Musespan-Image-Output-Index") != "1" {
			t.Fatal("bad transfer")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"url":"https://media.example.com/image.png"}}`))}, nil
	})}
	u, m, e := store(WithArchiveIdentity(context.Background(), "request-123", 1), "https://provider.example/image?secret=value", "http://app.internal", "internal", download, upload)
	if e != nil || u != "https://media.example.com/image.png" || m != "image/png" {
		t.Fatalf("unexpected result %q %q %v", u, m, e)
	}
	for _, status := range []int{302, 401, 500} {
		bad := &http.Client{Transport: transport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("private diagnostic"))}, nil
		})}
		u, _, e = store(context.Background(), "https://provider.example/image?secret=value", "http://app.internal", "internal", download, bad)
		if e != ErrStorage || u != "" {
			t.Fatal("must fail closed")
		}
	}
}
func TestDownloadRejectsPrivateAddresses(t *testing.T) {
	for _, v := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "::1", "fc00::1", "100.64.0.1"} {
		if publicIP(net.ParseIP(v)) {
			t.Fatal(v)
		}
	}
	if !publicIP(net.ParseIP("1.1.1.1")) {
		t.Fatal("public address rejected")
	}
}

func TestDeleteArchivedImageUsesScopedInternalRequest(t *testing.T) {
	client := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/internal/media/images" || r.Header.Get("Authorization") != "Bearer internal" {
			t.Fatal("invalid deletion route")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"contentType":"image/png","outputIndex":0,"taskId":"request-123"}` {
			t.Fatalf("invalid deletion identity: %s", body)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"deleted":true}}`))}, nil
	})}
	if err := deleteArchivedImage(context.Background(), "http://app.internal", "internal", "request-123", 0, "image/png", client); err != nil {
		t.Fatal(err)
	}
}
