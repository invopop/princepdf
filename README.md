# princepdf

Go library and HTTP service wrapper around Prince XML that makes it easy to generate PDFs from HTML sources.

## Usage

Once running, the princepdf service has a single API endpoint:

```
POST /pdf
```

Example using curl:

```bash
curl -X POST -F files=@examples/simple.html -F input='{"src":"simple.html"}' -F metadata='{"title":"Test Output","creator":"Go"}'  http://localhost:3000/pdf -v > output.pdf
```

### Sending Requests

## Development

You'll need to have [prince installed](https://www.princexml.com/doc/installing/) on your development machine to and available to be able to develop and test.
