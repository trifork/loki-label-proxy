// Command loki-label-proxy enforces a label matcher on every LogQL query it
// forwards to Loki.
//
// It is the Loki counterpart to prom-label-proxy, and takes the same view of
// its job: it performs no authentication and no authorization. Whatever sits in
// front of it decides who the caller is and sets the label value header
// accordingly; this process only guarantees that the value is honoured.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/trifork/loki-label-proxy/internal/enforce"
	"github.com/trifork/loki-label-proxy/internal/proxy"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		upstream       = flag.String("upstream", "", "Loki read endpoint to forward to (required)")
		label          = flag.String("label", "", "label name to enforce on every query (required)")
		headerName     = flag.String("header-name", "", "request header carrying the enforced label value (required)")
		errorOnReplace = flag.Bool("error-on-replace", true, "reject queries that already constrain the enforced label, instead of silently replacing the matcher")
		listenAddr     = flag.String("insecure-listen-address", ":8080", "address to serve the proxy on")
		internalAddr   = flag.String("internal-listen-address", ":8081", "address to serve /metrics and /healthz on, kept separate from caller-facing traffic")
		logLevel       = flag.String("log-level", "info", "one of debug, info, warn, error")
	)
	flag.Parse()

	level, err := parseLevel(*logLevel)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	for name, value := range map[string]string{
		"upstream":    *upstream,
		"label":       *label,
		"header-name": *headerName,
	} {
		if value == "" {
			return fmt.Errorf("-%s is required", name)
		}
	}

	target, err := url.Parse(*upstream)
	if err != nil {
		return fmt.Errorf("parsing -upstream: %w", err)
	}
	if target.Scheme == "" || target.Host == "" {
		return fmt.Errorf("-upstream must be an absolute URL, got %q", *upstream)
	}

	enforcer, err := enforce.New(*label, *errorOnReplace)
	if err != nil {
		return err
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	handler, err := proxy.New(proxy.Config{
		Upstream:   target,
		Enforcer:   enforcer,
		HeaderName: *headerName,
		Logger:     logger,
		Registerer: registry,
	})
	if err != nil {
		return err
	}

	// Kept on the internal listener: metrics and health are for the platform,
	// not for the callers being constrained.
	internal := http.NewServeMux()
	internal.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	internal.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	errs := make(chan error, 2)
	go serve(errs, logger, "internal", *internalAddr, internal)
	go serve(errs, logger, "proxy", *listenAddr, handler)

	logger.Info("started",
		"upstream", target.Redacted(),
		"label", *label,
		"header", *headerName,
		"error_on_replace", *errorOnReplace)

	return <-errs
}

func serve(errs chan<- error, logger *slog.Logger, name, addr string, handler http.Handler) {
	logger.Info("listening", "server", name, "address", addr)
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		errs <- fmt.Errorf("%s server on %s: %w", name, addr, err)
	}
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown -log-level %q", s)
	}
}
