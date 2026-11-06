package princepdf

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJobRequest(t *testing.T) {
	newJob := func() *Job {
		return &Job{
			Input: &Input{Src: "doc.html", Styles: []string{"b.css", "https://x.test/a.css"}, Scripts: []string{"c.js"}},
			PDF:   &PDF{Attach: []*Attachment{{Filename: "d.xml"}, {URL: "https://x.test/e.xml"}}},
			Files: map[string][]byte{
				"doc.html": []byte("html"),
				"b.css":    []byte("css"),
				"c.js":     []byte("js"),
				"d.xml":    []byte("xml"),
			},
		}
	}
	j := newJob()

	req := j.request()
	// Files are numbered in name order: b.css, c.js, d.xml, doc.html.
	assert.Equal(t, 4, req.JobResourceCount)
	assert.Equal(t, [][]byte{[]byte("css"), []byte("js"), []byte("xml"), []byte("html")}, req.resources)
	assert.Equal(t, "job-resource:3", req.Input.Src)
	assert.Equal(t, []string{"job-resource:0", "https://x.test/a.css"}, req.Input.Styles)
	assert.Equal(t, []string{"job-resource:1"}, req.Input.Scripts)
	require.Len(t, req.PDF.Attach, 2)
	assert.Equal(t, "job-resource:2", req.PDF.Attach[0].URL)
	assert.Equal(t, "https://x.test/e.xml", req.PDF.Attach[1].URL)

	// The job is untouched, so running it again gives the same request.
	assert.Equal(t, newJob(), j)
	assert.Equal(t, req, j.request())
}
