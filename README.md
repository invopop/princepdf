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
