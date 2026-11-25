package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/axin2024/media-stream/internal/catalog"
	"github.com/axin2024/media-stream/internal/media"
	"github.com/axin2024/media-stream/internal/pipeline"
	"github.com/axin2024/media-stream/internal/web"
)

var version = "dev"
var revision = "unknown"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(ctx, log, os.Args[1:], os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("media stream", "error", err)
		os.Exit(1)
	}
}

const usage = `media-stream <command> [flags]

  migrate       Apply embedded MySQL migrations
  keygen        Generate a MEDIA_STREAM_SIGNING_KEY
  grant         Provision an owner, director, editor, viewer, or operator token
  node          Register a worker bound and set its enabled status
  serve         Start the management API, HLS origin, and workbench
  scheduler     Run source selection and publication maintenance
  worker        Run a distributed FFmpeg worker
  sweep         Reconcile aged orphan media and run maintenance once
  push          Partition an input with FFmpeg and send paced source fragments
  sample        Generate a synthetic timecoded audio/video sample
  version       Print the executable version and Git revision

Database and signing key use MEDIA_STREAM_MYSQL_DSN and MEDIA_STREAM_SIGNING_KEY.
See docs/operations.md for examples.
`

func run(ctx context.Context, log *slog.Logger, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		_, err := io.WriteString(out, usage)
		return err
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(out)
	dsn := os.Getenv("MEDIA_STREAM_MYSQL_DSN")
	switch args[0] {
	case "keygen":
		if err := parse(flags, args[1:]); err != nil {
			return err
		}
		code := make([]byte, 32)
		rand.Read(code)
		_, err := fmt.Fprintln(out, hex.EncodeToString(code))
		return err
	case "version":
		if err := parse(flags, args[1:]); err != nil {
			return err
		}
		_, err := fmt.Fprintf(out, "media-stream %s (%s)\n", version, revision)
		return err
	case "push":
		return pushCommand(ctx, log, flags, args[1:])
	case "sample":
		engine := flags.String("ffmpeg", "ffmpeg", "FFmpeg executable")
		output := flags.String("out", "studio-sample.mp4", "new MP4 path")
		seconds := flags.Int("seconds", 120, "sample duration")
		width := flags.Int("width", 1_280, "frame width")
		height := flags.Int("height", 720, "frame height")
		if err := parse(flags, args[1:]); err != nil {
			return err
		}
		return (media.FFmpeg{Path: *engine, Threads: 2}).Sample(ctx, *output, *seconds, *width, *height)
	case "migrate":
		steps := flags.Int("steps", 0, "apply only the next N forward migrations")
		if err := parse(flags, args[1:]); err != nil {
			return err
		}
		if dsn == "" || *steps < 0 {
			return errors.New("set MEDIA_STREAM_MYSQL_DSN and use nonnegative steps")
		}
		return catalog.Migrate(dsn, *steps)
	case "grant", "node":
		return administer(ctx, flags, args, dsn, out)
	case "serve", "scheduler", "worker", "sweep":
		return runService(ctx, log, flags, args, dsn)
	default:
		return fmt.Errorf("unrecognized command %q; see media-stream help", args[0])
	}
}

func parse(flags *flag.FlagSet, args []string) error {
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional argument")
	}
	return nil
}

func database(ctx context.Context, dsn string, signing []byte) (*catalog.Store, error) {
	if dsn == "" {
		return nil, errors.New("set MEDIA_STREAM_MYSQL_DSN")
	}
	return catalog.Open(ctx, dsn, signing)
}

func administer(ctx context.Context, flags *flag.FlagSet, args []string, dsn string, out io.Writer) error {
	if args[0] == "node" {
		name := flags.String("name", "", "registered worker name")
		slots := flags.Int("slots", 1, "maximum simultaneous FFmpeg jobs")
		disabled := flags.Bool("disabled", false, "stop assigning new jobs")
		if err := parse(flags, args[1:]); err != nil {
			return err
		}
		database, err := database(ctx, dsn, nil)
		if err != nil {
			return err
		}
		defer database.Close()
		return database.RegisterNode(ctx, *name, *slots, !*disabled)
	}
	tenant := flags.String("tenant", "", "tenant ID")
	name := flags.String("name", "", "operator label")
	role := flags.String("role", "", "owner, director, editor, viewer, or operator")
	channel := flags.String("channel", "", "optional channel scope")
	ttl := flags.Duration("ttl", 24*time.Hour, "credential validity")
	if err := parse(flags, args[1:]); err != nil {
		return err
	}
	if *ttl < time.Minute || *ttl > 30*24*time.Hour {
		return errors.New("credential TTL must be 1 minute to 30 days")
	}
	db, err := database(ctx, dsn, nil)
	if err != nil {
		return err
	}
	defer db.Close()
	value, err := db.Provision(ctx, *tenant, *name, *role, *channel, time.Now().Add(*ttl))
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(value)
}

