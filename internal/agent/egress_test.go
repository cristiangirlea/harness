package agent

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

var egressVars = []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy", "NODE_USE_ENV_PROXY"}

// TestHelperEgressCLI is not a test: the test binary acts as an agent CLI that
// reports its proxy variables and asks its proxy for a host the list refuses.
func TestHelperEgressCLI(t *testing.T) {
	if os.Getenv("HRN_EGRESS_HELPER") != "1" {
		return
	}
	// Windows keeps one spelling of a name, so the report is what a lookup sees.
	got := map[string]string{}
	for _, k := range egressVars {
		if v, ok := os.LookupEnv(k); ok {
			got[k] = v
		}
	}
	got["connect"] = "none"
	if u, err := url.Parse(os.Getenv("HTTPS_PROXY")); err == nil && strings.HasPrefix(u.Host, "127.0.0.1:") {
		got["proxy"] = u.Host
		got["connect"] = connectStatus(u.Host, "denied.example.org:443", u.User)
		got["noauth"] = connectStatus(u.Host, "denied.example.org:443", nil)
	}
	report, _ := json.Marshal(got)
	out, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "num_turns": 1, "result": string(report)})
	fmt.Println(string(out))
	os.Exit(0)
}

// connectStatus asks proxy for target, with the credentials of user when it is not nil, as a client reading the proxy URL would.
func connectStatus(proxy, target string, user *url.Userinfo) string {
	c, err := net.DialTimeout("tcp", proxy, 5*time.Second)
	if err != nil {
		return err.Error()
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	auth := ""
	if user != nil {
		pass, _ := user.Password()
		auth = "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(user.Username()+":"+pass)) + "\r\n"
	}
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s\r\n", target, target, auth)
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
	if err != nil {
		return err.Error()
	}
	return fmt.Sprint(resp.StatusCode)
}

func egressHelper() *CLIBackend {
	return &CLIBackend{
		name:        os.Args[0],
		promptStdin: true,
		args:        func(Task, string) []string { return []string{"-test.run=^TestHelperEgressCLI$", "--"} },
		parse:       parseClaudeStream,
	}
}

func runEgressHelper(t *testing.T, opts Options) (map[string]string, []Event) {
	t.Helper()
	return runEgressHelperTask(t, Task{Options: opts})
}

func runEgressHelperTask(t *testing.T, task Task) (map[string]string, []Event) {
	t.Helper()
	t.Setenv("HRN_EGRESS_HELPER", "1")
	task.Prompt, task.Dir = "go", t.TempDir()
	var events []Event
	res := egressHelper().Run(context.Background(), task, func(e Event) { events = append(events, e) })
	if res.Status != StatusOK {
		t.Fatalf("run %s: %s", res.Status, res.Error)
	}
	got := map[string]string{}
	if err := json.Unmarshal([]byte(res.Final), &got); err != nil {
		t.Fatalf("helper said %q: %v", res.Final, err)
	}
	return got, events
}

func infoTexts(events []Event) []string {
	var out []string
	for _, e := range events {
		if e.Kind == EventInfo {
			out = append(out, e.Text)
		}
	}
	return out
}

// The child sees the run's proxy in every proxy variable and not the
// operator's own, the proxy refuses what is not on the list, the run records
// the refusal, and the proxy is gone when the run ends.
func TestAnEgressRunPointsTheCLIAtItsOwnProxy(t *testing.T) {
	egressDefaults[os.Args[0]] = egressDefault{hosts: []string{"api.example.com"}, node: true}
	t.Cleanup(func() { delete(egressDefaults, os.Args[0]) })
	for _, opts := range []Options{
		{Mode: ModeYolo, Egress: []string{"allowed.example.org", "api.example.com"}},
		{Mode: ModeYolo, Egress: []string{"allowed.example.org"}, InheritEnv: true},
		{Mode: ModeYolo, Egress: []string{"allowed.example.org"}, AllowDangerous: true, InheritEnv: true},
	} {
		t.Run(fmt.Sprintf("inherit=%v,dangerous=%v", opts.InheritEnv, opts.AllowDangerous), func(t *testing.T) {
			t.Setenv("HTTPS_PROXY", "http://operator-proxy.example:3128")
			t.Setenv("HTTP_PROXY", "http://operator-proxy.example:3128")
			t.Setenv("NO_PROXY", "*")
			got, events := runEgressHelper(t, opts)
			if got["proxy"] == "" {
				t.Fatalf("the child saw no proxy of the run's own: %v", got)
			}
			want := got["HTTPS_PROXY"]
			checkRunProxyURL(t, want, got["proxy"])
			if u, err := url.Parse(want); err == nil {
				pass, _ := u.User.Password()
				for _, e := range events {
					if pass != "" && strings.Contains(e.Text, pass) {
						t.Errorf("an event carries the proxy token: kind %v", e.Kind)
					}
				}
			}
			for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "https_proxy", "http_proxy", "all_proxy"} {
				if got[k] != want {
					t.Errorf("%s=%q, want %q", k, got[k], want)
				}
			}
			for _, k := range []string{"NO_PROXY", "no_proxy"} {
				if v, ok := got[k]; ok {
					t.Errorf("%s=%q reached the child; want it cleared", k, v)
				}
			}
			if got["NODE_USE_ENV_PROXY"] != "1" {
				t.Errorf("NODE_USE_ENV_PROXY=%q, want 1 for a Node CLI", got["NODE_USE_ENV_PROXY"])
			}
			if got["connect"] != "403" {
				t.Errorf("a host off the list got %s from the proxy with the child's credentials, want 403", got["connect"])
			}
			if got["noauth"] != "407" {
				t.Errorf("a request without the child's credentials got %s, want 407", got["noauth"])
			}
			info := infoTexts(events)
			if !slices.Contains(info, "egress: advisory; 2 hosts allowed; a process that ignores proxy variables is not stopped") {
				t.Errorf("no advisory line in %q", info)
			}
			if !slices.Contains(info, "egress: refused denied.example.org:443 (not allowed)") {
				t.Errorf("the refusal is not recorded in %q", info)
			}
			if c, err := net.DialTimeout("tcp", got["proxy"], time.Second); err == nil {
				c.Close()
				t.Error("the proxy outlived the run")
			}
		})
	}
}

