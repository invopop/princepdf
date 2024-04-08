# princepdf

Go library and HTTP service wrapper around Prince XML that makes it easy to generate PDFs from HTML sources.

## Usage

You'll need to have [prince installed](https://www.princexml.com/doc/installing/) on your development machine to and available to be able to develop and test. The included `Dockerfile` may be helpful for running the service.

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
