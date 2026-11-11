package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// Link states reported to the Worker, and from there to Claude.
const (
	stateStarting     = "starting"
	stateConnecting   = "connecting"
	stateConnected    = "connected"
	stateReconnecting = "reconnecting"
	stateOtherBrowser = "other_browser"
	stateNeedsPairing = "needs_pairing"
	stateError        = "error"
)

const (
	firstSyncDays      = 7
	syncEvery          = 20 * time.Minute
	heartbeatEvery     = 5 * time.Minute
	pairingCheckEvery  = 2 * time.Minute
	sessionSaveEvery   = 3 * time.Hour
	syncConversations  = 75
	syncPageSize       = 50
	syncMaxPages       = 8
	watermarkOverlapMs = 2 * 60 * 1000
)

var (
	errNeedsPairing = errors.New("Google Messages unlinked this device")
	errNewPairing   = errors.New("a newer pairing was saved from the computer")
	errReconnect    = errors.New("the connection to Google Messages stalled")
)

// runner is the long-lived service. It survives reconnects and re-pairing.
type runner struct {
	cfg    *Config
	wc     *workerClient
	store  *sessionStore
	queue  *uploadQueue
	log    zerolog.Logger
	libLog zerolog.Logger

	stMu sync.Mutex
	st   bridgeState

	stateMu     sync.Mutex
	state       string
	stateDetail string

	phoneResponding atomic.Bool
	batteryLow      atomic.Bool
	lastTextAt      atomic.Int64
	lastSyncAt      atomic.Int64
	hbWake          chan struct{}
}

func cmdRun(ctx context.Context, path string, log zerolog.Logger, debug bool) error {
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	wc := newWorkerClient(cfg)
	libLog := log.With().Str("component", "libgm").Logger()
	if !debug {
		libLog = libLog.Level(zerolog.WarnLevel)
	}
	r := &runner{
		cfg:    cfg,
		wc:     wc,
		store:  newSessionStore(cfg, wc, log),
		queue:  newUploadQueue(wc, log),
		log:    log,
		libLog: libLog,
		st:     loadBridgeState(),
		state:  stateStarting,
		hbWake: make(chan struct{}, 1),
	}
	r.phoneResponding.Store(true)
	log.Info().Str("version", Version).Str("worker", cfg.WorkerURL).Msg("texts-bridge starting")

	go r.queue.Run(ctx)
	go r.heartbeatLoop(ctx)

	backoff := 5 * time.Second
	for ctx.Err() == nil {
		ad, ss, err := r.store.Load(ctx)
		switch {
		case errors.Is(err, errNoSessionInWorker):
			r.setState(stateNeedsPairing, "Not linked yet. Run texts-bridge pair on your computer.")
			log.Info().Msg("No pairing saved yet. Run texts-bridge pair on your computer; this keeps checking every minute.")
			r.waitForNewPairing(ctx, "", 0)
			continue
		case errors.Is(err, errWrongPassphrase), errors.Is(err, errWorkerRefused):
			r.setState(stateError, "Wrong passphrase on the phone.")
			return err
		case err != nil:
			r.setState(stateError, "Cannot reach the Worker: "+shortErr(err))
			log.Warn().Err(err).Dur("retry_in", backoff).Msg("Could not load the pairing")
			sleepCtx(ctx, backoff)
			backoff = min(backoff*2, 5*time.Minute)
			continue
		}

		started := time.Now()
		err = r.runSession(ctx, ad, ss)
		if ctx.Err() != nil {
			break
		}
		if time.Since(started) > 10*time.Minute {
			backoff = 5 * time.Second
		}
		switch {
		case errors.Is(err, errNeedsPairing):
			r.setState(stateNeedsPairing, "Google Messages unlinked this phone's bridge. Run texts-bridge pair on your computer.")
			log.Error().Msg("Google Messages unlinked this bridge. Run texts-bridge pair on your computer to link it again.")
			r.store.RemoveLocal()
			r.waitForNewPairing(ctx, ss.PairingID, ss.SavedAt)
		case errors.Is(err, errNewPairing):
			log.Info().Msg("Switching to the new pairing from your computer")
			r.store.RemoveLocal()
		default:
			r.setState(stateReconnecting, shortErr(err))
			log.Warn().Err(err).Dur("retry_in", backoff).Msg("Disconnected from Google Messages")
			sleepCtx(ctx, backoff)
			backoff = min(backoff*2, 5*time.Minute)
		}
	}
	// Push out anything left before exiting.
	fctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = r.queue.Flush(fctx)
	return nil
}

