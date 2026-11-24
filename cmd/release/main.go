package main

import (
	"fmt"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printUsage()
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		printUsage()
		return 0
	case "build":
		if err := runBuild(args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "release build: %v\n", err)
			return 1
		}
		return 0
	case "check":
		if err := runCheck(args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "release check: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "release: unknown subcommand %q\n\n", args[0])
		printUsage()
		return 2
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `Usage:
  go run ./cmd/release build --version VERSION --out DIR [--target OS/ARCH]... [--no-cgo] [--go BIN]
  go run ./cmd/release check --version VERSION ARCHIVE.tar.gz`)
}
