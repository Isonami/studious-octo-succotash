package main

import "testing"

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
