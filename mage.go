//go:build mage
// +build mage

package main

import (
	"os"

	// Load environment variables from .env file
	_ "github.com/joho/godotenv/autoload"

	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"
	"github.com/magefile/mage/target"
)

const (
	name     = "princepdf"
	runImage = "yeslogic/prince"
)

func Build() error {
	changed, err := target.Dir("./"+name, ".")
	if os.IsNotExist(err) || (err == nil && changed) {
		args := []string{
			"GOOS=linux",
			"GOARCH=amd64",
			"CGO_ENABLED=0",
			"go", "build", "./cmd/" + name,
		}
		return sh.RunV("env", args...)
	}
	return nil
}

// Serve begins the web service
func Serve() error {
	mg.Deps(Build)
	return dockerRunCmd(name, "80", "./"+name, "serve", "-p", "80")
}

func dockerRunCmd(name, publicPort string, cmd ...string) error {
	args := []string{
		"run",
		"--rm",
		"--name", name,
		"--network", "invopop-local",
		"-v", "$PWD:/src",
		"-w", "/src",
		"-it", // interactive
		"--entrypoint", "",
	}
	if publicPort != "" {
		args = append(args,
			"--label", "traefik.enable=true",
			"--label", "traefik.http.routers."+name+".rule=Host(`"+name+".invopop.dev`)",
			"--label", "traefik.http.routers."+name+".tls=true",
			"--expose", publicPort,
		)
	}
	args = append(args, runImage)
	args = append(args, cmd...)
	return sh.RunV("docker", args...)
}

// Shell runs an interactive shell within a docker container.
func Shell() error {
	return dockerRunCmd(name+"-shell", "", "bash")
}
