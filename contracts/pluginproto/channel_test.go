package pluginproto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func channelFixture(t *testing.T, name string) []byte {
	t.Helper()
	line, err := os.ReadFile(filepath.Join("testdata", "v2", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return line
}

func TestV2GoldenLines(t *testing.T) {
	for _, dir := range []string{"host", "plugin", "tolerated"} {
		files, err := os.ReadDir(filepath.Join("testdata", "v2", dir))
		if err != nil || len(files) == 0 {
			t.Fatalf("%s fixtures: %v", dir, err)
		}
		for _, f := range files {
			name := strings.TrimSuffix(f.Name(), ".json")
			t.Run(dir+"/"+name, func(t *testing.T) {
				wantDir := dir
				if dir == "tolerated" {
					wantDir = "plugin"
					if name == "deliver-minimal" {
						wantDir = "host"
					}
				}
				parse := ParsePluginLineV2
				if wantDir == "host" {
					parse = ParseHostLineV2
				}
				frame, err := parse(channelFixture(t, dir+"/"+name), KindChannel)
				if err != nil {
					t.Fatal(err)
				}
				got, err := json.Marshal(frame)
				if err != nil {
					t.Fatal(err)
				}
				if want := channelFixture(t, wantDir+"/"+name); !bytes.Equal(append(got, '\n'), want) {
					t.Fatalf("got %s, want %s", got, want)
				}
			})
		}
	}
}

func TestV2FieldRules(t *testing.T) {
	for _, tc := range []struct {
		fixture, field, old, invalid string
	}{
		{"ready-channel", "kind", `"channel","channel"`, `"other","channel"`},
		{"ready-channel", "kind", `"channel","channel"`, `"","channel"`},
		{"ready-channel", "channel", `,"channel":{"name":"echo","label":"Echo chat"}`, ""},
		{"ready-channel", "name", `"name":"echo","label"`, `"name":"E","label"`},
		{"ready-channel", "name", `"name":"echo","label"`, `"name":"` + strings.Repeat("a", 65) + `","label"`},
		{"ready-channel", "name", `"name":"echo","label"`, `"name":"","label"`},
		{"ready-channel", "channel", `"kind":"channel"`, `"kind":"capability"`},
		{"ready-channel", "label", "Echo chat", strings.Repeat("é", 65)},
		{"ready-channel", "channel.label", "Echo chat", `Echo\nchat`},
		{"ready-default", "capabilities", `[{"name":"echo"}]`, "[]"},
		{"inbound", "id", "msg:18", strings.Repeat("a", 129)},
		{"inbound", "inbound.id", "msg:18", `msg\u0000`},
		{"inbound", "sender.id", "user:3", `user\t3`},
		{"inbound", "sender.name", "Jo", `J\ro`},
		{"inbound", "chat.id", "room:8", `room\u007f8`},
		{"inbound", "reply_to", "msg:17", `msg\u008017`},
		{"inbound", "action.delivery_id", "d1", `d\n1`},
		{"inbound", "action.button_id", "ok", `o\tk`},
		{"inbound", "time", "2026-01-15T10:30:00Z", "yesterday"},
		{"inbound", "time", "2026-01-15T10:30:00Z", "2026-01-15T10:30:00+24:00"},
		{"inbound", "time", "2026-01-15T10:30:00Z", "2026-01-15T10:30:00-10:60"},
		{"inbound", "time", "2026-01-15T10:30:00Z", "2026-01-15T1:30:00Z"},
		{"inbound", "time", "2026-01-15T10:30:00Z", "2026-01-15T10:30:00,4Z"},
		{"inbound", "sender", `"id":"user:3"`, `"id":""`},
		{"inbound", "sender", "user:3", strings.Repeat("a", 129)},
		{"inbound", "name", "Jo", strings.Repeat("é", 129)},
		{"inbound", "chat", "room:8", strings.Repeat("a", 129)},
		{"inbound", "chat", "group", "room"},
		{"inbound", "chat", "group", ""},
		{"inbound", "reply_to", "msg:17", ""},
		{"inbound", "reply_to", "msg:17", strings.Repeat("a", 129)},
		{"inbound", "text", `"text":"",`, ""},
		{"inbound", "delivery_id", "d1", "bad/id"},
		{"inbound", "button_id", "ok", strings.Repeat("a", 65)},
		{"inbound", "button_id", "ok", "bad/id"},
		{"inbound", "attachments", `[{"name":"notes.txt","mime":"text/plain","size":0,"ref":"file:42"}]`, repeatedJSON(`{"name":"a","mime":"b","size":1,"ref":"c"}`, 17)},
		{"inbound", "name", "notes.txt", ""},
		{"inbound", "mime", "text/plain", ""},
		{"inbound", "size", `"size":0,`, ""},
		{"inbound", "size", `"size":0`, `"size":-1`},
		{"inbound", "ref", "file:42", ""},
		{"inbound", "attachments[0].name", "notes.txt", strings.Repeat("é", 128) + "a"},
		{"inbound", "attachments[0].mime", "text/plain", strings.Repeat("é", 64) + "a"},
		{"inbound", "attachments[0].ref", "file:42", strings.Repeat("é", 64) + "a"},
		{"inbound", "attachments[0].name", "notes.txt", `notes\u009f.txt`},
		{"inbound", "attachments[0].mime", "text/plain", `text\n/plain`},
		{"inbound", "attachments[0].ref", "file:42", `file\u001b42`},
		{"inbound", "sender", `"id":"user:3"`, `"id":"user:3","id":"again"`},
		{"inbound-minimal", "text", "hello", ""},
		{"inbound-minimal", "text", "hello", strings.Repeat("x", MaxEventText+1)},
		{"ack", "id", "msg:17", strings.Repeat("a", 129)},
		{"ack", "ack.id", "msg:17", `msg\t17`},
		{"deliver", "id", "d1", "bad/id"},
		{"deliver", "id", "d1", strings.Repeat("a", 129)},
		{"deliver", "chat_id", "room:8", ""},
		{"deliver", "chat_id", "room:8", `room\r8`},
		{"deliver", "chat_id", "room:8", strings.Repeat("a", 129)},
		{"deliver", "text", `"text":"hello",`, ""},
		{"deliver", "text", "hello", strings.Repeat("é", MaxEventText/2+1)},
		{"deliver", "reply_to", "msg:17", ""},
		{"deliver", "reply_to", "msg:17", `msg\n17`},
		{"deliver", "buttons", `[{"id":"ok","label":"Confirm"}]`, repeatedJSON(`{"id":"a","label":"b"}`, 9)},
		{"deliver", "id", "ok", strings.Repeat("a", 65)},
		{"deliver", "id", "ok", "a/b"},
		{"deliver", "buttons[1].id", `{"id":"ok","label":"Confirm"}`, `{"id":"ok","label":"Yes"},{"id":"ok","label":"No"}`},
		{"deliver", "buttons[0].id", "ok", `o\rk`},
		{"deliver", "buttons[0].label", "Confirm", `Con\tfirm`},
		{"deliver", "label", "Confirm", strings.Repeat("é", 33)},
		{"deliver", "label", "Confirm", ""},
		{"delivered", "message_id", "msg:19", strings.Repeat("a", 129)},
		{"delivered", "id", "d1", "bad/id"},
		{"delivered", "message_id", "msg:19", `msg\u008519`},
		{"delivery-failed", "id", "d2", ""},
		{"delivery-failed", "delivery_failed.id", "d2", `d\n2`},
		{"delivery-failed", "error.message", "try again later", `try\nagain`},
		{"delivery-failed", "code", "rate_limited", "unknown"},
		{"delivery-failed", "message", "try again later", ""},
		{"delivery-failed", "retry_after_ms", `"retry_after_ms":0`, `"retry_after_ms":-1`},
		{"status", "state", "reconnecting", "sleeping"},
		{"status", "status.text", "waiting for service", `waiting\u001b`},
		{"status", "text", "waiting for service", strings.Repeat("é", 513)},
	} {
		t.Run(tc.fixture+"/"+tc.field, func(t *testing.T) {
			dir, parse := "plugin", ParsePluginLineV2
			if tc.fixture == "deliver" || tc.fixture == "ack" {
				dir, parse = "host", ParseHostLineV2
			}
			line := string(channelFixture(t, dir+"/"+tc.fixture))
			changed := strings.Replace(line, tc.old, tc.invalid, 1)
			if changed == line {
				t.Fatal("fixture did not contain the field")
			}
			if _, err := parse([]byte(changed), KindChannel); err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("expected error naming %s, got %v", tc.field, err)
			}
		})
	}
}

func TestV2RequiredFields(t *testing.T) {
	for _, fixture := range []string{"host/ack", "host/deliver-minimal", "plugin/inbound-minimal", "plugin/delivered", "plugin/delivery-failed-minimal", "plugin/status-minimal"} {
		parse := ParsePluginLineV2
		if strings.HasPrefix(fixture, "host/") {
			parse = ParseHostLineV2
		}
		fields := map[string]json.RawMessage{}
		if err := json.Unmarshal(channelFixture(t, fixture), &fields); err != nil {
			t.Fatal(err)
		}
		for key := range fields {
			t.Run(fixture+"/"+key, func(t *testing.T) {
				missing := maps.Clone(fields)
				delete(missing, key)
				line, err := json.Marshal(missing)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := parse(line, KindChannel); err == nil {
					t.Fatalf("accepted %s", line)
				}
			})
		}
	}
}

func TestChannelAcceptsBoundaryValues(t *testing.T) {
	line := string(channelFixture(t, "plugin/inbound"))
	attachment := fmt.Sprintf(`{"name":"%s","mime":"%s","size":0,"ref":"%s"}`, strings.Repeat("é", 128), strings.Repeat("é", 64), strings.Repeat("é", 64))
	line = strings.NewReplacer("msg:18", strings.Repeat("é", 64), "user:3", strings.Repeat("u", 128), "Jo", strings.Repeat("é", 128), "room:8", strings.Repeat("r", 128), `"text":""`, `"text":"`+strings.Repeat("é", MaxEventText/2)+`"`, `[{"name":"notes.txt","mime":"text/plain","size":0,"ref":"file:42"}]`, repeatedJSON(attachment, MaxAttachments)).Replace(line)
	for _, chat := range []string{ChatPrivate, ChatGroup, ChatChannel} {
		if _, err := ParsePluginLineV2([]byte(replace(line, "group", chat)), KindChannel); err != nil {
			t.Fatal(err)
		}
	}
	for _, stamp := range []string{"2016-12-31t23:59:60z", "2026-01-15T10:30:00.123456789+23:59"} {
		line := string(channelFixture(t, "plugin/inbound-minimal"))
		if _, err := ParsePluginLineV2([]byte(replace(line, "2026-01-15T10:30:00Z", stamp)), KindChannel); err != nil {
			t.Fatal(err)
		}
	}
	line = string(channelFixture(t, "host/deliver"))
	var buttons []string
	for i := range MaxButtons {
		buttons = append(buttons, fmt.Sprintf(`{"id":"%064d","label":"%s"}`, i, strings.Repeat("é", 32)))
	}
	line = strings.NewReplacer("d1", strings.Repeat("_", 128), "hello", strings.Repeat("a", MaxEventText), `{"id":"ok","label":"Confirm"}`, strings.Join(buttons, ",")).Replace(line)
	if _, err := ParseHostLineV2([]byte(line), KindChannel); err != nil {
		t.Fatal(err)
	}
	line = string(channelFixture(t, "plugin/ready-channel"))
	line = strings.NewReplacer(`"name":"echo","label"`, `"name":"9`+strings.Repeat("-", 63)+`","label"`, "Echo chat", strings.Repeat("é", 64)).Replace(line)
	if _, err := ParsePluginLineV2([]byte(line), KindChannel); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{StateConnected, StateReconnecting, StateUnauthorised, StateStopped} {
		line := string(channelFixture(t, "plugin/status"))
		line = strings.NewReplacer("reconnecting", state, "waiting for service", strings.Repeat("é", 512)).Replace(line)
		if _, err := ParsePluginLineV2([]byte(line), KindChannel); err != nil {
			t.Fatal(err)
		}
	}
}

func TestChannelMessageTextControls(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		parse   func([]byte, string) (any, error)
	}{
		{"plugin/inbound-minimal", ParsePluginLineV2},
		{"host/deliver", ParseHostLineV2},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			line := string(channelFixture(t, tc.fixture))
			for r := rune(0); r <= 0xa0; r++ {
				changed := replace(line, "hello", fmt.Sprintf(`a\u%04xb`, r))
				_, err := tc.parse([]byte(changed), KindChannel)
				allowed := r == '\t' || r == '\n' || r == '\r' || r >= ' ' && r < 0x7f || r == 0xa0
				if (err == nil) != allowed {
					t.Fatalf("text containing %U: allowed=%t, err=%v", r, allowed, err)
				}
			}
		})
	}
}

