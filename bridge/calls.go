package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // Android's zone files aren't always where Go looks.

	"github.com/rs/zerolog"
)

// Phone calls. On Android, Termux:API's termux-call-log command reads the
// phone's call log. The bridge reads the newest calls every few minutes and
// uploads the ones the Worker doesn't have yet, so Claude can see missed and
// recent calls. Texts keep working whether or not this does.

const (
	callsEvery        = 3 * time.Minute
	callsFirstCheck   = 20 * time.Second
	callsReadLimit    = 50
	callsSentKept     = 400
	callsCmdTimeout   = 30 * time.Second
	callsSlowRecheck  = 30 * time.Minute
	callsSetupRecheck = 10 * time.Minute
)

// Call log states reported to the Worker, and from there to Claude.
const (
	callsStateOK           = "ok"
	callsStateNotInstalled = "not_installed" // no termux-call-log command
	callsStateNoAnswer     = "no_answer"     // the Termux:API app didn't answer
	callsStateNoPermission = "no_permission" // call log permission not granted
	callsStateError        = "error"
	callsStateUnsupported  = "worker_too_old"
)

var (
	errCallsNotInstalled = errors.New("termux-call-log is not installed")
	errCallsNoAnswer     = errors.New("the Termux:API app did not answer")
	errCallsNoPermission = errors.New("Termux:API has no permission to read the call log")
	errCallsUnsupported  = errors.New("the Worker does not take calls yet")
)

// CallRecord is one call from the phone's call log.
type CallRecord struct {
	ID          string `json:"id"`
	TsMs        int64  `json:"ts"`
	Number      string `json:"number,omitempty"`
	Name        string `json:"name,omitempty"`
	Direction   string `json:"direction"`
	DurationSec int    `json:"duration_sec"`
	// Backfill marks older calls from the very first upload, so they
	// don't show up in Claude's next summary as if they just happened.
	Backfill bool `json:"backfill,omitempty"`
}

// termuxCall is one entry as termux-call-log prints it.
type termuxCall struct {
	Name        string `json:"name"`
	PhoneNumber string `json:"phone_number"`
	Type        string `json:"type"`
	Date        string `json:"date"`
	Duration    string `json:"duration"`
}

// parseCallLog turns termux-call-log's output into call records. Blocked
// calls are left out, like spam and blocked texts. Times are in loc, the
// phone's time zone, because that is how Termux:API prints them.
func parseCallLog(out []byte, loc *time.Location) ([]CallRecord, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, errors.New("termux-call-log printed nothing")
	}
	if trimmed[0] == '{' {
		var e struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(trimmed, &e); err == nil && e.Error != "" {
			if strings.Contains(strings.ToLower(e.Error), "permission") {
				return nil, fmt.Errorf("%w: %s", errCallsNoPermission, e.Error)
			}
			return nil, errors.New(e.Error)
		}
	}
	var raw []termuxCall
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return nil, fmt.Errorf("termux-call-log output was not a list: %w", err)
	}
	var calls []CallRecord
	for _, c := range raw {
		if strings.EqualFold(c.Type, "BLOCKED") {
			continue
		}
		t, err := time.ParseInLocation("2006-01-02 15:04:05", strings.TrimSpace(c.Date), loc)
		if err != nil {
			continue
		}
		number := strings.TrimSpace(c.PhoneNumber)
		// Android uses empty or negative numbers for private and unknown callers.
		if strings.HasPrefix(number, "-") {
			number = ""
		}
		name := strings.TrimSpace(c.Name)
		if name == "UNKNOWN_CALLER" {
			name = ""
		}
		rec := CallRecord{
			TsMs:        t.UnixMilli(),
			Number:      number,
			Name:        name,
			Direction:   callDirection(c.Type),
			DurationSec: parseCallDuration(c.Duration),
		}
		rec.ID = callID(rec)
		calls = append(calls, rec)
	}
	return calls, nil
}

