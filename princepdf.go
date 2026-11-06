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
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	chunkJob           = "job"
	chunkData          = "dat"
	chunkEnd           = "end"
	chunkErr           = "err"
	chunkLog           = "log"
	chunkPDF           = "pdf"
	strJobResource     = "job-resource:%d"
	strFilesFmt        = "files:%s"
	workerCountDefault = 1

	// maxJobsDefault is the number of jobs a single prince process will handle
	// before it is replaced with a fresh one. Prince's --control mode does not
	// release all of the memory used by a job, so a process that runs forever
	// grows without bound (roughly 50-80KB retained per document). Recycling
	// keeps that bounded; prince starts in milliseconds, so the cost is
	// negligible next to rendering a document.
	maxJobsDefault = 1000
)

// ErrStopped is returned by Run when the client is stopped before a worker
// picks the job up.
var ErrStopped = errors.New("princepdf: client stopped")

// ErrProcess is wrapped by the errors Run returns when the prince process
// handling the job died, hung or stopped following the control protocol,
// rather than prince rejecting the job. The process is replaced, so running
// the job again may well succeed.
var ErrProcess = errors.New("princepdf: prince process failed")

// Error is returned by Run when prince rejects a job, for example because the
// document could not be loaded. The prince process is unaffected, and running
// the same job again will fail the same way.
type Error struct {
	Message string
}

func (e *Error) Error() string {
	return "prince error: " + e.Message
}

var (
	// cmdPrince and cmdPrinceOpts locate the prince binary. They are variables
	// so that tests can stand in a fake that speaks the control protocol
	// without needing prince installed.
	cmdPrince     = "prince"
	cmdPrinceOpts = []string{"--control"}

	// newPrinceCmd builds the command for a prince process. Tests replace it
	// to simulate launch failures.
	newPrinceCmd = func() *exec.Cmd {
		return exec.Command(cmdPrince, cmdPrinceOpts...)
	}

	// relaunchDelay is the base backoff between attempts to bring a
	// replacement prince process up during a recycle, multiplied by the
	// attempt number and capped at relaunchMaxDelay. A worker keeps trying
	// until it succeeds or the client is stopped: retiring it instead would
	// leave Run blocked forever once every worker had gone.
	relaunchDelay    = 200 * time.Millisecond
	relaunchMaxDelay = 30 * time.Second

	// stopTimeout bounds how long Stop waits for a worker to finish the job it
	// is on. Prince can hang on a pathological document, and shutting the
	// client down must not block indefinitely on one. A variable so tests can
	// shorten it.
	stopTimeout = 20 * time.Second
)

// Client provides a client interface to be able to stream commands to the prince
// controller.
type Client struct {
	in          chan *task
	quit        chan struct{} // closed by Stop; in is never closed, as Run may still be sending
	workerCount int
	maxJobs     int
	workers     []*worker
}

// Option defines a functional option to configure the Client
type Option func(*Client)

// WithWorkerCounter sets the number of prince processes to launch
// thus increasing the number of concurrent requests that can be handled.
// The default is 1.
func WithWorkerCount(i int) Option {
	return func(c *Client) {
		c.workerCount = i
	}
}

// WithMaxJobsPerWorker sets how many jobs a single prince process will handle
// before being replaced by a fresh one, which keeps the memory retained by
// prince's --control mode from accumulating indefinitely. Use zero or a
// negative value to disable recycling and keep each process alive for the
// lifetime of the client. The default is 1000.
func WithMaxJobsPerWorker(i int) Option {
	return func(c *Client) {
		c.maxJobs = i
	}
}

// New instantiates a new PrincePDF client
func New(opts ...Option) *Client {
	c := &Client{
		in:          make(chan *task),
		quit:        make(chan struct{}),
		workerCount: workerCountDefault,
		maxJobs:     maxJobsDefault,
	}
	for _, opt := range opts {
		opt(c)
	}
	c.workers = make([]*worker, c.workerCount)
	return c
}

// Start begins the workers and prepares to run jobs
func (c *Client) Start() error {
	var err error
	for i := range c.workers {
		c.workers[i], err = newWorker(c.in, c.quit, c.maxJobs)
		if err != nil {
			return fmt.Errorf("starting: %w", err)
		}
		go c.workers[i].start()
	}
	return nil
}

