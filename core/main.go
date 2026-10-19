package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/labring/aiproxy/core/common"
	"github.com/labring/aiproxy/core/common/config"
	"github.com/labring/aiproxy/core/common/consume"
	"github.com/labring/aiproxy/core/common/env"
	"github.com/labring/aiproxy/core/controller"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/runtimeguard"
	"github.com/labring/aiproxy/core/task"
	requesttraceruntime "github.com/labring/aiproxy/core/trace"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

var (
	listen            string
	pprofPort         int
	controlledReadFD  int
	controlledWriteFD int
)

func init() {
	flag.IntVar(
		&controlledReadFD,
		"controlled-runtime-read-fd",
		0,
		"private supervisor pipe; local controlled mode only",
	)
	flag.IntVar(
		&controlledWriteFD,
		"controlled-runtime-write-fd",
		0,
		"private supervisor pipe; local controlled mode only",
	)
	flag.StringVar(&listen, "listen", "0.0.0.0:3000", "http server listen")
	flag.IntVar(&pprofPort, "pprof-port", 15000, "pport http server port")
}

func startRequestTraceRuntime(
	ctx context.Context,
	options requesttraceruntime.Options,
) *requesttraceruntime.Runtime {
	return requesttraceruntime.Start(ctx, model.LogDB, options)
}

// Swagger godoc
//
//	@title						AI Proxy Swagger API
//	@version					1.0
//	@securityDefinitions.apikey	ApiKeyAuth
//	@in							header
//	@name						Authorization
func main() {
	if err := runMain(); err != nil {
		log.Fatal(err)
	}
}

// runMain owns cleanup; process exit occurs only after its deferred cleanup ran.
func runMain() (runErr error) {
	flag.Parse()

	loadEnv()

	config.ReloadEnv()

	if err := ensureAdminKey(); err != nil {
		log.Warn("failed to ensure AdminKey: " + err.Error())
	}

	common.InitLog(log.StandardLogger(), config.DebugEnabled)

	printLoadedEnvFiles()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var initErr error
	if controlledReadFD != 0 || controlledWriteFD != 0 {
		if controlledReadFD < 3 || controlledWriteFD < 3 || controlledReadFD == controlledWriteFD {
			return errors.New("controlled runtime private session required")
		}

		in, out := os.NewFile(
			uintptr(controlledReadFD),
			"controlled-in",
		), os.NewFile(
			uintptr(controlledWriteFD),
			"controlled-out",
		)
		defer in.Close()
		defer out.Close()

		h, r, boot, e := runtimeguard.ChildHandshake(in, out)
		if e != nil {
			return errors.New("controlled runtime handshake failed")
		}

		var controlledDB *gorm.DB

		controlledDB, initErr = runtimeguard.OpenControlledStorage(h)
		if initErr == nil {
			model.DB = controlledDB
			model.LogDB = controlledDB
		}

		if initErr == nil {
			ready := make(chan error, 1)
			go func() { _ = runtimeguard.RunChild(ctx, h, r, in, out, boot, ready); stop() }()

			initErr = <-ready
		}

		if initErr == nil {
			initErr = initializeOptionAndCaches()
		}
	} else {
		initErr = initializeServices(pprofPort)
	}

	if initErr != nil {
		return fmt.Errorf("failed to initialize services: %w", initErr)
	}

	defer func() {
		if err := model.CloseDB(); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("failed to close database: %w", err))
		}
	}()

	traceKeys, traceKeyErr := requesttraceruntime.ParseTrustedKeys(
		env.String("REQUEST_TRACE_SERVICE_KEYS", ""),
	)
	if traceKeyErr != nil {
		log.Warn("request_trace_service_keys_unavailable")
	}

	traceRuntime := startRequestTraceRuntime(ctx, requesttraceruntime.Options{
		Enabled:     env.Bool("REQUEST_TRACE_ENABLED", false),
		TrustedKeys: traceKeys,
	})

	restoreTraceRuntime := requesttraceruntime.Install(traceRuntime)
	defer restoreTraceRuntime()

	var wg sync.WaitGroup
	startSyncServices(ctx, &wg)

	srv, _ := setupHTTPServer(listen)

	log.Info("auto test banned models task started")

	go task.AutoTestBannedModelsTask(ctx)

	log.Info("clean log task started")

	go task.CleanLogTask(ctx)

	log.Info("detect ip groups task started")

	go task.DetectIPGroupsTask(ctx)

	log.Info("usage alert task started")

	go task.UsageAlertTask(ctx)

	log.Info("async usage poll task started")

	startJoinedRecoveryTasks(ctx, &wg, task.AsyncUsagePollTask, task.NativeTaskRecoveryTask)

	log.Info("image result cleanup task started")

	go task.ImageResultCleanupTask(ctx)

	if common.RedisEnabled {
		log.Info("redis health check task started")

		go task.RedisHealthCheckTask(ctx)
	}

	log.Info("update channels balance task started")

	go controller.UpdateChannelsBalance(time.Minute * 10)

	batchProcessorCtx, batchProcessorCancel := context.WithCancel(context.Background())

	wg.Add(1)

	go model.StartBatchProcessorSummary(batchProcessorCtx, &wg)

	log.Infof("server started on http://%s", srv.Addr)
	log.Infof("swagger started on http://%s/swagger/index.html", srv.Addr)

	go listenAndServe(srv)

	<-ctx.Done()

	shutdownSrvCtx, shutdownSrvCancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer shutdownSrvCancel()

	log.Info("shutting down http server...")
	log.Info("max wait time: 600s")

	if err := srv.Shutdown(shutdownSrvCtx); err != nil {
		log.Error("server forced to shutdown: " + err.Error())
	} else {
		log.Info("server shutdown successfully")
	}

	log.Info("shutting down consumer...")
	consume.Wait()

	batchProcessorCancel()

	log.Info("shutting down sync services...")
	wg.Wait()

	log.Info("shutting down batch summary...")
	log.Info("max wait time: 600s")

	cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cleanCancel()

	model.CleanBatchUpdatesSummary(cleanCtx)

	traceCloseCtx, traceCloseCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer traceCloseCancel()

	if err := traceRuntime.Close(traceCloseCtx); err != nil {
		log.Error("request_trace_shutdown_failed")
	}

	log.Info("server exiting")

	return nil
}
