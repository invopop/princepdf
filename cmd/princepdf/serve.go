package main

import (
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/invopop/princepdf"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/spf13/cobra"
)

type serveOpts struct {
	*rootOpts
	port string

	pc   *princepdf.Client
	echo *echo.Echo
}

func serve(o *rootOpts) *serveOpts {
	return &serveOpts{rootOpts: o}
}

func (s *serveOpts) cmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the server to respond to HTTP requests",
		RunE:  s.runE,
	}

	f := cmd.Flags()
	f.StringVarP(&s.port, "port", "p", "3000", "port to listen on")

	return cmd
}

func (s *serveOpts) runE(cmd *cobra.Command, args []string) error {
	fmt.Printf("starting...\n")

	s.pc = princepdf.New()
	if err := s.pc.Start(); err != nil {
		return fmt.Errorf("starting princepdf: %w", err)
	}

	s.startWeb()

	waitForTerm()

	if err := s.pc.Stop(); err != nil {
		fmt.Printf("failed to stop: %v\n", err.Error())
	}

	return nil
}

func (s *serveOpts) startWeb() {
	e := echo.New()
	e.Pre(middleware.RemoveTrailingSlash())
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	s.echo = e

	e.POST("/pdf", s.handlePDF)

	go func() {
		if err := e.Start(":" + s.port); err != nil {
			fmt.Printf("failed to start: %v\n", err.Error())
		}
	}()
}

func (s *serveOpts) handlePDF(c echo.Context) error {
	job := princepdf.NewJob()

	ctyp := c.Request().Header.Get(echo.HeaderContentType)
	if strings.HasPrefix(ctyp, echo.MIMEMultipartForm) {
		// Try to extract multipart content
		form, err := c.MultipartForm()
		if err != nil {
			return fmt.Errorf("parsing form: %w", err)
		}

		if err := unmarshalJSONFormValue(form, "input", &job.Input); err != nil {
			return fmt.Errorf("parsing form input: %w", err)
		}
		if err := unmarshalJSONFormValue(form, "pdf", &job.PDF); err != nil {
			return fmt.Errorf("parsing form pdf: %w", err)
		}
		if err := unmarshalJSONFormValue(form, "metadata", &job.Metadata); err != nil {
			return fmt.Errorf("parsing form metadata: %w", err)
		}

		for _, file := range form.File["files"] {
			data, err := readFileContents(file)
			if err != nil {
				return fmt.Errorf("reading file: %w", err)
			}
			job.Files[file.Filename] = data
		}
	} else {
		// Assume regular JSON request
		if err := c.Bind(job); err != nil {
			return fmt.Errorf("binding data: %w", err)
		}
	}

	out, _ := json.Marshal(job)
	fmt.Printf("req: %v\n", string(out))

	data, err := s.pc.Run(job)
	if err != nil {
		return fmt.Errorf("running job: %w", err)
	}

	return c.Blob(200, "application/pdf", data)
}

func unmarshalJSONFormValue(form *multipart.Form, key string, v any) error {
	if vals, ok := form.Value[key]; ok {
		return json.Unmarshal([]byte(vals[0]), v)
	}
	return nil
}

func readFileContents(f *multipart.FileHeader) ([]byte, error) {
	src, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer src.Close() //nolint:errcheck

	return io.ReadAll(src)
}

func waitForTerm() {
	fmt.Printf("started, waiting for connections\n")
	quit := make(chan os.Signal, 2)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	close(quit)
}
