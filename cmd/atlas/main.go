package main

import (
	"atlas-render/internal/engine"
	"atlas-render/internal/handoff"
	"atlas-render/internal/httpapi"
	"atlas-render/internal/persist"
	"atlas-render/internal/service"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if e := run(); e != nil {
		log.Print(e)
		os.Exit(1)
	}
}
func run() error {
	addr := os.Getenv("ATLAS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8763"
	}
	path := os.Getenv("ATLAS_STATE")
	if path == "" {
		path = "var/atlas-state.json"
	}
	opts := persist.Options{Writer: os.Getenv("ATLAS_WRITER")}
	if manifest := os.Getenv("ATLAS_MANIFEST"); manifest != "" {
		// A destination host may only claim state that verifies against the
		// handoff manifest produced by the stopped source.
		adopt, e := handoff.Adopter(manifest)
		if e != nil {
			return fmt.Errorf("handoff manifest: %w", e)
		}
		opts.Adopt = adopt
		opts.ManifestPath = manifest
	}
	st, e := persist.OpenWith(path, opts)
	if e != nil {
		return e
	}
	defer st.Close()
	worker := engine.New(st)
	if e = worker.Recover(); e != nil {
		return e
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	workerCtx, workerStop := context.WithCancel(ctx)
	defer workerStop()
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(workerCtx) }()
	go func() {
		if e := worker.Watch(workerCtx, 250*time.Millisecond); e != nil {
			log.Printf("ownership lost, stopping render work: %v", e)
			workerStop()
		}
	}()
	srv := &http.Server{Addr: addr, Handler: (httpapi.API{Service: &service.Service{Store: st}, Worker: worker}).Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	errc := make(chan error, 1)
	go func() {
		log.Printf("Atlas Render listening on %s (generation %d, writer %s)", addr, st.Generation(), st.Writer())
		errc <- srv.ListenAndServe()
	}()
	select {
	case e = <-errc:
		stop()
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	<-done
	// Record the clean drain only while this process still owns the state.
	if e = st.MarkStopped(); e != nil {
		log.Printf("clean stop marker not recorded: %v", e)
	}
	if e == http.ErrServerClosed {
		return nil
	}
	return e
}
