package main

import (
	"os"

	"github.com/joelhelbling/coucou/internal/cli"
)

func main() {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr, cwd))
}
