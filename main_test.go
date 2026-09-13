package main

import (
	"context"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParseRsyncProgressTokens(t *testing.T) {
	current := &Sync{}
	tokens := []string{"1,234", "42%", "10.00kB/s", "0:00:12", "(xfr#0,", "to-chk=24/25)"}

	for _, token := range tokens {
		if err := parseRsyncProgressToken(current, token); err != nil {
			t.Fatalf("parseRsyncProgressToken(%q): %v", token, err)
		}
	}

	if current.Downloaded != 1234 {
		t.Errorf("Downloaded = %d, want 1234", current.Downloaded)
	}
	if current.Progress != 42 {
		t.Errorf("Progress = %d, want 42", current.Progress)
	}
	if current.Speed != 10000 {
		t.Errorf("Speed = %d, want 10000", current.Speed)
	}
	if current.TimeLeft != "0:00:12" {
		t.Errorf("TimeLeft = %q, want %q", current.TimeLeft, "0:00:12")
	}
}

func TestParseRsyncProgressTokenIgnoresStatusFields(t *testing.T) {
	current := &Sync{
		Downloaded: 1234,
		Progress:   42,
		Speed:      10000,
		TimeLeft:   "0:00:12",
	}
	want := *current

	for _, token := range []string{"(xfr#0,", "to-chk=24/25)", "ir-chk=3/25)", "receiving", "file.txt"} {
		if err := parseRsyncProgressToken(current, token); err != nil {
			t.Fatalf("parseRsyncProgressToken(%q): %v", token, err)
		}
	}

	if current.Downloaded != want.Downloaded || current.Progress != want.Progress || current.Speed != want.Speed || current.TimeLeft != want.TimeLeft {
		t.Fatalf("status fields changed sync progress: got %+v, want %+v", current, &want)
	}
}

func TestParseRsyncProgressTokenRejectsMalformedValues(t *testing.T) {
	for _, token := range []string{"nope%", "fast/s"} {
		if err := parseRsyncProgressToken(&Sync{}, token); err == nil {
			t.Errorf("parseRsyncProgressToken(%q) returned nil error", token)
		}
	}
}

func TestPathsOverlap(t *testing.T) {
	tests := []struct {
		left, right string
		want        bool
	}{
		{"/series/House", "/series/House", true},
		{"/series/House", "/series/House/Season 1", true},
		{"/series/House/Season 1", "/series/House", true},
		{"/series/House", "/series/Household", false},
		{"/series/House/Season 1", "/series/Other", false},
	}

	for _, test := range tests {
		if got := pathsOverlap(test.left, test.right); got != test.want {
			t.Errorf("pathsOverlap(%q, %q) = %t, want %t", test.left, test.right, got, test.want)
		}
	}
}

func TestSyncStorageRemovalReservationConflicts(t *testing.T) {
	storage := &syncStorage{
		Data:     map[string]*Sync{},
		Removing: map[string]struct{}{`/series/House`: {}},
	}

	storage.Lock()
	defer storage.Unlock()
	if !storage.hasConflictLocked("/series/House/Season 6") {
		t.Error("child sync did not conflict with removal reservation")
	}
	if storage.hasConflictLocked("/series/Other") {
		t.Error("unrelated sync conflicted with removal reservation")
	}
}

func TestRemoteRequestContextStopsOnShutdown(t *testing.T) {
	shutdownContext, stop := context.WithCancel(context.Background())
	ctx, cancel := remoteRequestContext(context.Background(), shutdownContext)
	defer cancel()

	stop()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("request context was not cancelled on shutdown")
	}
}

func TestSignalProcessGroupWithoutStartedProcess(t *testing.T) {
	if err := signalProcessGroup(nilCommand(), syscall.SIGTERM); err == nil {
		t.Fatal("signalProcessGroup returned nil for an unstarted process")
	}
}

func nilCommand() *exec.Cmd {
	return &exec.Cmd{}
}

func TestRsyncProgressWriterHandlesSplitTokens(t *testing.T) {
	current := &Sync{}
	storage := &syncStorage{Data: map[string]*Sync{}}
	writer := &rsyncProgressWriter{
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		runningSyncs: storage,
		currentSync:  current,
	}

	for _, chunk := range []string{"1,2", "34 4", "2% 10.00", "kB/s 0:00:12\n"} {
		if _, err := writer.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write(%q): %v", chunk, err)
		}
	}

	if current.Downloaded != 1234 || current.Progress != 42 || current.Speed != 10000 || current.TimeLeft != "0:00:12" {
		t.Fatalf("unexpected parsed progress: %+v", current)
	}
}

func TestScanAndLogPipeDrainsOversizedToken(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	input := strings.NewReader(strings.Repeat("x", maxScannerTokenSize+1) + "\ntrailing\n")
	done := make(chan struct{})
	go func() {
		scanAndLogPipe(logger, "test", input)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scanAndLogPipe did not drain after scanner token error")
	}
}
