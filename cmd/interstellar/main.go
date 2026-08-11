// Command interstellar is the whole product: scheduler, pipeline, API and UI in
// one binary, backed by one SQLite file in a mounted volume.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// Embeds the IANA timezone database in the binary so TZ works on a scratch /
	// distroless base with no tzdata package. §7 says the schedule honors TZ.
	_ "time/tzdata"

	"github.com/tammai/interstellar/internal/config"
	"github.com/tammai/interstellar/internal/httpapi"
	"github.com/tammai/interstellar/internal/pipeline"
	"github.com/tammai/interstellar/internal/store"
	"github.com/tammai/interstellar/web"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	env, err := config.FromEnv()
	if err != nil {
		return err
	}

	db, err := store.Open(env.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := db.Bootstrap(); err != nil {
		return err
	}

	version, err := db.Version()
	if err != nil {
		return err
	}
	log.Info("database ready", "path", env.DBPath, "schema", version)

	runner := pipeline.NewRunner(db, env, log)

	scheduler := pipeline.NewScheduler(db, env, runner, log)
	if err := scheduler.Start(); err != nil {
		// A bad schedule must not stop the app: the sky and an on-demand
		// refresh still work, and Settings is where the owner fixes it.
		log.Error("could not start scheduler", "err", err)
	}
	defer scheduler.Stop()

	srv := &http.Server{
		Addr:              ":" + env.Port,
		Handler:           httpapi.New(db, env, web.FS(), runner, scheduler, log).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Shut down on SIGTERM so `docker stop` closes the DB cleanly rather than
	// relying on WAL recovery.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr, "tz", env.Location.String())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		// Stop accepting work, then let an in-flight run notice the
		// cancellation. Every stage is idempotent, so an interrupted run costs
		// at most the batch it was in the middle of.
		scheduler.Stop()
		runner.Stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
