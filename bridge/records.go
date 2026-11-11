package main

import (
	"regexp"
	"strings"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// TextRecord is one text as stored by the Worker.
type TextRecord struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	TimestampMs    int64  `json:"ts"`
	FromMe         bool   `json:"from_me"`
	SenderName     string `json:"sender_name,omitempty"`
	SenderNumber   string `json:"sender_number,omitempty"`
	Text           string `json:"text"`
	// Kind is text, photo, video, audio, file, contact or other.
	Kind string `json:"kind"`
	// Status is sent, delivered, read, failed, sending or received.
	Status string `json:"status"`
	// CodeHidden is true when a login or verification code was blanked out.
	CodeHidden bool `json:"code_hidden,omitempty"`
	// Backfill marks texts from the first catch-up after pairing, so the Worker
	// does not report them as newly arrived.
	Backfill bool `json:"backfill,omitempty"`
}

// ConvRecord is one conversation's details.
type ConvRecord struct {
	ID              string        `json:"id"`
	Name            string        `json:"name"`
	IsGroup         bool          `json:"is_group"`
	Protocol        string        `json:"protocol"` // sms or rcs
	Unread          bool          `json:"unread"`
	Folder          string        `json:"folder"` // inbox or archived
	Pinned          bool          `json:"pinned"`
	LastMessageAtMs int64         `json:"last_message_at_ms"`
	Participants    []Participant `json:"participants"`
}

// Participant is a person in a conversation.
type Participant struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Number string `json:"number,omitempty"`
	IsMe   bool   `json:"is_me,omitempty"`
}

// skipConversation reports whether a conversation should never leave the phone.
func skipConversation(c *gmproto.Conversation) bool {
	switch c.GetStatus() {
	case gmproto.ConversationStatus_SPAM_FOLDER, gmproto.ConversationStatus_BLOCKED_FOLDER,
		gmproto.ConversationStatus_TRASH_FOLDER, gmproto.ConversationStatus_DELETED:
		return true
	}
	return false
}

func convertConversation(c *gmproto.Conversation) ConvRecord {
	rec := ConvRecord{
		ID:              c.GetConversationID(),
		Name:            strings.TrimSpace(c.GetName()),
		IsGroup:         c.GetIsGroupChat(),
		Protocol:        "sms",
		Unread:          c.GetUnread(),
		Folder:          "inbox",
		Pinned:          c.GetPinned(),
		LastMessageAtMs: c.GetLastMessageTimestamp() / 1000,
	}
	if c.GetType() == gmproto.ConversationType_RCS {
		rec.Protocol = "rcs"
	}
	switch c.GetStatus() {
	case gmproto.ConversationStatus_ARCHIVED, gmproto.ConversationStatus_KEEP_ARCHIVED:
		rec.Folder = "archived"
	}
	for _, p := range c.GetParticipants() {
		rec.Participants = append(rec.Participants, Participant{
			ID:     p.GetID().GetParticipantID(),
			Name:   firstNonEmpty(p.GetFullName(), p.GetFirstName()),
			Number: firstNonEmpty(p.GetFormattedNumber(), p.GetID().GetNumber()),
			IsMe:   p.GetIsMe(),
		})
	}
	return rec
}

// messageAction says what to do with a message from Google Messages.
type messageAction int

const (
	actionIgnore messageAction = iota
	actionStore
	actionDelete
)

func classifyMessage(m *gmproto.Message) messageAction {
	st := m.GetMessageStatus().GetStatus()
	switch st {
	case gmproto.MessageStatusType_MESSAGE_DELETED, gmproto.MessageStatusType_INCOMING_DELETED,
		gmproto.MessageStatusType_OUTGOING_DELETED:
		return actionDelete
	case gmproto.MessageStatusType_OUTGOING_DRAFT:
		return actionIgnore
	}
	// 200 and up are group events like "Alex joined" and protocol switches, not texts.
	if int32(st) >= 200 {
		return actionIgnore
	}
	if m.GetMessageID() == "" || m.GetConversationID() == "" {
		return actionIgnore
	}
	return actionStore
}

func statusName(st gmproto.MessageStatusType) (fromMe bool, name string) {
	switch st {
	case gmproto.MessageStatusType_OUTGOING_COMPLETE:
		return true, "sent"
	case gmproto.MessageStatusType_OUTGOING_DELIVERED:
		return true, "delivered"
	case gmproto.MessageStatusType_OUTGOING_DISPLAYED:
		return true, "read"
	case gmproto.MessageStatusType_OUTGOING_YET_TO_SEND, gmproto.MessageStatusType_OUTGOING_SENDING,
		gmproto.MessageStatusType_OUTGOING_RESENDING, gmproto.MessageStatusType_OUTGOING_AWAITING_RETRY,
		gmproto.MessageStatusType_OUTGOING_SEND_AFTER_PROCESSING, gmproto.MessageStatusType_OUTGOING_VALIDATING,
		gmproto.MessageStatusType_OUTGOING_NOT_DELIVERED_YET, gmproto.MessageStatusType_OUTGOING_SCHEDULED:
		return true, "sending"
	}
	if int32(st) >= 1 && int32(st) < 100 {
		return true, "failed"
	}
	return false, "received"
}