func TestV2ProblemCodesAndEnvelope(t *testing.T) {
	for _, code := range []string{CodeRateLimited, CodeUnauthorised} {
		line := []byte(replace(errorLine, "bad_request", code))
		if _, err := ParsePluginLineV2(line, KindChannel); err != nil {
			t.Fatal(err)
		}
		result := []byte(fmt.Sprintf(`{"v":1,"seq":2,"type":"result","id":"c1","status":"failed","error":{"code":"%s","message":"service unavailable"}}`, code))
		for _, frame := range [][]byte{line, result} {
			if _, err := ParsePluginLineV2(frame, KindCapability); err != nil {
				t.Fatalf("v2 capability rejected %s: %v", frame, err)
			}
			if _, err := ParsePluginLine(frame); err == nil {
				t.Fatalf("v1 accepted %s", frame)
			}
		}
	}
	for _, fixture := range []string{"inbound", "delivered", "delivery-failed", "status"} {
		line := string(channelFixture(t, "plugin/"+fixture))
		for _, bad := range []string{replace(line, `"v":1`, `"v":2`), replace(line, `"seq":`, `"seq":-`)} {
			if _, err := ParsePluginLineV2([]byte(bad), KindChannel); err == nil {
				t.Fatalf("accepted %s", bad)
			}
		}
	}
}

