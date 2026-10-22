// Package ownedartifact archives native files without interpreting active content.
package ownedartifact

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
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

// MaxBytes is the largest file archived. Transfers stream, so memory does not
// grow with it.
const MaxBytes = 256 << 20

// ArchiveBudget also bounds header/receipt stalls when Store is used directly.
const ArchiveBudget = 10 * time.Minute

// ChunkSize is the unit of a verified read. The digest of every chunk is
// recorded when the file is archived, so any byte range is served after
// checking only the chunks it touches, and a reader never holds more than one.
const ChunkSize = 2 << 20

// stallTimeout ends a transfer that has made no progress for this long. A
// large file may take minutes; a stuck one must not hold its worker.
var stallTimeout = 60 * time.Second

var (
	ErrStorage  = errors.New("native artifact storage unavailable")
	ErrTooLarge = errors.New("native artifact exceeds archive limit")
)

// ErrUnsupportedSource and ErrSourceRejected are permanent: the provider URL
// can never be archived, so retrying the same task cannot succeed.
var (
	ErrUnsupportedSource = errors.New("native artifact source is not archivable")
	ErrSourceRejected    = errors.New("native artifact source rejected the download")
	identity             = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	keyPattern           = regexp.MustCompile(
		`^native-results/[A-Za-z0-9_-]{1,128}/[0-9]{1,4}/[a-f0-9]{64}\.bin$`,
	)
	chunkDigests = regexp.MustCompile(`^(?:[a-f0-9]{64})*$`)
)

