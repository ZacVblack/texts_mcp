package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// What termux-call-log prints: the newest calls, oldest first, local times.
const sampleCallLog = `[
  {"name": "Mom", "phone_number": "+17345550101", "type": "MISSED", "date": "2026-11-14 13:58:02", "duration": "00:00", "sim_id": "1"},
  {"name": "UNKNOWN_CALLER", "phone_number": "7345550199", "type": "INCOMING", "date": "2026-11-14 14:03:22", "duration": "01:02:05", "sim_id": "1"},
  {"name": "UNKNOWN_CALLER", "phone_number": "8005550000", "type": "BLOCKED", "date": "2026-11-14 14:05:00", "duration": "00:00", "sim_id": "1"},
  {"name": "UNKNOWN_CALLER", "phone_number": "-2", "type": "REJECTED", "date": "2026-11-14 14:06:00", "duration": "00:00", "sim_id": null},
  {"name": "Coach Nick", "phone_number": "+17345550123", "type": "OUTGOING", "date": "2026-11-14 14:10:41", "duration": "02:15", "sim_id": "1"},
  {"name": "x", "phone_number": "1", "type": "OUTGOING", "date": "not a date", "duration": "00:01", "sim_id": "1"}
]`

func newYork(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestParseCallLog(t *testing.T) {
	loc := newYork(t)
	calls, err := parseCallLog([]byte(sampleCallLog), loc)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 4 {
		t.Fatalf("want 4 calls (blocked and unreadable left out), got %d: %+v", len(calls), calls)
	}
	mom := calls[0]
	want := time.Date(2026, 11, 14, 13, 58, 2, 0, loc).UnixMilli()
	if mom.TsMs != want || mom.Direction != "missed" || mom.Name != "Mom" || mom.Number != "+17345550101" || mom.DurationSec != 0 {
		t.Fatalf("bad missed call %+v", mom)
	}
	// 13:58 in New York on Nov 14 is 18:58 UTC.
	if got := time.UnixMilli(mom.TsMs).UTC().Format("15:04"); got != "18:58" {
		t.Fatalf("time zone not applied: %s", got)
	}
	if c := calls[1]; c.Name != "" || c.Direction != "incoming" || c.DurationSec != 3725 {
		t.Fatalf("bad unknown caller %+v", c)
	}
	if c := calls[2]; c.Number != "" || c.Direction != "rejected" {
		t.Fatalf("private number should have no number: %+v", c)
	}
	if c := calls[3]; c.Direction != "outgoing" || c.DurationSec != 135 {
		t.Fatalf("bad outgoing call %+v", c)
	}
	seen := map[string]bool{}
	for _, c := range calls {
		if c.ID == "" || seen[c.ID] {
			t.Fatalf("IDs must be set and unique: %+v", calls)
		}
		seen[c.ID] = true
	}
	// The same call read again gets the same ID.
	again, _ := parseCallLog([]byte(sampleCallLog), loc)
	if again[0].ID != mom.ID {
		t.Fatal("call IDs are not stable")
	}
}

func TestParseCallLogErrors(t *testing.T) {
	loc := time.UTC
	_, err := parseCallLog([]byte(`{"error":"Please grant the following permission to use this command: android.permission.READ_CALL_LOG"}`), loc)
	if !errors.Is(err, errCallsNoPermission) {
		t.Fatalf("want no permission, got %v", err)
	}
	if _, err := parseCallLog([]byte("  \n"), loc); err == nil {
		t.Fatal("empty output should fail")
	}
	if _, err := parseCallLog([]byte("garbage"), loc); err == nil {
		t.Fatal("garbage should fail")
	}
	calls, err := parseCallLog([]byte("[]"), loc)
	if err != nil || len(calls) != 0 {
		t.Fatalf("empty call log: %v %v", calls, err)
	}
}

func TestParseCallDuration(t *testing.T) {
	for in, want := range map[string]int{"00:00": 0, "00:59": 59, "12:34": 754, "01:00:00": 3600, "": 0, "x:1": 0, "1": 0, "1:2:3:4": 0} {
		if got := parseCallDuration(in); got != want {
			t.Errorf("%q: got %d want %d", in, got, want)
		}
	}
}

func testCallWatcher(t *testing.T, out *[]byte, readErr *error, uploaded *[][]CallRecord, upErr *error) (*callWatcher, *runner) {
	t.Helper()
	t.Setenv("TEXTS_MCP_HOME", t.TempDir())
	r := &runner{log: zerolog.Nop(), hbWake: make(chan struct{}, 1)}
	w := &callWatcher{
		r:   r,
		log: zerolog.Nop(),
		read: func(context.Context, int) ([]byte, error) {
			return *out, *readErr
		},
		loc: func(context.Context) *time.Location { return time.UTC },
		up: func(_ context.Context, calls []CallRecord) error {
			if *upErr != nil {
				return *upErr
			}
			*uploaded = append(*uploaded, calls)
			return nil
		},
		now: time.Now,
	}
	return w, r
}

func TestCallWatcherUploadsOnlyNewCalls(t *testing.T) {
	out := []byte(sampleCallLog)
	var readErr, upErr error
	var uploaded [][]CallRecord
	w, r := testCallWatcher(t, &out, &readErr, &uploaded, &upErr)
	// An hour after the sample calls: the first upload marks them as old.
	w.now = func() time.Time { return time.Date(2026, 11, 14, 15, 30, 0, 0, time.UTC) }

	if wait := w.check(context.Background()); wait != callsEvery {
		t.Fatalf("wait %v", wait)
	}
	if len(uploaded) != 1 || len(uploaded[0]) != 4 {
		t.Fatalf("first check should upload 4 calls, got %+v", uploaded)
	}
	for _, c := range uploaded[0] {
		if !c.Backfill {
			t.Fatalf("first upload should be backfill: %+v", c)
		}
	}
	if st, _ := r.callsState(); st != callsStateOK {
		t.Fatalf("state %q", st)
	}
	if r.callsCheckedAt.Load() == 0 {
		t.Fatal("check time not recorded")
	}
	// Nothing new: nothing uploaded.
	w.check(context.Background())
	if len(uploaded) != 1 {
		t.Fatalf("repeat check uploaded again: %d", len(uploaded))
	}
	// A new call shows up.
	out = []byte(`[{"name":"Dad","phone_number":"+17345550102","type":"MISSED","date":"2026-11-14 15:00:00","duration":"00:00","sim_id":"1"}]`)
	w.check(context.Background())
	if len(uploaded) != 2 || len(uploaded[1]) != 1 || uploaded[1][0].Name != "Dad" || uploaded[1][0].Backfill {
		t.Fatalf("new call not uploaded as news: %+v", uploaded)
	}
	// What was sent survives a restart.
	st := loadBridgeState()
	if len(st.CallsSent) != 5 {
		t.Fatalf("saved %d sent calls", len(st.CallsSent))
	}
	r2 := &runner{log: zerolog.Nop(), hbWake: make(chan struct{}, 1), st: st}
	calls, _ := parseCallLog([]byte(sampleCallLog), time.UTC)
	if fresh, first := r2.unsentCalls(calls); len(fresh) != 0 || first {
		t.Fatalf("calls sent before a restart counted as new: %+v", fresh)
	}
}

func TestCallWatcherFailedUploadRetries(t *testing.T) {
	out := []byte(sampleCallLog)
	var readErr error
	upErr := errors.New("worker down")
	var uploaded [][]CallRecord
	w, _ := testCallWatcher(t, &out, &readErr, &uploaded, &upErr)
	if wait := w.check(context.Background()); wait != time.Minute {
		t.Fatalf("wait %v", wait)
	}
	upErr = nil
	w.check(context.Background())
	if len(uploaded) != 1 || len(uploaded[0]) != 4 {
		t.Fatalf("calls not retried after a failed upload: %+v", uploaded)
	}
}

func TestCallWatcherProblems(t *testing.T) {
	cases := []struct {
		readErr error
		out     string
		upErr   error
		state   string
		wait    time.Duration
	}{
		{readErr: errCallsNotInstalled, state: callsStateNotInstalled, wait: callsSetupRecheck},
		{readErr: errCallsNoAnswer, state: callsStateNoAnswer, wait: callsSlowRecheck},
		{out: `{"error":"Please grant the following permission to use this command: android.permission.READ_CALL_LOG"}`, state: callsStateNoPermission, wait: callsSlowRecheck},
		{out: sampleCallLog, upErr: errCallsUnsupported, state: callsStateUnsupported, wait: callsSlowRecheck},
		{readErr: errors.New("boom"), state: callsStateError, wait: callsSetupRecheck},
	}
	for _, c := range cases {
		out := []byte(c.out)
		readErr, upErr := c.readErr, c.upErr
		var uploaded [][]CallRecord
		w, r := testCallWatcher(t, &out, &readErr, &uploaded, &upErr)
		wait := w.check(context.Background())
		st, detail := r.callsState()
		if st != c.state || wait != c.wait || detail == "" {
			t.Errorf("%v %q: state %q detail %q wait %v", c.readErr, c.out, st, detail, wait)
		}
		select {
		case <-r.hbWake:
		default:
			t.Errorf("%q: a state change should send a heartbeat soon", c.state)
		}
	}
}

func TestSentCallsAreCapped(t *testing.T) {
	t.Setenv("TEXTS_MCP_HOME", t.TempDir())
	r := &runner{log: zerolog.Nop(), hbWake: make(chan struct{}, 1)}
	var calls []CallRecord
	for i := 0; i < callsSentKept+50; i++ {
		calls = append(calls, CallRecord{ID: "c" + string(rune('a'+i%26)) + time.Duration(i).String()})
	}
	r.markCallsSent(calls)
	if len(r.st.CallsSent) != callsSentKept || r.st.CallsSent[callsSentKept-1] != calls[len(calls)-1].ID {
		t.Fatalf("kept %d, last %q", len(r.st.CallsSent), r.st.CallsSent[len(r.st.CallsSent)-1])
	}
}

// A termux-call-log that never answers (the Termux:API app missing) is
// stopped at the timeout, with whatever it started.
func TestReadTermuxCallLogTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "termux-call-log")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 600 &\nwait\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err := readTermuxCallLog(ctx, 5)
	if err == nil {
		t.Fatal("want an error")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
}

func TestReadTermuxCallLogRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "termux-call-log")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n[ \"$1\" = -l ] && [ \"$2\" = 7 ] || exit 3\necho '[]'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := readTermuxCallLog(context.Background(), 7)
	if err != nil || string(out) != "[]\n" {
		t.Fatalf("%q %v", out, err)
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("PREFIX", t.TempDir())
	if _, err := readTermuxCallLog(context.Background(), 7); !errors.Is(err, errCallsNotInstalled) {
		t.Fatalf("want not installed, got %v", err)
	}
}
