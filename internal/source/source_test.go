package source

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/minhquan2904/mysql-mcp-go/internal/config"
)

func TestOpenRejectsInvalidLimits(t *testing.T) {
	good := Limits{MaxRows: 1, MaxCellBytes: 1, MaxResultBytes: 1, MaxConcurrent: 1, Timeout: time.Second}
	for name, mutate := range map[string]func(*Limits){
		"MaxRows":        func(l *Limits) { l.MaxRows = 0 },
		"MaxCellBytes":   func(l *Limits) { l.MaxCellBytes = 0 },
		"MaxResultBytes": func(l *Limits) { l.MaxResultBytes = 0 },
		"MaxConcurrent":  func(l *Limits) { l.MaxConcurrent = 0 },
		"Timeout":        func(l *Limits) { l.Timeout = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			lim := good
			mutate(&lim)
			// Validation happens before any connection attempt.
			s, err := Open(context.Background(), config.Source{Name: "x", DSN: "u:p@tcp(127.0.0.1:1)/db"}, lim, nil)
			if err == nil {
				s.Close()
				t.Fatal("Open succeeded with invalid limits")
			}
		})
	}
}

func TestOpenValidatesLimitRelations(t *testing.T) {
	cfg := config.Source{Name: "x", DSN: "u:p@tcp(127.0.0.1:1)/db"}
	for _, tc := range []struct {
		name string
		lim  Limits
		want string
	}{
		{"cell larger than result", Limits{MaxRows: 1, MaxCellBytes: 100, MaxResultBytes: 99, MaxConcurrent: 1, Timeout: time.Second}, "MaxResultBytes"},
		// MaxConcurrent+2 query conns + 2 killer conns must fit MAX_USER_CONNECTIONS
		{"too many connections", Limits{MaxRows: 1, MaxCellBytes: 1, MaxResultBytes: 1, MaxConcurrent: maxUserConnections - 3, Timeout: time.Second}, "MAX_USER_CONNECTIONS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Open(context.Background(), cfg, tc.lim, nil)
			if err == nil {
				s.Close()
				t.Fatal("Open succeeded")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// deadCtx has a deadline in the past but, like a context whose runtime timer
// has not fired yet, does not report an error.
type deadCtx struct {
	context.Context
	dl time.Time
}

func (c deadCtx) Deadline() (time.Time, bool) { return c.dl, true }
func (c deadCtx) Err() error                  { return nil }

// unitSource has a killer pool that can never connect (nothing listens on port
// 1), so a KILL attempt fails fast and is only counted.
func unitSource(t *testing.T) *Source {
	t.Helper()
	killer, err := sql.Open("mysql", "u:p@tcp(127.0.0.1:1)/db?timeout=200ms")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killer.Close() })
	return &Source{Name: "x", killer: killer, lim: Limits{Timeout: 2 * time.Second},
		log: slog.New(slog.DiscardHandler)}
}

func TestCtxDone(t *testing.T) {
	if err := ctxDone(context.Background()); err != nil {
		t.Errorf("no deadline: %v, want nil", err)
	}
	future, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if err := ctxDone(future); err != nil {
		t.Errorf("future deadline: %v, want nil", err)
	}
	cancelled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if err := ctxDone(cancelled); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v, want Canceled", err)
	}
	lagging := deadCtx{context.Background(), time.Now().Add(-time.Millisecond)}
	if err := ctxDone(lagging); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("deadline passed, timer not fired: %v, want DeadlineExceeded", err)
	}
}

func TestClassify(t *testing.T) {
	const eff = 1500 * time.Millisecond
	other := errors.New("boom")
	serverTimeout := &mysql.MySQLError{Number: 3024, Message: "Query execution was interrupted, maximum statement execution time exceeded"}
	lagging := deadCtx{context.Background(), time.Now().Add(-time.Millisecond)}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("deadline passed kills and reports the effective timeout", func(t *testing.T) {
		s := unitSource(t)
		err := s.classify(lagging, 7, eff, other)
		if !errors.Is(err, ErrQueryTimeout) || !strings.Contains(err.Error(), "1.5s") {
			t.Errorf("err = %v, want ErrQueryTimeout mentioning 1.5s", err)
		}
		if got := s.kills.Load(); got != 1 {
			t.Errorf("kills = %d, want 1", got)
		}
	})
	t.Run("cancel kills and returns Canceled", func(t *testing.T) {
		s := unitSource(t)
		err := s.classify(cancelled, 7, eff, other)
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrQueryTimeout) {
			t.Errorf("err = %v, want plain context.Canceled", err)
		}
		if got := s.kills.Load(); got != 1 {
			t.Errorf("kills = %d, want 1", got)
		}
	})
	t.Run("unknown connection id is not killed", func(t *testing.T) {
		s := unitSource(t)
		if err := s.classify(lagging, 0, eff, other); !errors.Is(err, ErrQueryTimeout) {
			t.Errorf("err = %v, want ErrQueryTimeout", err)
		}
		if got := s.kills.Load(); got != 0 {
			t.Errorf("kills = %d, want 0", got)
		}
	})
	t.Run("server max_execution_time error maps without a kill", func(t *testing.T) {
		s := unitSource(t)
		err := s.classify(context.Background(), 7, eff, fmt.Errorf("query: %w", serverTimeout))
		var me *mysql.MySQLError
		if !errors.Is(err, ErrQueryTimeout) || !errors.As(err, &me) || me.Number != 3024 {
			t.Errorf("err = %v, want ErrQueryTimeout wrapping the server error", err)
		}
		if got := s.kills.Load(); got != 0 {
			t.Errorf("kills = %d, want 0", got)
		}
	})
	t.Run("other errors pass through", func(t *testing.T) {
		s := unitSource(t)
		if err := s.classify(context.Background(), 7, eff, other); err != other {
			t.Errorf("err = %v, want the original error", err)
		}
		if got := s.kills.Load(); got != 0 {
			t.Errorf("kills = %d, want 0", got)
		}
	})
}

func TestEffectiveTimeout(t *testing.T) {
	const limit = 30 * time.Second
	if got := effectiveTimeout(context.Background(), limit); got != limit {
		t.Errorf("no parent deadline: %v, want %v", got, limit)
	}
	long, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if got := effectiveTimeout(long, limit); got != limit {
		t.Errorf("parent longer: %v, want %v", got, limit)
	}
	short, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if got := effectiveTimeout(short, limit); got < 1900*time.Millisecond || got > 2*time.Second {
		t.Errorf("parent shorter: %v, want about 2s", got)
	}
	expired := deadCtx{context.Background(), time.Now().Add(-time.Second)}
	if got := effectiveTimeout(expired, limit); got != 0 {
		t.Errorf("parent expired: %v, want 0", got)
	}
}
