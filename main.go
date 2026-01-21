package main

import (
	"fmt"
	"log"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		printRootUsage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "client":
		if err := runClient(os.Args[2:]); err != nil {
			log.Fatalf("client failed: %v", err)
		}
	case "server":
		if err := runServer(os.Args[2:]); err != nil {
			log.Fatalf("server failed: %v", err)
		}
	case "-h", "--help", "help":
		printRootUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		printRootUsage()
		os.Exit(2)
	}
}

func printRootUsage() {
	fmt.Fprintf(os.Stderr, `tcp2http - tunnel TCP over HTTP (up/down)

Usage:
  tcp2http client [flags]
  tcp2http server [flags]

Run "tcp2http <command> -h" for command-specific flags.

`)
}
