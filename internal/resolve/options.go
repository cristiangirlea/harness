package resolve

import (
	"fmt"
	"os"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/internal/purpose"
	"github.com/cristiangirlea/harness/internal/route/table"
)

// Route fills an unspecified backend/model/effort from evidence.
func Route(o *agent.Options, declared, prompt string, backend *string, home string) (*table.RoutedBy, error) {
	routed, _, err := RouteWithNote(o, declared, prompt, backend, home)
	return routed, err
}

// RouteWithNote preserves why an explicit or invalid choice bypassed routing.
func RouteWithNote(o *agent.Options, declared, prompt string, backend *string, home string) (*table.RoutedBy, string, error) {
	if *backend != "" || o.Model != "" || o.Effort != "" {
		if *backend != "" {
			return nil, "backend " + *backend + " was named for this run", nil
		}
		if o.Model != "" && o.Effort != "" {
			return nil, "model and effort were named for this run", nil
		}
		if o.Model != "" {
			return nil, "model " + o.Model + " was named for this run", nil
		}
		return nil, "effort " + o.Effort + " was named for this run", nil
	}
	t, err := table.Load(home)
	if err != nil {
		return nil, "", err
	}
	resolved := purpose.Resolve(declared, prompt)
	category := resolved.Category
	r, ok := t.Pick(category)
	if !ok {
		return nil, "no route row for purpose " + category, nil
	}
	if r.Backend != "" {
		p, err := BackendPreferences(home)
		if err != nil {
			return nil, "", err
		}
		if !p.Enabled(r.Backend) {
			note := fmt.Sprintf("ignoring route for %s: backend %s is disabled", category, r.Backend)
			fmt.Fprintln(os.Stderr, "hrn: warning: "+note)
			return nil, note, nil
		}
		if _, err := agent.Get(r.Backend); err != nil {
			note := fmt.Sprintf("ignoring route for %s: %v", category, err)
			fmt.Fprintln(os.Stderr, "hrn: warning: "+note)
			return nil, note, nil
		}
	}
	if err := (agent.Options{Effort: r.Effort}).Validate(); err != nil {
		note := fmt.Sprintf("ignoring route for %s: %v", category, err)
		fmt.Fprintln(os.Stderr, "hrn: warning: "+note)
		return nil, note, nil
	}
	if r.Backend == "" && r.Model == "" && r.Effort == "" {
		return nil, "no route row for purpose " + category, nil
	}
	*backend, o.Model, o.Effort = r.Backend, r.Model, r.Effort
	return &table.RoutedBy{Category: category, Declared: resolved.Rule == "declared", Eval: r.Eval, Set: r.Set}, "", nil
}

// ApplyTurnDefault sets the Claude review and test limit when no limit was supplied.
func ApplyTurnDefault(t *agent.Task, backend string) bool {
	prompt := t.Prompt
	if t.OriginalPrompt != "" {
		prompt = t.OriginalPrompt
	}
	category := purpose.Resolve(t.Purpose, prompt).Category
	if backend != "claude" || t.MaxTurns != 0 || (category != "review" && category != "test") {
		return false
	}
	t.MaxTurns = 30
	return true
}

// TurnDefaultInfo explains the default applied to a run.
const TurnDefaultInfo = "claude review and test runs default to 30 max turns"