type Receipt struct {
	Key    string `json:"key"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	// Type is the media type read from the file's first bytes when it was
	// archived; "" when unrecognised or archived before types were recorded.
	Type string `json:"type,omitempty"`
	// Chunks is the hex SHA-256 of each ChunkSize piece, concatenated. A
	// receipt without it (archived before ranges existed) is served whole.
	Chunks string `json:"chunks,omitempty"`
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func publicIP(ip net.IP) bool {
	for _, cidr := range []string{"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "2001:db8::/32", "2001::/32", "2002::/16", "64:ff9b::/96", "64:ff9b:1::/48"} {
		_, block, _ := net.ParseCIDR(cidr)
		if block.Contains(ip) {
			return false
		}
	}

	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() &&
		!ip.IsLinkLocalUnicast() &&
		!ip.IsUnspecified()
}

func downloadClient() *http.Client {
	// No whole-transfer timeout: a large file streams for as long as it keeps
	// moving (stallTimeout) within the caller's deadline.
	return &http.Client{
		CheckRedirect: noRedirect,
		Transport: &http.Transport{
			DisableCompression:     true,
			MaxResponseHeaderBytes: 1 << 20,
			ResponseHeaderTimeout:  60 * time.Second,
			TLSHandshakeTimeout:    15 * time.Second,
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
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

				return (&net.Dialer{Timeout: 15 * time.Second}).DialContext(
					ctx,
					network,
					net.JoinHostPort(ips[0].IP.String(), port),
				)
			},
		},
	}
}

// uploadClient is bounded by the archive context while waiting for the
// application, which stores the whole file before it answers.
func uploadClient() *http.Client {
	transport := artifactTransport()
	transport.ResponseHeaderTimeout = 10 * time.Minute
	return &http.Client{CheckRedirect: noRedirect, Transport: transport}
}

func internalEndpoint(base, key string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil ||
		u.RawQuery != "" ||
		u.Fragment != "" ||
		key == "" {
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
	download, upload := downloadClient(), uploadClient()
	defer download.CloseIdleConnections()
	defer upload.CloseIdleConnections()

	return store(
		ctx,
		task,
		index,
		source,
		os.Getenv("EXTERNAL_BALANCE_URL"),
		os.Getenv("EXTERNAL_BALANCE_KEY"),
		download,
		upload,
	)
}

// StoreWithSourceClient preserves archive validation and application storage,
// while allowing a caller to supply the outer provider download transport.
// The production Store uses the SSRF-restricted downloadClient above.
func StoreWithSourceClient(ctx context.Context, task string, index int, source string, download *http.Client) (Receipt, error) {
	if download == nil {
		return Receipt{}, ErrStorage
	}
	upload := uploadClient()
	defer upload.CloseIdleConnections()
	return store(ctx, task, index, source, os.Getenv("EXTERNAL_BALANCE_URL"), os.Getenv("EXTERNAL_BALANCE_KEY"), download, upload)
}

// stallReader cancels the transfer when no byte has arrived for stallTimeout.
type stallReader struct {
	io.Reader
	timer *time.Timer
}

func (r *stallReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 {
		r.timer.Reset(stallTimeout)
	}

	return n, err
}

// digester records what was streamed: the whole file's SHA-256, each chunk's
// SHA-256 and the size.
type digester struct {
	whole  hash.Hash
	chunk  hash.Hash
	filled int64
	chunks strings.Builder
	size   int64
}

func newDigester() *digester { return &digester{whole: sha256.New(), chunk: sha256.New()} }
func (d *digester) Write(p []byte) (int, error) {
	written := len(p)
	d.whole.Write(p)

	d.size += int64(len(p))
	for len(p) > 0 {
		take := min(int64(len(p)), ChunkSize-d.filled)
		d.chunk.Write(p[:take])
		d.filled += take
		p = p[take:]

		if d.filled == ChunkSize {
			d.closeChunk()
		}
	}

	return written, nil
}

func (d *digester) closeChunk() {
	d.chunks.WriteString(hex.EncodeToString(d.chunk.Sum(nil)))
	d.chunk.Reset()
	d.filled = 0
}

func (d *digester) finish() (string, string) {
	if d.filled > 0 {
		d.closeChunk()
	}
	return hex.EncodeToString(d.whole.Sum(nil)), d.chunks.String()
}

// store streams the provider file into the application's storage: bytes are
// hashed and forwarded as they arrive, never held whole. The receipt's digests
// are computed here and must match what the application stored.
func store(
	ctx context.Context,
	task string,
	index int,
	source, base, key string,
	download, upload *http.Client,
) (Receipt, error) {
	var empty Receipt
	if !identity.MatchString(task) || index < 0 || index >= 1024 {
		return empty, ErrStorage
	}

	if err := validateArtifactSource(source); err != nil {
		return empty, err
	}

	endpoint, err := internalEndpoint(base, key)
	if err != nil {
		return empty, err
	}

	ctx, cancel := context.WithTimeout(ctx, ArchiveBudget)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return empty, ErrStorage
	}

	request.Header.Set("Accept-Encoding", "identity")

	//nolint:bodyclose // Deferred close covers every return; the additional early close unblocks the copier when upload responds early.
	response, err := download.Do(request)
	if err != nil {
		return empty, ErrStorage
	}
	defer response.Body.Close()

	if err := validateArtifactDownload(response); err != nil {
		return empty, err
	}

	stall := time.AfterFunc(stallTimeout, cancel)
	defer stall.Stop()

	body := bufio.NewReaderSize(
		io.LimitReader(&stallReader{Reader: response.Body, timer: stall}, MaxBytes+1),
		sniffBytes,
	)

	head, err := body.Peek(sniffBytes)
	if err != nil && !errors.Is(err, io.EOF) {
		return empty, ErrStorage
	}

	mediaType := DetectType(head)
	digest := newDigester()
	reader, writer := io.Pipe()

	copied := make(chan error, 1)
	go func() {
		n, err := io.Copy(io.MultiWriter(digest, writer), body)

		stall.Stop()

		switch {
		case n > MaxBytes:
			err = ErrTooLarge
		case err != nil:
			err = ErrStorage
		case response.ContentLength >= 0 && n != response.ContentLength:
			err = ErrStorage
		}

		writer.CloseWithError(err)

		copied <- err
	}()

	// #nosec G704 -- The internal media base is trusted server configuration and this client disables redirects; private service addresses are intentional.
	request, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint, reader)
	if err != nil {
		reader.CloseWithError(ErrStorage)

		_ = <-copied //nolint:staticcheck // Join the copier; the request setup failure already determines the result.
		return empty, ErrStorage
	}

	request.ContentLength = response.ContentLength
	if response.ContentLength == 0 {
		request.Body = http.NoBody
	}

	request.GetBody = nil // avoid implicit replay: caller owns durable archive identity
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-Native-Task-Id", task)
	request.Header.Set("X-Native-Artifact-Index", strconv.Itoa(index))

	if mediaType != "" {
		request.Header.Set("X-Native-Artifact-Type", mediaType)
	}

	// #nosec G704 -- The internal media base is trusted server configuration and this client disables redirects; private service addresses are intentional.
	stored, err := upload.Do(request)

	stall.Stop()
	// Also stop a source blocked in Read when the application answers early.
	response.Body.Close()
	// The application may answer before reading every byte; stop the copy.
	reader.CloseWithError(ErrStorage)

	copyErr := <-copied
	if errors.Is(copyErr, ErrTooLarge) {
		if err == nil {
			stored.Body.Close()
		}
		return empty, ErrTooLarge
	}

	if err != nil {
		return empty, ErrStorage
	}

	defer stored.Body.Close()

	if copyErr != nil || stored.StatusCode != http.StatusOK {
		return empty, ErrStorage
	}

	raw, err := io.ReadAll(io.LimitReader(stored.Body, 16385))
	if err != nil || len(raw) > 16384 {
		return empty, ErrStorage
	}

	return decodeArtifactReceipt(raw, digest, task, index, mediaType)
}

// Download receives only a recorded storage key, never a caller-supplied URL.
func Download(ctx context.Context, key string) (*http.Response, error) {
	return download(ctx, key, "")
}

// DownloadRange reads bytes from..to (inclusive) of a stored file.
func DownloadRange(ctx context.Context, key string, from, to int64) (*http.Response, error) {
	if from < 0 || to < from || to >= MaxBytes {
		return nil, ErrStorage
	}

	response, err := download(
		ctx,
		key,
		"bytes="+strconv.FormatInt(from, 10)+"-"+strconv.FormatInt(to, 10),
	)
	if err != nil {
		return nil, err
	}

	prefix := "bytes " + strconv.FormatInt(from, 10) + "-" + strconv.FormatInt(to, 10) + "/"

	total, parseErr := strconv.ParseInt(
		strings.TrimPrefix(response.Header.Get("Content-Range"), prefix),
		10,
		64,
	)
	if response.StatusCode != http.StatusPartialContent || response.ContentLength != to-from+1 ||
		!strings.HasPrefix(response.Header.Get("Content-Range"), prefix) ||
		parseErr != nil ||
		total <= to ||
		total > MaxBytes {
		response.Body.Close()
		return nil, ErrStorage
	}

	return response, nil
}

func download(ctx context.Context, key, byteRange string) (*http.Response, error) {
	if !keyPattern.MatchString(key) {
		return nil, ErrStorage
	}

	token := os.Getenv("EXTERNAL_BALANCE_KEY")

	endpoint, err := internalEndpoint(os.Getenv("EXTERNAL_BALANCE_URL"), token)
	if err != nil {
		return nil, err
	}

	// #nosec G704 -- The internal media base is trusted server configuration and this client disables redirects; private service addresses are intentional.
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		endpoint+"?key="+url.QueryEscape(key),
		nil,
	)
	if err != nil {
		return nil, ErrStorage
	}

	request.Header.Set("Authorization", "Bearer "+token)

	if byteRange != "" {
		request.Header.Set("Range", byteRange)
	}
	// A caller's deadline bounds the whole transfer, which may stream for longer.
	timeout := 60 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}

	transferCtx, cancel := context.WithTimeout(ctx, timeout)
	request = request.WithContext(transferCtx)
	request.Header.Set("Accept-Encoding", "identity")

	transport := artifactTransport()
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.DisableCompression = true

	// #nosec G704 -- The internal media base is trusted server configuration and this client disables redirects; private service addresses are intentional.
	response, err := (&http.Client{CheckRedirect: noRedirect, Transport: transport}).Do(request)
	if err != nil {
		cancel()
		transport.CloseIdleConnections()
		return nil, ErrStorage
	}

	if (byteRange == "" && response.StatusCode != http.StatusOK) || response.ContentLength < 0 ||
		response.ContentLength > MaxBytes ||
		(response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity") {
		response.Body.Close()
		cancel()
		transport.CloseIdleConnections()
		return nil, ErrStorage
	}

	wrapped := &timedBody{
		ReadCloser: response.Body,
		cancel:     cancel,
		closeIdle:  transport.CloseIdleConnections,
	}
	wrapped.timer = time.AfterFunc(stallTimeout, cancel)
	response.Body = wrapped

	return response, nil
}

type timedBody struct {
	io.ReadCloser
	timer     *time.Timer
	cancel    context.CancelFunc
	closeIdle func()
}

func (b *timedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.timer.Reset(stallTimeout)
	}

	return n, err
}

func (b *timedBody) Close() error {
	b.timer.Stop()
	b.cancel()
	err := b.ReadCloser.Close()
	b.closeIdle()

	return err
}

// ChunkCount is how many ChunkSize pieces a file of size bytes has.
func ChunkCount(size int64) int64 { return (size + ChunkSize - 1) / ChunkSize }

func ValidReceipt(task string, index int, r Receipt) bool {
	if !identity.MatchString(task) || index < 0 || index >= 1024 || r.Size < 0 ||
		r.Size > MaxBytes ||
		!keyPattern.MatchString(r.Key) ||
		!strings.HasPrefix(r.Key, "native-results/"+task+"/"+strconv.Itoa(index)+"/") ||
		len(r.SHA256) != 64 {
		return false
	}

	if r.Type != "" && !knownType(r.Type) {
		return false
	}

	if r.Chunks != "" &&
		(int64(len(r.Chunks)) != 64*ChunkCount(r.Size) || !chunkDigests.MatchString(r.Chunks)) {
		return false
	}

	decoded, err := hex.DecodeString(r.SHA256)

	return err == nil && len(decoded) == 32
}

func artifactTransport() *http.Transport {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		return transport.Clone()
	}
	// Preserve a usable transport even when a host application replaces DefaultTransport.
	return (&http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: true, MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ExpectContinueTimeout: time.Second}).Clone()
}

func validateArtifactSource(source string) error {
	u, err := url.Parse(source)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.Fragment != "" ||
		(u.Port() != "" && u.Port() != "443") {
		return ErrUnsupportedSource
	}

	return nil
}

func validateArtifactDownload(response *http.Response) error {
	if response.StatusCode >= 400 && response.StatusCode < 500 &&
		response.StatusCode != http.StatusRequestTimeout &&
		response.StatusCode != http.StatusTooManyRequests {
		return ErrSourceRejected
	}

	if response.StatusCode != http.StatusOK {
		return ErrStorage
	}

	if response.ContentLength < 0 ||
		(response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity") {
		return ErrStorage
	}

	if response.ContentLength > MaxBytes {
		return ErrTooLarge
	}

	return nil
}

func decodeArtifactReceipt(
	raw []byte,
	digest *digester,
	task string,
	index int,
	mediaType string,
) (Receipt, error) {
	var (
		empty  Receipt
		result struct {
			Code *int    `json:"code"`
			Data Receipt `json:"data"`
		}
	)

	sum, chunks := digest.finish()
	if json.Unmarshal(raw, &result) != nil || result.Code == nil || *result.Code != 0 ||
		!keyPattern.MatchString(result.Data.Key) ||
		!strings.HasPrefix(result.Data.Key, "native-results/"+task+"/"+strconv.Itoa(index)+"/") ||
		result.Data.Size != digest.size ||
		result.Data.SHA256 != sum {
		return empty, ErrStorage
	}

	return Receipt{
		Key:    result.Data.Key,
		SHA256: sum,
		Size:   digest.size,
		Type:   mediaType,
		Chunks: chunks,
	}, nil
}