// checkRunProxyURL wants raw to name the proxy at host with the run's user and a token.
func checkRunProxyURL(t *testing.T, raw, host string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("proxy URL: %v", err)
	}
	if pass, _ := u.User.Password(); u.Scheme != "http" || u.User.Username() != "hrn" || len(pass) != 64 || u.Host != host {
		t.Errorf("proxy URL scheme %q, user %q, %d-character token, host %q; want http, hrn, 64 and %s", u.Scheme, u.User.Username(), len(pass), u.Host, host)
	}
}

func TestProxyVariablesFromTheRunsChildEnvAreReplacedToo(t *testing.T) {
	egressDefaults[os.Args[0]] = egressDefault{hosts: []string{"api.example.com"}}
	t.Cleanup(func() { delete(egressDefaults, os.Args[0]) })
	got, _ := runEgressHelperTask(t, Task{
		Options:  Options{Mode: ModeYolo, Egress: []string{"allowed.example.org"}},
		ChildEnv: []string{"HTTPS_PROXY=http://child-proxy.example:3128", "all_proxy=socks5://child-proxy.example:1080", "NO_PROXY=*"},
	})
	if got["proxy"] == "" {
		t.Fatalf("the child saw no proxy of the run's own: %v", got)
	}
	for _, k := range []string{"HTTPS_PROXY", "ALL_PROXY", "all_proxy"} {
		checkRunProxyURL(t, got[k], got["proxy"])
	}
	if v, ok := got["NO_PROXY"]; ok {
		t.Errorf("NO_PROXY=%q from the child environment reached the child", v)
	}
	if got["connect"] != "403" {
		t.Errorf("the child's proxy answered %s, want 403 for a host off the list", got["connect"])
	}
}

