// Package ownedartifact archives native files without interpreting active content.
package ownedartifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const MaxBytes = 64 << 20

var ErrStorage = errors.New("native artifact storage unavailable")
var ErrTooLarge = errors.New("native artifact exceeds archive limit")

// ErrUnsupportedSource and ErrSourceRejected are permanent: the provider URL
// can never be archived, so retrying the same task cannot succeed.
var ErrUnsupportedSource = errors.New("native artifact source is not archivable")
var ErrSourceRejected = errors.New("native artifact source rejected the download")
var identity = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var keyPattern = regexp.MustCompile(`^native-results/[A-Za-z0-9_-]{1,128}/[0-9]{1,4}/[a-f0-9]{64}\.bin$`)

type Receipt struct {
	Key    string `json:"key"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func publicIP(ip net.IP) bool {
	for _, cidr := range []string{"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "2001:db8::/32", "2001::/32", "2002::/16", "64:ff9b::/96", "64:ff9b:1::/48"} {
		_, block, _ := net.ParseCIDR(cidr)
		if block.Contains(ip) {
			return false
		}
	}
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified()
}
func downloadClient() *http.Client {
	return &http.Client{Timeout: 60 * time.Second, CheckRedirect: noRedirect, Transport: &http.Transport{MaxResponseHeaderBytes: 1 << 20, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != "443" {
			return nil, ErrStorage
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(ips) == 0 {
			return nil, ErrStorage
		}
		for _, ip := range ips {
			if !publicIP(ip.IP) {
				return nil, ErrStorage
			}
		}
		return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}}}
}
func internalEndpoint(base, key string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || key == "" {
		return "", ErrStorage
	}
	return strings.TrimRight(base, "/") + "/api/internal/media/files", nil
}

// Configured reports local transport configuration, not remote storage health.
func Configured() bool {
	_, err := internalEndpoint(os.Getenv("EXTERNAL_BALANCE_URL"), os.Getenv("EXTERNAL_BALANCE_KEY"))
	return err == nil
}
func Store(ctx context.Context, task string, index int, source string) (Receipt, error) {
	download := downloadClient()
	defer download.CloseIdleConnections()
	return store(ctx, task, index, source, os.Getenv("EXTERNAL_BALANCE_URL"), os.Getenv("EXTERNAL_BALANCE_KEY"), download, &http.Client{Timeout: 60 * time.Second, CheckRedirect: noRedirect})
}
func store(ctx context.Context, task string, index int, source, base, key string, download, upload *http.Client) (Receipt, error) {
	var empty Receipt
	if !identity.MatchString(task) || index < 0 || index >= 1024 {
		return empty, ErrStorage
	}
	u, err := url.Parse(source)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return empty, ErrUnsupportedSource
	}
	endpoint, err := internalEndpoint(base, key)
	if err != nil {
		return empty, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return empty, ErrStorage
	}
	response, err := download.Do(request)
	if err != nil {
		return empty, ErrStorage
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != http.StatusRequestTimeout && response.StatusCode != http.StatusTooManyRequests {
		return empty, ErrSourceRejected
	}
	if response.StatusCode != http.StatusOK {
		return empty, ErrStorage
	}
	if response.ContentLength > MaxBytes {
		return empty, ErrTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxBytes+1))
	if len(data) > MaxBytes {
		return empty, ErrTooLarge
	}
	if err != nil || (response.ContentLength >= 0 && response.ContentLength != int64(len(data))) {
		return empty, ErrStorage
	}
	digest := sha256.Sum256(data)
	request, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return empty, ErrStorage
	}
	request.GetBody = nil // avoid implicit replay: caller owns durable archive identity
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-Native-Task-ID", task)
	request.Header.Set("X-Native-Artifact-Index", strconv.Itoa(index))
	stored, err := upload.Do(request)
	if err != nil {
		return empty, ErrStorage
	}
	defer stored.Body.Close()
	if stored.StatusCode != http.StatusOK {
		return empty, ErrStorage
	}
	raw, err := io.ReadAll(io.LimitReader(stored.Body, 16385))
	if err != nil || len(raw) > 16384 {
		return empty, ErrStorage
	}
	var result struct {
		Code *int    `json:"code"`
		Data Receipt `json:"data"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Code == nil || *result.Code != 0 || !keyPattern.MatchString(result.Data.Key) || !strings.HasPrefix(result.Data.Key, "native-results/"+task+"/"+strconv.Itoa(index)+"/") || result.Data.Size != int64(len(data)) || result.Data.SHA256 != hex.EncodeToString(digest[:]) {
		return empty, ErrStorage
	}
	return result.Data, nil
}

// Download receives only a recorded storage key, never a caller-supplied URL.
func Download(ctx context.Context, key string) (*http.Response, error) {
	if !keyPattern.MatchString(key) {
		return nil, ErrStorage
	}
	token := os.Getenv("EXTERNAL_BALANCE_KEY")
	endpoint, err := internalEndpoint(os.Getenv("EXTERNAL_BALANCE_URL"), token)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?key="+url.QueryEscape(key), nil)
	if err != nil {
		return nil, ErrStorage
	}
	request.Header.Set("Authorization", "Bearer "+token)
	// A caller's deadline bounds the whole transfer, which may stream for longer.
	timeout := 60 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}
	response, err := (&http.Client{Timeout: timeout, CheckRedirect: noRedirect}).Do(request)
	if err != nil {
		return nil, ErrStorage
	}
	if response.StatusCode != http.StatusOK || response.ContentLength > MaxBytes {
		response.Body.Close()
		return nil, ErrStorage
	}
	return response, nil
}

func ValidReceipt(task string, index int, r Receipt) bool {
	if !identity.MatchString(task) || index < 0 || index >= 1024 || r.Size < 0 || r.Size > MaxBytes || !keyPattern.MatchString(r.Key) || !strings.HasPrefix(r.Key, "native-results/"+task+"/"+strconv.Itoa(index)+"/") || len(r.SHA256) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(r.SHA256)
	return err == nil && len(decoded) == 32
}
