// Package imagecompat translates an explicitly enabled retail image profile.
// It does not route providers, reserve money, or execute generation.
package imagecompat

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"

	"github.com/labring/aiproxy/core/common/nativeresult"
	_ "golang.org/x/image/webp"
)

const (
	GenerateModel = "openai/gpt-image-1.5/text-to-image"
	EditModel     = "openai/gpt-image-1.5/image-to-image"
	MaxImageBytes = 2 << 20
	MaxBodyBytes  = 3 << 20
)

var (
	ErrInput    = errors.New("unsupported or invalid image parameter")
	ErrBusy     = errors.New("image validation is temporarily busy")
	ErrIdentity = errors.New(
		"Idempotency-Key must be 1-128 letters, digits, underscores or hyphens",
	)
	ErrRetry = errors.New(
		"automatic retry without Idempotency-Key is unsafe; query the original operation",
	)
	decodeSlots = make(chan struct{}, 2)
	keyPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	// Reuse native strict JSON parsing (duplicate keys, depth, UTF-8, trailing bytes).
	objectValidator, _ = nativeresult.Compile([]byte(`{"type":"object"}`))
)

type Request struct {
	Model  string
	Format string
	Body   []byte
}

// Parse consumes a bounded body only after the controller selected this profile.
// For multipart, filename and MIME declarations are not trusted as image content.
func Parse(r *http.Request, edit bool) (Request, error) {
	if r.ContentLength > MaxBodyBytes {
		return Request{}, ErrInput
	}

	fields, imageBytes, err := readFields(r, edit)
	if err != nil {
		return Request{}, err
	}

	result := Request{Format: "b64_json"}

	input := map[string]json.RawMessage{"num_images": json.RawMessage(`1`)}
	for key, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return Request{}, ErrInput
		}

		switch key {
		case "model":
			if json.Unmarshal(value, &result.Model) != nil || result.Model == "" {
				return Request{}, ErrInput
			}
		case "response_format":
			if json.Unmarshal(value, &result.Format) != nil ||
				(result.Format != "url" && result.Format != "b64_json") {
				return Request{}, ErrInput
			}
		case "n":
			var n int
			if json.Unmarshal(value, &n) != nil || n != 1 {
				return Request{}, ErrInput
			}
		case "prompt", "size", "quality", "background", "output_format", "input_fidelity":
			var s string
			if json.Unmarshal(value, &s) != nil || s == "" || (key == "input_fidelity" && !edit) {
				return Request{}, ErrInput
			}

			value, err = json.Marshal(s)
			if err != nil {
				return Request{}, ErrInput
			}

			if key == "size" {
				input["image_size"] = value
			} else {
				input[key] = value
			}
		default:
			return Request{}, ErrInput
		}
	}

	if result.Model == "" || input["prompt"] == nil {
		return Request{}, ErrInput
	}

	if edit {
		input["image_urls"], err = inlineImage(imageBytes)
		if err != nil {
			return Request{}, err
		}
	}

	result.Body, err = json.Marshal(struct {
		Model string `json:"model"`
		Input any    `json:"input"`
	}{result.Model, input})
	if err != nil || len(result.Body) > nativeresult.MaxBytes {
		return Request{}, ErrInput
	}

	return result, nil
}

// readFields bounds multipart parsing before any image decode or wallet access.
func readFields(r *http.Request, edit bool) (map[string]json.RawMessage, []byte, error) {
	fields := map[string]json.RawMessage{}

	var imageBytes []byte

	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return nil, nil, ErrInput
	}

	if !edit {
		if media != "application/json" {
			return nil, nil, ErrInput
		}

		raw, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
		if err != nil || len(raw) > MaxBodyBytes {
			return nil, nil, ErrInput
		}

		if _, err = objectValidator.Validate(raw); err != nil {
			return nil, nil, ErrInput
		}

		if json.Unmarshal(raw, &fields) != nil {
			return nil, nil, ErrInput
		}
	} else {
		if media != "multipart/form-data" {
			return nil, nil, ErrInput
		}
		// ParseMultipartForm may already have been used to select the route.
		if r.MultipartForm == nil {
			r.Body = http.MaxBytesReader(nil, r.Body, MaxBodyBytes)
			// #nosec G120 -- MaxBytesReader bounds the complete HTTP body immediately above.
			if r.ParseMultipartForm(MaxBodyBytes) != nil {
				return nil, nil, ErrInput
			}
		}

		if r.MultipartForm == nil {
			return nil, nil, ErrInput
		}
		defer func() { _ = r.MultipartForm.RemoveAll() }()

		for key, values := range r.MultipartForm.Value {
			if len(values) != 1 {
				return nil, nil, ErrInput
			}

			if key == "n" {
				n, err := strconv.Atoi(values[0])
				if err != nil || values[0] != "1" || n != 1 {
					return nil, nil, ErrInput
				}

				fields[key] = json.RawMessage(`1`)
			} else {
				fields[key], err = json.Marshal(values[0])
				if err != nil {
					return nil, nil, ErrInput
				}
			}
		}

		count := 0
		for key, files := range r.MultipartForm.File {
			if key != "image" && key != "image[]" {
				return nil, nil, ErrInput
			}

			for _, file := range files {
				count++
				if count != 1 || file.Size <= 0 || file.Size > MaxImageBytes {
					return nil, nil, ErrInput
				}

				f, err := file.Open()
				if err != nil {
					return nil, nil, ErrInput
				}

				imageBytes, err = io.ReadAll(io.LimitReader(f, MaxImageBytes+1))
				_ = f.Close()

				if err != nil || len(imageBytes) > MaxImageBytes {
					return nil, nil, ErrInput
				}
			}
		}

		if count != 1 {
			return nil, nil, ErrInput
		}
	}

	return fields, imageBytes, nil
}

func inlineImage(imageBytes []byte) (json.RawMessage, error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(imageBytes))
	if err != nil || config.Width <= 0 || config.Height <= 0 ||
		int64(config.Width)*int64(config.Height) > 16_777_216 {
		return nil, ErrInput
	}

	if format != "png" && format != "jpeg" && format != "webp" {
		return nil, ErrInput
	}

	select {
	case decodeSlots <- struct{}{}:
		defer func() { <-decodeSlots }()
	default:
		return nil, ErrBusy
	}

	if _, decodedFormat, err := image.Decode(
		bytes.NewReader(imageBytes),
	); err != nil ||
		decodedFormat != format {
		return nil, ErrInput
	}

	inline := "data:image/" + format + ";base64," + base64.StdEncoding.EncodeToString(
		imageBytes,
	)

	return json.Marshal([]string{inline})
}

// OperationID deliberately ignores edge-generated X-Request-ID. Owners cannot
// collide through a chosen key, and raw customer keys are not stored or logged.
func OperationID(headers http.Header, group string, token int) (string, error) {
	keys := headers.Values("Idempotency-Key")
	if len(keys) > 1 {
		return "", ErrIdentity
	}

	key := headers.Get("Idempotency-Key")
	if len(keys) > 0 && !keyPattern.MatchString(key) {
		return "", ErrIdentity
	}

	if key == "" {
		retry := headers.Get("X-Stainless-Retry-Count")
		if retry != "" && retry != "0" {
			return "", ErrRetry
		}

		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}

		return "oi_" + hex.EncodeToString(raw), nil
	}

	raw, err := json.Marshal([]any{"openai-images-v1", group, token, key})
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(raw)

	return "oi_" + hex.EncodeToString(sum[:]), nil
}
