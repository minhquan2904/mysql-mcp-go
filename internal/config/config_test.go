package config

import (
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	goodToken = "0123456789abcdef0123456789abcdef" // 32 chars
	goodDSN   = "u:pw@tcp(h:3306)/db"
)

// baseEnv is a minimal valid environment; extra entries are appended and win
// over earlier ones with the same key.
func baseEnv(extra ...string) []string {
	return append([]string{"MCP_AUTH_TOKEN=" + goodToken, "MYSQL_DSN_PROD=" + goodDSN}, extra...)
}

// envNoDSN is a valid environment without any source.
func envNoDSN(extra ...string) []string {
	return append([]string{"MCP_AUTH_TOKEN=" + goodToken}, extra...)
}

func sourceNames(c Config) []string {
	names := make([]string, 0, len(c.Sources))
	for _, s := range c.Sources {
		names = append(names, s.Name)
	}
	return names
}

func TestLoadSourceDiscovery(t *testing.T) {
	tests := []struct {
		name    string
		environ []string
		want    []string // expected source names
		wantErr string   // substring; empty means success
	}{
		{"single source", envNoDSN("MYSQL_DSN_PROD=" + goodDSN), []string{"prod"}, ""},
		{"sorted by name", envNoDSN("MYSQL_DSN_B="+goodDSN, "MYSQL_DSN_A="+goodDSN), []string{"a", "b"}, ""},
		{"lowercased", envNoDSN("MYSQL_DSN_My_Db=" + goodDSN), []string{"my_db"}, ""},
		{"unrelated vars ignored", envNoDSN("MYSQL_DSN_A="+goodDSN, "PATH=/bin", "MYSQL_DSNX=1"), []string{"a"}, ""},
		{"duplicate after lowercase", envNoDSN("MYSQL_DSN_A="+goodDSN, "MYSQL_DSN_a="+goodDSN), nil, `duplicate source name "a"`},
		{"invalid name char", envNoDSN("MYSQL_DSN_A-B=" + goodDSN), nil, "MYSQL_DSN_A-B"},
		{"empty name", envNoDSN("MYSQL_DSN_=" + goodDSN), nil, "MYSQL_DSN_"},
		{"empty value", envNoDSN("MYSQL_DSN_X="), nil, "MYSQL_DSN_X"},
		{"no sources", envNoDSN("PATH=/bin"), nil, "no MYSQL_DSN_<NAME> configured"},
		{"invalid dsn names var", envNoDSN("MYSQL_DSN_X=u:secretpw@tcp(h:3306"), nil, "MYSQL_DSN_X"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(tc.environ)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tc.wantErr)
				}
				if strings.Contains(err.Error(), "secretpw") {
					t.Fatalf("error leaks password: %q", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := sourceNames(cfg); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("sources = %v, want %v", got, tc.want)
			}
			for _, s := range cfg.Sources {
				if s.DSN == "" {
					t.Fatalf("source %q has empty DSN", s.Name)
				}
			}
		})
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(baseEnv())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxRows != 1000 || cfg.MaxCellBytes != 65536 || cfg.MaxResultBytes != 4<<20 ||
		cfg.QueryTimeout != 30*time.Second || cfg.MaxConcurrentQueries != 8 {
		t.Errorf("numeric defaults wrong: %+v", cfg)
	}
	if cfg.ListenAddr != ":8080" || cfg.LogLevel != slog.LevelInfo || cfg.AllowUnsafePrivileges {
		t.Errorf("other defaults wrong: addr=%q level=%v unsafe=%v", cfg.ListenAddr, cfg.LogLevel, cfg.AllowUnsafePrivileges)
	}
	if cfg.AuthToken != goodToken {
		t.Errorf("AuthToken = %q", cfg.AuthToken)
	}
	if len(cfg.AllowedOrigins) != 0 {
		t.Errorf("AllowedOrigins = %v, want empty", cfg.AllowedOrigins)
	}
}

