package ownedartifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(code int, data string) *http.Response {
	return &http.Response{StatusCode: code, ContentLength: int64(len(data)), Body: io.NopCloser(strings.NewReader(data)), Header: make(http.Header)}
}
func TestArchivePreservesOpaqueBytesAndChecksReceipt(t *testing.T) {
	data := "<svg><script>alert('x')</script></svg>"
	digest := sha256.Sum256([]byte(data))
	key := "native-results/task/0/" + strings.Repeat("a", 64) + ".bin"
	downloaded, uploaded := 0, 0
	download := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		downloaded++
		if r.Header.Get("Authorization") != "" {
			t.Fatal("credential leaked")
		}
		return response(200, data), nil
	})}
	upload := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		uploaded++
		body, _ := io.ReadAll(r.Body)
		if string(body) != data || r.Header.Get("X-Native-Artifact-Index") != "0" || r.Header.Get("X-Native-Task-ID") != "task" || r.Header.Get("Content-Type") != "application/octet-stream" || r.Header.Get("Authorization") != "Bearer internal" {
			t.Fatal("invalid internal upload")
		}
		raw, _ := json.Marshal(map[string]any{"code": 0, "data": Receipt{Key: key, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(data))}})
		return response(200, string(raw)), nil
	})}
	got, err := store(context.Background(), "task", 0, "https://assets.example/a.svg", "http://trusted-app", "internal", download, upload)
	if err != nil || got.Key != key || downloaded != 1 || uploaded != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	for _, source := range []string{"http://assets.example/x", "https://u:p@assets.example/x", "https://assets.example:444/x"} {
		if _, err = store(context.Background(), "task", 0, source, "http://trusted-app", "internal", download, upload); err == nil {
			t.Fatal(source)
		}
	}
	bad := &http.Client{Transport: transport(func(*http.Request) (*http.Response, error) {
		return response(200, `{"code":0,"data":{"key":"native-results/other/0/`+strings.Repeat("a", 64)+`.bin","sha256":"bad","size":0}}`), nil
	})}
	if _, err = store(context.Background(), "task", 0, "https://assets.example/x", "http://trusted-app", "internal", download, bad); err == nil {
		t.Fatal("accepted wrong receipt")
	}
}
func TestNetworkAddressPolicy(t *testing.T) {
	for _, v := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "192.0.2.1", "::1", "::ffff:127.0.0.1", "fc00::1", "2001:db8::1", "2002:7f00:1::", "64:ff9b::7f00:1"} {
		if publicIP(net.ParseIP(v)) {
			t.Fatal(v)
		}
	}
	if !publicIP(net.ParseIP("8.8.8.8")) {
		t.Fatal("public denied")
	}
}
func TestArchiveRejectsOversizeBeforeUpload(t *testing.T) {
	download := &http.Client{Transport: transport(func(*http.Request) (*http.Response, error) {
		r := response(200, "")
		r.ContentLength = MaxBytes + 1
		return r, nil
	})}
	upload := &http.Client{Transport: transport(func(*http.Request) (*http.Response, error) { t.Fatal("must not upload"); return nil, nil })}
	if _, err := store(context.Background(), "task", 0, "https://example.com/x", "http://app", "key", download, upload); err != ErrTooLarge {
		t.Fatal(err)
	}
}
