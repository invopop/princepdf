package princepdf

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	fakeEnv      = "PRINCEPDF_FAKE"
	fakeTallyEnv = "PRINCEPDF_FAKE_TALLY"
	fakePDF      = "%PDF-fake"
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
			if _, err := io.ReadFull(in, make([]byte, n)); err != nil {
				return
			}
			if _, err := in.ReadString('\n'); err != nil {
				return
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