func callDirection(t string) string {
	switch strings.ToUpper(strings.TrimSpace(t)) {
	case "INCOMING":
		return "incoming"
	case "OUTGOING":
		return "outgoing"
	case "MISSED":
		return "missed"
	case "REJECTED":
		return "rejected"
	case "VOICEMAIL":
		return "voicemail"
	}
	return "unknown"
}

// parseCallDuration reads "MM:SS" or "HH:MM:SS" as seconds.
func parseCallDuration(s string) int {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0
	}
	total := 0
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0
		}
		total = total*60 + n
	}
	return total
}

// callID identifies a call by when it started and who was on the other end.
// The call log has no ID of its own that termux-call-log prints.
func callID(c CallRecord) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s", c.TsMs, onlyDigits(c.Number), c.Direction)))
	return "call-" + hex.EncodeToString(sum[:10])
}

func onlyDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// phoneLocation returns the phone's time zone. Go on Android can't see it by
// itself, so it asks Android when TZ isn't set.
func phoneLocation(ctx context.Context) *time.Location {
	if tz := os.Getenv("TZ"); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	if path, err := exec.LookPath("getprop"); err == nil {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		out, err := exec.CommandContext(cctx, path, "persist.sys.timezone").Output()
		cancel()
		if err == nil {
			if loc, err := time.LoadLocation(strings.TrimSpace(string(out))); err == nil {
				return loc
			}
		}
	}
	return time.Local
}

// callLogCommand finds termux-call-log.
func callLogCommand() (string, error) {
	if path, err := exec.LookPath("termux-call-log"); err == nil {
		return path, nil
	}
	if prefix := os.Getenv("PREFIX"); prefix != "" {
		path := filepath.Join(prefix, "bin", "termux-call-log")
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			return path, nil
		}
	}
	return "", errCallsNotInstalled
}

// readTermuxCallLog runs termux-call-log. It is slow (it asks the Termux:API
// app) and hangs when that app isn't installed, so it gets a timeout and the
// whole process group is stopped when it runs out.
func readTermuxCallLog(ctx context.Context, limit int) ([]byte, error) {
	path, err := callLogCommand()
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, callsCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, path, "-l", strconv.Itoa(limit))
	setProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if cctx.Err() == context.DeadlineExceeded {
		return nil, errCallsNoAnswer
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		if len(msg) > 200 {
			msg = msg[:200]
		}
		if msg != "" {
			return nil, fmt.Errorf("termux-call-log failed: %v: %s", err, msg)
		}
		return nil, fmt.Errorf("termux-call-log failed: %w", err)
	}
	return out, nil
}

// callWatcher reads the call log and uploads new calls.
type callWatcher struct {
	r    *runner
	log  zerolog.Logger
	read func(ctx context.Context, limit int) ([]byte, error)
	loc  func(ctx context.Context) *time.Location
	up   func(ctx context.Context, calls []CallRecord) error
	now  func() time.Time
}

func newCallWatcher(r *runner) *callWatcher {
	return &callWatcher{
		r:    r,
		log:  r.log.With().Str("component", "calls").Logger(),
		read: readTermuxCallLog,
		loc:  phoneLocation,
		up:   r.wc.UploadCalls,
		now:  time.Now,
	}
}

// check reads the call log once, uploads new calls and says when to look again.
func (w *callWatcher) check(ctx context.Context) time.Duration {
	r := w.r
	out, err := w.read(ctx, callsReadLimit)
	var calls []CallRecord
	if err == nil {
		calls, err = parseCallLog(out, w.loc(ctx))
	}
	r.callsCheckedAt.Store(w.now().UnixMilli())
	if err != nil {
		state, detail, wait := callsProblem(err)
		if prev, _ := r.callsState(); prev != state {
			w.log.Warn().Err(err).Msg("Can't read phone calls; texts are not affected")
		}
		r.setCallsState(state, detail)
		return wait
	}
	fresh, first := r.unsentCalls(calls)
	if first {
		hourAgo := w.now().Add(-time.Hour).UnixMilli()
		for i := range fresh {
			fresh[i].Backfill = fresh[i].TsMs < hourAgo
		}
	}
	if len(fresh) > 0 {
		uctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		err := w.up(uctx, fresh)
		cancel()
		if err != nil {
			if errors.Is(err, errCallsUnsupported) {
				r.setCallsState(callsStateUnsupported, "The Worker is an older version without calls. Redeploy it from GitHub.")
				return callsSlowRecheck
			}
			w.log.Warn().Err(err).Msg("Uploading calls failed; will retry")
			return time.Minute
		}
		r.markCallsSent(fresh)
		w.log.Debug().Int("calls", len(fresh)).Msg("Uploaded calls")
	}
	if prev, _ := r.callsState(); prev != callsStateOK {
		w.log.Info().Msg("Reading phone calls")
	}
	r.setCallsState(callsStateOK, "")
	return callsEvery
}

