package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	goodToken = "0123456789abcdef0123456789abcdef"
	// Nothing listens on port 1, so Open fails immediately without a database.
	deadDSN = "appuser:s3cret-pw@tcp(127.0.0.1:1)/db"
)

func runWith(t *testing.T, env ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	err := run(ctx, env, &buf)
	return buf.String(), err
}

func TestRunMissingToken(t *testing.T) {
	logs, err := runWith(t, "MYSQL_DSN_A="+deadDSN)
	if err == nil || !strings.Contains(err.Error(), "MCP_AUTH_TOKEN") {
		t.Fatalf("err = %v, want one naming MCP_AUTH_TOKEN", err)
	}
	if !strings.Contains(logs, `"level":"ERROR"`) {
		t.Errorf("config failure not logged at ERROR: %s", logs)
	}
}

func TestRunMissingDSN(t *testing.T) {
	logs, err := runWith(t, "MCP_AUTH_TOKEN="+goodToken)
	if err == nil || !strings.Contains(err.Error(), "no MYSQL_DSN") {
		t.Fatalf("err = %v, want one containing %q", err, "no MYSQL_DSN")
	}
	if !strings.Contains(logs, "no MYSQL_DSN") {
		t.Errorf("log lacks %q: %s", "no MYSQL_DSN", logs)
	}
}

func TestRunShortToken(t *testing.T) {
	_, err := runWith(t, "MCP_AUTH_TOKEN=short", "MYSQL_DSN_A="+deadDSN)
	if err == nil {
		t.Fatal("a token under 32 characters must be refused")
	}
}

func TestRunOpenFailureLogsRedactedWarningsAndNoSecrets(t *testing.T) {
	logs, err := runWith(t,
		"MCP_AUTH_TOKEN="+goodToken,
		"MYSQL_DSN_A="+deadDSN+"?multiStatements=true",
	)
	if err == nil {
		t.Fatal("expected an error: the database is unreachable")
	}
	for _, secret := range []string{"s3cret-pw", goodToken} {
		if strings.Contains(logs, secret) || strings.Contains(err.Error(), secret) {
			t.Errorf("secret %q leaked\nlogs: %s\nerr: %v", secret, logs, err)
		}
	}
	if !strings.Contains(logs, `"level":"WARN"`) || !strings.Contains(logs, "multiStatements") {
		t.Errorf("config warning not logged at WARN: %s", logs)
	}
	if !strings.Contains(logs, `"level":"ERROR"`) || !strings.Contains(logs, `"source":"a"`) {
		t.Errorf("open failure not logged with the source name: %s", logs)
	}
}

func TestRunInvalidConfigStopsBeforeOpeningSources(t *testing.T) {
	logs, err := runWith(t, "MCP_AUTH_TOKEN="+goodToken, "MYSQL_DSN_A="+deadDSN, "LOG_LEVEL=loud")
	if err == nil || !strings.Contains(err.Error(), "LOG_LEVEL") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(logs, "cannot open source") {
		t.Errorf("sources were opened despite invalid config: %s", logs)
	}
}

type fakeServer struct {
	serveErr    error
	shutdownErr error // returned immediately when set, else Shutdown waits for ctx
	closed      bool
	gotDeadline time.Duration
	stop        chan struct{}
}

func (f *fakeServer) Serve(net.Listener) error {
	if f.serveErr != nil {
		return f.serveErr
	}
	<-f.stop
	return http.ErrServerClosed
}

func (f *fakeServer) Shutdown(ctx context.Context) error {
	if d, ok := ctx.Deadline(); ok {
		f.gotDeadline = time.Until(d)
	}
	if f.shutdownErr != nil {
		return f.shutdownErr
	}
	<-ctx.Done() // a query that never finishes
	return ctx.Err()
}

func (f *fakeServer) Close() error {
	if !f.closed {
		f.closed = true
		close(f.stop)
	}
	return nil
}

func serveWith(t *testing.T, f *fakeServer, grace time.Duration) (string, error) {
	t.Helper()
	f.stop = make(chan struct{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // shut down right away
	err = serve(ctx, f, ln, grace, log)
	return buf.String(), err
}

func TestShutdownTimeoutIsQueryTimeoutPlusFive(t *testing.T) {
	if got := shutdownTimeout(30 * time.Second); got != 35*time.Second {
		t.Fatalf("shutdownTimeout(30s) = %v, want 35s", got)
	}
}

func TestServeUsesTheGivenGrace(t *testing.T) {
	f := &fakeServer{}
	serveWith(t, f, 80*time.Millisecond)
	if f.gotDeadline <= 0 || f.gotDeadline > 80*time.Millisecond {
		t.Errorf("Shutdown deadline = %v, want within 80ms", f.gotDeadline)
	}
}

func TestServeForcedCloseAfterDeadlineIsCleanExitWithWarn(t *testing.T) {
	f := &fakeServer{}
	logs, err := serveWith(t, f, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("err = %v, want nil: a forced close after the grace period is a normal exit", err)
	}
	if !f.closed {
		t.Error("server was not force-closed")
	}
	if !strings.Contains(logs, `"level":"WARN"`) || !strings.Contains(logs, "in-flight") {
		t.Errorf("no WARN about the forced close: %s", logs)
	}
}

func TestServeOtherShutdownErrorIsReturned(t *testing.T) {
	f := &fakeServer{shutdownErr: errors.New("boom")}
	_, err := serveWith(t, f, time.Second)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestServeReturnsServeFailure(t *testing.T) {
	f := &fakeServer{serveErr: errors.New("accept failed")}
	f.stop = make(chan struct{})
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	var buf bytes.Buffer
	err := serve(t.Context(), f, ln, time.Second, slog.New(slog.NewJSONHandler(&buf, nil)))
	if err == nil || !strings.Contains(err.Error(), "accept failed") {
		t.Fatalf("err = %v", err)
	}
}