func runService(ctx context.Context, log *slog.Logger, flags *flag.FlagSet, args []string, dsn string) error {
	command := args[0]
	root := flags.String("media", ".local/media", "shared durable media directory")
	var listen, cert, privateKey, binary, node, unavailable string
	var schedule, proxied bool
	var threads, concurrency, inputSlots int
	grace := 24 * time.Hour
	if command == "serve" || command == "worker" {
		flags.StringVar(&binary, "ffmpeg", "ffmpeg", "FFmpeg executable with libx264 and AAC")
		flags.IntVar(&threads, "threads", 2, "maximum FFmpeg threads per codec")
	}
	if command == "serve" {
		flags.StringVar(&listen, "listen", "127.0.0.1:8080", "management and HLS address")
		flags.StringVar(&cert, "tls-cert", "", "PEM certificate path")
		flags.StringVar(&privateKey, "tls-key", "", "PEM private key path")
		flags.BoolVar(&proxied, "secure-proxy", false, "use secure cookies with a trusted TLS proxy on loopback")
		flags.BoolVar(&schedule, "schedule", true, "run scheduler and cleanup with the origin")
		flags.IntVar(&inputSlots, "input-slots", 4, "maximum concurrent source verifiers")
	}
	if command == "worker" {
		flags.StringVar(&node, "node", "", "registered worker name")
		flags.StringVar(&unavailable, "exclude-tracks", "", "tracks unavailable on this worker, separated by commas")
		flags.IntVar(&concurrency, "concurrency", 1, "local limit on concurrent jobs")
	}
	if command != "worker" {
		flags.DurationVar(&grace, "retention-grace", grace, "retired media cleanup grace")
	}
	if err := parse(flags, args[1:]); err != nil {
		return err
	}
	if grace < time.Minute {
		return errors.New("retention grace must be at least one minute")
	}
	var signing []byte
	if command == "serve" {
		var err error
		signing, err = hex.DecodeString(os.Getenv("MEDIA_STREAM_SIGNING_KEY"))
		if err != nil || len(signing) != 32 {
			return errors.New("set MEDIA_STREAM_SIGNING_KEY to a 32-byte hex key from media-stream keygen")
		}
	}
	db, err := database(ctx, dsn, signing)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.Ready(ctx); err != nil {
		return fmt.Errorf("apply migrations before starting: %w", err)
	}
	files, err := media.OpenFiles(*root)
	if err != nil {
		return err
	}
	defer files.Close()
	engine := media.FFmpeg{Path: binary, Threads: threads}
	scheduler := pipeline.Scheduler{Catalog: db, Files: files, Log: log, Grace: grace}
	switch command {
	case "serve":
		if err := engine.Check(ctx); err != nil {
			return err
		}
		if inputSlots < 1 || inputSlots > 64 {
			return errors.New("input verifier slots must be 1 to 64")
		}
		server := &web.Server{DB: db, Files: files, FFmpeg: engine, Log: log, SecureCookie: proxied,
			IngestSlots: make(chan struct{}, inputSlots)}
		groupCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		var group sync.WaitGroup
		if schedule {
			group.Go(func() { _ = scheduler.Run(groupCtx) })
		}
		err := runHTTP(groupCtx, log, server.Handler(), listen, cert, privateKey)
		cancel()
		group.Wait()
		return err
	case "scheduler":
		return scheduler.Run(ctx)
	case "worker":
		excluded := make(map[string]bool)
		if unavailable != "" {
			for track := range strings.SplitSeq(unavailable, ",") {
				if track != "240p" && track != "360p" && track != "720p" {
					return fmt.Errorf("unrecognized track %q", track)
				}
				excluded[track] = true
			}
		}
		worker := &pipeline.Worker{Catalog: db, Files: files, FFmpeg: engine, Log: log, Node: node,
			Concurrency: concurrency, ExcludedTracks: excluded}
		return worker.Run(ctx)
	case "sweep":
		if err := scheduler.Collect(ctx); err != nil {
			return err
		}
		return scheduler.SweepOrphans(ctx)
	}
	return catalog.ErrInvalid
}

func runHTTP(ctx context.Context, log *slog.Logger, handler http.Handler, listen, cert, privateKey string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return err
	}
	if (cert == "") != (privateKey == "") || cert == "" && !(host == "localhost" || net.ParseIP(host).IsLoopback()) {
		return errors.New("configure both TLS files or serve plain HTTP on loopback")
	}
	server := &http.Server{Addr: listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 45 * time.Second,
		WriteTimeout: 3 * time.Minute, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 32 << 10}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 7*time.Second)
			defer cancel()
			if err := server.Shutdown(cleanup); err != nil {
				_ = server.Close()
			}
		case <-done:
		}
	}()
	log.Info("origin starting", "listen", listen, "tls", cert != "")
	if cert != "" {
		err = server.ListenAndServeTLS(cert, privateKey)
	} else {
		err = server.ListenAndServe()
	}
	close(done)
	<-stopped
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
