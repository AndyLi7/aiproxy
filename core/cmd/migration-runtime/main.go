// migration-runtime controls a fixed local child set only. It is not a Railway
// issuer. The seed is copied into a NEW private namespace; old sessions cannot
// resume until the independent restore-checkpoint protocol is implemented.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/labring/aiproxy/core/runtimeguard"
)

func run() (runErr error) {
	path := flag.String("launch", "", "reviewed local launch JSON; no credentials")

	flag.Parse()

	if *path == "" {
		return errors.New("launch file required")
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
		e = errors.New("trailing JSON")
	}

	f.Close()

	if e != nil {
		return errors.New("invalid launch file")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	s, e := runtimeguard.Start(ctx, c)
	if e != nil {
		return errors.New("controlled start refused")
	}
	defer func() { runErr = errors.Join(runErr, s.Stop()) }()

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
				Command                string `json:"command"`
				ValidUntilMs           int64  `json:"validUntilMs"`
				RevocationFreshUntilMs int64  `json:"revocationFreshUntilMs"`
			}

			decoder := json.NewDecoder(bytes.NewBufferString(line))
			decoder.DisallowUnknownFields()

			if decoder.Decode(&r) != nil || decoder.Decode(new(any)) != io.EOF ||
				r.Command != "renew" {
				return errors.New("invalid local command")
			}

			if e = s.Renew(r.ValidUntilMs, r.RevocationFreshUntilMs); e != nil {
				return errors.New("renewal refused")
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