func TestLoadScalarOverrides(t *testing.T) {
	cfg, err := Load(baseEnv("MAX_ROWS=5", "MAX_CELL_BYTES=10", "MAX_RESULT_BYTES=2048",
		"MAX_CONCURRENT_QUERIES=2", "QUERY_TIMEOUT=2s", "LISTEN_ADDR=127.0.0.1:9000"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxRows != 5 || cfg.MaxCellBytes != 10 || cfg.MaxResultBytes != 2048 ||
		cfg.MaxConcurrentQueries != 2 || cfg.QueryTimeout != 2*time.Second || cfg.ListenAddr != "127.0.0.1:9000" {
		t.Errorf("overrides not applied: %+v", cfg)
	}
}

func TestLoadRejectsInvalidScalars(t *testing.T) {
	var tests []struct{ key, val string }
	for _, key := range []string{"MAX_ROWS", "MAX_CELL_BYTES", "MAX_RESULT_BYTES", "MAX_CONCURRENT_QUERIES"} {
		for _, val := range []string{"0", "-1", "abc", "1.5"} {
			tests = append(tests, struct{ key, val string }{key, val})
		}
	}
	for _, val := range []string{"0s", "-5s", "xyz", "30"} {
		tests = append(tests, struct{ key, val string }{"QUERY_TIMEOUT", val})
	}
	for _, val := range []string{"yes", "1", "TRUE", "maybe"} {
		tests = append(tests, struct{ key, val string }{"ALLOW_UNSAFE_PRIVILEGES", val})
	}
	for _, val := range []string{"verbose", "trace", "info2"} {
		tests = append(tests, struct{ key, val string }{"LOG_LEVEL", val})
	}
	for _, tc := range tests {
		t.Run(tc.key+"="+tc.val, func(t *testing.T) {
			_, err := Load(baseEnv(tc.key + "=" + tc.val))
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error %q does not name %s", err, tc.key)
			}
		})
	}
}

func TestLoadEmptyScalarMeansDefault(t *testing.T) {
	cfg, err := Load(baseEnv("MAX_ROWS=", "QUERY_TIMEOUT=", "LOG_LEVEL=", "ALLOW_UNSAFE_PRIVILEGES="))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxRows != 1000 || cfg.QueryTimeout != 30*time.Second || cfg.LogLevel != slog.LevelInfo || cfg.AllowUnsafePrivileges {
		t.Errorf("empty values did not fall back to defaults: %+v", cfg)
	}
}

func TestLoadAuthToken(t *testing.T) {
	tests := []struct {
		name    string
		environ []string
		wantErr string
	}{
		{"missing", []string{"MYSQL_DSN_PROD=" + goodDSN}, "MCP_AUTH_TOKEN is required"},
		{"empty", []string{"MYSQL_DSN_PROD=" + goodDSN, "MCP_AUTH_TOKEN="}, "MCP_AUTH_TOKEN is required"},
		{"31 chars", []string{"MYSQL_DSN_PROD=" + goodDSN, "MCP_AUTH_TOKEN=" + goodToken[:31]}, "at least 32"},
		{"example value", []string{"MYSQL_DSN_PROD=" + goodDSN, "MCP_AUTH_TOKEN=change-me-token"}, "MCP_AUTH_TOKEN"},
		{"example prefix, long", []string{"MYSQL_DSN_PROD=" + goodDSN, "MCP_AUTH_TOKEN=change-me-0123456789abcdef0123456789abcdef"}, "MCP_AUTH_TOKEN"},
		{"example prefix, other case", []string{"MYSQL_DSN_PROD=" + goodDSN, "MCP_AUTH_TOKEN=Change-Me-0123456789abcdef0123456789abcdef"}, "MCP_AUTH_TOKEN"},
		{"32 chars ok", []string{"MYSQL_DSN_PROD=" + goodDSN, "MCP_AUTH_TOKEN=" + goodToken}, ""},
		{"multibyte counts characters", []string{"MYSQL_DSN_PROD=" + goodDSN, "MCP_AUTH_TOKEN=" + strings.Repeat("é", 32)}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(tc.environ)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if cfg.AuthToken == "" {
					t.Error("AuthToken empty")
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			}
			for _, e := range tc.environ {
				if tok, ok := strings.CutPrefix(e, "MCP_AUTH_TOKEN="); ok && tok != "" && strings.Contains(err.Error(), tok) {
					t.Errorf("error leaks the token: %q", err)
				}
			}
		})
	}
}

