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

func TestDeliverArtifactsNamesFilesAndRemovesProviderAddresses(t *testing.T) {
	raw := []byte(`{"images":[{"url":"https://fal.media/a.png","file_name":"tmp1.png","content_type":"image/png","width":1}],` +
		`"debug":{"source":"http://fal.run/x","note":"see docs"},"extra_urls":["https://fal.media/b","data:image/png;base64,AAAA"],"inline":"data:text/plain,hi","seed":12345678901234567890}`)
	bindings := []ArtifactBinding{{Path: []string{"images", "*", "url"}}}
	owned := map[string]OwnedFile{"/images/0/url": {URL: "https://gw.example/v1/model-tasks/t/artifacts/0", FileName: "t.png", ContentType: "image/png"}}
	removed, err := func() (int, error) {
		out, n, err := DeliverArtifacts(raw, bindings, owned, RemoveUnboundURLs)
		if err != nil {
			return 0, err
		}
		text := string(out)
		if strings.Contains(text, "fal.") || !strings.Contains(text, `"file_name":"t.png"`) || !strings.Contains(text, `"data:image/png;base64,AAAA"`) ||
			!strings.Contains(text, `"inline":"data:text/plain,hi"`) || !strings.Contains(text, `"note":"see docs"`) || strings.Contains(text, `"source"`) ||
			!strings.Contains(text, `12345678901234567890`) {
			t.Fatalf("unexpected delivery %s", text)
		}
		return n, nil
	}()
	if err != nil || removed != 2 {
		t.Fatalf("removed %d, %v", removed, err)
	}
	blank, n, err := DeliverArtifacts(raw, bindings, owned, BlankUnboundURLs)
	if err != nil || n != 2 || !strings.Contains(string(blank), `"source":""`) || !strings.Contains(string(blank), `"extra_urls":["","data:image/png;base64,AAAA"]`) {
		t.Fatalf("blank %s %d %v", blank, n, err)
	}
	// RewriteArtifacts keeps its contract: only declared leaves change.
	plain, err := RewriteArtifacts(raw, bindings, map[string]string{"/images/0/url": "https://gw.example/v1/model-tasks/t/artifacts/0"})
	if err != nil || !strings.Contains(string(plain), "http://fal.run/x") || !strings.Contains(string(plain), `"file_name":"tmp1.png"`) {
		t.Fatalf("rewrite %s %v", plain, err)
	}
}

func TestDeliveryRemovesEmbeddedAndKeyURLsAndRootURLs(t *testing.T) {
	for _, raw := range []string{`{"caption":"see HTTPS://fal.media/private?signature=secret", "https://fal.run/key":"value"}`, `" https://fal.media/root"`} {
		out, count, err := DeliverArtifacts([]byte(raw), nil, map[string]OwnedFile{}, BlankUnboundURLs)
		if err != nil || count == 0 || strings.Contains(strings.ToLower(string(out)), "http") || strings.Contains(string(out), "secret") {
			t.Fatalf("unsafe delivery %s %d %v", out, count, err)
		}
	}
}

func TestDeliveredArtifactOriginDoesNotRelaxProviderSources(t *testing.T) {
	bindings := []ArtifactBinding{{Path: []string{"video", "url"}}}
	output := []byte(`{"video":{"url":"https://127.0.0.1:3443/v1/model-tasks/trial_video/artifacts/0"}}`)
	if _, err := PlanArtifacts(output, bindings); err == nil {
		t.Fatal("provider source must still reject nonstandard ports")
	}
	if files, err := PlanDeliveredArtifacts(output, bindings, "https://127.0.0.1:3443"); err != nil || len(files) != 1 {
		t.Fatalf("owned output at configured origin: %v, %v", files, err)
	}
	for _, origin := range []string{"https://127.0.0.1:4443", "https://other.example:3443", "http://127.0.0.1:3443", "https://user:secret@127.0.0.1:3443", "https://127.0.0.1:3443/path", "https://127.0.0.1:3443?x=y"} {
		if _, err := PlanDeliveredArtifacts(output, bindings, origin); err == nil {
			t.Fatalf("accepted wrong or malformed origin %q", origin)
		}
	}
}
