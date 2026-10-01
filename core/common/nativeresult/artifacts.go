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

// RewriteArtifacts replaces exactly the declared leaves after durable storage.
// Every replacement must be present, so partial archive success is not delivery.
// Unknown data and exact JSON numbers remain intact. Unclassified URL-like
// strings are data; they are neither fetched nor recursively rewritten.
func RewriteArtifacts(raw []byte, bindings []ArtifactBinding, owned map[string]string) (json.RawMessage, error) {
	artifacts, err := PlanArtifacts(raw, bindings)
	if err != nil {
		return nil, err
	}
	if len(owned) != len(artifacts) {
		return nil, ErrInvalid
	}
	if len(artifacts) == 0 {
		return append(json.RawMessage(nil), raw...), nil
	}
	root, err := decode(raw)
	if err != nil {
		return nil, err
	}
	for _, artifact := range artifacts {
		destination, ok := owned[artifact.Pointer]
		if !ok {
			return nil, ErrInvalid
		}
		u, err := url.Parse(destination)
		if err != nil || u.User != nil || u.Fragment != "" || !((u.Scheme == "https" && u.Host != "") || (u.Scheme == "" && u.Host == "" && strings.HasPrefix(u.Path, "/v1/model-tasks/"))) {
			return nil, ErrInvalid
		}
		node := root
		for i, part := range artifact.Path {
			last := i == len(artifact.Path)-1
			switch value := node.(type) {
			case map[string]any:
				if last {
					value[part] = destination
				} else {
					node = value[part]
				}
			case []any:
				index, err := strconv.Atoi(part)
				if err != nil || index < 0 || index >= len(value) {
					return nil, ErrInvalid
				}
				if last {
					value[index] = destination
				} else {
					node = value[index]
				}
			default:
				return nil, ErrInvalid
			}
		}
	}
	result, err := json.Marshal(root)
	if err != nil {
		return nil, ErrInvalid
	}
	return result, nil
}