// waitForNewPairing polls the Worker until a pairing other than the dead one shows up.
func (r *runner) waitForNewPairing(ctx context.Context, deadPairingID string, deadSavedAt int64) {
	for {
		if !sleepCtx(ctx, time.Minute) {
			return
		}
		ss, err := r.wc.GetSession(ctx)
		if err != nil {
			if !errors.Is(err, errNoSessionInWorker) {
				r.log.Debug().Err(err).Msg("Checking for a new pairing failed")
			}
			continue
		}
		if ss.PairingID != deadPairingID || ss.SavedAt > deadSavedAt && deadPairingID == "" {
			r.log.Info().Msg("Found a new pairing")
			return
		}
	}
}

func (r *runner) setState(state, detail string) {
	r.stateMu.Lock()
	changed := r.state != state || r.stateDetail != detail
	r.state, r.stateDetail = state, detail
	r.stateMu.Unlock()
	if changed {
		r.log.Debug().Str("state", state).Str("detail", detail).Msg("Link state")
		select {
		case r.hbWake <- struct{}{}:
		default:
		}
	}
}

func (r *runner) currentState() (string, string) {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	return r.state, r.stateDetail
}

func (r *runner) heartbeatLoop(ctx context.Context) {
	// Send the first one soon, so the Worker knows the phone is up.
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-r.hbWake:
			// Let quick state flips settle into one heartbeat.
			if !sleepCtx(ctx, 2*time.Second) {
				return
			}
		}
		state, detail := r.currentState()
		hb := &Heartbeat{
			State:          state,
			Detail:         detail,
			PhoneResponds:  r.phoneResponding.Load(),
			BatteryLow:     r.batteryLow.Load(),
			LastTextAtMs:   r.lastTextAt.Load(),
			LastSyncAtMs:   r.lastSyncAt.Load(),
			BridgeVersion:  Version,
			PendingUploads: r.queue.Pending(),
		}
		hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if err := r.wc.SendHeartbeat(hctx, hb); err != nil {
			r.log.Debug().Err(err).Msg("Heartbeat failed")
		}
		cancel()
		timer.Reset(heartbeatEvery)
	}
}

func (r *runner) watermark() int64 {
	r.stMu.Lock()
	defer r.stMu.Unlock()
	return r.st.WatermarkMs
}

func (r *runner) setWatermark(ms int64) {
	r.stMu.Lock()
	if ms > r.st.WatermarkMs {
		r.st.WatermarkMs = ms
	}
	st := r.st
	r.stMu.Unlock()
	saveBridgeState(st)
}

func (r *runner) noteText(tsMs int64) {
	for {
		cur := r.lastTextAt.Load()
		if tsMs <= cur || r.lastTextAt.CompareAndSwap(cur, tsMs) {
			return
		}
	}
}

// convInfo is what the bridge remembers about a conversation.
type convInfo struct {
	skip         bool
	participants map[string]Participant
}

// session is one connection to Google Messages with one pairing.
type session struct {
	r         *runner
	ctx       context.Context
	client    *libgm.Client
	pairingID string
	savedAt   int64
	log       zerolog.Logger

	convMu sync.Mutex
	convs  map[string]*convInfo

	events  chan any
	syncReq chan struct{}
	fatal   chan error

	active        atomic.Bool
	reclaiming    atomic.Bool
	hackyDone     atomic.Bool
	needsSave     atomic.Bool
	lastDataAt    atomic.Int64
	stallAttempts atomic.Int32
}

func (r *runner) runSession(ctx context.Context, ad *libgm.AuthData, ss *sealedSession) error {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s := &session{
		r:         r,
		ctx:       sctx,
		pairingID: ss.PairingID,
		savedAt:   ss.SavedAt,
		log:       r.log,
		convs:     map[string]*convInfo{},
		events:    make(chan any, 4096),
		syncReq:   make(chan struct{}, 1),
		fatal:     make(chan error, 1),
	}
	s.lastDataAt.Store(time.Now().UnixMilli())
	s.client = libgm.NewClient(ad, nil, r.libLog, httpSettings())
	s.client.SetEventHandler(s.handleEvent)
	r.phoneResponding.Store(true)
	r.setState(stateConnecting, "")

	if err := s.client.FetchConfig(sctx); err != nil {
		// Same as mautrix-gmessages: not fatal, Connect finds out for sure.
		s.log.Warn().Err(err).Msg("Could not fetch the Google Messages config")
	} else if ad.IsGoogleAccount() && s.client.Config.GetDeviceInfo().GetEmail() == "" {
		return errNeedsPairing
	}
	if err := s.client.Connect(sctx); err != nil {
		if errors.Is(err, events.ErrRequestedEntityNotFound) || errors.Is(err, events.ErrInvalidCredentials) {
			return errNeedsPairing
		}
		return fmt.Errorf("connecting: %w", err)
	}
	defer s.client.Disconnect()
	s.log.Info().Msg("Connected to Google Messages")
	r.setState(stateConnected, "")
	s.active.Store(true)

	go s.eventLoop(sctx)
	go s.syncLoop(sctx)
	go s.pairingLoop(sctx)

	select {
	case <-sctx.Done():
		return ctx.Err()
	case err := <-s.fatal:
		return err
	}
}

