package plugin

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/cristiangirlea/harness/internal/pluginproto"
)

func init() {
	if os.Getenv("ECHO_CHANNEL_HANDSHAKE_HELPER") != "1" {
		return
	}
	line, err := bufio.NewReader(os.Stdin).ReadBytes('\n')
	if err != nil {
		os.Exit(1)
	}
	frame, err := pluginproto.ParseHostLine(line)
	hello, ok := frame.(*pluginproto.Hello)
	if err != nil || !ok || !slices.Equal(hello.Protocols, []string{"v1"}) {
		os.Exit(1)
	}
	switch hello.Host.Name {
	case "refusal":
		fmt.Println(`{"v":1,"seq":1,"type":"error","code":"unsupported_protocol","message":"no common protocol","protocols":["v2"]}`)
		os.Exit(2)
	case "repeat":
		fmt.Println(`{"v":1,"seq":1,"type":"error","code":"unsupported_protocol","message":"no common protocol","protocols":["v2","v2"]}`)
		os.Exit(2)
	case "capability":
		fmt.Println(`{"v":1,"seq":1,"type":"ready","protocol":"v2","plugin":{"name":"echo","version":"1"},"capabilities":[{"name":"echo"}]}`)
	case "ready":
		fmt.Println(`{"v":1,"seq":1,"type":"ready","protocol":"v2","plugin":{"name":"echo","version":"1"},"kind":"channel","channel":{"name":"echo"},"capabilities":[]}`)
	case "kind":
		fmt.Println(`{"v":1,"seq":1,"type":"ready","protocol":"v1","plugin":{"name":"echo","version":"1"},"kind":"channel","capabilities":[]}`)
	}
	os.Exit(0)
}

func TestChannelPluginNeedsAChannelHost(t *testing.T) {
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ECHO_CHANNEL_HANDSHAKE_HELPER", "1")
	for _, reply := range []string{"refusal", "repeat", "ready", "kind", "capability"} {
		t.Run(reply, func(t *testing.T) {
			session, err := Start(context.Background(), Found{Name: "echo", Path: path}, Options{
				Host: pluginproto.Program{Name: reply, Version: "1"},
			})
			if err == nil {
				session.Close()
				t.Fatal("hrn accepted a channel plugin")
			}
			for _, part := range []string{"channel plugin", "channel host", "hrn"} {
				if !strings.Contains(err.Error(), part) {
					t.Fatalf("missing %q in %v", part, err)
				}
			}
		})
	}
}
