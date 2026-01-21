package main

import (
	"fmt"
	"log"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	switch os.Args[1] {
	case "client":
		if err := runClient(os.Args[2:]); err != nil {
			fatalf("client: %v", err)
		}
	case "server":
		if err := runServer(os.Args[2:]); err != nil {
			fatalf("server: %v", err)
		}
	default:
		printUsage()
	}
}

func printUsage() {
	fmt.Println("Usage: tcp2http [client|server] [flags]")
}
