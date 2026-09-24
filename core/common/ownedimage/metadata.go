package ownedimage

import (
	"bufio"
	"context"
	"image"
	"io"
	"net/http"
	"net/url"
)

// ReadMetadata reads only a bounded image header, with the same DNS/private-IP
// and redirect protection used when persisting generated images. No credentials
// are sent to the result host and no generation is submitted.
func ReadMetadata(ctx context.Context, source string) (Metadata, error) {
	client := downloadClient()
	defer client.CloseIdleConnections()
	return readMetadata(ctx, source, client)
}
func readMetadata(ctx context.Context, source string, client *http.Client) (Metadata, error) {
	target, err := url.Parse(source)
	if err != nil || target.Scheme != "https" || target.Hostname() == "" || target.User != nil || target.Fragment != "" || (target.Port() != "" && target.Port() != "443") {
		return Metadata{}, ErrStorage
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return Metadata{}, ErrStorage
	}
	req.Header.Set("Range", "bytes=0-1048575")
	response, err := client.Do(req)
	if err != nil {
		return Metadata{}, ErrStorage
	}
	defer response.Body.Close()
	if response.StatusCode != 200 && response.StatusCode != 206 {
		return Metadata{}, ErrStorage
	}
	reader := bufio.NewReader(io.LimitReader(response.Body, 1<<20))
	prefix, _ := reader.Peek(512)
	mime := http.DetectContentType(prefix)
	if mime != "image/png" && mime != "image/jpeg" && mime != "image/webp" {
		return Metadata{}, ErrStorage
	}
	config, _, err := image.DecodeConfig(reader)
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return Metadata{}, ErrStorage
	}
	return Metadata{Width: config.Width, Height: config.Height, ContentType: mime}, nil
}
