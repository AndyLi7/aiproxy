package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMainRecoveryJoinWaitsForActualReturn(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var wg sync.WaitGroup

	entered := make(chan struct{}, 2)

	release := make(chan struct{})
	defer close(release)

	work := func(ctx context.Context) { entered <- struct{}{}; <-ctx.Done(); <-release }
	startJoinedRecoveryTasks(ctx, &wg, work, work)

	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("task not started")
		}
	}

	cancel()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	for range 2 {
		select {
		case <-done:
			t.Fatal("shutdown did not join owned task")
		case <-time.After(25 * time.Millisecond):
		}

		release <- struct{}{}
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("join did not finish")
	}

	startJoinedRecoveryTasks(ctx, &wg, func(context.Context) { t.Error("cancelled startup") })
	wg.Wait()
}

func TestMainRegistersRecoveryTasksWithShutdownWaitGroup(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	require.NoError(t, err)

	var found bool
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		name, ok := call.Fun.(*ast.Ident)
		if !ok || name.Name != "startJoinedRecoveryTasks" {
			return true
		}

		require.Len(t, call.Args, 4)
		address, ok := call.Args[1].(*ast.UnaryExpr)
		require.True(t, ok, "recovery tasks must receive the shutdown wait group address")
		waitGroup, ok := address.X.(*ast.Ident)
		require.True(t, ok)
		require.Equal(t, "wg", waitGroup.Name)

		for index, expected := range []string{"AsyncUsagePollTask", "NativeTaskRecoveryTask"} {
			task, ok := call.Args[index+2].(*ast.SelectorExpr)
			require.True(t, ok, "recovery callback must be a task function")
			require.Equal(t, expected, task.Sel.Name)
		}

		found = true

		return true
	})
	require.True(t, found, "actual main must register both loops before shutdown Wait")
}
