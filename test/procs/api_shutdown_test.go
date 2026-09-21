// SF-063 and SF-070 (docs/phase-14-plan.md §16): the real cmd/api
// OS-process proofs for Phase 14's graceful-shutdown redesign (§6.4, §19
// OD-4). Each test starts a real compiled cmd/api binary, talks to it
// over a real HTTP connection, sends a real SIGTERM, and asserts on
// externally observable behavior (HTTP responses, connection state,
// process exit) exactly as the plan's proof obligations require.
package procs_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
)

// testPepper is a fixed, 32-byte-plus API-key pepper used only by this
// package's spawned cmd/api processes and the principal.NewStore this
// test uses to mint a matching credential -- never a production secret.
const testPepper = "test-only-pepper-for-phase14-os-process-tests-not-a-secret!!"

// mintCredential creates a real principal + API key against db (using
// testPepper, matching what the spawned cmd/api process is given via
// TASKFORGE_API_KEY_PEPPER) and returns the bearer credential string.
func mintCredential(t *testing.T, dsn string) string {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer db.Close()

	ps, err := principal.NewStore(db, []byte(testPepper))
	require.NoError(t, err)

	p, err := ps.CreatePrincipal(context.Background(), principal.KindCaller, "procs-test caller")
	require.NoError(t, err)

	cred, err := ps.CreateAPIKey(context.Background(), p.ID, []string{principal.ScopeJobs}, nil)
	require.NoError(t, err)
	return cred.Credential
}

// waitForListening polls addr until a TCP connection succeeds.
func waitForListening(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("procs: %s never started accepting connections", addr)
}

func submitJob(t *testing.T, client *http.Client, addr, credential, jobType string) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"job_type": jobType,
		"payload":  map[string]any{},
	})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/jobs", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+credential)
	resp, err := client.Do(req)
	require.NoError(t, err)
	return resp
}

// TestProc_SF063_APIGracefulShutdown_OrdinaryRequestCompletes_NewConnectionsRefused
// is SF-063: a real cmd/api binary under SIGTERM refuses new connections
// immediately, while an already-in-flight ordinary request completes
// because it finishes within TASKFORGE_API_SHUTDOWN_TIMEOUT.
func TestProc_SF063_APIGracefulShutdown_OrdinaryRequestCompletes_NewConnectionsRefused(t *testing.T) {
	_, apiBin := buildBinaries(t)
	_, dsn := openDB(t)
	credential := mintCredential(t, dsn)
	addr := freeTCPAddr(t)

	h := startProcess(t, apiBin,
		"TASKFORGE_DATABASE_URL="+dsn,
		"TASKFORGE_HTTP_ADDR="+addr,
		"TASKFORGE_API_KEY_PEPPER="+testPepper,
		"TASKFORGE_API_SHUTDOWN_TIMEOUT=3s",
	)
	waitForListening(t, addr, 5*time.Second)

	client := &http.Client{Timeout: 5 * time.Second}

	// Confirm the server is genuinely healthy and authenticated requests
	// succeed before shutdown begins.
	resp := submitJob(t, client, addr, credential, "procs.sf063.warmup")
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	_ = resp.Body.Close()

	// Fire an ordinary request and SIGTERM as close to simultaneously as
	// this process can manage: the request must still complete
	// successfully, because ordinary handling (well under a millisecond
	// of real work) finishes long before the 3s shutdown budget.
	type result struct {
		resp *http.Response
		err  error
	}
	inFlight := make(chan result, 1)
	go func() {
		resp, err := client.Do(mustRequest(t, addr, credential, "procs.sf063.inflight"))
		inFlight <- result{resp, err}
	}()
	time.Sleep(2 * time.Millisecond) // give the goroutine a head start into the connect/write path
	require.NoError(t, h.cmd.Process.Signal(syscall.SIGTERM))

	select {
	case r := <-inFlight:
		require.NoError(t, r.err, "an ordinary request racing SIGTERM must still complete, within the shutdown budget")
		require.Equal(t, http.StatusCreated, r.resp.StatusCode)
		_ = r.resp.Body.Close()
	case <-time.After(4 * time.Second):
		t.Fatal("the in-flight request never completed")
	}

	// A brand-new connection attempt shortly after SIGTERM must be
	// refused -- Shutdown closes the listener immediately, before it
	// waits for any in-flight handler.
	time.Sleep(100 * time.Millisecond)
	_, err := (&http.Client{Timeout: 1 * time.Second}).Do(mustRequest(t, addr, credential, "procs.sf063.rejected"))
	require.Error(t, err, "a new connection attempted after SIGTERM must be refused, not accepted")

	waitErr := make(chan error, 1)
	go func() { waitErr <- h.cmd.Wait() }()
	select {
	case err := <-waitErr:
		if err != nil {
			h.logOutput(t)
			t.Fatalf("api process exited with error: %v", err)
		}
	case <-time.After(5 * time.Second):
		h.logOutput(t)
		t.Fatal("api process did not exit after SIGTERM within its shutdown budget")
	}
}

