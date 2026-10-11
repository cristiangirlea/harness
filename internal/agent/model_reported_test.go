package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestAPIRecordsOnlyModelsReportedByTheResponse(t *testing.T) {
	for _, model := range []string{"claude-opus-5", "served-model", ""} {
		t.Run("reported="+model, func(t *testing.T) {
			turn := textTurn("done")
			turn[0] = strings.ReplaceAll(turn[0], `"model":"claude-opus-5"`, `"model":`+fmt.Sprintf("%q", model))
			res, _, _ := runFake(t, [][]string{turn}, Task{Prompt: "work", Dir: t.TempDir(), Options: Options{Model: "claude-opus-5", Mode: ModeReadOnly}})
			wantModel := model
			if wantModel == "" {
				wantModel = "claude-opus-5"
			}
			if res.Status != StatusOK || res.Model != wantModel || res.ModelReported != (model != "") {
				t.Fatalf("reported %q: result %+v", model, res)
			}
		})
	}
}

func TestHelperCLIModels(t *testing.T) {
	adapter := os.Getenv("HRN_MODEL_HELPER")
	if adapter == "" {
		return
	}
	model := os.Getenv("HRN_HELPER_REPORTED_MODEL")
	switch adapter {
	case "claude":
		fmt.Printf("{\"type\":\"system\",\"subtype\":\"init\",\"model\":%q}\n", model)
		fmt.Println(`{"type":"result","result":"done"}`)
	case "gemini":
		models := map[string]any{}
		if model != "" {
			models[model] = map[string]any{}
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"response": "done", "stats": map[string]any{"models": models}})
	case "codex":
		fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"done"}}`)
	case "opencode":
		fmt.Println(`{"type":"text","part":{"type":"text","text":"done"}}`)
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func TestCLIsDistinguishReportedModelsFromRequestedModels(t *testing.T) {
	for _, newBackend := range []func() *CLIBackend{NewClaudeBackend, NewGeminiBackend, NewCodexBackend, NewOpencodeBackend} {
		adapter := newBackend().Name()
		for _, model := range []string{"requested-model", "served-model", ""} {
			t.Run(adapter+"/reported="+model, func(t *testing.T) {
				t.Setenv("HRN_MODEL_HELPER", adapter)
				t.Setenv("HRN_HELPER_REPORTED_MODEL", model)
				b := newBackend()
				b.name, b.candidates = os.Args[0], nil
				b.args = func(Task, string) []string { return []string{"-test.run=^TestHelperCLIModels$", "--"} }
				res := b.Run(context.Background(), Task{Prompt: "work", Dir: t.TempDir(), Options: Options{Model: "requested-model"}}, func(Event) {})
				wantReported := model != "" && (adapter == "claude" || adapter == "gemini")
				wantModel := "requested-model"
				if wantReported {
					wantModel = model
				}
				if res.Status != StatusOK || res.Model != wantModel || res.ModelReported != wantReported {
					t.Fatalf("%s reported %q: result %+v", adapter, model, res)
				}
			})
		}
	}
}

func TestModelReportedIsAdditiveAndOmittedWhenFalse(t *testing.T) {
	var old Result
	if err := json.Unmarshal([]byte(`{"backend":"old","model":"requested","status":"ok","turns":1,"duration_ns":42}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.ModelReported || old.Model != "requested" || old.Duration != 42 {
		t.Fatalf("old result: %+v", old)
	}
	data, err := json.Marshal(old)
	if err != nil || strings.Contains(string(data), "model_reported") {
		t.Fatalf("false must be omitted: %s %v", data, err)
	}
	old.ModelReported = true
	data, err = json.Marshal(old)
	if err != nil || !strings.Contains(string(data), `"model_reported":true`) {
		t.Fatalf("true must be saved: %s %v", data, err)
	}
}
