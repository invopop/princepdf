package princepdf

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartup(t *testing.T) {
	pc := New()
	require.NoError(t, pc.Start())
	defer func() {
		assert.NoError(t, pc.Stop())
	}()

	t.Run("test with URL", func(t *testing.T) {
		j := new(Job)
		j.Input = &Input{
			Src: "https://www.princexml.com/samples/invoice/invoicesample.html",
		}
		out, err := pc.Run(j)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile("output1.pdf", out, 0644))
	})

	t.Run("embedded data", func(t *testing.T) {
		// test with embedded data
		j := new(Job)
		j.Input = &Input{
			Src: "data.html",
		}
		j.Files = map[string][]byte{
			"data.html": []byte("<html><body><h1>Hello, World!</h1></body></html>"),
		}

		out, err := pc.Run(j)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile("output2.pdf", out, 0644))
	})

}

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

const (
	fakeEnv      = "PRINCEPDF_FAKE"
	fakeTallyEnv = "PRINCEPDF_FAKE_TALLY"
	fakePDF      = "%PDF-fake"

	// Job sources that make the fake prince misbehave.
	fakeSrcDie    = "fake:die"
	fakeSrcHang   = "fake:hang"
	fakeSrcReject = "fake:reject"
)

// TestFakePrince is not a test. It is the entry point used when the test binary
// re-executes itself to stand in for the prince binary; see useFakePrince.
func TestFakePrince(t *testing.T) {
	if os.Getenv(fakeEnv) != "1" {
		t.Skip("helper process, only run when re-executed as a fake prince")
	}
	fakePrince()
	// Exit before the testing package can write its own summary to stdout,
	// which would otherwise corrupt the control stream.
	os.Exit(0)
}

// fakePrince implements just enough of the prince --control protocol to serve
// jobs that carry no resources: announce a version, answer each job with a pdf
// and a log chunk, and quit on "end".
func fakePrince() {
	if tally := os.Getenv(fakeTallyEnv); tally != "" {
		// Record this launch so tests can count how often we were recycled.
		f, err := os.OpenFile(tally, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.WriteString("launch\n")
			_ = f.Close()
		}
	}

	out := bufio.NewWriter(os.Stdout)
	in := bufio.NewReader(os.Stdin)

	writeFakeChunk(out, "ver", []byte("fake-prince 1.0"))

	for {
		line, err := in.ReadString('\n')
		if err != nil {
			return
		}
		parts := strings.Fields(strings.TrimSpace(line))
		if len(parts) == 0 {
			continue
		}
		switch parts[0] {
		case chunkEnd:
			return
		case chunkJob:
			n, err := strconv.Atoi(parts[1])
			if err != nil {
				return
			}
			body := make([]byte, n)
			if _, err := io.ReadFull(in, body); err != nil {
				return
			}
			if _, err := in.ReadString('\n'); err != nil {
				return
			}
			// The job's source picks a failure to act out.
			var req struct {
				Input struct {
					Src string `json:"src"`
				} `json:"input"`
			}
			_ = json.Unmarshal(body, &req)
			switch req.Input.Src {
			case fakeSrcDie:
				os.Exit(3) // crash without answering
			case fakeSrcHang:
				select {} // never answer
			case fakeSrcReject:
				writeFakeChunk(out, chunkErr, []byte("cannot load document"))
				continue
			}
			// Real prince writes diagnostics to stderr; doing the same keeps
			// the stderr drainers busy across recycles, so -race exercises
			// them.
			fmt.Fprintln(os.Stderr, "fake-prince: rendering")
			writeFakeChunk(out, chunkPDF, []byte(fakePDF))
			writeFakeChunk(out, chunkLog, []byte("done"))
		}
	}
}

func writeFakeChunk(w *bufio.Writer, msg string, data []byte) {
	fmt.Fprintf(w, "%s %d\n", msg, len(data))
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n"))
	_ = w.Flush()
}

// useFakePrince points the package at the fake prince above and returns the
// path of the file recording each launch.
func useFakePrince(t *testing.T) (tally string) {
	t.Helper()

	tally = filepath.Join(t.TempDir(), "launches")
	t.Setenv(fakeEnv, "1")
	t.Setenv(fakeTallyEnv, tally)

	origCmd, origOpts := cmdPrince, cmdPrinceOpts
	cmdPrince = os.Args[0]
	cmdPrinceOpts = []string{"-test.run=^TestFakePrince$"}
	t.Cleanup(func() {
		cmdPrince, cmdPrinceOpts = origCmd, origOpts
	})

	return tally
}

func countLaunches(t *testing.T, tally string) int {
	t.Helper()
	data, err := os.ReadFile(tally)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		require.NoError(t, err)
	}
	return strings.Count(string(data), "launch\n")
}

func simpleJob() *Job {
	j := new(Job)
	j.Input = &Input{Src: "https://example.test/doc.html"}
	return j
}