// Stop ends the workers and closes the client.
func (c *Client) Stop() error {
	// Signal shutdown through quit rather than by closing in: a Run blocked
	// sending on in would panic if it were closed under it.
	close(c.quit)
	for _, w := range c.workers {
		if w == nil {
			continue // Start failed before reaching this one
		}
		// Wait for the worker to leave its loop before touching its process:
		// it may be part-way through a recycle, and both paths shut a prince
		// process down.
		select {
		case <-w.done:
			w.stop()
		case <-time.After(stopTimeout):
			// Still rendering, probably hung. Killing the process makes the
			// worker fail the job and leave its loop, after which it is safe
			// to clean up.
			fmt.Printf("worker still busy after %s, killing prince\n", stopTimeout)
			if w.killUntilDone() {
				w.stop()
			} else {
				fmt.Printf("gave up waiting for worker to finish after killing prince\n")
			}
		}
	}
	return nil

}

// Run sends a job to the prince controller and returns the output. It is
// RunContext without a deadline.
func (c *Client) Run(job *Job) ([]byte, error) {
	return c.RunContext(context.Background(), job)
}

// RunContext sends a job to the prince controller and returns the output.
//
// If ctx ends before the job finishes, RunContext returns ctx's error. When
// prince was already rendering the job, its process is killed and replaced,
// since it may be hung and is in any case busy with work nobody will read.
//
// Errors where prince rejected the job are of type *Error. Errors where the
// prince process failed wrap ErrProcess. Such a job is retried, as the
// process may have died before the job reached it, up to once per worker:
// each worker replaces a failed process before taking another job, so by
// then the job has met at least one fresh process.
func (c *Client) RunContext(ctx context.Context, job *Job) ([]byte, error) {
	if job == nil || job.Input == nil {
		// Checked here, as a nil input would panic inside the worker.
		return nil, errors.New("princepdf: job has no input")
	}
	for attempt := 1; ; attempt++ {
		data, err := c.send(ctx, job)
		if errors.Is(err, ErrProcess) && attempt <= c.workerCount && ctx.Err() == nil {
			continue
		}
		return data, err
	}
}

// send hands the job to a worker and waits for its reply or for ctx to end.
func (c *Client) send(ctx context.Context, job *Job) ([]byte, error) {
	// Buffered, so a worker can reply after the caller has given up.
	t := &task{ctx: ctx, job: job, reply: make(chan *output, 1)}
	select {
	case c.in <- t:
	case <-c.quit:
		return nil, ErrStopped
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// No need to watch quit here: the send only completes once a worker has
	// taken the task, and a worker always replies to a task it took. Stop
	// kills a worker that is stuck.
	select {
	case out := <-t.reply:
		if out.err != nil && ctx.Err() != nil {
			// The failure was most likely us killing the process.
			return nil, ctx.Err()
		}
		return out.data, out.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// task is a job handed to a worker, with what it needs to answer.
type task struct {
	ctx   context.Context
	job   *Job
	reply chan *output
}

// worker represents and individual execution of a prince command that can
// respond to an process a single stream of requests.
type worker struct {
	cmd  *exec.Cmd
	in   chan *task
	quit chan struct{}

	// proc is the current process, for Client.Stop to kill a worker stuck
	// in a job without racing with the worker replacing cmd.
	proc atomic.Pointer[os.Process]

	stderr *bufio.Reader
	stdout *bufio.Reader
	stdin  io.Writer

	maxJobs int  // recycle the process after this many jobs, if positive
	jobs    int  // jobs handled by the current process
	ended   bool // the current process has already been shut down

	// done is closed when the worker has left its job loop, after which no
	// further recycling can happen and its process is safe to shut down.
	done chan struct{}
}

func newWorker(in chan *task, quit chan struct{}, maxJobs int) (*worker, error) {
	w := &worker{
		in:      in,
		quit:    quit,
		maxJobs: maxJobs,
		done:    make(chan struct{}),
	}
	if err := w.launch(); err != nil {
		return nil, err
	}
	return w, nil
}

// launch spawns a prince process for the worker and wires up its pipes,
// replacing those of any previous process.
func (w *worker) launch() error {
	w.cmd = newPrinceCmd()
	stderr, err := w.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("preparing stderr: %w", err)
	}
	w.stderr = bufio.NewReader(stderr)
	stdout, err := w.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("preparing stdout: %w", err)
	}
	w.stdout = bufio.NewReader(stdout)
	if w.stdin, err = w.cmd.StdinPipe(); err != nil {
		return fmt.Errorf("preparing stdin: %w", err)
	}
	if err := w.cmd.Start(); err != nil {
		return err
	}
	w.proc.Store(w.cmd.Process)
	w.jobs = 0
	w.ended = false
	return nil
}

// greet consumes the version banner prince emits on startup and begins
// relaying its stderr.
func (w *worker) greet() {
	// first grab the version information which is sent automatically by prince
	_, data, err := w.readChunk()
	if err != nil {
		// The first job will fail and replace the process.
		fmt.Printf("reading prince version: %s\n", err.Error())
	}
	fmt.Printf("started version: '%s'\n", string(data))
	// The drainer gets this process's reader rather than reading w.stderr,
	// which the next recycle replaces while the old drainer may still be
	// running.
	go printStderr(w.stderr)
}

func (w *worker) start() {
	defer close(w.done)
	w.greet()
	for {
		var t *task
		select {
		case <-w.quit:
			return
		case t = <-w.in:
		}
		out := w.run(t)
		t.reply <- out
		w.jobs++
		// Recycle only after the reply has been sent, so that a failure to
		// bring up the replacement can never affect a job that already
		// succeeded. A broken process is replaced straight away: it would
		// fail every later job.
		if out.broken || (w.maxJobs > 0 && w.jobs >= w.maxJobs) {
			if out.broken {
				fmt.Printf("replacing failed prince process: %v\n", out.err)
			}
			if err := w.recycle(out.broken); err != nil {
				// Only returned once the client is stopping.
				fmt.Printf("recycling worker: %s\n", err.Error())
				return
			}
		}
	}
}

// recycle replaces the worker's prince process with a fresh one, discarding
// the memory the old process accumulated. A broken process is killed rather
// than asked to end, as it may be hung. recycle retries a failed launch until
// it succeeds, and only returns an error if the client is stopped first.
func (w *worker) recycle(broken bool) error {
	if broken {
		w.kill()
	} else {
		w.stop()
	}

	for attempt := 1; ; attempt++ {
		select {
		case <-w.quit:
			// Don't launch a process that Stop may already have given up on.
			return errors.New("client stopped")
		default:
		}
		err := w.launch()
		if err == nil {
			w.greet()
			return nil
		}
		delay := time.Duration(attempt) * relaunchDelay
		if delay > relaunchMaxDelay {
			delay = relaunchMaxDelay
		}
		fmt.Printf("relaunching prince (attempt %d, retrying in %s): %s\n", attempt, delay, err.Error())
		select {
		case <-w.quit:
			return fmt.Errorf("relaunching prince: client stopped after %d failed attempts: %w", attempt, err)
		case <-time.After(delay):
		}
	}
}

func printStderr(r *bufio.Reader) {
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				fmt.Printf("reading stderr: %s\n", err.Error())
			}
			return
		}
		fmt.Printf("stderr: %s\n", line)
	}
}

