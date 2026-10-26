package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// NewAgyBackend drives Antigravity's agy CLI, not its IDE launcher.
func NewAgyBackend() *CLIBackend {
	return &CLIBackend{
		name: "agy",
		candidates: func() []string {
			if runtime.GOOS == "windows" {
				if local := os.Getenv("LOCALAPPDATA"); local != "" {
					return []string{filepath.Join(local, "agy", "bin", "agy.exe")}
				}
				return nil
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return nil
			}
			return []string{filepath.Join(home, ".local", "bin", "agy")}
		},
		promptStdin: true,
		encodePrompt: func(prompt string) string {
			data, _ := json.Marshal(map[string]any{"event": "user", "message": map[string]string{"content": prompt}})
			return string(data) + "\n"
		},
		args: func(t Task, _ string) []string {
			// Stream input selects print mode and avoids Windows argument limits.
			a := []string{"--input-format", "stream-json", "--output-format", "stream-json"}
			switch t.Mode {
			case ModeReadOnly, ModeAsk:
				a = append(a, "--mode", "plan")
			case ModeAuto:
				a = append(a, "--mode", "accept-edits")
			case ModeYolo:
				a = append(a, "--dangerously-skip-permissions")
			}
			if t.Model != "" {
				a = append(a, "--model", t.Model)
			}
			if t.Effort != "" {
				a = append(a, "--effort", t.Effort)
			}
			if t.ForkSession != "" {
				a = append(a, "--conversation", t.ForkSession)
			}
			return a
		},
		// The Google sign-in lives on disk; these pick the account, the ADC
		// sign-in and its Cloud project, and where agy keeps its state.
		ownEnv: []string{"AGY_ACCOUNT", "AGY_ADC_AUTH", "ANTIGRAVITY_GEMINI_DIR", "ANTIGRAVITY_APP_DATA_DIR",
			"GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_QUOTA_PROJECT", "GOOGLE_CLOUD_LOCATION",
			"GOOGLE_CLOUD_REGION", "GOOGLE_GENAI_USE_VERTEXAI", "GOOGLE_GENAI_USE_ENTERPRISE"},
		newParse: newAgyParse,
		end:      agyStreamEnded,
		// Agy uses the fork hook but resumes its session in place.
		forks:          true,
		resumesInPlace: true,
		mapsModes:      []Mode{ModeReadOnly, ModeAuto, ModeYolo},
		mapsModel:      true,
		mapsEffort:     true,
		limits:         true,
	}
}

type agyUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	CacheRead    int64 `json:"cache_read_tokens"`
}

type agyStep struct {
	ConversationID string    `json:"conversation_id"`
	Model          string    `json:"model"`
	StepType       string    `json:"step_type"`
	State          string    `json:"state"`
	TextDelta      string    `json:"text_delta"`
	Usage          *agyUsage `json:"usage"`
}

func newAgyParse() func([]byte, func(Event), *Result) bool {
	var answer strings.Builder
	return func(line []byte, emit func(Event), res *Result) bool {
		var l struct {
			Event          string  `json:"event"`
			ConversationID string  `json:"conversation_id"`
			Model          string  `json:"model"`
			Init           agyStep `json:"init"`
			Step           agyStep `json:"step_update"`
			Result         struct {
				ConversationID string    `json:"conversation_id"`
				Model          string    `json:"model"`
				Status         string    `json:"status"`
				Message        string    `json:"message"`
				Response       string    `json:"response"`
				NumTurns       int       `json:"num_turns"`
				Usage          *agyUsage `json:"usage"`
			} `json:"result"`
		}
		if json.Unmarshal(line, &l) != nil || l.Event == "" {
			return false
		}
		session, model := l.ConversationID, l.Model
		now := time.Now()
		switch l.Event {
		case "init":
			if l.Init.ConversationID != "" {
				session = l.Init.ConversationID
			}
			if l.Init.Model != "" {
				model = l.Init.Model
			}
		case "step_update":
			if l.Step.ConversationID != "" {
				session = l.Step.ConversationID
			}
			if l.Step.Model != "" {
				model = l.Step.Model
			}
			// A user_input step carries the prompt back; it is not the answer.
			if l.Step.TextDelta != "" && l.Step.StepType != "user_input" {
				emit(Event{Time: now, Kind: EventText, Text: l.Step.TextDelta, Partial: true})
				if l.Step.StepType == "agent_response" {
					answer.WriteString(l.Step.TextDelta)
					res.Final = answer.String()
				}
			}
			if l.Step.State == "DONE" && l.Step.Usage != nil {
				u := l.Step.Usage
				res.addUsage("exclusive_cache", u.InputTokens, u.OutputTokens, u.CacheRead, 0)
			}
		case "result":
			r := l.Result
			if r.ConversationID != "" {
				session = r.ConversationID
			}
			if r.Model != "" {
				model = r.Model
			}
			if r.Response != "" {
				res.Final = r.Response
			}
			res.Turns = r.NumTurns
			res.Status = StatusOK
			if r.Status != "SUCCESS" {
				res.Status = StatusError
				res.Error = "agy result status: " + r.Status
				if r.Message != "" {
					res.Error += ": " + r.Message
				}
			}
			res.InputTokens, res.OutputTokens, res.CacheRead = 0, 0, 0
			if r.Usage != nil {
				res.InputTokens, res.OutputTokens, res.CacheRead = r.Usage.InputTokens, r.Usage.OutputTokens, r.Usage.CacheRead
				// output_tokens already includes thinking_tokens.
				res.Usage = &UsageMetadata{InputSemantics: "exclusive_cache", Status: "reported", CostStatus: "missing"}
			}
			emit(Event{Time: now, Kind: EventText, Text: r.Response, Streamed: answer.Len() > 0})
		}
		if session != "" {
			res.Session = session
		}
		if model != "" {
			res.Model, res.ModelReported = model, true
		}
		return true
	}
}

// Without a result event, only finished steps supply usage.
func agyStreamEnded(emit func(Event), res *Result, _ bool) {
	if res.Status != "" {
		return
	}
	res.Usage = &UsageMetadata{InputSemantics: "exclusive_cache", Status: "missing", CostStatus: "missing"}
	emit(Event{Time: time.Now(), Kind: EventInfo, Text: "agy exited without a result event: the answer is what it streamed, and the usage is only what its finished steps reported"})
}
