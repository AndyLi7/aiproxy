package nativeresult

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
)

// ArtifactBinding is frozen by the registry, never inferred from returned URL
// strings. A * path segment traverses an array; ordinary object keys are literal.
type ArtifactBinding struct {
	Path []string `json:"path"`
}
type Artifact struct {
	Pointer string
	Source  string
	Path    []string
}

const MaxArtifacts = 1024

func PlanArtifacts(raw []byte, bindings []ArtifactBinding) ([]Artifact, error) {
	root, err := decode(raw)
	if err != nil {
		return nil, err
	}
	if len(bindings) > 64 {
		return nil, ErrInvalid
	}
	result := []Artifact{}
	seen := map[string]bool{}
	for _, binding := range bindings {
		if len(binding.Path) == 0 || len(binding.Path) > MaxDepth {
			return nil, ErrInvalid
		}
		var visit func(any, int, []string) error
		visit = func(node any, at int, path []string) error {
			if node == nil {
				return nil
			}
			if at == len(binding.Path) {
				source, ok := node.(string)
				if !ok {
					return ErrInvalid
				}
				parsed, err := url.Parse(source)
				if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || (parsed.Port() != "" && parsed.Port() != "443") {
					return ErrInvalid
				}
				pointer := ""
				for _, part := range path {
					pointer += "/" + strings.ReplaceAll(strings.ReplaceAll(part, "~", "~0"), "/", "~1")
				}
				if seen[pointer] || len(result) >= MaxArtifacts {
					return ErrInvalid
				}
				seen[pointer] = true
				result = append(result, Artifact{Pointer: pointer, Source: source, Path: append([]string(nil), path...)})
				return nil
			}
			part := binding.Path[at]
			if part == "*" {
				list, ok := node.([]any)
				if !ok {
					return ErrInvalid
				}
				for i, child := range list {
					if err := visit(child, at+1, append(append([]string(nil), path...), strconv.Itoa(i))); err != nil {
						return err
					}
				}
				return nil
			}
			object, ok := node.(map[string]any)
			if !ok {
				return ErrInvalid
			}
			child, exists := object[part]
			if !exists {
				return nil
			}
			return visit(child, at+1, append(append([]string(nil), path...), part))
		}
		if err := visit(root, 0, nil); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// OwnedFile is where an archived artifact is delivered from: the URL that
// replaces the provider's, and the name and type the file is served with.
type OwnedFile struct {
	URL         string
	FileName    string
	ContentType string
}

// UnboundURLs says what DeliverArtifacts does with an http(s) string outside
// the declared artifact leaves.
type UnboundURLs int

const (
	// RemoveUnboundURLs deletes the member or array element holding it.
	RemoveUnboundURLs UnboundURLs = iota
	// BlankUnboundURLs keeps the member and empties the string, for an output
	// schema that requires the member.
	BlankUnboundURLs
)

// RewriteArtifacts replaces exactly the declared leaves after durable storage.
// Every replacement must be present, so partial archive success is not delivery.
// Unknown data and exact JSON numbers remain intact. Unclassified URL-like
// strings are data; they are neither fetched nor recursively rewritten.
func RewriteArtifacts(raw []byte, bindings []ArtifactBinding, owned map[string]string) (json.RawMessage, error) {
	files := make(map[string]OwnedFile, len(owned))
	for pointer, destination := range owned {
		files[pointer] = OwnedFile{URL: destination}
	}
	output, _, err := rewrite(raw, bindings, files, nil)
	return output, err
}

// DeliverArtifacts is RewriteArtifacts for delivery. A declared leaf's sibling
// "file_name" and "content_type" strings, when present, are set to the owned
// file's. Every other http(s) string is removed or blanked (unbound), so a
// provider address outside the declared files never reaches a customer; the
// count of those is returned for the operator log.
func DeliverArtifacts(raw []byte, bindings []ArtifactBinding, owned map[string]OwnedFile, unbound UnboundURLs) (json.RawMessage, int, error) {
	return rewrite(raw, bindings, owned, &unbound)
}

func rewrite(raw []byte, bindings []ArtifactBinding, owned map[string]OwnedFile, unbound *UnboundURLs) (json.RawMessage, int, error) {
	artifacts, err := PlanArtifacts(raw, bindings)
	if err != nil {
		return nil, 0, err
	}
	if len(owned) != len(artifacts) {
		return nil, 0, ErrInvalid
	}
	if len(artifacts) == 0 && unbound == nil {
		return append(json.RawMessage(nil), raw...), 0, nil
	}
	root, err := decode(raw)
	if err != nil {
		return nil, 0, err
	}
	destinations := map[string]bool{}
	for _, artifact := range artifacts {
		file, ok := owned[artifact.Pointer]
		if !ok {
			return nil, 0, ErrInvalid
		}
		destination := file.URL
		u, err := url.Parse(destination)
		if err != nil || u.User != nil || u.Fragment != "" || !((u.Scheme == "https" && u.Host != "") || (u.Scheme == "" && u.Host == "" && strings.HasPrefix(u.Path, "/v1/model-tasks/"))) {
			return nil, 0, ErrInvalid
		}
		destinations[destination] = true
		node := root
		for i, part := range artifact.Path {
			last := i == len(artifact.Path)-1
			switch value := node.(type) {
			case map[string]any:
				if last {
					value[part] = destination
					if unbound != nil {
						setString(value, "file_name", file.FileName)
						setString(value, "content_type", file.ContentType)
					}
				} else {
					node = value[part]
				}
			case []any:
				index, err := strconv.Atoi(part)
				if err != nil || index < 0 || index >= len(value) {
					return nil, 0, ErrInvalid
				}
				if last {
					value[index] = destination
				} else {
					node = value[index]
				}
			default:
				return nil, 0, ErrInvalid
			}
		}
	}
	removed := 0
	if unbound != nil {
		root = dropUnboundURLs(root, destinations, *unbound, &removed)
	}
	result, err := json.Marshal(root)
	if err != nil {
		return nil, 0, ErrInvalid
	}
	return result, removed, nil
}

// setString replaces an existing string member only; it never adds one.
func setString(object map[string]any, key, value string) {
	if _, isString := object[key].(string); isString && value != "" {
		object[key] = value
	}
}

func unboundURL(value string, owned map[string]bool) bool {
	if owned[value] {
		return false
	}
	lower := strings.ToLower(value)
	return strings.Contains(lower, "https://") || strings.Contains(lower, "http://")
}

func dropUnboundURLs(node any, owned map[string]bool, mode UnboundURLs, removed *int) any {
	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			if unboundURL(key, nil) {
				delete(value, key)
				*removed++
				continue
			}
			if text, ok := child.(string); ok && unboundURL(text, owned) {
				*removed++
				if mode == BlankUnboundURLs {
					value[key] = ""
				} else {
					delete(value, key)
				}
				continue
			}
			value[key] = dropUnboundURLs(child, owned, mode, removed)
		}
		return value
	case []any:
		kept := value[:0]
		for _, child := range value {
			if text, ok := child.(string); ok && unboundURL(text, owned) {
				*removed++
				if mode == BlankUnboundURLs {
					kept = append(kept, "")
				}
				continue
			}
			kept = append(kept, dropUnboundURLs(child, owned, mode, removed))
		}
		return kept
	}
	if text, ok := node.(string); ok && unboundURL(text, owned) {
		*removed++
		return ""
	}
	return node
}
