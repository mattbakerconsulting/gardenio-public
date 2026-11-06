package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
)

func TestIsClientDisconnect(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("nope"), false},
		{"EPIPE direct", syscall.EPIPE, true},
		{"EPIPE wrapped", fmt.Errorf("write tcp: %w", syscall.EPIPE), true},
		{"ECONNRESET wrapped", fmt.Errorf("read tcp: %w", syscall.ECONNRESET), true},
		{"net.ErrClosed", net.ErrClosed, true},
		{"net.ErrClosed wrapped", fmt.Errorf("listener: %w", net.ErrClosed), true},
		{"context.Canceled", context.Canceled, true},
		{"context.DeadlineExceeded", context.DeadlineExceeded, true},
		{
			"OpError EPIPE",
			&net.OpError{Op: "write", Err: syscall.EPIPE},
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isClientDisconnect(tc.err)
			if got != tc.want {
				t.Fatalf("isClientDisconnect(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