func TestWorkerRecycling(t *testing.T) {
	t.Run("recycles after the configured number of jobs", func(t *testing.T) {
		tally := useFakePrince(t)

		pc := New(WithWorkerCount(1), WithMaxJobsPerWorker(2))
		require.NoError(t, pc.Start())

		for i := range 6 {
			out, err := pc.Run(simpleJob())
			require.NoError(t, err, "job %d", i)
			assert.Equal(t, fakePDF, string(out), "job %d", i)
		}

		require.NoError(t, pc.Stop())

		// One launch to start, plus a recycle after each pair of jobs.
		assert.Equal(t, 4, countLaunches(t, tally))
	})

	t.Run("keeps one process when recycling is disabled", func(t *testing.T) {
		tally := useFakePrince(t)

		pc := New(WithWorkerCount(1), WithMaxJobsPerWorker(0))
		require.NoError(t, pc.Start())

		for i := range 5 {
			out, err := pc.Run(simpleJob())
			require.NoError(t, err, "job %d", i)
			assert.Equal(t, fakePDF, string(out), "job %d", i)
		}

		require.NoError(t, pc.Stop())

		assert.Equal(t, 1, countLaunches(t, tally))
	})

	t.Run("serves jobs across recycles with several workers", func(t *testing.T) {
		tally := useFakePrince(t)

		pc := New(WithWorkerCount(3), WithMaxJobsPerWorker(2))
		require.NoError(t, pc.Start())

		for i := range 30 {
			out, err := pc.Run(simpleJob())
			require.NoError(t, err, "job %d", i)
			assert.Equal(t, fakePDF, string(out), "job %d", i)
		}

		require.NoError(t, pc.Stop())

		// 30 jobs recycled every 2 gives 15 recycles on top of the 3 initial
		// launches, however the jobs are distributed between workers.
		assert.Equal(t, 18, countLaunches(t, tally))
	})
}

// failLaunches makes the next n prince launches fail to start, as if the
// binary were missing, and shortens the relaunch backoff so tests stay fast.
// A negative n fails every launch after the first.
func failLaunches(t *testing.T, n int64) {
	t.Helper()

	var remaining atomic.Int64
	remaining.Store(n)
	var first atomic.Bool

	origCmd, origDelay, origMax := newPrinceCmd, relaunchDelay, relaunchMaxDelay
	newPrinceCmd = func() *exec.Cmd {
		if !first.Swap(true) {
			return origCmd() // let Start bring the worker up
		}
		if n < 0 || remaining.Add(-1) >= 0 {
			return exec.Command(filepath.Join(t.TempDir(), "missing-prince"))
		}
		return origCmd()
	}
	relaunchDelay, relaunchMaxDelay = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() {
		newPrinceCmd, relaunchDelay, relaunchMaxDelay = origCmd, origDelay, origMax
	})
}

// runWithin runs a job, failing the test instead of hanging if no worker
// picks it up in time.
func runWithin(t *testing.T, pc *Client, d time.Duration) ([]byte, error) {
	t.Helper()

	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := pc.Run(simpleJob())
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		return r.out, r.err
	case <-time.After(d):
		t.Fatalf("job not served within %s", d)
		return nil, nil
	}
}

func TestWorkerRelaunchFailures(t *testing.T) {
	t.Run("keeps the worker after more failed relaunches than it used to allow", func(t *testing.T) {
		useFakePrince(t)
		// The previous version retired a worker after 5 failed attempts,
		// which with a single worker left every later Run blocked forever.
		failLaunches(t, 8)

		pc := New(WithWorkerCount(1), WithMaxJobsPerWorker(1))
		require.NoError(t, pc.Start())

		for i := range 3 {
			out, err := runWithin(t, pc, 5*time.Second)
			require.NoError(t, err, "job %d", i)
			assert.Equal(t, fakePDF, string(out), "job %d", i)
		}

		require.NoError(t, pc.Stop())
	})

	t.Run("stops promptly while relaunches keep failing", func(t *testing.T) {
		useFakePrince(t)
		failLaunches(t, -1)

		pc := New(WithWorkerCount(1), WithMaxJobsPerWorker(1))
		require.NoError(t, pc.Start())

		// The first job succeeds, then the recycle after it fails forever.
		out, err := runWithin(t, pc, 5*time.Second)
		require.NoError(t, err)
		assert.Equal(t, fakePDF, string(out))
		time.Sleep(20 * time.Millisecond) // let a few relaunches fail

		stopped := make(chan error, 1)
		go func() { stopped <- pc.Stop() }()
		select {
		case err := <-stopped:
			require.NoError(t, err)
		case <-time.After(2 * time.Second):
			t.Fatal("Stop did not return while relaunches were failing")
		}
	})
}

func TestStopWithBlockedRun(t *testing.T) {
	useFakePrince(t)
	failLaunches(t, -1)

	pc := New(WithWorkerCount(1), WithMaxJobsPerWorker(1))
	require.NoError(t, pc.Start())

	// The first job succeeds, then the recycle after it fails forever, so
	// nothing receives the second job and its Run blocks.
	_, err := runWithin(t, pc, 5*time.Second)
	require.NoError(t, err)

	blocked := make(chan error, 1)
	go func() {
		_, err := pc.Run(simpleJob())
		blocked <- err
	}()
	time.Sleep(20 * time.Millisecond) // let Run block on the send

	require.NoError(t, pc.Stop())

	select {
	case err := <-blocked:
		assert.ErrorIs(t, err, ErrStopped)
	case <-time.After(2 * time.Second):
		t.Fatal("blocked Run did not return after Stop")
	}

	// Run after Stop fails fast rather than blocking or panicking.
	_, err = runWithin(t, pc, time.Second)
	assert.ErrorIs(t, err, ErrStopped)
}