// stop shuts the current prince process down. It is safe to call more than
// once: recycling stops the outgoing process itself, and Client.Stop may then
// be called on the same worker.
func (w *worker) stop() {
	if w.ended {
		return
	}
	w.ended = true
	if err := w.end(); err != nil {
		fmt.Printf("ending session: %s\n", err.Error())
	}
	if err := w.cmd.Wait(); err != nil {
		fmt.Printf("waiting for command to close: %s\n", err.Error())
	}
}

// kill shuts the current prince process down without asking, for a process
// that is hung, dead or out of step with the control protocol.
func (w *worker) kill() {
	if w.ended {
		return
	}
	w.ended = true
	if err := w.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		fmt.Printf("killing prince: %s\n", err.Error())
	}
	_ = w.cmd.Wait() // reaps the process; it reports the kill as an error
}

// killUntilDone kills the worker's current process until the worker leaves its
// loop, for up to stopTimeout. Killing again covers a replacement process
// launched between two kills. It reports whether the worker left.
func (w *worker) killUntilDone() bool {
	deadline := time.After(stopTimeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		w.killCurrent()
		select {
		case <-w.done:
			return true
		case <-deadline:
			return false
		case <-tick.C:
		}
	}
}

// killCurrent kills the current process from outside the worker's goroutine.
// The worker notices when its next read or write fails.
func (w *worker) killCurrent() {
	if p := w.proc.Load(); p != nil {
		if err := p.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			fmt.Printf("killing prince: %s\n", err.Error())
		}
	}
}

// run sends the task's job to prince and reads the result. If the task's
// context ends first, the process is killed, which ends the exchange, and the
// output is marked broken so the process gets replaced.
func (w *worker) run(t *task) *output {
	if err := t.ctx.Err(); err != nil {
		// The caller gave up while the job was queued.
		return &output{err: err}
	}
	req := t.job.request()
	data, err := json.Marshal(req)
	if err != nil {
		return &output{err: fmt.Errorf("princepdf: encoding job: %w", err)}
	}

	// Capture the process rather than reading w.cmd later: by the time the
	// context ends, the worker may have moved on to a new process.
	proc := w.cmd.Process
	stopKill := context.AfterFunc(t.ctx, func() {
		_ = proc.Kill()
	})
	out := w.exchange(data, req.resources)
	if !stopKill() {
		// The kill ran, or is running: the process is gone even if the
		// job completed first.
		out.broken = true
	}
	return out
}

