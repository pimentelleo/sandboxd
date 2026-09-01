// Command runtimed-tunnel is the only Kubernetes exec bridge to runtimed's
// private Unix socket and workspace files. It never opens a TCP listener.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if len(os.Args) < 2 {
		fail(fmt.Errorf("usage: runtimed-tunnel <stdio|file>"))
	}
	switch os.Args[1] {
	case "stdio":
		socket, err := requiredFlag(os.Args[2:], "--socket")
		if err != nil {
			fail(err)
		}
		if err := bridgeUnix(ctx, socket, os.Stdin, os.Stdout); err != nil {
			fail(err)
		}
	case "file":
		root, err := requiredFlag(os.Args[2:], "--root")
		if err != nil {
			fail(err)
		}
		if err := serveFileRequest(ctx, root, os.Stdin, os.Stdout); err != nil {
			fail(err)
		}
	default:
		fail(fmt.Errorf("unknown subcommand %q", os.Args[1]))
	}
}

func requiredFlag(args []string, name string) (string, error) {
	if len(args) != 2 || args[0] != name || args[1] == "" {
		return "", fmt.Errorf("%s requires %s PATH", os.Args[1], name)
	}
	return args[1], nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "runtimed-tunnel:", err)
	os.Exit(1)
}