func mustRequest(t *testing.T, addr, credential, jobType string) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{"job_type": jobType, "payload": map[string]any{}})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/jobs", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+credential)
	return req
}

// pacedReader is an io.Reader that trickles out one filler byte every
// interval, forever, and never signals EOF on its own -- simulating a
// slow client whose request body is still arriving. It deliberately
// sleeps rather than blocking on anything connection-related, so
// http.Transport's writer goroutine keeps periodically attempting a real
// write on the underlying socket; that periodic write is what lets this
// test actually observe the moment the server forcibly closes the
// connection (a write to an already-closed connection fails immediately),
// rather than hanging forever the way blocking on an unrelated in-memory
// channel would.
type pacedReader struct {
	interval time.Duration
}

func (r *pacedReader) Read(p []byte) (int, error) {
	time.Sleep(r.interval)
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = 'x' // filler; never completes valid JSON on its own, so the handler's decode never finishes unaided
	return 1, nil
}

// TestProc_SF070_APIShutdown_SlowHandlerObservesCancellationAtDeadline is
// SF-070: cmd/api under SIGTERM with a deliberately slow request (a real
// client that streams its body far slower than TASKFORGE_API_SHUTDOWN_TIMEOUT):
// the handler's blocked read is forcibly ended once the shutdown deadline
// passes (cmd/api's BaseContext cancellation plus srv.Close, docs/phase-14-plan.md
// §6.4) rather than left to run unobserved forever, and the process still
// exits promptly afterward.
func TestProc_SF070_APIShutdown_SlowHandlerObservesCancellationAtDeadline(t *testing.T) {
	_, apiBin := buildBinaries(t)
	_, dsn := openDB(t)
	credential := mintCredential(t, dsn)
	addr := freeTCPAddr(t)

	h := startProcess(t, apiBin,
		"TASKFORGE_DATABASE_URL="+dsn,
		"TASKFORGE_HTTP_ADDR="+addr,
		"TASKFORGE_API_KEY_PEPPER="+testPepper,
		"TASKFORGE_API_SHUTDOWN_TIMEOUT=1s",
	)
	waitForListening(t, addr, 5*time.Second)

	// A request whose body never finishes arriving -- the server's JSON
	// decoder blocks trying to read past the opening fragment, exactly
	// like a handler genuinely still doing I/O when shutdown begins.
	slow := &pacedReader{interval: 50 * time.Millisecond}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/jobs", io.NopCloser(slow))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+credential)
	req.ContentLength = -1 // chunked; the server must not expect a fixed, ever-completing length

	client := &http.Client{Timeout: 10 * time.Second}
	reqErr := make(chan error, 1)
	go func() {
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		reqErr <- err
	}()

	time.Sleep(150 * time.Millisecond) // let the slow request genuinely reach the server and start blocking
	require.NoError(t, h.cmd.Process.Signal(syscall.SIGTERM))

	// The shutdown deadline (1s) must pass, srv.Close must force the
	// still-open connection closed, and the client must observe its
	// request fail -- never hang forever, never silently succeed.
	select {
	case err := <-reqErr:
		require.Error(t, err, "a request still blocked past the shutdown deadline must be forcibly terminated, not left to complete silently later")
	case <-time.After(5 * time.Second):
		h.logOutput(t)
		t.Fatal("the slow request was never terminated by shutdown -- it was abandoned exactly as the pre-Phase-14 gap described")
	}

	// Unlike SF-063's graceful path, exiting non-zero here is the
	// CORRECT, expected outcome: run() legitimately returns
	// context.DeadlineExceeded when the shutdown budget is exceeded, and
	// main() reports that as a process failure exit code -- what this
	// assertion actually checks is that the process exits promptly at
	// all, rather than hanging forever behind the abandoned slow
	// handler.
	waitErr := make(chan error, 1)
	go func() { waitErr <- h.cmd.Wait() }()
	select {
	case <-waitErr:
	case <-time.After(3 * time.Second):
		h.logOutput(t)
		t.Fatal("api process did not exit promptly after forcing the slow connection closed")
	}

	require.Contains(t, h.stdout.String(), `"outcome":"deadline_exceeded"`,
		"the process's own structured log must record that shutdown hit its deadline, not completed gracefully")
}
