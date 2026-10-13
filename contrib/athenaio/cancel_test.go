package athenaio

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/athena"
	athenatypes "github.com/aws/aws-sdk-go-v2/service/athena/types"
	gluetypes "github.com/aws/aws-sdk-go-v2/service/glue/types"
)

// newCancelClient builds a client whose query never finishes on its
// own (the mock stays RUNNING for a very long time).
func newCancelClient(t *testing.T, a AthenaAPI, cfg func(*ClientConfig)) *Client {
	t.Helper()
	cc := ClientConfig{
		Workgroup:      "wg",
		ResultLocation: "s3://test-bucket/results/",
		Database:       "test_db",
		Athena:         a,
		S3:             &mockS3{objects: map[string][]byte{}},
		Glue:           &mockGlue{tables: map[glueTableKey]*gluetypes.Table{}},
		PollInterval:   time.Millisecond,
	}
	if cfg != nil {
		cfg(&cc)
	}
	c, err := NewClient(cc)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func assertStopped(t *testing.T, got []string, want string) {
	t.Helper()
	if len(got) != 1 || got[0] != want {
		t.Fatalf("StopQueryExecution calls = %v, want [%s]", got, want)
	}
}

// TestRawQuery_CancelStopsQuery — cancelling the request while the
// query runs stops it in Athena, and the error still matches
// context.Canceled.
func TestRawQuery_CancelStopsQuery(t *testing.T) {
	m := &mockAthena{pollsBeforeDone: 1 << 30}
	c := newCancelClient(t, m, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()

	_, err := c.RawQuery(ctx, "SELECT 1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "test-query-id-abc123") {
		t.Errorf("error should name the query: %v", err)
	}
	assertStopped(t, m.stoppedIDs(), "test-query-id-abc123")
}

// TestRawQuery_DeadlineStopsQuery — same for a context deadline.
func TestRawQuery_DeadlineStopsQuery(t *testing.T) {
	m := &mockAthena{pollsBeforeDone: 1 << 30}
	c := newCancelClient(t, m, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := c.RawQuery(ctx, "SELECT 1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	assertStopped(t, m.stoppedIDs(), "test-query-id-abc123")
}

// TestRawQuery_MaxPollDurationStopsQuery — athenaio giving up on its
// own (MaxPollDuration) also stops the query.
func TestRawQuery_MaxPollDurationStopsQuery(t *testing.T) {
	m := &mockAthena{pollsBeforeDone: 1 << 30}
	c := newCancelClient(t, m, func(cc *ClientConfig) { cc.MaxPollDuration = 20 * time.Millisecond })

	_, err := c.RawQuery(context.Background(), "SELECT 1")
	if !errors.Is(err, ErrQueryTimeout) {
		t.Fatalf("err = %v, want ErrQueryTimeout", err)
	}
	assertStopped(t, m.stoppedIDs(), "test-query-id-abc123")
}

// ctxAwareAthena makes GetQueryExecution fail with ctx.Err() once the
// context is done, as the real SDK does, so the poll call itself (not
// the wait between polls) observes the cancellation.
type ctxAwareAthena struct{ *mockAthena }

func (a ctxAwareAthena) GetQueryExecution(ctx context.Context, in *athena.GetQueryExecutionInput, opts ...func(*athena.Options)) (*athena.GetQueryExecutionOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("operation error Athena: GetQueryExecution: %w", err)
	}
	return a.mockAthena.GetQueryExecution(ctx, in, opts...)
}

func TestRawQuery_CancelDuringPollCallStopsQuery(t *testing.T) {
	m := &mockAthena{pollsBeforeDone: 1 << 30}
	c := newCancelClient(t, ctxAwareAthena{m}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the first poll call fails

	_, err := c.RawQuery(ctx, "SELECT 1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	assertStopped(t, m.stoppedIDs(), "test-query-id-abc123")
}

// TestRawQuery_StopFailureReported — when StopQueryExecution fails on
// an abandoned query, the failure goes to WarnLog and the error's
// StopErr, but not into the message: callers see the timeout or
// cancellation alone, which errors.Is still matches.
func TestRawQuery_StopFailureReported(t *testing.T) {
	stopFailure := errors.New("AccessDeniedException: not authorized to perform athena:StopQueryExecution")
	cases := []struct {
		name  string
		cfg   func(*ClientConfig)
		ctx   func() (context.Context, context.CancelFunc)
		cause error
	}{
		{
			name:  "timeout",
			cfg:   func(cc *ClientConfig) { cc.MaxPollDuration = 20 * time.Millisecond },
			ctx:   func() (context.Context, context.CancelFunc) { return context.Background(), func() {} },
			cause: ErrQueryTimeout,
		},
		{
			name: "deadline",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 20*time.Millisecond)
			},
			cause: context.DeadlineExceeded,
		},
		{
			name: "cancel",
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				time.AfterFunc(20*time.Millisecond, cancel)
				return ctx, cancel
			},
			cause: context.Canceled,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &mockAthena{pollsBeforeDone: 1 << 30}
			m.stopErr = stopFailure
			var mu sync.Mutex
			var warnings []string
			c := newCancelClient(t, m, func(cc *ClientConfig) {
				cc.WarnLog = func(format string, args ...any) {
					mu.Lock()
					warnings = append(warnings, fmt.Sprintf(format, args...))
					mu.Unlock()
				}
				if tc.cfg != nil {
					tc.cfg(cc)
				}
			})
			ctx, cancel := tc.ctx()
			defer cancel()

			_, err := c.RawQuery(ctx, "SELECT 1")
			if !errors.Is(err, tc.cause) {
				t.Fatalf("err = %v, want errors.Is %v", err, tc.cause)
			}
			if strings.Contains(err.Error(), "AccessDenied") || strings.Contains(err.Error(), "StopQueryExecution") {
				t.Errorf("message should not include the stop failure: %v", err)
			}
			if !strings.Contains(err.Error(), "abandoned") {
				t.Errorf("message should say the query was abandoned, not stopped: %v", err)
			}
			var ab *AbandonedQueryError
			if !errors.As(err, &ab) {
				t.Fatalf("err = %T, want *AbandonedQueryError", err)
			}
			if ab.QueryID != "test-query-id-abc123" || !errors.Is(ab.StopErr, stopFailure) {
				t.Errorf("AbandonedQueryError = %+v", ab)
			}
			assertStopped(t, m.stoppedIDs(), "test-query-id-abc123")
			mu.Lock()
			defer mu.Unlock()
			if len(warnings) != 1 || !strings.Contains(warnings[0], "may still be running") || !strings.Contains(warnings[0], "AccessDenied") {
				t.Errorf("warnings = %v, want one stop-failure warning", warnings)
			}
		})
	}
}

// TestRawQuery_StoppedErrorHasNoStopErr — a successful stop yields an
// *AbandonedQueryError with StopErr nil and a "stopped" message.
func TestRawQuery_StoppedErrorHasNoStopErr(t *testing.T) {
	m := &mockAthena{pollsBeforeDone: 1 << 30}
	c := newCancelClient(t, m, func(cc *ClientConfig) { cc.MaxPollDuration = 20 * time.Millisecond })
	_, err := c.RawQuery(context.Background(), "SELECT 1")
	var ab *AbandonedQueryError
	if !errors.As(err, &ab) || ab.StopErr != nil {
		t.Fatalf("err = %v, want *AbandonedQueryError with nil StopErr", err)
	}
	if want := "athenaio: query test-query-id-abc123 stopped: "; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("message = %q, want prefix %q", err.Error(), want)
	}
}