func TestTheCodexGateServerIsHandedTheProxyVariables(t *testing.T) {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NODE_USE_ENV_PROXY"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	names := codexGateEnvNames(Task{Options: Options{Mode: ModeYolo, Egress: []string{"allowed.example.org"}}})
	for _, k := range egressProxyNames {
		if !slices.Contains(names, k) {
			t.Errorf("%s is not handed to the gate server: %v", k, names)
		}
	}
	names = codexGateEnvNames(Task{Options: Options{Mode: ModeYolo}})
	if slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, "HTTPS_PROXY") }) {
		t.Errorf("a run without egress names a proxy variable the operator did not set: %v", names)
	}
}

func TestWithoutEgressTheProxyVariablesAreTheOperators(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://operator-proxy.example:3128")
	t.Setenv("NO_PROXY", "internal.example")
	got, events := runEgressHelper(t, Options{Mode: ModeYolo})
	if got["HTTPS_PROXY"] != "http://operator-proxy.example:3128" || got["NO_PROXY"] != "internal.example" || got["connect"] != "none" {
		t.Errorf("the child saw %v; want the operator's proxy untouched", got)
	}
	if _, ok := got["NODE_USE_ENV_PROXY"]; ok {
		t.Errorf("NODE_USE_ENV_PROXY was set without egress")
	}
	for _, text := range infoTexts(events) {
		if strings.HasPrefix(text, "egress:") {
			t.Errorf("a run without egress said %q", text)
		}
	}
}

func TestEgressRefusalsAreDeduplicatedAndCapped(t *testing.T) {
	var r egressRefusals
	for range 3 {
		r.add("a.example:443", "not allowed")
	}
	for i := range maxEgressRefusals + 5 {
		r.add(fmt.Sprintf("h%d.example:443", i), "not allowed")
	}
	var got []string
	r.report(func(e Event) { got = append(got, e.Text) })
	if len(got) != maxEgressRefusals+1 || got[0] != "egress: refused a.example:443 (not allowed)" {
		t.Fatalf("report %q", got)
	}
	if last := got[len(got)-1]; last != "egress: 6 more refusals not listed" {
		t.Errorf("last line %q", last)
	}
}

func TestEachBackendsEgressDefaults(t *testing.T) {
	for name, want := range map[string]egressDefault{
		"claude":   {hosts: []string{"api.anthropic.com"}, node: true, unmeasured: true},
		"codex":    {hosts: []string{"chatgpt.com", "api.openai.com", "auth.openai.com"}, unmeasured: true},
		"gemini":   {node: true, unmeasured: true},
		"agy":      {unmeasured: true},
		"opencode": {unmeasured: true},
	} {
		got, ok := egressDefaults[name]
		if !ok || !slices.Equal(got.hosts, want.hosts) || got.node != want.node || got.unmeasured != want.unmeasured {
			t.Errorf("%s: %+v, want %+v", name, got, want)
		}
	}
	if got := egressHosts("codex", []string{"api.openai.com", "pypi.org"}); !slices.Equal(got, []string{"api.openai.com", "pypi.org", "chatgpt.com", "auth.openai.com"}) {
		t.Errorf("merged %q", got)
	}
}

func TestWithEgressProxyReplacesEverySpelling(t *testing.T) {
	env := []string{"PATH=/bin", "https_proxy=http://op:1", "Http_Proxy=http://op:1", "ALL_PROXY=socks5://op:2", "No_Proxy=*", "no_proxy=*", "NODE_USE_ENV_PROXY=0"}
	got := withEgressProxy(env, "http://127.0.0.1:9", false)
	want := []string{"PATH=/bin", "HTTPS_PROXY=http://127.0.0.1:9", "https_proxy=http://127.0.0.1:9", "HTTP_PROXY=http://127.0.0.1:9", "http_proxy=http://127.0.0.1:9", "ALL_PROXY=http://127.0.0.1:9", "all_proxy=http://127.0.0.1:9"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := withEgressProxy(env, "http://127.0.0.1:9", true); got[len(got)-1] != "NODE_USE_ENV_PROXY=1" {
		t.Errorf("a Node CLI got %q", got)
	}
}

func TestARefusalWithoutATargetNamesItsReason(t *testing.T) {
	var r egressRefusals
	r.add("", "malformed request")
	var got []string
	r.report(func(e Event) { got = append(got, e.Text) })
	if !slices.Equal(got, []string{"egress: refused a request (malformed request)"}) {
		t.Errorf("report %q", got)
	}
}
