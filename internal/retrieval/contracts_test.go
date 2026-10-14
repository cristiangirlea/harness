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

func TestPacketAcceptsImportLimitMetadataWithNearCapText(t *testing.T) {
	entry := Entry{ID: "entry", Key: strings.Repeat("k", 128), Source: "source", Evidence: "evidence", Identifiers: make([]string, 32)}
	for i := range entry.Identifiers {
		entry.Identifiers[i] = strings.Repeat("x", 256)
	}
	label := fmt.Sprintf("[Reference %s; source=%q; revision=main; evidence=%q]\n", entry.ID, entry.Source, entry.Evidence)
	closer := "\n[/Reference]\n"
	entry.Content = strings.Repeat("c", MaxPacketBytes-len(ReferencePrefix)-len(ReferenceSuffix)-len(label)-len(closer))
	text := label + entry.Content + closer
	p := Packet{VersionID: "v_fixture", Text: text, Bytes: len(text), Digest: TextDigest(text), Entries: []Entry{entry}}
	if err := ValidatePacket(p, "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestPacketRejectsOversizedEntryContent(t *testing.T) {
	text := "[Reference entry; source=\"source\"; revision=main; evidence=\"evidence\"]\nshort\n[/Reference]\n"
	p := Packet{VersionID: "v_fixture", Text: text, Bytes: len(text), Digest: TextDigest(text), Entries: []Entry{{ID: "entry", Content: strings.Repeat("x", 1<<20)}}}
	if err := ValidatePacket(p, "", ""); err == nil || !strings.Contains(err.Error(), "entries") || !strings.Contains(err.Error(), "8192") {
		t.Fatalf("oversized entry content: %v", err)
	}
}

func TestPacketRejectsCombinedEntryTextOverCap(t *testing.T) {
	text := "[Reference first; source=\"source\"; revision=main; evidence=\"evidence\"]\nshort\n[/Reference]\n" +
		"[Reference second; source=\"source\"; revision=main; evidence=\"evidence\"]\nshort\n[/Reference]\n"
	p := Packet{VersionID: "v_fixture", Text: text, Bytes: len(text), Digest: TextDigest(text), Entries: []Entry{
		{ID: "first", Content: strings.Repeat("x", MaxPacketBytes/2)},
		{ID: "second", Content: strings.Repeat("x", MaxPacketBytes/2)},
	}}
	if err := ValidatePacket(p, "", ""); err == nil || !strings.Contains(err.Error(), "entries") || !strings.Contains(err.Error(), "8192") {
		t.Fatalf("combined entry text over cap: %v", err)
	}
}

func TestPacketRejectsMetadataBeyondImportLimits(t *testing.T) {
	text := "[Reference entry; source=\"source\"; revision=main; evidence=\"evidence\"]\nshort\n[/Reference]\n"
	for _, tc := range []struct {
		name  string
		field string
		entry Entry
	}{
		{name: "key", field: "key", entry: Entry{ID: "entry", Key: strings.Repeat("x", 129), Content: "short"}},
		{name: "identifier count", field: "identifiers", entry: Entry{ID: "entry", Content: "short", Identifiers: make([]string, 33)}},
		{name: "identifier length", field: "identifier", entry: Entry{ID: "entry", Content: "short", Identifiers: []string{strings.Repeat("x", 257)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Packet{VersionID: "v_fixture", Text: text, Bytes: len(text), Digest: TextDigest(text), Entries: []Entry{tc.entry}}
			if err := ValidatePacket(p, "", ""); err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("oversized %s: %v", tc.name, err)
			}
		})
	}
}

func TestPacketRejectsEntryIDMissingFromText(t *testing.T) {
	text := "[Reference listed; source=\"source\"; revision=main; evidence=\"evidence\"]\nshort\n[/Reference]\n"
	p := Packet{VersionID: "v_fixture", Text: text, Bytes: len(text), Digest: TextDigest(text), Entries: []Entry{{ID: "unlisted", Content: "short"}}}
	if err := ValidatePacket(p, "", ""); err == nil || !strings.Contains(err.Error(), "entry id") {
		t.Fatalf("entry id missing from text: %v", err)
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
