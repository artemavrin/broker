package core

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestDisconnected(t *testing.T) {
	// The cancellation reaches us wrapped by the db layer, so matching must
	// unwrap rather than compare.
	if !Disconnected(fmt.Errorf("fetch inbox: %w", context.Canceled)) {
		t.Fatal("wrapped context.Canceled must count as a disconnect")
	}
	if !Disconnected(context.Canceled) {
		t.Fatal("bare context.Canceled must count as a disconnect")
	}
	// A deadline that expired is a real timeout: it must stay loud.
	if Disconnected(context.DeadlineExceeded) {
		t.Fatal("context.DeadlineExceeded must not count as a disconnect")
	}
	if Disconnected(errors.New("connection refused")) {
		t.Fatal("an ordinary failure must not count as a disconnect")
	}
	if Disconnected(nil) {
		t.Fatal("nil must not count as a disconnect")
	}
}
