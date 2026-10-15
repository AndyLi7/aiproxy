package nativetask

import (
	"github.com/labring/aiproxy/core/model"
	log "github.com/sirupsen/logrus"
)

// PublicStatus is the only mapping from a stored task status to the four
// statuses customers see (queued, running, completed, failed). Both the
// customer API and the administrative task read use it. A status this version
// does not know reads as running and is logged, never passed through: the
// application parses these four strictly.
func PublicStatus(status string) string {
	switch status {
	case "reserved", model.NativeStatusWaiting, model.NativeStatusDispatching, "submitting", "submission_unknown", "queued":
		return "queued"
	case "running", "result_received", "delivery_ready":
		return "running"
	case "completed", "failed":
		return status
	}

	log.WithFields(log.Fields{"lane": "native", "status": status}).Warn("native task has an unknown status; shown as running")

	return "running"
}