// convertMessage turns a Google Messages message into a TextRecord.
// participants maps participant IDs to people, from the conversation details.
func convertMessage(m *gmproto.Message, participants map[string]Participant) TextRecord {
	fromMe, status := statusName(m.GetMessageStatus().GetStatus())
	rec := TextRecord{
		ID:             m.GetMessageID(),
		ConversationID: m.GetConversationID(),
		TimestampMs:    m.GetTimestamp() / 1000,
		FromMe:         fromMe,
		Status:         status,
		Kind:           "text",
	}
	if sp := m.GetSenderParticipant(); sp != nil {
		if sp.GetIsMe() {
			rec.FromMe = true
		}
		rec.SenderName = firstNonEmpty(sp.GetFullName(), sp.GetFirstName())
		rec.SenderNumber = firstNonEmpty(sp.GetFormattedNumber(), sp.GetID().GetNumber())
	}
	if p, ok := participants[m.GetParticipantID()]; ok {
		if p.IsMe {
			rec.FromMe = true
		}
		rec.SenderName = firstNonEmpty(rec.SenderName, p.Name)
		rec.SenderNumber = firstNonEmpty(rec.SenderNumber, p.Number)
	}
	if rec.FromMe {
		rec.SenderName = "Me"
		rec.SenderNumber = ""
		if status == "received" {
			rec.Status = "sent"
		}
	}

	var parts []string
	var mediaKinds []string
	if subj := strings.TrimSpace(m.GetSubject()); subj != "" {
		parts = append(parts, "Subject: "+subj)
	}
	for _, info := range m.GetMessageInfo() {
		if mc := info.GetMessageContent(); mc != nil {
			if t := strings.TrimSpace(mc.GetContent()); t != "" {
				parts = append(parts, t)
			}
		}
		if media := info.GetMediaContent(); media != nil {
			kind := mediaKind(media.GetMimeType(), media.GetFormat())
			mediaKinds = append(mediaKinds, kind)
			label := "[" + kind + "]"
			if name := strings.TrimSpace(media.GetMediaName()); name != "" && (kind == "file" || kind == "contact") {
				label = "[" + kind + ": " + name + "]"
			}
			parts = append(parts, label)
		}
	}
	if len(mediaKinds) > 0 {
		rec.Kind = mediaKinds[0]
		if len(parts) > len(mediaKinds) {
			// Text plus attachment: still mostly a text.
			rec.Kind = "text"
		}
	}
	rec.Text = strings.Join(parts, "\n")
	if rec.Text == "" {
		rec.Kind = "other"
		rec.Text = "[message with no text]"
	}
	rec.Text, rec.CodeHidden = redactCodes(rec.Text)
	return rec
}

func mediaKind(mime string, format gmproto.MediaFormats) string {
	mime = strings.ToLower(mime)
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "photo"
	case strings.HasPrefix(mime, "video/"):
		return "video"
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	case strings.Contains(mime, "vcard"):
		return "contact"
	case mime != "":
		return "file"
	}
	f := int32(format)
	switch {
	case f >= 1 && f <= 7:
		return "photo"
	case f >= 8 && f <= 13:
		return "video"
	case f >= 14 && f <= 23:
		return "audio"
	case f == 24:
		return "contact"
	}
	return "file"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// Login and verification codes are blanked out on the phone, so they never reach
// the Worker or Claude. A text counts as a code text when it mentions a code word.
var (
	codeWords = regexp.MustCompile(`(?i)\b(code|codes|verification|verify|verifying|passcode|one[- ]time|otp|pin|2fa|two[- ]factor|security|login|log in|sign[- ]in|authenticat\w*|confirm\w*|token)\b`)
	// 4 to 8 digits (optionally split by a space or dash, like 123-456), or a
	// prefixed code like G-123456.
	codePattern = regexp.MustCompile(`\b(?:[A-Z]{1,3}-)?\d{3,4}[- ]?\d{1,4}\b|\b\d{4,8}\b`)
	// Things that look like codes but are not: times, years, prices, phone numbers.
	notCodePattern = regexp.MustCompile(`^(?:19|20)\d{2}$`)
)

func redactCodes(text string) (string, bool) {
	if !codeWords.MatchString(text) {
		return text, false
	}
	hidden := false
	out := codePattern.ReplaceAllStringFunc(text, func(match string) string {
		digits := 0
		for _, r := range match {
			if r >= '0' && r <= '9' {
				digits++
			}
		}
		if digits < 4 || digits > 8 || notCodePattern.MatchString(match) {
			return match
		}
		hidden = true
		return "[code hidden]"
	})
	return out, hidden
}
