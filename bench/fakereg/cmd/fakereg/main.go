// Command fakereg runs the fake registry on its own, for manual runs:
//
//	go run ./bench/fakereg/cmd/fakereg -config bench/fakereg/fakereg.yaml
//
// A benchmark can embed the server instead (fakereg.New) and not need
// this binary.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/angelia-dev/angelia/bench/fakereg"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fakereg", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "", "behaviour YAML (bench/fakereg/README.md); empty: defaults")
	listen := flag.String("listen", "127.0.0.1:8099", "listen address")
	flag.Parse()
	cfg := fakereg.DefaultConfig()
	if *cfgPath != "" {
		var err error
		if cfg, err = fakereg.LoadConfig(*cfgPath); err != nil {
			return err
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: *listen, Handler: fakereg.New(cfg), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	for _, e := range fakereg.Ecosystems() {
		fmt.Printf("%-7s http://%s/%s\n", e, *listen, e)
	}
	fmt.Printf("stats   http://%s%s\n", *listen, fakereg.StatsPath)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