func (s *session) fail(err error) {
	select {
	case s.fatal <- err:
	default:
	}
}

func (s *session) requestSync() {
	select {
	case s.syncReq <- struct{}{}:
	default:
	}
}

func (s *session) requestSyncAfter(d time.Duration) {
	go func() {
		if sleepCtx(s.ctx, d) {
			s.requestSync()
		}
	}()
}

// handleEvent is called by libgm synchronously, so it must never block.
func (s *session) handleEvent(raw any) {
	r := s.r
	switch evt := raw.(type) {
	case *events.ListenFatalError:
		if errors.Is(evt.Error, events.ErrInvalidCredentials) || strings.Contains(evt.Error.Error(), "http 401") {
			s.fail(errNeedsPairing)
		} else {
			s.fail(fmt.Errorf("connection lost: %w", evt.Error))
		}
	case *events.ListenTemporaryError:
		r.setState(stateReconnecting, shortErr(evt.Error))
	case *events.ListenRecovered:
		if s.active.Load() {
			r.setState(stateConnected, "")
		}
		s.requestSync()
	case *events.PhoneNotResponding:
		r.phoneResponding.Store(false)
		r.setState(stateConnected, "Phone not responding")
	case *events.PhoneRespondingAgain:
		r.phoneResponding.Store(true)
		r.setState(stateConnected, "")
		s.requestSync()
	case *events.HackySetActiveMayFail:
		go s.hackyResetActive()
	case *events.PingFailed:
		if errors.Is(evt.Error, events.ErrRequestedEntityNotFound) {
			s.fail(errNeedsPairing)
		} else if evt.ErrorCount > 3 {
			s.fail(fmt.Errorf("pings keep failing: %w", evt.Error))
		}
	case *gmproto.RevokePairData, *events.GaiaLoggedOut:
		s.fail(errNeedsPairing)
	case *events.AuthTokenRefreshed:
		s.needsSave.Store(true)
	case *events.NoDataReceived:
		go s.recoverStall()
	case *gmproto.UserAlertEvent:
		s.handleAlert(evt)
	case *gmproto.Conversation, *libgm.WrappedMessage:
		s.lastDataAt.Store(time.Now().UnixMilli())
		s.stallAttempts.Store(0)
		select {
		case s.events <- raw:
		default:
			// Too much at once. The next sync picks up anything dropped here.
			s.log.Warn().Msg("Event queue full; a sync will catch up")
			s.requestSync()
		}
	}
}

func (s *session) handleAlert(evt *gmproto.UserAlertEvent) {
	r := s.r
	switch evt.GetAlertType() {
	case gmproto.AlertType_BROWSER_ACTIVE:
		wasInactive := !s.active.Swap(true)
		r.setState(stateConnected, "")
		if wasInactive {
			s.log.Info().Msg("This bridge is the active Messages for web session again")
		}
		s.requestSync()
	case gmproto.AlertType_BROWSER_INACTIVE, gmproto.AlertType_BROWSER_INACTIVE_FROM_TIMEOUT,
		gmproto.AlertType_BROWSER_INACTIVE_FROM_INACTIVITY:
		s.active.Store(false)
		r.setState(stateOtherBrowser, "Messages for web was opened somewhere else; taking it back")
		s.log.Warn().Str("alert", evt.GetAlertType().String()).Msg("Another Messages for web session took over; will take it back")
		go s.reclaim()
	case gmproto.AlertType_MOBILE_DATABASE_SYNC_COMPLETE:
		s.requestSync()
	case gmproto.AlertType_MOBILE_BATTERY_LOW:
		r.batteryLow.Store(true)
		r.setState(r.currentStateOnly(), "Phone battery low")
	case gmproto.AlertType_MOBILE_BATTERY_RESTORED:
		r.batteryLow.Store(false)
		r.setState(r.currentStateOnly(), "")
	}
}

func (r *runner) currentStateOnly() string {
	st, _ := r.currentState()
	return st
}

