package princepdf

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
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
	cmdPrince          = "prince"
	workerCountDefault = 1
)

var (
	cmdPrinceOpts = []string{"--control"}
)

// Client provides a client interface to be able to stream commands to the prince
// controller.
type Client struct {
	in          chan *Job
	workerCount int
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

// New instantiates a new PrincePDF client
func New(opts ...Option) *Client {
	c := &Client{
		in:          make(chan *Job),
		workerCount: workerCountDefault,
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
		c.workers[i], err = newWorker(c.in)
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
		w.stop()
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
}

func newWorker(in chan *Job) (*worker, error) {
	w := &worker{
		cmd: exec.Command(cmdPrince, cmdPrinceOpts...),
		in:  in,
	}
	stderr, err := w.cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("preparing stderr: %w", err)
	}
	w.stderr = bufio.NewReader(stderr)
	stdout, err := w.cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("preparing stdout: %w", err)
	}
	w.stdout = bufio.NewReader(stdout)
	if w.stdin, err = w.cmd.StdinPipe(); err != nil {
		return nil, fmt.Errorf("preparing stdin: %w", err)
	}
	if err := w.cmd.Start(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *worker) start() {
	// first grab the version information which is sent automatically by prince
	out := w.read()
	fmt.Printf("started version: '%s'\n", string(out.data))
	go w.printStderr()
	for job := range w.in {
		w.run(job)
	}
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

func (w *worker) stop() {
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
		w.stdin.Write([]byte(msg + "\n"))
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