func TestNegotiatedKindScopesFrames(t *testing.T) {
	for _, kind := range []string{"", KindCapability} {
		if _, err := ParseHostLineV2([]byte(callLine), kind); err != nil {
			t.Fatal(err)
		}
		if _, err := ParsePluginLineV2([]byte(resultLine), kind); err != nil {
			t.Fatal(err)
		}
		if _, err := ParseHostLineV2(channelFixture(t, "host/ack"), kind); err == nil {
			t.Fatal("capability accepted ack")
		}
		if _, err := ParsePluginLineV2(channelFixture(t, "plugin/status"), kind); err == nil {
			t.Fatal("capability accepted status")
		}
	}
	for _, line := range []string{callLine, cancelLine, `{"type":"future"}`} {
		if _, err := ParseHostLineV2([]byte(line), KindChannel); err == nil {
			t.Fatalf("channel accepted %s", line)
		}
	}
	for _, line := range []string{eventLine, resultLine, `{"type":"future"}`} {
		if _, err := ParsePluginLineV2([]byte(line), KindChannel); err == nil {
			t.Fatalf("channel accepted %s", line)
		}
	}
	if _, err := ParseHostLine(channelFixture(t, "host/ack")); err == nil {
		t.Fatal("v1 accepted ack")
	}
	if _, err := ParsePluginLine(channelFixture(t, "plugin/status")); err == nil {
		t.Fatal("v1 accepted status")
	}
}
