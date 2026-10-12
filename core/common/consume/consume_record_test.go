package consume_test

import (
	"net/http"
	"testing"

	"github.com/labring/aiproxy/core/common/consume"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/mode"
)

func TestNeedRecordConsumeSkipsSuccessfulStoredVideoReads(t *testing.T) {
	tests := []mode.Mode{
		mode.VideoGenerationsGetJobs,
		mode.VideoGenerationsContent,
		mode.VideosGet,
		mode.VideosContent,
		mode.VideosDelete,
		mode.GeminiFiles,
		mode.GeminiVideoOperations,
		mode.AliVideoTasks,
		mode.DoubaoVideoTasks,
		mode.DoubaoVideoTasksDelete,
	}

	for _, relayMode := range tests {
		t.Run(relayMode.String(), func(t *testing.T) {
			if consume.NeedRecordConsumeForTest(
				http.StatusOK,
				&meta.Meta{Mode: relayMode},
			) {
				t.Fatalf("expected successful %s request not to record consume", relayMode)
			}

			if !consume.NeedRecordConsumeForTest(
				http.StatusInternalServerError,
				&meta.Meta{Mode: relayMode},
			) {
				t.Fatalf("expected failed %s request to record consume", relayMode)
			}
		})
	}
}

func TestNeedRecordConsumeSkipsSuccessfulAlphaSearch(t *testing.T) {
	if consume.NeedRecordConsumeForTest(http.StatusOK, &meta.Meta{Mode: mode.AlphaSearch}) {
		t.Fatal("successful alpha search must not record consumption")
	}

	if !consume.NeedRecordConsumeForTest(
		http.StatusBadGateway,
		&meta.Meta{Mode: mode.AlphaSearch},
	) {
		t.Fatal("failed alpha search must record consumption")
	}
}

func TestNeedRecordConsumeSkipsSuccessfulDoubaoNativeDeleteNoContent(t *testing.T) {
	if consume.NeedRecordConsumeForTest(
		http.StatusNoContent,
		&meta.Meta{Mode: mode.DoubaoVideoTasksDelete},
	) {
		t.Fatal("expected successful doubao native delete 204 request not to record consume")
	}
}

func TestNeedRecordConsumeRecordsVideoCreateAndRemix(t *testing.T) {
	tests := []mode.Mode{
		mode.Videos,
		mode.VideosRemix,
	}

	for _, relayMode := range tests {
		t.Run(relayMode.String(), func(t *testing.T) {
			if !consume.NeedRecordConsumeForTest(http.StatusOK, &meta.Meta{Mode: relayMode}) {
				t.Fatalf("expected successful %s request to record consume", relayMode)
			}
		})
	}
}

// Consume's logging rule and the call classifier read one table: a successful
// read is never logged, and every other mode logs its successes.
func TestConsumeSkipsExactlyReadModes(t *testing.T) {
	pinnedReads := map[mode.Mode]bool{
		mode.VideoGenerationsGetJobs: true,
		mode.VideoGenerationsContent: true,
		mode.ResponsesGet:            true,
		mode.ResponsesDelete:         true,
		mode.ResponsesCancel:         true,
		mode.ResponsesInputItems:     true,
		mode.VideosGet:               true,
		mode.VideosContent:           true,
		mode.VideosDelete:            true,
		mode.GeminiVideoOperations:   true,
		mode.GeminiFiles:             true,
		mode.AliVideoTasks:           true,
		mode.DoubaoVideoTasks:        true,
		mode.DoubaoVideoTasksDelete:  true,
		mode.AlphaSearch:             true,
	}

	reads := 0

	for _, relayMode := range mode.All() {
		records := consume.NeedRecordConsumeForTest(http.StatusOK, &meta.Meta{Mode: relayMode})
		if records != !model.IsReadMode(relayMode) {
			t.Fatalf("mode %s: records success = %v, IsReadMode = %v", relayMode, records, model.IsReadMode(relayMode))
		}

		if model.IsReadMode(relayMode) != pinnedReads[relayMode] {
			t.Fatalf("mode %s: read = %v, pinned list says %v", relayMode, model.IsReadMode(relayMode), pinnedReads[relayMode])
		}

		if model.IsReadMode(relayMode) {
			reads++

			if !consume.NeedRecordConsumeForTest(http.StatusBadGateway, &meta.Meta{Mode: relayMode}) {
				t.Fatalf("mode %s: a failed read must still be logged", relayMode)
			}
		}
	}

	if reads != len(pinnedReads) {
		t.Fatalf("read modes = %d, want %d", reads, len(pinnedReads))
	}

	if consume.NeedRecordConsumeForTest(http.StatusNoContent, &meta.Meta{Mode: mode.DoubaoVideoTasksDelete}) {
		t.Fatal("a successful Doubao task delete (204) must not be logged")
	}
}
