package princepdf

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func jobWithSrc(src string) *Job {
	return &Job{Input: &Input{Src: src}}
}

// startFake starts a client with one worker on the fake prince and stops it
// when the test ends.
func startFake(t *testing.T) (*Client, string) {
	t.Helper()
	tally := useFakePrince(t)
	pc := New(WithWorkerCount(1))
	require.NoError(t, pc.Start())
	t.Cleanup(func() { _ = pc.Stop() })
	return pc, tally
}

// runCtxWithin runs RunContext, failing the test instead of hanging.
func runCtxWithin(t *testing.T, pc *Client, ctx context.Context, job *Job, d time.Duration) ([]byte, error) {
	t.Helper()
	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := pc.RunContext(ctx, job)
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		return r.out, r.err
	case <-time.After(d):
		t.Fatalf("job did not return within %s", d)
		return nil, nil
	}
}

func assertServes(t *testing.T, pc *Client) {
	t.Helper()
	out, err := runCtxWithin(t, pc, context.Background(), simpleJob(), 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, fakePDF, string(out))
}

func TestDocumentError(t *testing.T) {
	pc, tally := startFake(t)

	_, err := runCtxWithin(t, pc, context.Background(), jobWithSrc(fakeSrcReject), 5*time.Second)
	var perr *Error
	require.ErrorAs(t, err, &perr)
	assert.Equal(t, "cannot load document", perr.Message)
	assert.EqualError(t, err, "prince error: cannot load document")
	assert.NotErrorIs(t, err, ErrProcess)

	// The process is fine and keeps serving.
	assertServes(t, pc)
	assert.Equal(t, 1, countLaunches(t, tally))
}

func TestProcessDiesDuringJob(t *testing.T) {
	pc, tally := startFake(t)

	// The job kills every process it reaches: it is retried once, then fails.
	_, err := runCtxWithin(t, pc, context.Background(), jobWithSrc(fakeSrcDie), 5*time.Second)
	require.ErrorIs(t, err, ErrProcess)
	var perr *Error
	assert.False(t, errors.As(err, &perr))

	// Each dead process was replaced.
	assertServes(t, pc)
	assert.Equal(t, 3, countLaunches(t, tally))
}

func TestProcessDiedBetweenJobs(t *testing.T) {
	pc, tally := startFake(t)
	assertServes(t, pc)

	// Kill the idle process, as the OOM killer might.
	p := pc.workers[0].proc.Load()
	require.NoError(t, p.Kill())

	// The next job finds the process dead and is retried on a new one, so
	// the caller doesn't see the failure.
	assertServes(t, pc)
	assert.Equal(t, 2, countLaunches(t, tally))
}

func TestHungJobContext(t *testing.T) {
	pc, tally := startFake(t)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := runCtxWithin(t, pc, ctx, jobWithSrc(fakeSrcHang), 5*time.Second)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 2*time.Second)

	// The hung process was killed and replaced.
	assertServes(t, pc)
	assert.Equal(t, 2, countLaunches(t, tally))
}

func TestContextEndsWhileQueued(t *testing.T) {
	pc, tally := startFake(t)

	// Occupy the only worker.
	hangCtx, stopHang := context.WithCancel(context.Background())
	hung := make(chan error, 1)
	go func() {
		_, err := pc.RunContext(hangCtx, jobWithSrc(fakeSrcHang))
		hung <- err
	}()
	time.Sleep(100 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := runCtxWithin(t, pc, ctx, simpleJob(), 5*time.Second)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	stopHang()
	require.ErrorIs(t, <-hung, context.Canceled)
	assertServes(t, pc)
	assert.Equal(t, 2, countLaunches(t, tally))
}

func TestJobWithoutInput(t *testing.T) {
	pc, _ := startFake(t)

	_, err := pc.Run(nil)
	require.Error(t, err)
	_, err = pc.Run(NewJob())
	require.Error(t, err)

	assertServes(t, pc)
}

func TestStopKillsHungWorker(t *testing.T) {
	useFakePrince(t)
	orig := stopTimeout
	stopTimeout = 100 * time.Millisecond
	t.Cleanup(func() { stopTimeout = orig })

	pc := New(WithWorkerCount(1))
	require.NoError(t, pc.Start())

	hung := make(chan error, 1)
	go func() {
		_, err := pc.Run(jobWithSrc(fakeSrcHang))
		hung <- err
	}()
	time.Sleep(100 * time.Millisecond)
	p := pc.workers[0].proc.Load()

	stopped := make(chan error, 1)
	go func() { stopped <- pc.Stop() }()
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return with a hung worker")
	}

	// The blocked Run returns too, and the hung process is gone.
	select {
	case err := <-hung:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after Stop")
	}
	assert.ErrorIs(t, p.Signal(syscall.Signal(0)), os.ErrProcessDone)
}

// The tests below run against the real prince, when it is installed.

func requirePrince(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("prince"); err != nil {
		t.Skip("prince not installed")
	}
}

func princeJob() *Job {
	return &Job{
		Input: &Input{Src: "doc.html"},
		Files: map[string][]byte{"doc.html": []byte("<html><body><h1>Hello</h1></body></html>")},
	}
}

func TestRealPrinceKilled(t *testing.T) {
	requirePrince(t)
	pc := New(WithWorkerCount(2))
	require.NoError(t, pc.Start())
	defer func() { assert.NoError(t, pc.Stop()) }()
	for range 2 {
		_, err := pc.Run(princeJob())
		require.NoError(t, err)
	}

	var old []*os.Process
	for _, w := range pc.workers {
		p := w.proc.Load()
		old = append(old, p)
		require.NoError(t, p.Kill())
	}
	for i := range 6 {
		out, err := pc.Run(princeJob())
		require.NoError(t, err, "job %d", i)
		assert.True(t, len(out) > 4 && string(out[:4]) == "%PDF", "job %d", i)
	}
	for i, w := range pc.workers {
		assert.NotEqual(t, old[i].Pid, w.proc.Load().Pid, "worker %d replaced", i)
	}
}

func TestRealPrinceHung(t *testing.T) {
	requirePrince(t)
	pc := New(WithWorkerCount(1))
	require.NoError(t, pc.Start())
	defer func() { assert.NoError(t, pc.Stop()) }()

	// Let the worker get past prince's startup banner first.
	_, err := pc.Run(princeJob())
	require.NoError(t, err)
	p := pc.workers[0].proc.Load()
	require.NoError(t, p.Signal(syscall.SIGSTOP))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = pc.RunContext(ctx, princeJob())
	require.ErrorIs(t, err, context.DeadlineExceeded)

	out, err := pc.Run(princeJob())
	require.NoError(t, err)
	assert.Equal(t, "%PDF", string(out[:4]))
	assert.NotEqual(t, p.Pid, pc.workers[0].proc.Load().Pid)
	assert.ErrorIs(t, p.Signal(syscall.Signal(0)), os.ErrProcessDone)
}

func TestAllProcessesDiedBetweenJobs(t *testing.T) {
	useFakePrince(t)
	pc := New(WithWorkerCount(2))
	require.NoError(t, pc.Start())
	t.Cleanup(func() { _ = pc.Stop() })
	for range 2 {
		assertServes(t, pc)
	}

	for _, w := range pc.workers {
		require.NoError(t, w.proc.Load().Kill())
	}

	// A job may meet both dead processes before a fresh one; it still
	// succeeds, and so do the ones after it.
	for range 4 {
		assertServes(t, pc)
	}
}
