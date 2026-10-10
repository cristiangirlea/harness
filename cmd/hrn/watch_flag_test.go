package main

import (
	"flag"
	"testing"
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
