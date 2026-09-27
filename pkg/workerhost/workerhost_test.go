package workerhost_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
	"github.com/SamudralaAjaykumarrr/taskforge/pkg/workerhost"
)

func TestMain(m *testing.M) { testutil.RunMain(m) }

// countingHandler proves an external package can implement
// workerhost.Handler (== the real internal/handler.Handler, via the type
// alias) without importing anything from internal/.
type countingHandler struct {
	calls   atomic.Int32
	payload chan string
}

func (h *countingHandler) Execute(_ context.Context, j *workerhost.Job) (workerhost.Result, error) {
	h.calls.Add(1)
	var p struct {
		Msg string `json:"msg"`
	}
	_ = json.Unmarshal(j.Payload, &p)
	select {
	case h.payload <- p.Msg:
	default:
	}
	return workerhost.Result{}, nil
}

// TestHost_ExecutesRegisteredHandler proves the minimal generic surface
// this package exists to provide (see workerhost.go's package doc): an
// external module can register its own Handler and have TaskForge's real
// claim-execute-complete loop run it against real PostgreSQL, using
// nothing but this package's exported API.
func TestHost_ExecutesRegisteredHandler(t *testing.T) {
	db := testutil.DB(t)

	h := &countingHandler{payload: make(chan string, 1)}
	registry := workerhost.NewRegistry()
	registry.Register("workerhost.count", h)

	host, err := workerhost.NewHost(context.Background(), db, registry, workerhost.HostConfig{
		WorkerID: "workerhost-test", PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = host.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	st := store.New(db)
	payload, _ := json.Marshal(map[string]string{"msg": "hello from an external module"})
	if _, err := st.Insert(context.Background(), job.NewParams{
		PrincipalID:             principal.SystemPrincipalID,
		JobType:                 "workerhost.count",
		Payload:                 payload,
		MaxAttempts:             job.DefaultMaxAttempts,
		ExecutionTimeoutSeconds: job.DefaultExecutionTimeoutSeconds,
		QueueName:               "default",
	}); err != nil {
		t.Fatalf("insert job: %v", err)
	}

	select {
	case msg := <-h.payload:
		if msg != "hello from an external module" {
			t.Fatalf("unexpected payload: %q", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler was never invoked")
	}
	if h.calls.Load() != 1 {
		t.Fatalf("expected exactly 1 call, got %d", h.calls.Load())
	}
}

// TestRetryable_Permanent_Classify proves the re-exported failure
// classification helpers behave exactly like internal/handler's (they
// are thin wrappers, not reimplementations).
func TestRetryable_Permanent_Classify(t *testing.T) {
	class, err := workerhost.Classify(workerhost.Retryable(context.DeadlineExceeded))
	if class != workerhost.FailureClass("RETRYABLE") {
		t.Fatalf("expected RETRYABLE, got %s", class)
	}
	if err == nil {
		t.Fatal("expected the original error to be preserved")
	}

	class, _ = workerhost.Classify(context.DeadlineExceeded) // never wrapped
	if class != workerhost.FailureClass("PERMANENT") {
		t.Fatalf("expected an unclassified error to default to PERMANENT, got %s", class)
	}
}
