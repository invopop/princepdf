# princepdf

Go library and HTTP service wrapper around Prince XML that makes it easy to generate PDFs from HTML sources.

## Usage

You'll need to have [prince installed](https://www.princexml.com/doc/installing/) on your machine and available in your `$PATH` to be able to use this library. The included `Dockerfile` may be helpful for running the service inside the container using the official binary without having to install anything locally.

### Go Package

```bash
go get github.com/invopop/princepdf
```

```go
// Prepare the Client and start the binary connection
pc := princepdf.New()
if err := pc.Start(); err != nil {
    panic(err)
}

// Prepare a Job with source HTML data
j := &princepdf.Job{
    Input: &princepdf.Job{
        Src: "data.html",
    },
    Files: map[string][]byte{
        "data.html": data,
    }
}

// Run it, deal with the `out`
out, err := pc.client.Run(j)
if err != nil {
    panic(err)
}
```

### Memory and worker recycling

Prince's `--control` mode does not release all of the memory a job uses, so a
process kept alive indefinitely grows without bound — on the order of 50-80KB
retained per document, which adds up to gigabytes on a busy service. Each
worker therefore replaces its prince process after a number of jobs, 1000 by
default. Starting prince takes milliseconds, so this costs little next to
rendering a document, and it keeps memory flat over the long run.

Tune it with `WithMaxJobsPerWorker`, or pass zero to keep each process alive for
the lifetime of the client:

```go
pc := princepdf.New(
    princepdf.WithWorkerCount(4),
    princepdf.WithMaxJobsPerWorker(500),
)
```

### Timeouts and failures

`RunContext` takes a context. If it ends before the PDF is ready, the call
returns the context's error, and if prince was already rendering the job, its
process is killed and replaced, as it may be hung. `Run` waits as long as it
takes.

```go
ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
defer cancel()
out, err := pc.RunContext(ctx, j)
```

Errors tell you whose fault a failure was:

- `*princepdf.Error`: prince rejected the job, for example because the
  document couldn't be loaded. Running it again will fail the same way.
- `princepdf.ErrProcess` (check with `errors.Is`): the prince process died, was
  killed or stopped following the control protocol. The process is replaced
  straight away, and the job is retried once per worker before this error is
  returned, so it usually means the job itself crashes prince.
- the context's error, or `princepdf.ErrStopped` after `Stop`.

### Launch as Web Service

Build and run from Go:

```bash
go build ./cmd/princepdf
./princepdf -p 3000
```

Alternatively you can also use the included `Dockerfile`.

Once running, the princepdf service has a single API endpoint:

```
POST /pdf
```

Example using curl:

```bash
curl -X POST -F files=@examples/simple.html -F input='{"src":"simple.html"}' -F metadata='{"title":"Test Output","creator":"Go"}'  http://localhost:3000/pdf -v > output.pdf
```
