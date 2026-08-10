package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/eduardotorresdev/dokkup/internal/dokku"
	"github.com/eduardotorresdev/dokkup/internal/server"
)

// flagHelp is returned when a command was asked for its own usage, so that Run
// can exit zero rather than treating it as a failure.
var flagHelp = errors.New("help requested")

func newFlagSet(env Env, name string) *flag.FlagSet {
	fs := flag.NewFlagSet("dokkup "+name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flagHelp
		}
		return err
	}
	return nil
}

func runServe(env Env, args []string) error {
	fs := newFlagSet(env, "serve")
	listen := fs.String("listen", "127.0.0.1:8080", "address to listen on")
	mode := fs.String("mode", string(server.ModeIP), "reachability mode: published or ip")
	dokkuBinary := fs.String("dokku-binary", dokku.DefaultBinary, "path to the dokku executable")

	if err := parseFlags(fs, args); err != nil {
		return err
	}

	logger := slog.Default()

	srv := server.New(server.Config{
		Dokku:  &dokku.ExecClient{Binary: *dokkuBinary},
		Mode:   server.Mode(*mode),
		Logger: logger,
	})

	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		logger.Info("dokkup listening", "address", *listen, "mode", *mode)
		fmt.Fprintf(env.Stdout, "dokkup listening on http://%s\n", *listen)
		errc <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}
