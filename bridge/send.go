package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/util"
)

// Sending texts the user approved in Claude.
//
// The Worker hands out a send only after Claude confirmed the user's approved
// preview with its one-time token. The bridge sends it into the conversation
// the preview named (a group chat as the group), or starts a new conversation
// only when the send carries a number instead of a conversation. It reports the
// outcome back, and never sends the same send twice.

const (
	outboxWaitSeconds = 25
	// How long to wait for Google Messages to confirm a text went out.
	sendConfirmWait = 90 * time.Second
	// A Worker without the outbox (not updated yet) is asked again this often.
	outboxMissingRetry = 10 * time.Minute
	outboxDisabledWait = 5 * time.Minute
)

// OutboxItem is one approved text waiting to be sent.
type OutboxItem struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	NewNumber      string `json:"new_number"`
	Text           string `json:"text"`
}

type outboxNext struct {
	Item           *OutboxItem `json:"item"`
	SendingEnabled bool        `json:"sending_enabled"`
}

// OutboxResult reports how a send went: accepted (Google Messages took it),
// sent, failed, or unknown (it may or may not have gone out).
type OutboxResult struct {
	ID             string `json:"id"`
	State          string `json:"state"`
	Detail         string `json:"detail,omitempty"`
	MessageID      string `json:"message_id,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
}

var errOutboxMissing = errors.New("this Worker can't send texts yet (update the Worker)")

func (w *workerClient) NextOutbox(ctx context.Context, waitSeconds int) (*outboxNext, error) {
	var out outboxNext
	status, err := w.do(ctx, http.MethodGet, fmt.Sprintf("/bridge/outbox?wait=%d", waitSeconds), nil, &out)
	if status == http.StatusNotFound {
		return nil, errOutboxMissing
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (w *workerClient) PostOutboxResult(ctx context.Context, res *OutboxResult) error {
	_, err := w.do(ctx, http.MethodPost, "/bridge/outbox/result", res, nil)
	return err
}

// sendLedger remembers which sends this bridge already handled, so even a
// mistaken second hand-out from the Worker is never sent twice.
type sendLedger struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// claim reports whether id is new, and remembers it.
func (l *sendLedger) claim(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = map[string]time.Time{}
	}
	if _, ok := l.seen[id]; ok {
		return false
	}
	for k, t := range l.seen {
		if time.Since(t) > 48*time.Hour {
			delete(l.seen, k)
		}
	}
	l.seen[id] = time.Now()
	return true
}

// echoWaiters lets a send wait for Google Messages to echo the sent text back.
type echoWaiters struct {
	mu sync.Mutex
	m  map[string]chan *gmproto.Message
}

func (e *echoWaiters) add(tmpID string) chan *gmproto.Message {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.m == nil {
		e.m = map[string]chan *gmproto.Message{}
	}
	ch := make(chan *gmproto.Message, 16)
	e.m[tmpID] = ch
	return ch
}

func (e *echoWaiters) remove(tmpID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.m, tmpID)
}

// deliver is called from libgm's event handler, so it never blocks.
func (e *echoWaiters) deliver(m *gmproto.Message) {
	if m == nil || m.GetTmpID() == "" {
		return
	}
	e.mu.Lock()
	ch := e.m[m.GetTmpID()]
	e.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- m:
	default:
	}
}

// simCards remembers the phone's SIM cards by their participant ID, from the
// settings Google Messages sends after connecting.
type simCards struct {
	mu sync.Mutex
	m  map[string]*gmproto.SIMCard
}

func (s *simCards) set(cards []*gmproto.SIMCard) {
	if len(cards) == 0 {
		return
	}
	m := map[string]*gmproto.SIMCard{}
	for _, c := range cards {
		if id := c.GetSIMParticipant().GetID(); id != "" {
			m[id] = c
		}
	}
	s.mu.Lock()
	s.m = m
	s.mu.Unlock()
}

func (s *simCards) get(participantID string) *gmproto.SIMCard {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[participantID]
}

// outboxLoop waits for approved texts and sends them, one at a time.
func (s *session) outboxLoop(ctx context.Context) {
	backoff := 5 * time.Second
	toldMissing := false
	for ctx.Err() == nil {
		// Only take a send when it can go out now; otherwise it waits in the
		// Worker, which drops it unsent after 5 minutes.
		if !s.active.Load() || !s.r.phoneResponding.Load() {
			if !sleepCtx(ctx, 5*time.Second) {
				return
			}
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, (outboxWaitSeconds+20)*time.Second)
		next, err := s.r.wc.NextOutbox(pctx, outboxWaitSeconds)
		cancel()
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, errOutboxMissing):
			if !toldMissing {
				s.log.Info().Msg("The Worker doesn't support sending texts yet; reading still works. Will check again later.")
				toldMissing = true
			}
			sleepCtx(ctx, outboxMissingRetry)
			continue
		case err != nil:
			s.log.Debug().Err(err).Dur("retry_in", backoff).Msg("Checking for texts to send failed")
			sleepCtx(ctx, backoff)
			backoff = min(backoff*2, maxRetryWait)
			continue
		}
		backoff = 5 * time.Second
		if !next.SendingEnabled {
			sleepCtx(ctx, outboxDisabledWait)
			continue
		}
		if next.Item == nil {
			continue
		}
		s.sendApproved(ctx, next.Item)
	}
}

// sendApproved sends one approved text and reports the outcome to the Worker.
func (s *session) sendApproved(ctx context.Context, it *OutboxItem) {
	log := s.log.With().Str("send_id", it.ID).Logger()
	report := func(state, detail, msgID, convID string) {
		res := &OutboxResult{ID: it.ID, State: state, Detail: detail, MessageID: msgID, ConversationID: convID}
		for attempt := 0; attempt < 5; attempt++ {
			// Report even while shutting down, so the user hears how it went.
			rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := s.r.wc.PostOutboxResult(rctx, res)
			cancel()
			if err == nil {
				return
			}
			log.Warn().Err(err).Msg("Could not report the send result to the Worker")
			time.Sleep(time.Duration(attempt+1) * 3 * time.Second)
		}
	}
	if !s.r.sent.claim(it.ID) {
		log.Warn().Msg("Already handled this send; not sending it again")
		return
	}

	plan, err := planSend(it)
	if err != nil {
		report("failed", err.Error(), "", "")
		return
	}

	var conv *gmproto.Conversation
	if plan.conversationID != "" {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		conv, err = s.client.GetConversation(cctx, plan.conversationID)
		cancel()
		if err != nil || conv == nil || conv.GetConversationID() != plan.conversationID {
			report("failed", "couldn't find that conversation on the phone", "", "")
			return
		}
	} else {
		cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		resp, err := s.client.GetOrCreateConversation(cctx, &gmproto.GetOrCreateConversationRequest{
			Numbers: []*gmproto.ContactNumber{{MysteriousInt: 2, Number: plan.newNumber, Number2: plan.newNumber}},
		})
		cancel()
		if err != nil || resp.GetConversation().GetConversationID() == "" {
			report("failed", "couldn't start a conversation with that number: "+shortErr(errOr(err, "no conversation came back")), "", "")
			return
		}
		conv = resp.GetConversation()
		s.handleConversation(conv)
	}
	if why := cannotSendTo(conv); why != "" {
		report("failed", why, "", conv.GetConversationID())
		return
	}

	sim := s.r.sims.get(conv.GetDefaultOutgoingID())
	if sim == nil {
		sim = conv.GetSimCard()
	}
	tmpID := util.GenerateTmpID()
	req := buildSendRequest(conv, plan.text, tmpID, sim)

	echo := s.r.echoes.add(tmpID)
	defer s.r.echoes.remove(tmpID)

	log.Info().Str("conversation_id", conv.GetConversationID()).Bool("new_conversation", plan.newNumber != "").Msg("Sending an approved text")
	resp, err := s.sendWithRetry(ctx, req, log)
	if err != nil {
		if errors.Is(err, libgm.ErrPhoneNotResponding) {
			report("unknown", "Google Messages took it, but the phone didn't answer; it may still go out when the phone responds", "", conv.GetConversationID())
		} else {
			report("failed", "Google Messages didn't take it: "+shortErr(err), "", conv.GetConversationID())
		}
		return
	}
	if resp.GetStatus() != gmproto.SendMessageResponse_SUCCESS {
		report("failed", "the phone refused it ("+resp.GetStatus().String()+")", "", conv.GetConversationID())
		return
	}
	report("accepted", "", "", conv.GetConversationID())

	// Wait for the phone to say the text went out (or failed).
	timer := time.NewTimer(sendConfirmWait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			log.Info().Msg("No sent confirmation yet; Claude can still see it once the text syncs")
			return
		case m := <-echo:
			_, status := statusName(m.GetMessageStatus().GetStatus())
			switch status {
			case "sent", "delivered", "read":
				log.Info().Msg("Approved text sent")
				report("sent", "", m.GetMessageID(), conv.GetConversationID())
				return
			case "failed":
				log.Warn().Str("status", m.GetMessageStatus().GetStatus().String()).Msg("The phone couldn't send the text")
				report("failed", "the phone couldn't send it ("+m.GetMessageStatus().GetStatus().String()+")", m.GetMessageID(), conv.GetConversationID())
				return
			}
		}
	}
}

var sendRetryWaits = []time.Duration{3 * time.Second, 8 * time.Second, 20 * time.Second}

// sendWithRetry retries only the statuses Google Messages uses for a
// temporary refusal, with the same tmpID, as mautrix-gmessages does.
func (s *session) sendWithRetry(ctx context.Context, req *gmproto.SendMessageRequest, log zerolog.Logger) (*gmproto.SendMessageResponse, error) {
	sctx, cancel := context.WithTimeout(ctx, time.Minute)
	resp, err := s.client.SendMessage(sctx, req)
	cancel()
	for attempt := 0; err == nil && isTransientSendStatus(resp.GetStatus()) && attempt < len(sendRetryWaits); attempt++ {
		log.Warn().Str("status", resp.GetStatus().String()).Msg("The phone refused the text for now; retrying")
		if !sleepCtx(ctx, sendRetryWaits[attempt]) {
			return nil, ctx.Err()
		}
		sctx, cancel := context.WithTimeout(ctx, time.Minute)
		resp, err = s.client.SendMessage(sctx, req)
		cancel()
	}
	return resp, err
}

func isTransientSendStatus(st gmproto.SendMessageResponse_Status) bool {
	return st == gmproto.SendMessageResponse_FAILURE_2 || st == gmproto.SendMessageResponse_FAILURE_3
}

type sendPlan struct {
	conversationID string
	newNumber      string
	text           string
}

// planSend checks an outbox item: exactly one of a conversation or a new number,
// and some text.
func planSend(it *OutboxItem) (sendPlan, error) {
	p := sendPlan{
		conversationID: strings.TrimSpace(it.ConversationID),
		newNumber:      strings.TrimSpace(it.NewNumber),
		text:           strings.TrimSpace(it.Text),
	}
	switch {
	case p.text == "":
		return p, errors.New("the text was empty")
	case p.conversationID != "" && p.newNumber != "":
		return p, errors.New("the send named both a conversation and a new number")
	case p.conversationID == "" && p.newNumber == "":
		return p, errors.New("the send named no conversation")
	case p.newNumber != "" && !validE164(p.newNumber):
		return p, errors.New("the new number isn't a full phone number")
	}
	return p, nil
}

func validE164(n string) bool {
	if len(n) < 9 || len(n) > 16 || n[0] != '+' {
		return false
	}
	for _, r := range n[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// cannotSendTo says why a conversation must not get a text, or "" if it may.
func cannotSendTo(c *gmproto.Conversation) string {
	if skipConversation(c) {
		return "that conversation is in spam, blocked or deleted on the phone"
	}
	if c.GetReadOnly() {
		return "that conversation is read-only on the phone"
	}
	return ""
}

func buildSendRequest(conv *gmproto.Conversation, text, tmpID string, sim *gmproto.SIMCard) *gmproto.SendMessageRequest {
	convID := conv.GetConversationID()
	return &gmproto.SendMessageRequest{
		ConversationID: convID,
		MessagePayload: &gmproto.MessagePayload{
			TmpID:          tmpID,
			ConversationID: convID,
			ParticipantID:  conv.GetDefaultOutgoingID(),
			TmpID2:         tmpID,
			MessageInfo: []*gmproto.MessageInfo{{
				Data: &gmproto.MessageInfo_MessageContent{MessageContent: &gmproto.MessageContent{Content: text}},
			}},
		},
		SIMPayload: sim.GetSIMData().GetSIMPayload(),
		TmpID:      tmpID,
	}
}

func errOr(err error, fallback string) error {
	if err != nil {
		return err
	}
	return errors.New(fallback)
}
