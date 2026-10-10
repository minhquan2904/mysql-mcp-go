// Package config reads the server configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
)

const dsnPrefix = "MYSQL_DSN_"

const (
	defaultQueryTimeout = 30 * time.Second
	minQueryTimeout     = time.Millisecond // max_execution_time=0 means "no limit"
	maxQueryTimeout     = time.Hour        // keeps readTimeout (+10s) far from overflow
	minTokenLen         = 32
	tokenPlaceholder    = "change-me" // prefix of the example value in .env.example
)

var sourceNameRe = regexp.MustCompile(`^[a-z0-9_]+$`)

// Source is one configured MySQL database. DSN is already normalized.
type Source struct{ Name, DSN string }

// Config is the validated server configuration.
type Config struct {
	Sources               []Source // sorted by Name
	MaxRows               int
	MaxCellBytes          int
	MaxResultBytes        int
	QueryTimeout          time.Duration
	MaxConcurrentQueries  int
	AuthToken             string
	AllowedOrigins        []string
	AllowUnsafePrivileges bool
	ListenAddr            string
	LogLevel              slog.Level

	// Warnings are non-fatal findings (e.g. DSN flags forced back to safe
	// values). They never contain secrets; the caller should log them at WARN.
	Warnings []string
}

// Load builds a Config from environ ("KEY=VALUE" entries, as os.Environ).
// Errors never contain DSNs, passwords or the auth token.
func Load(environ []string) (Config, error) {
	env := parseEnviron(environ)

	cfg, err := parseScalars(env)
	if err != nil {
		return Config{}, err
	}
	// DSNs are normalized after the scalars: they embed QueryTimeout.
	sources, warnings, err := discoverSources(env, cfg.QueryTimeout)
	if err != nil {
		return Config{}, err
	}
	cfg.Sources = sources
	cfg.Warnings = warnings
	return cfg, nil
}

// parseScalars reads every non-DSN setting. An empty value counts as unset.
func parseScalars(env map[string]string) (Config, error) {
	var (
		cfg Config
		err error
	)
	for _, p := range []struct {
		key string
		def int
		dst *int
	}{
		{"MAX_ROWS", 1000, &cfg.MaxRows},
		{"MAX_CELL_BYTES", 64 << 10, &cfg.MaxCellBytes},
		{"MAX_RESULT_BYTES", 4 << 20, &cfg.MaxResultBytes},
		{"MAX_CONCURRENT_QUERIES", 8, &cfg.MaxConcurrentQueries},
	} {
		if *p.dst, err = positiveInt(env, p.key, p.def); err != nil {
			return Config{}, err
		}
	}

	cfg.QueryTimeout = defaultQueryTimeout
	if v := env["QUERY_TIMEOUT"]; v != "" {
		cfg.QueryTimeout, err = time.ParseDuration(v)
		if err != nil || cfg.QueryTimeout < minQueryTimeout || cfg.QueryTimeout > maxQueryTimeout {
			return Config{}, fmt.Errorf("QUERY_TIMEOUT must be a duration between %v and %v, such as 30s", minQueryTimeout, maxQueryTimeout)
		}
	}

	if cfg.AuthToken, err = authToken(env["MCP_AUTH_TOKEN"]); err != nil {
		return Config{}, err
	}

	for o := range strings.SplitSeq(env["ALLOWED_ORIGINS"], ",") {
		if o = strings.TrimSpace(o); o != "" {
			cfg.AllowedOrigins = append(cfg.AllowedOrigins, o)
		}
	}

	switch env["ALLOW_UNSAFE_PRIVILEGES"] {
	case "", "false":
	case "true":
		cfg.AllowUnsafePrivileges = true
	default:
		return Config{}, errors.New(`ALLOW_UNSAFE_PRIVILEGES must be "true" or "false"`)
	}

	cfg.ListenAddr = ":8080"
	if v := env["LISTEN_ADDR"]; v != "" {
		cfg.ListenAddr = v
	}

	switch strings.ToLower(env["LOG_LEVEL"]) {
	case "", "info":
		cfg.LogLevel = slog.LevelInfo
	case "debug":
		cfg.LogLevel = slog.LevelDebug
	case "warn":
		cfg.LogLevel = slog.LevelWarn
	case "error":
		cfg.LogLevel = slog.LevelError
	default:
		return Config{}, errors.New("LOG_LEVEL must be one of debug, info, warn, error")
	}
	return cfg, nil
}

func positiveInt(env map[string]string, key string, def int) (int, error) {
	v := env[key]
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return n, nil
}

