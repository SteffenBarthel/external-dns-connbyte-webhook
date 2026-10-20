// Command external-dns-connbyte-webhook is an external-dns webhook provider
// for ConnByte DNS (https://connbyte.com/).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/SteffenBarthel/external-dns-connbyte-webhook/internal/connbyte"
	"github.com/SteffenBarthel/external-dns-connbyte-webhook/internal/provider"
	"github.com/SteffenBarthel/external-dns-connbyte-webhook/internal/webhook"
)

var version = "dev"

type config struct {
	apiURL         string
	token          string
	domainFilter   string
	excludeDomains string
	defaultTTL     int
	dryRun         bool
	listenAddr     string
	healthAddr     string
	logLevel       string
	logFormat      string
	requestTimeout time.Duration
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func parseConfig(args []string) (config, error) {
	var c config
	fs := flag.NewFlagSet("external-dns-connbyte-webhook", flag.ContinueOnError)
	fs.StringVar(&c.apiURL, "connbyte-api-url", env("CONNBYTE_API_URL", connbyte.DefaultBaseURL), "ConnByte API base URL (env CONNBYTE_API_URL)")
	tokenFile := fs.String("connbyte-api-token-file", env("CONNBYTE_API_TOKEN_FILE", ""), "file with the ConnByte API token (env CONNBYTE_API_TOKEN_FILE); the token itself is read from env CONNBYTE_API_TOKEN")
	fs.StringVar(&c.domainFilter, "domain-filter", env("DOMAIN_FILTER", ""), "comma separated list of domains to manage (env DOMAIN_FILTER); empty means all zones of the account")
	fs.StringVar(&c.excludeDomains, "exclude-domains", env("EXCLUDE_DOMAINS", ""), "comma separated list of domains to never touch (env EXCLUDE_DOMAINS)")
	fs.IntVar(&c.defaultTTL, "default-ttl", envInt("DEFAULT_TTL", 300), "TTL in seconds for records without a TTL, 60-86400 (env DEFAULT_TTL)")
	fs.BoolVar(&c.dryRun, "dry-run", envBool("DRY_RUN", false), "log changes but do not apply them (env DRY_RUN)")
	fs.StringVar(&c.listenAddr, "listen-address", env("WEBHOOK_LISTEN_ADDRESS", "127.0.0.1:8888"), "address of the provider API used by external-dns (env WEBHOOK_LISTEN_ADDRESS)")
	fs.StringVar(&c.healthAddr, "health-address", env("HEALTH_LISTEN_ADDRESS", "0.0.0.0:8080"), "address of /healthz (env HEALTH_LISTEN_ADDRESS)")
	fs.StringVar(&c.logLevel, "log-level", env("LOG_LEVEL", "info"), "debug, info, warn or error (env LOG_LEVEL)")
	fs.StringVar(&c.logFormat, "log-format", env("LOG_FORMAT", "text"), "text or json (env LOG_FORMAT)")
	fs.DurationVar(&c.requestTimeout, "request-timeout", 25*time.Second, "timeout for one external-dns request, keep it below the external-dns webhook timeouts")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	c.token = strings.TrimSpace(os.Getenv("CONNBYTE_API_TOKEN"))
	if c.token == "" && *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			return c, fmt.Errorf("read token file: %w", err)
		}
		c.token = strings.TrimSpace(string(b))
	}
	if c.token == "" {
		return c, errors.New("CONNBYTE_API_TOKEN (or --connbyte-api-token-file) is required")
	}
	if c.defaultTTL < provider.MinTTL || c.defaultTTL > provider.MaxTTL {
		return c, fmt.Errorf("--default-ttl must be between %d and %d", provider.MinTTL, provider.MaxTTL)
	}
	return c, nil
}

func newLogger(level, format string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: l}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func main() {
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	log := newLogger(cfg.logLevel, cfg.logFormat)

	client, err := connbyte.New(cfg.apiURL, cfg.token,
		connbyte.WithHTTPClient(&http.Client{Timeout: 20 * time.Second}),
		connbyte.WithUserAgent("external-dns-connbyte-webhook/"+version))
	if err != nil {
		log.Error("create client", "error", err)
		os.Exit(1)
	}
	p := provider.New(client, provider.Config{
		DomainFilter:   strings.Split(cfg.domainFilter, ","),
		ExcludeDomains: strings.Split(cfg.excludeDomains, ","),
		DefaultTTL:     cfg.defaultTTL,
		DryRun:         cfg.dryRun,
	}, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	servers := []*http.Server{
		newServer(cfg.listenAddr, webhook.Handler(p, log, cfg.requestTimeout)),
		newServer(cfg.healthAddr, webhook.HealthHandler()),
	}
	errCh := make(chan error, len(servers))
	for _, s := range servers {
		ln, err := net.Listen("tcp", s.Addr)
		if err != nil {
			log.Error("listen", "address", s.Addr, "error", err)
			os.Exit(1)
		}
		log.Info("listening", "address", s.Addr)
		go func(s *http.Server, ln net.Listener) {
			if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}(s, ln)
	}
	log.Info("started", "version", version, "domainFilter", cfg.domainFilter, "dryRun", cfg.dryRun)

	select {
	case <-ctx.Done():
	case err := <-errCh:
		log.Error("server failed", "error", err)
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdown)
	}
}

func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}
