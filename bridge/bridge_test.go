package main

import (
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

const testPass = "correct horse battery staple"

// The Worker's tests use the same vectors, so both sides derive the same tokens.
func TestDeriveTokenVectors(t *testing.T) {
	if got := deriveToken(testPass, purposeBridge); got != "2fcb263fcf2ad5a7dcd3140645a3e79ad3d6138f2aa32933" {
		t.Fatalf("bridge token %s", got)
	}
	if got := deriveToken(testPass, purposeMCP); got != "85e11021d458a0dc801812000fb8056266e73797f19c82eb" {
		t.Fatalf("mcp token %s", got)
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	s := newSessionSealer(testPass)
	ss, err := s.Seal([]byte(`{"hello":"world"}`), "pair-1")
	if err != nil {
		t.Fatal(err)
	}
	if ss.PairingID != "pair-1" || ss.SavedAt == 0 || ss.V != 1 {
		t.Fatalf("bad sealed session %+v", ss)
	}
	plain, err := newSessionSealer(testPass).Open(ss)
	if err != nil || string(plain) != `{"hello":"world"}` {
		t.Fatalf("open: %v %q", err, plain)
	}
	if _, err := newSessionSealer("a different passphrase!").Open(ss); err != errWrongPassphrase {
		t.Fatalf("wrong passphrase should fail, got %v", err)
	}
}

func TestParseCookies(t *testing.T) {
	want := map[string]string{"SID": "s1", "HSID": "h1", "SSID": "ss1", "APISID": "a1", "SAPISID": "sa1", "OSID": "o1"}
	header := "SID=s1; HSID=h1; SSID=ss1; APISID=a1; SAPISID=sa1; OSID=o1; NID=junk; __Secure-1PSIDTS=ts1"
	inputs := map[string]string{
		"bash": "curl 'https://messages.google.com/web/config' \\\n  -H 'accept: */*' \\\n  -b '" + header + "' \\\n  -H 'user-agent: x'",
		"bash-header": "curl 'https://messages.google.com/web/config' \\\n  -H 'cookie: " + header + "'",
		"cmd":         "curl ^\"https://messages.google.com/web/config^\" ^\n  -b ^\"" + strings.ReplaceAll(header, "%", "%%") + "^\" ^\n  -H ^\"accept: */*^\"",
		"powershell":  `$session.Cookies.Add((New-Object System.Net.Cookie("SID", "s1", "/", ".google.com")))` + "\n" + `$session.Cookies.Add((New-Object System.Net.Cookie("HSID", "h1", "/", ".google.com")))` + "\n" + `$session.Cookies.Add((New-Object System.Net.Cookie("SSID", "ss1", "/", ".google.com")))` + "\n" + `$session.Cookies.Add((New-Object System.Net.Cookie("APISID", "a1", "/", ".google.com")))` + "\n" + `$session.Cookies.Add((New-Object System.Net.Cookie("SAPISID", "sa1", "/", ".google.com")))` + "\n" + `$session.Cookies.Add((New-Object System.Net.Cookie("OSID", "o1", "/", "messages.google.com")))`,
		"json":        `{"SID":"s1","HSID":"h1","SSID":"ss1","APISID":"a1","SAPISID":"sa1","OSID":"o1"}`,
		"raw":         "Cookie: " + header,
	}
	for name, in := range inputs {
		got, err := parseCookieInput(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("%s: cookie %s = %q, want %q", name, k, got[k], v)
			}
		}
		if _, ok := got["NID"]; ok {
			t.Fatalf("%s: kept a cookie that is not needed", name)
		}
	}
	if _, err := parseCookieInput("curl 'https://x' -b 'SID=1; HSID=2'"); err == nil || !strings.Contains(err.Error(), "OSID") {
		t.Fatalf("missing cookies should be named, got %v", err)
	}
}

func TestRedactCodes(t *testing.T) {
	cases := []struct {
		in     string
		hidden bool
		keep   string
	}{
		{"Your verification code is 482913", true, "[code hidden]"},
		{"G-583920 is your Google verification code.", true, "[code hidden]"},
		{"Use 123-456 to log in", true, "[code hidden]"},
		{"Practice is at 6 tonight, room 2041", false, "2041"},
		{"Your code: 2024 was a great year", false, "2024"},
		{"Call me at 5551234", false, "5551234"},
	}
	for _, c := range cases {
		out, hidden := redactCodes(c.in)
		if hidden != c.hidden || !strings.Contains(out, c.keep) {
			t.Fatalf("%q -> %q hidden=%v", c.in, out, hidden)
		}
	}
	out, _ := redactCodes("Your verification code is 482913")
	if strings.Contains(out, "482913") {
		t.Fatal("code leaked")
	}
}

func TestConvertMessage(t *testing.T) {
	m := &gmproto.Message{
		MessageID:      "m1",
		ConversationID: "c1",
		ParticipantID:  "p2",
		Timestamp:      1_700_000_000_123_456,
		MessageStatus:  &gmproto.MessageStatus{Status: gmproto.MessageStatusType_INCOMING_COMPLETE},
		MessageInfo: []*gmproto.MessageInfo{
			{Data: &gmproto.MessageInfo_MessageContent{MessageContent: &gmproto.MessageContent{Content: "See you at 5"}}},
			{Data: &gmproto.MessageInfo_MediaContent{MediaContent: &gmproto.MediaContent{MimeType: "image/jpeg"}}},
		},
	}
	parts := map[string]Participant{"p2": {ID: "p2", Name: "Alex", Number: "+1 555-0100"}}
	rec := convertMessage(m, parts)
	if rec.TimestampMs != 1_700_000_000_123 || rec.FromMe || rec.SenderName != "Alex" || rec.Status != "received" {
		t.Fatalf("bad record %+v", rec)
	}
	if rec.Text != "See you at 5\n[photo]" || rec.Kind != "text" {
		t.Fatalf("bad text %q kind %s", rec.Text, rec.Kind)
	}

	m.MessageStatus.Status = gmproto.MessageStatusType_OUTGOING_DELIVERED
	rec = convertMessage(m, parts)
	if !rec.FromMe || rec.SenderName != "Me" || rec.Status != "delivered" {
		t.Fatalf("own text wrong %+v", rec)
	}

	for st, want := range map[gmproto.MessageStatusType]messageAction{
		gmproto.MessageStatusType_MESSAGE_DELETED:   actionDelete,
		gmproto.MessageStatusType_OUTGOING_DRAFT:    actionIgnore,
		gmproto.MessageStatusType(200):              actionIgnore,
		gmproto.MessageStatusType_INCOMING_COMPLETE: actionStore,
	} {
		m.MessageStatus.Status = st
		if got := classifyMessage(m); got != want {
			t.Fatalf("status %v: got %v want %v", st, got, want)
		}
	}
}

func TestSkipConversation(t *testing.T) {
	if !skipConversation(&gmproto.Conversation{Status: gmproto.ConversationStatus_SPAM_FOLDER}) {
		t.Fatal("spam should be skipped")
	}
	c := &gmproto.Conversation{ConversationID: "c", Status: gmproto.ConversationStatus_ARCHIVED, Type: gmproto.ConversationType_RCS}
	if skipConversation(c) {
		t.Fatal("archived should be kept")
	}
	rec := convertConversation(c)
	if rec.Folder != "archived" || rec.Protocol != "rcs" {
		t.Fatalf("bad conv %+v", rec)
	}
}

func TestQueuePutBackKeepsOrderAndDedupes(t *testing.T) {
	q := newUploadQueue(nil, testLogger())
	q.AddText(TextRecord{ID: "a"})
	q.AddText(TextRecord{ID: "b"})
	q.AddText(TextRecord{ID: "a", Text: "edited"})
	b := q.take()
	if len(b.Messages) != 2 || b.Messages[0].Text != "edited" {
		t.Fatalf("take: %+v", b.Messages)
	}
	q.AddText(TextRecord{ID: "c"})
	q.AddDeleted("b")
	q.putBack(b)
	b2 := q.take()
	ids := []string{}
	for _, m := range b2.Messages {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "a,c" || len(b2.DeletedIDs) != 1 {
		t.Fatalf("after putBack: %v deleted=%v", ids, b2.DeletedIDs)
	}
}

func TestNormalizeWorkerURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://texts-mcp.zac.workers.dev/":       "https://texts-mcp.zac.workers.dev",
		"texts-mcp.zac.workers.dev":                "https://texts-mcp.zac.workers.dev",
		"https://texts-mcp.zac.workers.dev/mcp/xx": "https://texts-mcp.zac.workers.dev",
		"http://localhost:8787":                    "http://localhost:8787",
	} {
		got, err := normalizeWorkerURL(in)
		if err != nil || got != want {
			t.Fatalf("%q -> %q, %v", in, got, err)
		}
	}
	if _, err := normalizeWorkerURL("http://example.com"); err == nil {
		t.Fatal("plain http should be refused")
	}
}

func testLogger() zerolog.Logger { return zerolog.Nop() }