// authToken validates MCP_AUTH_TOKEN. Errors never include the token.
func authToken(tok string) (string, error) {
	switch {
	case tok == "":
		return "", errors.New("MCP_AUTH_TOKEN is required")
	case utf8.RuneCountInString(tok) < minTokenLen:
		return "", fmt.Errorf("MCP_AUTH_TOKEN must be at least %d characters", minTokenLen)
	case strings.HasPrefix(strings.ToLower(tok), tokenPlaceholder):
		return "", errors.New("MCP_AUTH_TOKEN must not be the example value; generate a random token")
	}
	return tok, nil
}

func parseEnviron(environ []string) map[string]string {
	env := make(map[string]string, len(environ))
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}

// discoverSources collects MYSQL_DSN_<NAME> variables into sources sorted by
// lowercase name, normalizing each DSN. Warnings are prefixed with the source.
func discoverSources(env map[string]string, queryTimeout time.Duration) ([]Source, []string, error) {
	var keys []string
	for k := range env {
		if strings.HasPrefix(k, dsnPrefix) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)

	var sources []Source
	var warnings []string
	seen := make(map[string]bool, len(keys))
	for _, k := range keys {
		name := strings.ToLower(strings.TrimPrefix(k, dsnPrefix))
		if !sourceNameRe.MatchString(name) {
			return nil, nil, fmt.Errorf("%s: source name must match [a-z0-9_]+", k)
		}
		if seen[name] {
			return nil, nil, fmt.Errorf("duplicate source name %q", name)
		}
		seen[name] = true

		raw := env[k]
		if raw == "" {
			return nil, nil, fmt.Errorf("%s is empty", k)
		}
		dsn, warns, err := normalizeDSN(raw, queryTimeout)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", k, err)
		}
		for _, w := range warns {
			warnings = append(warnings, fmt.Sprintf("source %q: %s", name, w))
		}
		sources = append(sources, Source{Name: name, DSN: dsn})
	}
	if len(sources) == 0 {
		return nil, nil, errors.New("no MYSQL_DSN_<NAME> configured")
	}
	slices.SortFunc(sources, func(a, b Source) int { return strings.Compare(a.Name, b.Name) })
	return sources, warnings, nil
}

// Redacted returns user@net(addr)/db: safe to log, no password or parameters.
func (s Source) Redacted() string {
	cfg, err := mysql.ParseDSN(s.DSN)
	if err != nil {
		return "<invalid DSN>"
	}
	return fmt.Sprintf("%s@%s(%s)/%s", cfg.User, cfg.Net, cfg.Addr, cfg.DBName)
}

// String returns Redacted so a Source printed with fmt never shows its DSN.
func (s Source) String() string { return s.Name + "=" + s.Redacted() }

// GoString makes %#v safe too.
func (s Source) GoString() string { return s.String() }

// LogValue makes slog log the redacted form only.
func (s Source) LogValue() slog.Value {
	return slog.GroupValue(slog.String("name", s.Name), slog.String("dsn", s.Redacted()))
}

// plainConfig has no methods, so formatting it does not recurse into
// Config.String.
type plainConfig Config

// String formats the Config with the auth token masked and DSNs redacted
// (Source.String applies to the nested sources).
func (c Config) String() string {
	if c.AuthToken != "" {
		c.AuthToken = "<redacted>"
	}
	return fmt.Sprintf("%+v", plainConfig(c))
}

// GoString makes %#v safe too.
func (c Config) GoString() string { return c.String() }

// LogValue makes slog log the settings without the token or DSNs.
func (c Config) LogValue() slog.Value {
	sources := make([]any, 0, len(c.Sources))
	for _, s := range c.Sources {
		sources = append(sources, slog.String(s.Name, s.Redacted()))
	}
	return slog.GroupValue(
		slog.Group("sources", sources...),
		slog.Int("max_rows", c.MaxRows),
		slog.Int("max_cell_bytes", c.MaxCellBytes),
		slog.Int("max_result_bytes", c.MaxResultBytes),
		slog.Duration("query_timeout", c.QueryTimeout),
		slog.Int("max_concurrent_queries", c.MaxConcurrentQueries),
		slog.String("auth_token", "<redacted>"),
		slog.Any("allowed_origins", c.AllowedOrigins),
		slog.Bool("allow_unsafe_privileges", c.AllowUnsafePrivileges),
		slog.String("listen_addr", c.ListenAddr),
		slog.String("log_level", c.LogLevel.String()),
	)
}