// callsProblem turns a failure into a state, a line for Claude, and a wait.
func callsProblem(err error) (state, detail string, wait time.Duration) {
	switch {
	case errors.Is(err, errCallsNotInstalled):
		return callsStateNotInstalled,
			"Calls are off: the termux-api package isn't installed in Termux. Run the texts-bridge installer again.",
			callsSetupRecheck
	case errors.Is(err, errCallsNoAnswer):
		return callsStateNoAnswer,
			"Calls are off: the Termux:API app didn't answer. Install Termux:API from the same place as Termux (F-Droid or GitHub), then run termux-call-log -l 1 in Termux once.",
			callsSlowRecheck
	case errors.Is(err, errCallsNoPermission):
		return callsStateNoPermission,
			"Calls are off: Termux:API isn't allowed to read the call log. Run termux-call-log -l 1 in Termux and tap Allow, or allow Call logs in Settings > Apps > Termux:API > Permissions.",
			callsSlowRecheck
	}
	return callsStateError, "Calls: " + shortErr(err), callsSetupRecheck
}

// callLoop runs for as long as the bridge does, separately from the Google
// Messages connection.
func (r *runner) callLoop(ctx context.Context) {
	w := newCallWatcher(r)
	wait := callsFirstCheck
	for sleepCtx(ctx, wait) {
		wait = w.check(ctx)
	}
}

func (r *runner) callsState() (string, string) {
	r.callsMu.Lock()
	defer r.callsMu.Unlock()
	return r.callsSt, r.callsDetail
}

func (r *runner) setCallsState(state, detail string) {
	r.callsMu.Lock()
	changed := r.callsSt != state || r.callsDetail != detail
	r.callsSt, r.callsDetail = state, detail
	r.callsMu.Unlock()
	if changed {
		select {
		case r.hbWake <- struct{}{}:
		default:
		}
	}
}

// unsentCalls returns the calls not uploaded yet, oldest first, and whether
// no call was ever uploaded before.
func (r *runner) unsentCalls(calls []CallRecord) ([]CallRecord, bool) {
	r.stMu.Lock()
	defer r.stMu.Unlock()
	sent := make(map[string]bool, len(r.st.CallsSent))
	for _, id := range r.st.CallsSent {
		sent[id] = true
	}
	var out []CallRecord
	seen := map[string]bool{}
	for _, c := range calls {
		if sent[c.ID] || seen[c.ID] {
			continue
		}
		seen[c.ID] = true
		out = append(out, c)
	}
	return out, len(r.st.CallsSent) == 0
}

// markCallsSent remembers uploaded calls, keeping the newest callsSentKept.
func (r *runner) markCallsSent(calls []CallRecord) {
	r.stMu.Lock()
	for _, c := range calls {
		r.st.CallsSent = append(r.st.CallsSent, c.ID)
	}
	if n := len(r.st.CallsSent); n > callsSentKept {
		r.st.CallsSent = append([]string(nil), r.st.CallsSent[n-callsSentKept:]...)
	}
	st := r.st
	r.stMu.Unlock()
	saveBridgeState(st)
}

// UploadCalls sends calls to the Worker.
func (w *workerClient) UploadCalls(ctx context.Context, calls []CallRecord) error {
	code, err := w.do(ctx, http.MethodPost, "/bridge/calls", map[string]any{"calls": calls}, nil)
	if code == http.StatusNotFound {
		return errCallsUnsupported
	}
	return err
}