// exchange writes a job and its resources to prince and reads the reply.
func (w *worker) exchange(data []byte, resources [][]byte) *output {
	if err := w.write(chunkJob, data); err != nil {
		return processFailure("sending job", err)
	}
	for _, d := range resources {
		if err := w.write(chunkData, d); err != nil {
			return processFailure("sending resource", err)
		}
	}
	return w.readReply()
}

// processFailure is the output for a job whose prince process failed.
func processFailure(what string, err error) *output {
	return &output{err: fmt.Errorf("%w: %s: %w", ErrProcess, what, err), broken: true}
}

func (w *worker) end() error {
	return w.write(chunkEnd, nil)
}

func (w *worker) write(msg string, data []byte) error {
	if len(data) == 0 {
		// Bare command, no length prefix or payload follows. Recycling sends
		// one of these ("end") on every rotation, so returning here matters:
		// falling through would write the command a second time.
		_, err := w.stdin.Write([]byte(msg + "\n"))
		return err
	}

	msg = fmt.Sprintf("%s %d", msg, len(data))
	if _, err := w.stdin.Write([]byte(msg + "\n")); err != nil {
		return fmt.Errorf("writing header: %w", err)
	}

	if _, err := w.stdin.Write(data); err != nil {
		return fmt.Errorf("writing data: %w", err)
	}
	if _, err := w.stdin.Write([]byte("\n")); err != nil {
		return fmt.Errorf("writing last newline: %w", err)
	}

	return nil
}

// readReply reads prince's answer to a job: a pdf chunk followed by a log
// chunk when the conversion succeeded, a log chunk alone when it failed, or an
// err chunk when prince could not read the job.
func (w *worker) readReply() *output {
	msg, data, err := w.readChunk()
	if err != nil {
		return processFailure("reading reply", err)
	}
	switch msg {
	case chunkPDF:
		lmsg, ldata, err := w.readChunk()
		if err != nil {
			return processFailure("reading log after pdf", err)
		}
		if lmsg != chunkLog {
			return processFailure("reading log after pdf", fmt.Errorf("unexpected %q chunk", lmsg))
		}
		fmt.Printf("log: %s\n", string(ldata))
		return &output{data: data}
	case chunkLog:
		fmt.Printf("log: %s\n", string(data))
		return &output{err: &Error{Message: conversionErrors(data)}}
	case chunkErr:
		// Prince could not read the job. Don't trust the stream after it.
		return &output{err: &Error{Message: string(data)}, broken: true}
	default:
		return processFailure("reading reply", fmt.Errorf("unexpected %q chunk", msg))
	}
}

// conversionErrors extracts the error messages from the log of a failed
// conversion, whose lines look like "msg|err|<location>|<message>".
func conversionErrors(log []byte) string {
	var msgs []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(log), "\n") {
		f := strings.SplitN(line, "|", 4)
		if len(f) != 4 || f[0] != "msg" || f[1] != "err" {
			continue
		}
		m := f[3]
		if f[2] != "" {
			m = f[2] + ": " + m
		}
		if !seen[m] {
			seen[m] = true
			msgs = append(msgs, m)
		}
	}
	if len(msgs) == 0 {
		return "conversion failed"
	}
	return strings.Join(msgs, "; ")
}

// readChunk reads one chunk from prince: a "<msg> [<length>]" line, then
// that many bytes of data and a newline.
func (w *worker) readChunk() (string, []byte, error) {
	line, err := w.stdout.ReadString('\n')
	if err != nil {
		return "", nil, fmt.Errorf("reading string: %w", err)
	}
	line = strings.TrimSpace(line)
	fmt.Printf("line: '%s'\n", line)

	parts := strings.Fields(line)
	if len(parts) == 0 {
		return "", nil, errors.New("invalid empty response")
	}
	if len(parts) == 1 {
		return parts[0], nil, nil
	}

	l, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", nil, fmt.Errorf("invalid length: %w", err)
	}
	if l < 0 {
		return "", nil, fmt.Errorf("invalid length: %d", l)
	}
	data := make([]byte, l)
	if _, err := io.ReadFull(w.stdout, data); err != nil {
		return "", nil, fmt.Errorf("reading data: %w", err)
	}
	if _, err := w.stdout.ReadString('\n'); err != nil { // the newline at the end
		return "", nil, fmt.Errorf("reading newline: %w", err)
	}
	return parts[0], data, nil
}

// output wraps around output provided from a job
type output struct {
	data []byte
	err  error
	// broken is set when the prince process can't be trusted with another
	// job: it died, was killed, or the stream got out of step.
	broken bool
}
