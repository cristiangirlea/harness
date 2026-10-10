package main

import (
	"flag"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/watch"
)

func TestLanTakesNoValueOrAnAdapter(t *testing.T) {
	for _, tc := range []struct {
		args []string
		on   bool
		only string
	}{
		{nil, false, ""},
		{[]string{"--lan"}, true, ""},
		{[]string{"--lan=Ethernet 3"}, true, "Ethernet 3"},
		{[]string{"--lan=192.168.1.218"}, true, "192.168.1.218"},
		{[]string{"--lan=false"}, false, ""},
	} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		var lan lanFlag
		fs.Var(&lan, "lan", "")
		if err := fs.Parse(tc.args); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if lan.on != tc.on || lan.only != tc.only {
			t.Errorf("%v: on=%v only=%q; want %v %q", tc.args, lan.on, lan.only, tc.on, tc.only)
		}
	}
}

func TestWatchDrawsTheQRCodeOfItsOnlyNetwork(t *testing.T) {
	one := []watch.Endpoint{{URL: "http://192.168.1.218:8089/", Interface: "Ethernet 3"}}
	var b strings.Builder
	printWatchStart(&b, "127.0.0.1:8089", true, one)
	if out := b.String(); !strings.Contains(out, "http://192.168.1.218:8089/ (Ethernet 3)") || !strings.Contains(out, "█") {
		t.Errorf("one network: the link and its QR code; got\n%s", out)
	}
	b.Reset()
	printWatchStart(&b, "127.0.0.1:8089", true, append(one, watch.Endpoint{URL: "http://172.22.176.1:8089/", Interface: "vEthernet (WSL)"}))
	if out := b.String(); strings.Contains(out, "█") || !strings.Contains(out, "QR codes for each are on http://127.0.0.1:8089/") {
		t.Errorf("several networks: no code here, a pointer to the page; got\n%s", out)
	}
	b.Reset()
	printWatchStart(&b, "127.0.0.1:8089", false, nil)
	if out := b.String(); strings.Contains(out, "█") || !strings.Contains(out, "this computer only") {
		t.Errorf("without --lan: got\n%s", out)
	}
}
