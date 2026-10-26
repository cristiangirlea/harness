package agent

import (
	"encoding/json"
	"strings"
	"time"
)

// NewGeminiBackend drives the Gemini CLI.
func NewGeminiBackend() *CLIBackend {
	return &CLIBackend{
		name:        "gemini",
		mapsModes:   []Mode{ModeAuto, ModeYolo},
		mapsModel:   true,
		candidates:  func() []string { return npmShim("gemini") },
		promptStdin: true,
		// Which Cloud project and sign-in route the Google login uses.
		ownEnv: []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION", "GOOGLE_GENAI_USE_VERTEXAI", "GOOGLE_GENAI_USE_GCA"},
		args: func(t Task, _ string) []string {
			a := []string{"-o", "json"}
			if t.Model != "" {
				a = append(a, "-m", t.Model)
			}
			mode := map[Mode]string{ModeReadOnly: "default", ModeAsk: "default", ModeAuto: "auto_edit", ModeYolo: "yolo"}[t.Mode]
			return append(a, "--approval-mode", mode)
		},
		finalize: func(out []byte, emit func(Event), res *Result) {
			// stdout may carry log lines before the JSON document.
			start := strings.IndexByte(string(out), '{')
			var doc struct {
				Response string `json:"response"`
				Error    *struct {
					Message string `json:"message"`
				} `json:"error"`
				Stats struct {
					Models map[string]struct {
						Tokens struct {
							Prompt     int64 `json:"prompt"`
							Candidates int64 `json:"candidates"`
							Cached     int64 `json:"cached"`
						} `json:"tokens"`
					} `json:"models"`
				} `json:"stats"`
			}
			if start < 0 || json.Unmarshal(out[start:], &doc) != nil {
				res.Final = strings.TrimSpace(string(out))
				emit(Event{Time: time.Now(), Kind: EventText, Text: res.Final})
				return
			}
			if doc.Error != nil {
				res.Status, res.Error = StatusError, doc.Error.Message
			}
			for name, m := range doc.Stats.Models {
				res.Model = name
				res.ModelReported = true
				// The prompt count includes the cached tokens.
				res.addUsage("inclusive_cache", m.Tokens.Prompt, m.Tokens.Candidates, m.Tokens.Cached, 0)
			}
			res.Final = doc.Response
			emit(Event{Time: time.Now(), Kind: EventText, Text: doc.Response})
		},
	}
}