func TestLoadAllowedOrigins(t *testing.T) {
	cfg, err := Load(baseEnv("ALLOWED_ORIGINS=https://a.com, https://b.com ,, "))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"https://a.com", "https://b.com"}; !reflect.DeepEqual(cfg.AllowedOrigins, want) {
		t.Errorf("AllowedOrigins = %q, want %q", cfg.AllowedOrigins, want)
	}
}

func TestLoadAllowUnsafePrivileges(t *testing.T) {
	for val, want := range map[string]bool{"true": true, "false": false} {
		cfg, err := Load(baseEnv("ALLOW_UNSAFE_PRIVILEGES=" + val))
		if err != nil {
			t.Fatalf("%s: %v", val, err)
		}
		if cfg.AllowUnsafePrivileges != want {
			t.Errorf("%s: AllowUnsafePrivileges = %v", val, cfg.AllowUnsafePrivileges)
		}
	}
}

func TestLoadLogLevel(t *testing.T) {
	for val, want := range map[string]slog.Level{
		"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError, "DEBUG": slog.LevelDebug,
	} {
		cfg, err := Load(baseEnv("LOG_LEVEL=" + val))
		if err != nil {
			t.Fatalf("%s: %v", val, err)
		}
		if cfg.LogLevel != want {
			t.Errorf("%s: LogLevel = %v, want %v", val, cfg.LogLevel, want)
		}
	}
}

func TestLoadQueryTimeoutBounds(t *testing.T) {
	for _, v := range []string{"2h", "1h1s", "500us", "999999h"} {
		_, err := Load(baseEnv("QUERY_TIMEOUT=" + v))
		if err == nil || !strings.Contains(err.Error(), "QUERY_TIMEOUT") {
			t.Errorf("QUERY_TIMEOUT=%s: error = %v, want one naming QUERY_TIMEOUT", v, err)
		}
	}
	for v, want := range map[string]time.Duration{"1ms": time.Millisecond, "1h": time.Hour} {
		cfg, err := Load(baseEnv("QUERY_TIMEOUT=" + v))
		if err != nil {
			t.Errorf("QUERY_TIMEOUT=%s: %v", v, err)
		} else if cfg.QueryTimeout != want {
			t.Errorf("QUERY_TIMEOUT=%s: got %v", v, cfg.QueryTimeout)
		}
	}
}

func TestConfigAndSourceDoNotPrintSecrets(t *testing.T) {
	cfg, err := Load(envNoDSN("MYSQL_DSN_PROD=u:secretpw@tcp(h:3306)/db"))
	if err != nil {
		t.Fatal(err)
	}
	var logged strings.Builder
	logger := slog.New(slog.NewJSONHandler(&logged, nil))
	logger.Info("cfg", "config", cfg, "source", cfg.Sources[0], "sources", cfg.Sources[0].Name)
	logger = slog.New(slog.NewTextHandler(&logged, nil))
	logger.Info("cfg", "config", cfg, "source", cfg.Sources[0])

	outputs := map[string]string{
		"Sprint(cfg)":      fmt.Sprint(cfg),
		"Sprintf %+v cfg":  fmt.Sprintf("%+v", cfg),
		"Sprintf %#v cfg":  fmt.Sprintf("%#v", cfg),
		"Sprintf %v &cfg":  fmt.Sprintf("%v", &cfg),
		"Sprint(sources)":  fmt.Sprint(cfg.Sources),
		"Sprintf %+v src":  fmt.Sprintf("%+v", cfg.Sources[0]),
		"Sprintf %#v src":  fmt.Sprintf("%#v", cfg.Sources[0]),
		"slog json + text": logged.String(),
	}
	for name, out := range outputs {
		if strings.Contains(out, "secretpw") || strings.Contains(out, goodToken) {
			t.Errorf("%s leaks a secret: %s", name, out)
		}
	}
	if !strings.Contains(outputs["slog json + text"], "<redacted>") || !strings.Contains(outputs["slog json + text"], "u@tcp(h:3306)/db") {
		t.Errorf("log output lacks redacted fields: %s", outputs["slog json + text"])
	}
}
