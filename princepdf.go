package princepdf

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
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

	// relaunchAttempts is how many times we try to bring a replacement prince
	// process up during a recycle before giving up on the worker.
	relaunchAttempts = 5

	// relaunchDelay is the base backoff between relaunch attempts, multiplied
	// by the attempt number.
	relaunchDelay = 200 * time.Millisecond

	// stopTimeout bounds how long Stop waits for a worker to finish the job it
	// is on. Prince can hang on a pathological document, and shutting the
	// client down must not block indefinitely on one.
	stopTimeout = 20 * time.Second
)

var (
	// cmdPrince and cmdPrinceOpts locate the prince binary. They are variables
	// so that tests can stand in a fake that speaks the control protocol
	// without needing prince installed.
	cmdPrince     = "prince"
	cmdPrinceOpts = []string{"--control"}
)

// Client provides a client interface to be able to stream commands to the prince
// controller.
type Client struct {
	in          chan *Job
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
		in:          make(chan *Job),
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
		c.workers[i], err = newWorker(c.in, c.maxJobs)
		if err != nil {
			return fmt.Errorf("starting: %w", err)
		}
		go c.workers[i].start()
	}
	return nil
}

// Stop ends the workers and closes the client.
func (c *Client) Stop() error {
	close(c.in)
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
			// Still rendering, so its process is not ours to touch. Leave it
			// to be cleaned up as the parent exits.
			fmt.Printf("gave up waiting for worker to finish after %s\n", stopTimeout)
		}
	}
	return nil

}

// Run sends a job to the prince controller and returns the output.
func (c *Client) Run(job *Job) ([]byte, error) {
	job.reply = make(chan *output)
	defer close(job.reply)
	c.in <- job
	out := <-job.reply
	return out.data, out.err
}

// worker represents and individual execution of a prince command that can
// respond to an process a single stream of requests.
type worker struct {
	cmd *exec.Cmd
	in  chan *Job

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

func newWorker(in chan *Job, maxJobs int) (*worker, error) {
	w := &worker{
		in:      in,
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
	w.cmd = exec.Command(cmdPrince, cmdPrinceOpts...)
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
	w.jobs = 0
	w.ended = false
	return nil
}

// greet consumes the version banner prince emits on startup and begins
// relaying its stderr.
func (w *worker) greet() {
	// first grab the version information which is sent automatically by prince
	out := w.read()
	fmt.Printf("started version: '%s'\n", string(out.data))
	go w.printStderr()
}

func (w *worker) start() {
	defer close(w.done)
	w.greet()
	for job := range w.in {
		w.run(job)
		w.jobs++
		// Recycle only after the reply has been sent, so that a failure to
		// bring up the replacement can never affect a job that already
		// succeeded.
		if w.maxJobs > 0 && w.jobs >= w.maxJobs {
			if err := w.recycle(); err != nil {
				// The pool has lost this worker; the remaining ones carry on.
				fmt.Printf("recycling worker: %s\n", err.Error())
				return
			}
		}
	}
}

// recycle replaces the worker's prince process with a fresh one, discarding
// the memory the old process accumulated.
func (w *worker) recycle() error {
	w.stop()

	var err error
	for attempt := 1; attempt <= relaunchAttempts; attempt++ {
		if err = w.launch(); err == nil {
			w.greet()
			return nil
		}
		time.Sleep(time.Duration(attempt) * relaunchDelay)
	}

	return fmt.Errorf("relaunching prince after %d attempts: %w", relaunchAttempts, err)
}

func (w *worker) printStderr() {
	for {
		line, err := w.stderr.ReadString('\n')
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

func (w *worker) run(job *Job) {
	// send request to command
	req := job.request()
	data, err := json.Marshal(req)
	if err != nil {
		fmt.Printf("failed to marshal job: %s\n", err.Error())
		return
	}

	// Send to the stream
	w.write(chunkJob, data)
	for _, d := range req.resources {
		w.write(chunkData, d)
	}

	job.reply <- w.read()
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
	w.stdin.Write([]byte(msg + "\n"))

	if _, err := w.stdin.Write(data); err != nil {
		return fmt.Errorf("writing data: %w", err)
	}
	if _, err := w.stdin.Write([]byte("\n")); err != nil {
		return fmt.Errorf("writing last newline: %w", err)
	}

	return nil
}

func (w *worker) read() *output {
	o := new(output)

	// Read the first line
	var line string
	var err error
	line, err = w.stdout.ReadString('\n')
	if err != nil {
		o.err = fmt.Errorf("reading string: %w", err)
		return o
	}
	line = strings.TrimSpace(line)
	fmt.Printf("line: '%s'\n", line)

	parts := strings.Fields(line)
	if len(parts) == 0 {
		o.err = fmt.Errorf("invalid empty response")
		return o
	}

	o.msg = parts[0]

	// read data
	if len(parts) > 1 {
		l, err := strconv.Atoi(parts[1])
		if err != nil {
			o.err = fmt.Errorf("invalid length: %w", err)
			return o
		}

		o.data = make([]byte, l)
		_, err = io.ReadFull(w.stdout, o.data)
		if err != nil {
			o.err = fmt.Errorf("reading data: %w", err)
			return o
		}
		_, err = w.stdout.ReadString('\n') // read the newline at the end
		if err != nil {
			o.err = fmt.Errorf("reading newline: %w", err)
			return o
		}
	}

	switch o.msg {
	case chunkErr:
		o.err = fmt.Errorf("prince error: %s", string(o.data))
		o.data = nil
	case chunkPDF:
		// pdf messages are always followed by a 'log' message
		w.read()
	case chunkLog:
		fmt.Printf("log: %s\n", string(o.data))
	}

	return o
}

// output wraps around output provided from a job
type output struct {
	msg  string // type of message from prince
	data []byte
	err  error
}
