package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// levelOf returns the level recorded on the single log line in buf.
func levelOf(t *testing.T, buf *bytes.Buffer) string {
	t.Helper()
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("nothing was logged")
	}
	if strings.Count(line, "\n") > 0 {
		t.Fatalf("expected exactly one log line, got:\n%s", line)
	}
	for _, field := range strings.Fields(line) {
		if lvl, ok := strings.CutPrefix(field, "level="); ok {
			return lvl
		}
	}
	t.Fatalf("no level in log line: %s", line)
	return ""
}

// TestLogFailureDemotesDisconnect pins the behaviour monitoring depends on: a
// caller that simply went away is debug, anything else stays error.
func TestLogFailureDemotesDisconnect(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"disconnect", fmt.Errorf("fetch inbox: %w", context.Canceled), "DEBUG"},
		{"real failure", errors.New("connection refused"), "ERROR"},
		{"timeout", context.DeadlineExceeded, "ERROR"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			a := New(nil, nil, nil, 0,
				slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			a.logFailure("fetch failed", c.err)
			if got := levelOf(t, &buf); got != c.want {
				t.Fatalf("level = %s, want %s", got, c.want)
			}
		})
	}
}

// TestLogFailureDisconnectStaysSilentAtInfo confirms the demoted line really
// disappears at the default level, which is the point of the change.
func TestLogFailureDisconnectStaysSilentAtInfo(t *testing.T) {
	var buf bytes.Buffer
	a := New(nil, nil, nil, 0,
		slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	a.logFailure("fetch failed", context.Canceled)
	if buf.Len() != 0 {
		t.Fatalf("disconnect must not be logged at info level, got: %s", buf.String())
	}
}
