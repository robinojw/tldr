package main

import (
	"fmt"
	"os"

	"github.com/robinojw/tldr/internal/cli"
)

func main() {
	root := cli.NewRootCmd()
	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
