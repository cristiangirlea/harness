package retrieval

import (
	"strings"
	"testing"
)

func TestPacketRespectsFramingCapAndExecutorDropsCredentials(t *testing.T) {
	text := strings.Repeat("x", MaxPacketBytes-len(ReferencePrefix)-len(ReferenceSuffix))
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
