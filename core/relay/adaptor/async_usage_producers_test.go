package adaptor_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
)

// asyncUsageProducers pins every function under relay/adaptor that can return
// AsyncUsage, with the relay modes that reach it (from each adaptor's
// DoResponse switch). Reviewed 2026-10-11.
var asyncUsageProducers = map[string][]mode.Mode{
	"ali.AliNativeVideoHandler":                   {mode.AliVideo},
	"ali.AliVideoHandler":                         {mode.VideoGenerationsJobs},
	"ali.aliVideosHandler":                        {mode.Videos, mode.VideosRemix, mode.VideosEdits, mode.VideosExtensions},
	"doubao.DoubaoNativeVideoSubmitHandler":       {mode.DoubaoVideo},
	"doubao.VideoGenerationJobSubmitHandler":      {mode.VideoGenerationsJobs},
	"doubao.VideosSubmitHandler":                  {mode.Videos, mode.VideosEdits, mode.VideosExtensions},
	"gemini.NativeVideoHandler":                   {mode.GeminiVideo},
	"gemini.VideoGenerationJobSubmitHandler":      {mode.VideoGenerationsJobs},
	"gemini.VideosSubmitHandler":                  {mode.Videos, mode.VideosEdits, mode.VideosExtensions},
	"openai.VideoHandler":                         {mode.VideoGenerationsJobs},
	"openai.videosHandler":                        {mode.Videos, mode.VideosRemix, mode.VideosEdits, mode.VideosExtensions},
	"siliconflow.VideoGenerationJobSubmitHandler": {mode.VideoGenerationsJobs},
	"siliconflow.VideosSubmitHandler":             {mode.Videos},
	// Background responses are accepted queued or in progress (async-submit).
	"openai.ResponseHandler": {mode.Responses},
	// A cancel answered while still in progress; reads are never calls.
	"openai.CancelResponseHandler": {mode.ResponsesCancel},
	// Responses-only models behind chat, completions, Anthropic and Gemini
	// requests: only the bill is fetched later (billing-only sync modes).
	"openai.ConvertResponsesToChatCompletionResponse":       {mode.Completions, mode.ChatCompletions},
	"openai.ConvertResponsesToChatCompletionStreamResponse": {mode.Completions, mode.ChatCompletions},
	"openai.ConvertResponsesToClaudeResponse":               {mode.Anthropic},
	"openai.ConvertResponsesToGeminiResponse":               {mode.Gemini},
	"openai.responsesStreamErrorState.result": {
		mode.Responses,
		mode.Completions,
		mode.ChatCompletions,
		mode.Anthropic,
		mode.Gemini,
	},
}

func isFalseLiteral(expr ast.Expr) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == "false"
}

func producerName(dir string, decl ast.Decl) string {
	function, ok := decl.(*ast.FuncDecl)
	if !ok {
		return dir + ".(package scope)"
	}

	if function.Recv == nil || len(function.Recv.List) == 0 {
		return dir + "." + function.Name.Name
	}

	receiver := function.Recv.List[0].Type
	if star, ok := receiver.(*ast.StarExpr); ok {
		receiver = star.X
	}

	if generic, ok := receiver.(*ast.IndexExpr); ok {
		receiver = generic.X
	}

	if ident, ok := receiver.(*ast.Ident); ok {
		return dir + "." + ident.Name + "." + function.Name.Name
	}

	return dir + ".?." + function.Name.Name
}

// findAsyncUsageProducers parses every non-test file under relay/adaptor and
// returns the functions that set AsyncUsage to anything but literal false,
// in a composite literal or an assignment.
func findAsyncUsageProducers(t *testing.T) map[string]int {
	t.Helper()

	producers := map[string]int{}
	files := token.NewFileSet()

	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() && entry.Name() == "testdata" {
			return filepath.SkipDir
		}

		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, err := parser.ParseFile(files, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}

		dir := filepath.ToSlash(filepath.Dir(path))
		for _, decl := range file.Decls {
			name := producerName(dir, decl)

			ast.Inspect(decl, func(node ast.Node) bool {
				switch typed := node.(type) {
				case *ast.KeyValueExpr:
					if key, ok := typed.Key.(*ast.Ident); ok && key.Name == "AsyncUsage" &&
						!isFalseLiteral(typed.Value) {
						producers[name]++
					}
				case *ast.AssignStmt:
					for index, lhs := range typed.Lhs {
						selector, ok := lhs.(*ast.SelectorExpr)
						if !ok || selector.Sel.Name != "AsyncUsage" {
							continue
						}

						if len(typed.Rhs) != len(typed.Lhs) || !isFalseLiteral(typed.Rhs[index]) {
							producers[name]++
						}
					}
				}

				return true
			})
		}

		return nil
	})
	require.NoError(t, err)

	return producers
}

// A complete class table does not prove a mode sits in the right class: a
// video mode filed as sync-call would show pending jobs as succeeded and never
// as unknown. Every function that can return AsyncUsage must be reviewed and
// listed above, and every mode reaching it must expect async usage.
func TestAsyncUsageProducersAreAsyncSubmit(t *testing.T) {
	found := findAsyncUsageProducers(t)

	sites := 0
	for _, count := range found {
		sites += count
	}

	t.Logf("%d AsyncUsage sites in %d functions", sites, len(found))

	foundNames := make([]string, 0, len(found))
	for name := range found {
		foundNames = append(foundNames, name)
	}

	pinnedNames := make([]string, 0, len(asyncUsageProducers))
	for name := range asyncUsageProducers {
		pinnedNames = append(pinnedNames, name)
	}

	slices.Sort(foundNames)
	slices.Sort(pinnedNames)
	require.Equal(t, pinnedNames, foundNames,
		"review new AsyncUsage producers, list the relay modes that reach them, and classify those modes")

	for name, modes := range asyncUsageProducers {
		require.NotEmpty(t, modes, name)

		for _, relayMode := range modes {
			require.True(t, model.AsyncUsageExpected(relayMode),
				"%s returns AsyncUsage on mode %s, which the call classifier treats as a plain sync call",
				name, relayMode)
		}
	}
}
