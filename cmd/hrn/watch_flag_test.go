package main

import (
	"flag"
	"io"
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
		{[]string{"--lan=Local Area Connection"}, true, "Local Area Connection"},
		{[]string{"--lan=192.168.1.40"}, true, "192.168.1.40"},
		{[]string{"--lan=false"}, false, ""},
		{[]string{"--lan=TRUE"}, true, ""},
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

// An empty --lan= (an unset variable, say) must not quietly mean every
// adapter, VPNs included.
func TestAnEmptyLanIsRefused(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var lan lanFlag
	fs.Var(&lan, "lan", "")
	if err := fs.Parse([]string{"--lan="}); err == nil {
		t.Fatalf("--lan= parsed as on=%v only=%q; want an error", lan.on, lan.only)
	}
}

func TestAnAdapterAfterASpaceIsPointedToTheEqualsSign(t *testing.T) {
	err := cmdWatch([]string{"--lan", "Local Area Connection"})
	if err == nil || !strings.Contains(err.Error(), `--lan="Local Area Connection"`) {
		t.Fatalf("err = %v; want the --lan=NAME form suggested", err)
	}
}

func TestWatchDrawsTheQRCodeOfItsOnlyNetwork(t *testing.T) {
	one := []watch.Endpoint{{URL: "http://192.168.1.40:8089/", Interface: "Ethernet"}}
	var b strings.Builder
	printWatchStart(&b, "127.0.0.1:8089", true, one, false)
	if out := b.String(); !strings.Contains(out, "http://192.168.1.40:8089/ (Ethernet)") || !strings.Contains(out, "█") || strings.Contains(out, "\x1b[") {
		t.Errorf("one network, no colour: the link and its QR code; got\n%s", out)
	}
	b.Reset()
	printWatchStart(&b, "127.0.0.1:8089", true, append(one, watch.Endpoint{URL: "http://172.20.0.1:8089/", Interface: "vEthernet (WSL)"}), false)
	if out := b.String(); strings.Contains(out, "█") || !strings.Contains(out, "QR codes for each are on http://127.0.0.1:8089/") {
		t.Errorf("several networks: no code here, a pointer to the page; got\n%s", out)
	}
	b.Reset()
	printWatchStart(&b, "127.0.0.1:8089", false, nil, false)
	if out := b.String(); strings.Contains(out, "█") || !strings.Contains(out, "this computer only") {
		t.Errorf("without --lan: got\n%s", out)
	}
}

// On a terminal the code is drawn white on black whatever the theme: a light
// theme would otherwise invert it, and not every reader copes.
func TestTheTerminalCodeForcesItsColours(t *testing.T) {
	var b strings.Builder
	printWatchStart(&b, "127.0.0.1:8089", true, []watch.Endpoint{{URL: "http://192.168.1.40:8089/", Interface: "Ethernet"}}, true)
	for _, line := range strings.Split(b.String(), "\n") {
		if strings.ContainsAny(line, "█▀▄") && (!strings.HasPrefix(line, "\x1b[97;40m") || !strings.HasSuffix(line, "\x1b[0m")) {
			t.Fatalf("a line of the code is not white on black: %q", line)
		}
	}
}
