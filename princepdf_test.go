package princepdf_test

import (
	"os"
	"testing"

	"github.com/invopop/princepdf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartup(t *testing.T) {
	pc := princepdf.New()
	require.NoError(t, pc.Start())
	defer func() {
		assert.NoError(t, pc.Stop())
	}()

	t.Run("test with URL", func(t *testing.T) {
		j := new(princepdf.Job)
		j.Input = &princepdf.Input{
			Src: "https://www.princexml.com/samples/invoice/invoicesample.html",
		}
		out, err := pc.Run(j)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile("output1.pdf", out, 0644))
	})

	t.Run("embedded data", func(t *testing.T) {
		// test with embedded data
		j := new(princepdf.Job)
		j.Input = &princepdf.Input{
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
