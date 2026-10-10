package nativeresult

import (
	"strings"
	"testing"
)

func TestNativeArtifactsRewriteOnlyDeclaredPaths(t *testing.T) {
	raw := []byte(`{"images":[{"url":"https://files.example/a.svg"},{"url":"https://files.example/b.svg"}],"file":null,"seed":9007199254740993,"other_url":"https://unclassified.example/data"}`)
	bindings := []ArtifactBinding{{Path: []string{"images", "*", "url"}}, {Path: []string{"file", "url"}}}
	plan, err := PlanArtifacts(raw, bindings)
	if err != nil || len(plan) != 2 {
		t.Fatalf("plan %v %v", plan, err)
	}
	got, err := RewriteArtifacts(raw, bindings, map[string]string{"/images/0/url": "/v1/model-tasks/task/artifacts/0", "/images/1/url": "/v1/model-tasks/task/artifacts/1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `9007199254740993`) || !strings.Contains(string(got), `https://unclassified.example/data`) || strings.Contains(string(got), `https://files.example`) {
		t.Fatal(string(got))
	}
	if _, err := RewriteArtifacts(raw, bindings, map[string]string{"/images/0/url": "/v1/model-tasks/task/artifacts/0"}); err == nil {
		t.Fatal("accepted partial delivery")
	}
}
func TestNativeArtifactsRejectAmbiguousBindingsAndUnsafeSchemes(t *testing.T) {
	for _, raw := range []string{`{"file":{"url":"http://files.example/a.zip"}}`, `{"file":{"url":"https://user:secret@files.example/a.zip"}}`, `{"file":{"url":7}}`} {
		if _, err := PlanArtifacts([]byte(raw), []ArtifactBinding{{Path: []string{"file", "url"}}}); err == nil {
			t.Fatal(raw)
		}
	}
	binding := ArtifactBinding{Path: []string{"file", "url"}}
	if _, err := PlanArtifacts([]byte(`{"file":{"url":"https://files.example/a.zip"}}`), []ArtifactBinding{binding, binding}); err == nil {
		t.Fatal("duplicate binding")
	}
}
