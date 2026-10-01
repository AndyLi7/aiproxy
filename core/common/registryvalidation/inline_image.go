package registryvalidation

import (
	"encoding/base64"
	"net/http"
	"slices"
	"strings"
)

// NativeInlineImagePolicy describes only an explicitly declared image leaf.
// Callers must bind it to a frozen contract path, never infer it from a request.
// This validator is not enabled for existing URL-only image contracts.
type NativeInlineImagePolicy struct {
	Encoding            string   `json:"encoding"`
	MaxDecodedBytes     int64    `json:"maxDecodedBytes"`
	MediaTypes          []string `json:"mediaTypes"`
	MediaTypeValidation string   `json:"mediaTypeValidation,omitempty"`
}

// A leaf policy must be tied to one of the contract's discovered image paths.
type NativeInlineImageBinding struct {
	Path []string `json:"path"`
	NativeInlineImagePolicy
	AllowURL bool `json:"allowUrl,omitempty"`
}

func validateInlineImagePolicy(policy NativeInlineImagePolicy) error {
	if (policy.Encoding != "raw-base64" && policy.Encoding != "data-uri") || policy.MaxDecodedBytes < 1 || policy.MaxDecodedBytes > 50*1024*1024 || (len(policy.MediaTypes) == 0 && policy.MediaTypeValidation != "upstream") || len(policy.MediaTypes) > 3 {
		return ErrProviderContract
	}
	if policy.MediaTypeValidation != "" && (policy.MediaTypeValidation != "upstream" || len(policy.MediaTypes) != 0) {
		return ErrProviderContract
	}
	seen := map[string]bool{}
	for _, mediaType := range policy.MediaTypes {
		if !slices.Contains([]string{"image/jpeg", "image/png", "image/webp"}, mediaType) || seen[mediaType] {
			return ErrProviderContract
		}
		seen[mediaType] = true
	}
	return nil
}

// ValidateNativeInlineImage checks encoding, a bounded decoded size and the
// declared file-header type. Actual image decoding remains the provider's job.
// It does not fetch URLs, rewrite values, or claim that a file is uncorrupted.
func ValidateNativeInlineImage(text string, policy NativeInlineImagePolicy) error {
	if err := validateInlineImagePolicy(policy); err != nil {
		return err
	}
	payload := text
	declaredType := ""
	if policy.Encoding == "data-uri" {
		header, body, ok := strings.Cut(text, ",")
		if !ok || !strings.HasPrefix(header, "data:") || !strings.HasSuffix(header, ";base64") {
			return ErrProviderContract
		}
		declaredType = strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
		if policy.MediaTypeValidation == "upstream" {
			if !strings.HasPrefix(declaredType, "image/") || len(declaredType) > 100 || strings.ContainsAny(declaredType, " ;\r\n\t") || len(declaredType) == 6 {
				return ErrProviderContract
			}
		} else if !slices.Contains(policy.MediaTypes, declaredType) {
			return ErrProviderContract
		}
		payload = body
	}
	// Keep the existing 50 MiB request-body ceiling even before allocation.
	// CR/LF are standard base64 line separators, not decoded image bytes.
	maxEncoded := ((policy.MaxDecodedBytes + 2) / 3) * 4
	if len(payload) == 0 || len(text) > 50*1024*1024 {
		return ErrProviderContract
	}
	compact := strings.NewReplacer("\r", "", "\n", "").Replace(payload)
	if len(compact) == 0 || len(compact)%4 != 0 || int64(len(compact)) > maxEncoded {
		return ErrProviderContract
	}
	padding := 0
	if strings.HasSuffix(compact, "=") {
		padding++
	}
	if strings.HasSuffix(compact, "==") {
		padding++
	}
	decodedSize := int64(len(compact)/4*3 - padding)
	if decodedSize < 1 || decodedSize > policy.MaxDecodedBytes {
		return ErrProviderContract
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(compact)
	if err != nil {
		return ErrProviderContract
	}
	if policy.MediaTypeValidation == "upstream" {
		return nil
	}
	actualType := http.DetectContentType(decoded)
	if !slices.Contains(policy.MediaTypes, actualType) || (declaredType != "" && declaredType != actualType) {
		return ErrProviderContract
	}
	return nil
}
