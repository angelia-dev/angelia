// Command ciharness holds the two test doubles and the assertion of the CI
// quickstart check (deploy/compose/README.md, "Quickstart check (CI)"). Standard library only, plus
// the repository's own fake registry and webhook verifier.
//
//	ciharness registry -listen :8099        fake PyPI whose clock POST /_ci/publish moves forward
//	ciharness receiver -listen :9000        webhook receiver; secret in $CI_WEBHOOK_SECRET
//	ciharness await -receiver URL -version V -timeout 9m
//
// It is a test tool: it has no authentication and must never face a network
// you do not control.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const usage = "usage: ciharness registry|receiver|await [flags]"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Environ(), os.Stdout); err != nil {
		slog.Error("ciharness", "err", err)
		stop()
		os.Exit(1) //nolint:gocritic // stop is called by hand before the exit
	}
}

func run(ctx context.Context, args, environ []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	switch args[0] {
	case "registry":
		listen := fs.String("listen", ":8099", "listen address")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		h, err := newRegistry()
		if err != nil {
			return err
		}
		return serve(ctx, *listen, h)
	case "receiver":
		listen := fs.String("listen", ":9000", "listen address")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		secrets := splitSecrets(lookup(environ, "CI_WEBHOOK_SECRET"))
		if len(secrets) == 0 {
			return errors.New("receiver: CI_WEBHOOK_SECRET is not set")
		}
		return serve(ctx, *listen, newReceiver(secrets, time.Now))
	case "await":
		url := fs.String("receiver", "http://127.0.0.1:9000", "receiver base URL")
		version := fs.String("version", "", "version the release event must carry")
		timeout := fs.Duration("timeout", 9*time.Minute, "give up after")
		poll := fs.Duration("poll", 2*time.Second, "time between polls")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *version == "" {
			return errors.New("await: -version is required")
		}
		ctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		got, err := await(ctx, http.DefaultClient, *url, *version, *poll)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "received %s for version %s (delivery %s)\n", got.Type, *version, got.WebhookID)
		return nil
	default:
		return errors.New(usage)
	}
}

// lookup returns the value of key in an os.Environ-style list.
func lookup(environ []string, key string) string {
	for _, kv := range environ {
		if len(kv) > len(key) && kv[:len(key)] == key && kv[len(key)] == '=' {
			return kv[len(key)+1:]
		}
	}
	return ""
}

func serve(ctx context.Context, addr string, h http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	slog.Info("listening", "addr", addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
