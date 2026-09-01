package main

import (
	"context"
	"fmt"
	"io"
	"net"
)

// bridgeUnix copies raw HTTP bytes between stdio and a Unix socket. It closes
// the socket write side at stdin EOF so runtimed can safely parse requests with
// no Content-Length, while retaining the read half for streaming responses.
func bridgeUnix(ctx context.Context, socket string, stdin io.Reader, stdout io.Writer) error {
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return fmt.Errorf("dial private socket: %w", err)
	}
	defer connection.Close()
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("private socket is not a Unix connection")
	}
	inputDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(unixConnection, stdin)
		if closeErr := unixConnection.CloseWrite(); err == nil {
			err = closeErr
		}
		inputDone <- err
	}()
	outputDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(stdout, unixConnection)
		outputDone <- err
	}()
	select {
	case <-ctx.Done():
		_ = connection.Close()
		return ctx.Err()
	case err := <-outputDone:
		_ = connection.Close()
		inputErr := <-inputDone
		if err != nil {
			return err
		}
		return inputErr
	}
}
