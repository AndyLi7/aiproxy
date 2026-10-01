package task

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"github.com/labring/aiproxy/core/controller"
	log "github.com/sirupsen/logrus"
	"time"
)

// NativeTaskRecoveryTask is separate from legacy image usage projection.
func NativeTaskRecoveryTask(ctx context.Context) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return
	}
	owner := "native-" + hex.EncodeToString(id[:])
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := controller.RecoverNativeTasks(ctx, owner); err != nil {
				log.Warn("native task recovery deferred")
			}
		}
	}
}
