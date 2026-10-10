package task

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"github.com/labring/aiproxy/core/controller"
	log "github.com/sirupsen/logrus"
	"time"
)

// nativeRecoveryWorkers bound concurrent provider polls and archives per
// instance; each archive buffers at most one artifact.
const nativeRecoveryWorkers = 4

// NativeTaskRecoveryTask is separate from legacy image usage projection. It is
// the only place native tasks advance: customer reads never poll or archive.
func NativeTaskRecoveryTask(ctx context.Context) {
	for i := 0; i < nativeRecoveryWorkers; i++ {
		go nativeRecoveryWorker(ctx)
	}
	<-ctx.Done()
}

func nativeRecoveryWorker(ctx context.Context) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return
	}
	owner := "native-" + hex.EncodeToString(id[:])
	ticker := time.NewTicker(2 * time.Second)
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
