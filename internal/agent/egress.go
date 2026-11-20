package agent

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// egressDefault is what a backend's CLI needs with --egress: the hosts it signs in and talks to the model through, and whether it runs on Node.
type egressDefault struct {
	hosts []string
	// node CLIs read the proxy variables only with NODE_USE_ENV_PROXY=1.
	node bool
	// unmeasured stays set until a real proxied run confirms the list.
	unmeasured bool
}

// egressDefaults is the one table of backend hosts; docs/backends.md mirrors it.
var egressDefaults = map[string]egressDefault{
	"claude":   {hosts: []string{"api.anthropic.com"}, node: true, unmeasured: true},
	"codex":    {hosts: []string{"chatgpt.com", "api.openai.com", "auth.openai.com"}, unmeasured: true},
	"gemini":   {node: true, unmeasured: true},
	"agy":      {unmeasured: true},
	"opencode": {unmeasured: true},
}

// egressHosts is the run's own hosts followed by the backend's, each once.
func egressHosts(backend string, own []string) []string {
	out := slices.Clone(own)
	for _, h := range egressDefaults[backend].hosts {
		if !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

var egressEnv = []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "NO_PROXY", "NODE_USE_ENV_PROXY"}

// withEgressProxy replaces every proxy variable, in either case, with the run's proxy and clears NO_PROXY.
func withEgressProxy(env []string, proxyURL string, node bool) []string {
	out := slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return slices.Contains(egressEnv, strings.ToUpper(k))
	})
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY"} {
		out = append(out, k+"="+proxyURL, strings.ToLower(k)+"="+proxyURL)
	}
	if node {
		out = append(out, "NODE_USE_ENV_PROXY=1")
	}
	return out
}

const maxEgressRefusals = 20

// egressRefusals collects the proxy's refusals, each target and reason once, for the run's events.
type egressRefusals struct {
	mu    sync.Mutex
	seen  []string
	extra int
}

func (r *egressRefusals) add(target, reason string) {
	if target == "" {
		target = "a request"
	}
	line := fmt.Sprintf("%s (%s)", target, reason)
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case slices.Contains(r.seen, line):
	case len(r.seen) < maxEgressRefusals:
		r.seen = append(r.seen, line)
	default:
		r.extra++
	}
}

// report emits the refusals once the proxy has stopped, so no event races the CLI's stream.
func (r *egressRefusals) report(emit func(Event)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range r.seen {
		emit(Event{Time: time.Now(), Kind: EventInfo, Text: "egress: refused " + line})
	}
	if r.extra > 0 {
		emit(Event{Time: time.Now(), Kind: EventInfo, Text: fmt.Sprintf("egress: %d more refusals not listed", r.extra)})
	}
}