// reclaim makes this bridge the active Messages for web session again after
// another browser took over. It keeps trying until it works.
func (s *session) reclaim() {
	if !s.reclaiming.CompareAndSwap(false, true) {
		return
	}
	defer s.reclaiming.Store(false)
	waits := []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute}
	for i := 0; ; i++ {
		wait := 10 * time.Minute
		if i < len(waits) {
			wait = waits[i]
		}
		if !sleepCtx(s.ctx, wait) || s.active.Load() {
			return
		}
		ctx, cancel := context.WithTimeout(s.ctx, time.Minute)
		err := s.client.SetActiveSession(ctx)
		cancel()
		if err != nil {
			s.log.Warn().Err(err).Msg("Could not take Messages for web back yet")
			continue
		}
		s.log.Info().Msg("Asked Google Messages to make this bridge active again")
	}
}

func (s *session) hackyResetActive() {
	if !s.hackyDone.CompareAndSwap(false, true) {
		return
	}
	if !sleepCtx(s.ctx, 10*time.Second) || s.active.Load() && s.r.phoneResponding.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, time.Minute)
	_ = s.client.SetActiveSession(ctx)
	cancel()
}

// recoverStall runs when no data has arrived for a while. A sync both catches up
// and tells us whether something was missed; if so, the push channel is re-armed.
func (s *session) recoverStall() {
	before := s.lastDataAt.Load()
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Minute)
	defer cancel()
	gap, err := s.sync(ctx)
	if err != nil || !gap {
		return
	}
	switch s.stallAttempts.Add(1) {
	case 1:
		s.log.Warn().Msg("Texts arrived without being pushed; re-arming")
		_ = s.client.SetActiveSession(ctx)
	default:
		if s.lastDataAt.Load() == before {
			s.fail(errReconnect)
		}
	}
}

func (s *session) eventLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case raw := <-s.events:
			switch evt := raw.(type) {
			case *gmproto.Conversation:
				s.handleConversation(evt)
			case *libgm.WrappedMessage:
				s.handleMessage(ctx, evt.Message)
			}
		}
	}
}

func (s *session) rememberConversation(c *gmproto.Conversation) *convInfo {
	info := &convInfo{skip: skipConversation(c), participants: map[string]Participant{}}
	for _, p := range convertConversation(c).Participants {
		info.participants[p.ID] = p
	}
	s.convMu.Lock()
	s.convs[c.GetConversationID()] = info
	s.convMu.Unlock()
	return info
}

func (s *session) handleConversation(c *gmproto.Conversation) {
	if c.GetConversationID() == "" {
		return
	}
	info := s.rememberConversation(c)
	rec := convertConversation(c)
	if info.skip {
		// Moved to spam, blocked or deleted: the Worker removes it and its texts.
		rec = ConvRecord{ID: rec.ID, Folder: "hidden"}
	}
	s.r.queue.AddConversation(rec)
}

func (s *session) conversation(ctx context.Context, id string) *convInfo {
	s.convMu.Lock()
	info := s.convs[id]
	s.convMu.Unlock()
	if info != nil {
		return info
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	conv, err := s.client.GetConversation(cctx, id)
	if err != nil || conv == nil {
		s.log.Debug().Err(err).Str("conversation_id", id).Msg("Could not look up conversation")
		return nil
	}
	s.handleConversation(conv)
	s.convMu.Lock()
	defer s.convMu.Unlock()
	return s.convs[id]
}

func (s *session) handleMessage(ctx context.Context, m *gmproto.Message) {
	if m == nil {
		return
	}
	action := classifyMessage(m)
	if action == actionIgnore {
		return
	}
	if action == actionDelete {
		s.r.queue.AddDeleted(m.GetMessageID())
		return
	}
	info := s.conversation(ctx, m.GetConversationID())
	var participants map[string]Participant
	if info != nil {
		if info.skip {
			return
		}
		participants = info.participants
	}
	rec := convertMessage(m, participants)
	s.r.queue.AddText(rec)
	s.r.noteText(rec.TimestampMs)
}

func (s *session) syncLoop(ctx context.Context) {
	// Google sends a burst of data right after connecting; sync once it settles.
	s.requestSyncAfter(10 * time.Second)
	ticker := time.NewTicker(syncEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.syncReq:
		}
		if !s.r.phoneResponding.Load() {
			// Nothing to fetch from a phone that is not answering; try again later.
			continue
		}
		if _, err := s.sync(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn().Err(err).Msg("Sync failed; will retry")
			s.requestSyncAfter(time.Minute)
		}
	}
}

var syncMu sync.Mutex

