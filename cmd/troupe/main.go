// Command troupe manages a troupe of Hermes agents, each in its own Docker Sandbox.
package main

import (
	"os"

	"github.com/starknet-innovation/troupe/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
