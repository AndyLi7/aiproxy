// migration-runtime controls a fixed local child set only. It is not a Railway
// issuer. The seed is copied into a NEW private namespace; old sessions cannot
// resume until the independent restore-checkpoint protocol is implemented.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/labring/aiproxy/core/runtimeguard"
	"io"
	"os"
	"os/signal"
	"syscall"
)

func run() error {
	path := flag.String("launch", "", "reviewed local launch JSON; no credentials")
	flag.Parse()
	if *path == "" {
		return fmt.Errorf("launch file required")
	}
	f, e := os.Open(*path)
	if e != nil {
		return e
	}
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	var c runtimeguard.Launch
	e = d.Decode(&c)
	if e == nil && d.Decode(new(any)) != io.EOF {
		e = fmt.Errorf("trailing JSON")
	}
	f.Close()
	if e != nil {
		return fmt.Errorf("invalid launch file")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	s, e := runtimeguard.Start(ctx, c)
	if e != nil {
		return fmt.Errorf("controlled start refused")
	}
	defer s.Stop()
	fmt.Println("local controlled namespace active; migrations disabled")
	lines := make(chan string)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
		close(lines)
	}()
	for {
		select {
		case <-ctx.Done():
			return s.Stop()
		case <-s.Done():
			return s.Stop()
		case line, ok := <-lines:
			if !ok || line == "stop" {
				return s.Stop()
			}
			var r struct {
				Command                string
				ValidUntilMs           int64
				RevocationFreshUntilMs int64
			}
			decoder := json.NewDecoder(bytes.NewBufferString(line))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&r) != nil || decoder.Decode(new(any)) != io.EOF || r.Command != "renew" {
				return fmt.Errorf("invalid local command")
			}
			if e = s.Renew(r.ValidUntilMs, r.RevocationFreshUntilMs); e != nil {
				return fmt.Errorf("renewal refused")
			}
			fmt.Println("renewed")
		}
	}
}
func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