// sync fetches every conversation with activity since the watermark and queues
// its texts. It reports whether it found texts newer than the last pushed data,
// which means pushes stopped arriving.
func (s *session) sync(ctx context.Context) (gap bool, err error) {
	syncMu.Lock()
	defer syncMu.Unlock()
	start := time.Now().UnixMilli()
	since := s.r.watermark() - watermarkOverlapMs
	backfill := s.r.watermark() == 0
	if backfill {
		since = start - int64(firstSyncDays)*24*int64(time.Hour/time.Millisecond)
	}
	lastData := s.lastDataAt.Load()
	queued := 0
	for _, folder := range []gmproto.ListConversationsRequest_Folder{
		gmproto.ListConversationsRequest_INBOX, gmproto.ListConversationsRequest_ARCHIVE,
	} {
		lctx, cancel := context.WithTimeout(ctx, time.Minute)
		resp, err := s.client.ListConversations(lctx, &gmproto.ListConversationsRequest{
			Count:  syncConversations,
			Folder: folder,
		})
		cancel()
		if err != nil {
			return gap, fmt.Errorf("listing conversations: %w", err)
		}
		for _, c := range resp.GetConversations() {
			s.handleConversation(c)
			if skipConversation(c) {
				continue
			}
			lastMs := c.GetLastMessageTimestamp() / 1000
			if lastMs < since {
				continue
			}
			if lastMs > lastData+60_000 {
				gap = true
			}
			n, err := s.syncConversation(ctx, c.GetConversationID(), since, backfill)
			if err != nil {
				return gap, err
			}
			queued += n
		}
	}
	if err := s.r.queue.Flush(ctx); err != nil {
		return gap, fmt.Errorf("uploading: %w", err)
	}
	s.r.setWatermark(start)
	s.r.lastSyncAt.Store(time.Now().UnixMilli())
	s.log.Debug().Int("texts", queued).Bool("gap", gap).Msg("Sync done")
	return gap, nil
}

func (s *session) syncConversation(ctx context.Context, convID string, since int64, backfill bool) (int, error) {
	info := s.conversation(ctx, convID)
	var participants map[string]Participant
	if info != nil {
		participants = info.participants
	}
	var cursor *gmproto.Cursor
	n := 0
	for page := 0; page < syncMaxPages; page++ {
		fctx, cancel := context.WithTimeout(ctx, time.Minute)
		resp, err := s.client.FetchMessages(fctx, convID, syncPageSize, cursor)
		cancel()
		if err != nil {
			return n, fmt.Errorf("fetching texts: %w", err)
		}
		msgs := resp.GetMessages()
		oldest := int64(1<<62 - 1)
		for _, m := range msgs {
			ts := m.GetTimestamp() / 1000
			if ts < oldest {
				oldest = ts
			}
			if ts < since {
				continue
			}
			switch classifyMessage(m) {
			case actionStore:
				rec := convertMessage(m, participants)
				// Only older texts are backfill; anything from the last hour is news.
				rec.Backfill = backfill && rec.TimestampMs < time.Now().Add(-time.Hour).UnixMilli()
				s.r.queue.AddText(rec)
				s.r.noteText(rec.TimestampMs)
				n++
			case actionDelete:
				s.r.queue.AddDeleted(m.GetMessageID())
			}
		}
		next := resp.GetCursor()
		if len(msgs) < syncPageSize || oldest < since || next == nil || next.GetLastItemID() == "" {
			break
		}
		cursor = next
		// Be gentle with the phone.
		if !sleepCtx(ctx, 300*time.Millisecond) {
			return n, ctx.Err()
		}
	}
	return n, nil
}

// pairingLoop keeps the saved pairing fresh in the Worker and notices when the
// computer saved a newer pairing.
func (s *session) pairingLoop(ctx context.Context) {
	lastSave := time.Now()
	ticker := time.NewTicker(pairingCheckEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		remote, err := s.r.wc.GetSession(ctx)
		switch {
		case errors.Is(err, errWorkerRefused):
			s.r.log.Error().Msg("The Worker no longer accepts this phone's passphrase. Run texts-bridge setup again.")
			continue
		case err == nil && remote.PairingID != s.pairingID && remote.SavedAt > s.savedAt:
			s.fail(errNewPairing)
			return
		case errors.Is(err, errNoSessionInWorker):
			// The Worker lost its copy (for example a reset database). Put ours back.
			s.needsSave.Store(true)
		}
		if s.needsSave.Load() || time.Since(lastSave) > sessionSaveEvery {
			ss, err := s.r.store.Save(ctx, s.client.AuthData, false)
			if err == nil {
				s.savedAt = ss.SavedAt
				s.needsSave.Store(false)
				lastSave = time.Now()
			}
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func shortErr(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > 160 {
		msg = msg[:160]
	}
	return msg
}