// TestRawQuery_NoStopWhenQueryEnds — finished queries (succeeded or
// failed) are never stopped.
func TestRawQuery_NoStopWhenQueryEnds(t *testing.T) {
	failed := &mockAthena{
		pollsBeforeDone: 0,
		forceFailState:  athenatypes.QueryExecutionStateFailed,
		forceFailReason: "SYNTAX_ERROR",
	}
	c := newCancelClient(t, failed, nil)
	if _, err := c.RawQuery(context.Background(), "SELEC 1"); !errors.Is(err, ErrQueryFailed) {
		t.Fatalf("err = %v, want ErrQueryFailed", err)
	}
	if got := failed.stoppedIDs(); len(got) != 0 {
		t.Errorf("failed query was stopped: %v", got)
	}
}

// TestUnloadAndRead_CancelStopsCTAS — the CTAS paths share the poll
// loop, so cancelling one stops the CTAS query too.
func TestUnloadAndRead_CancelStopsCTAS(t *testing.T) {
	m := &mockCTASAthena{pollsBeforeDone: 1 << 30}
	c := newCancelClient(t, m, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := c.UnloadAndRead(ctx, UnloadSpec{SQL: "SELECT id FROM t", PartitionBy: []string{"id"}, BucketCount: 4})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	assertStopped(t, m.stoppedIDs(), "ctas-abc123")
}
