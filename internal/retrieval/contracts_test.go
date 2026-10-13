package retrieval

import (
	"fmt"
	"strings"
	"testing"
)

func TestPacketRespectsFramingCapAndExecutorDropsCredentials(t *testing.T) {
	label := "[Reference e_fixture; source=\"source\"; revision=main; evidence=\"evidence\"]\n"
	closer := "\n[/Reference]\n"
	text := label + strings.Repeat("x", MaxPacketBytes-len(ReferencePrefix)-len(ReferenceSuffix)-len(label)-len(closer)) + closer
	p := Packet{Text: text, Bytes: len(text), Digest: TextDigest(text), VersionID: "v_fixture", Entries: []Entry{{ID: "e_fixture"}}}
	if e := ValidatePacket(p, "", ""); e != nil {
		t.Fatal(e)
	}
	p.Text += "x"
	p.Bytes++
	p.Digest = TextDigest(p.Text)
	if ValidatePacket(p, "", "") == nil {
		t.Fatal("framing cap ignored")
	}
	env := ExecutorEnv([]string{"PATH=fixture", "HRN_TENANT_TOKEN=secret", "hrn_operator_token=secret", "HRN_DATABASE_URL=private"})
	if len(env) != 1 || env[0] != "PATH=fixture" {
		t.Fatal("credential environment escaped")
	}
	if TextDigest("abc") != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatal("raw UTF8 digest contract")
	}
}

func TestPacketAcceptsOneServerLabelPairPerEntry(t *testing.T) {
	entries := []Entry{
		{ID: "e_first", Content: "first content", Source: "first source", Evidence: "first evidence"},
		{ID: "e_second", Content: "second content", Source: "second source", Evidence: "second evidence"},
	}
	var text strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&text, "[Reference %s; source=%q; revision=%s; evidence=%q]\n%s\n[/Reference]\n", e.ID, e.Source, "main", e.Evidence, e.Content)
	}
	p := Packet{VersionID: "v_fixture", Entries: entries, Text: text.String(), Bytes: text.Len(), Digest: TextDigest(text.String())}
	if err := ValidatePacket(p, "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestPacketRejectsExtraLabelsAndOuterFrameMarkersInText(t *testing.T) {
	baseText := "[Reference entry; source=\"source\"; revision=main; evidence=\"evidence\"]\ncontent\n[/Reference]\n"
	for _, marker := range []string{"<project-reference", "</project-reference>", "[Reference ", "[/Reference]"} {
		t.Run(marker, func(t *testing.T) {
			text := baseText + marker
			p := Packet{VersionID: "v_fixture", Text: text, Bytes: len(text), Digest: TextDigest(text), Entries: []Entry{{ID: "entry", Content: "content", Source: "source", Evidence: "evidence"}}}
			if err := ValidatePacket(p, "", ""); err == nil {
				t.Fatal("text marker accepted")
			}
		})
	}
}

func TestPacketRejectsFrameMarkersInEntryFields(t *testing.T) {
	for _, marker := range []string{"<project-reference", "</project-reference>", "[Reference ", "[/Reference]"} {
		t.Run(marker, func(t *testing.T) {
			text := "[Reference entry; source=\"source\"; revision=main; evidence=\"evidence\"]\ncontent\n[/Reference]\n"
			base := Packet{VersionID: "v_fixture", Text: text, Bytes: len(text), Digest: TextDigest(text), Entries: []Entry{{ID: "entry", Key: "key", Content: "content", Source: "source", Evidence: "evidence", Identifiers: []string{"identifier"}}}}
			for _, field := range []string{"id", "key", "content", "source", "evidence", "identifier"} {
				withEntry := base
				withEntry.Entries = append([]Entry(nil), base.Entries...)
				switch field {
				case "id":
					withEntry.Entries[0].ID += marker
				case "key":
					withEntry.Entries[0].Key += marker
				case "content":
					withEntry.Entries[0].Content += marker
				case "source":
					withEntry.Entries[0].Source += marker
				case "evidence":
					withEntry.Entries[0].Evidence += marker
				case "identifier":
					withEntry.Entries[0].Identifiers = []string{"identifier" + marker}
				}
				if err := ValidatePacket(withEntry, "", ""); err == nil {
					t.Errorf("%s marker accepted", field)
				}
			}
		})
	}
}
