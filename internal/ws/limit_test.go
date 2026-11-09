package ws

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSlotLimitPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HRN_HOME", home)
	f := newFixture(t)
	ctx := context.Background()
	check := func(want int) {
		t.Helper()
		got, err := RangeBeyondSessions(ctx, f.repo, Limit)
		if err != nil || got != want {
			t.Errorf("range = %d, %v; want %d", got, err, want)
		}
	}
	check(Limit)
	f.write(home, "ws.json", `{"max_slots":4}`)
	check(4)
	f.write(f.repo, ".hrn/ws.json", `{"max_slots":2}`)
	check(2)
	f.write(home, "ws.json", `{broken`)
	check(2)
}

func TestSlotLimitInvalidFiles(t *testing.T) {
	for _, scope := range []string{"repository", "machine"} {
		for _, body := range []string{`{`, `{"max_slots":0}`, `{"max_slots":11}`, `{"max_slots":-1}`, `{}`, `{"max_slots":1.5}`, `{"max_slots":"3"}`, `null`, "directory"} {
			t.Run(scope+"/"+body, func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("HRN_HOME", home)
				f := newFixture(t)
				dir := home
				if scope == "repository" {
					dir = filepath.Join(f.repo, ".hrn")
					f.write(home, "ws.json", `{"max_slots":3}`)
				}
				path := filepath.Join(dir, "ws.json")
				if body == "directory" {
					if err := os.MkdirAll(path, 0o755); err != nil {
						t.Fatal(err)
					}
				} else {
					f.write(dir, "ws.json", body)
				}
				ctx := context.Background()
				_, takeErr := Take(ctx, TakeOptions{Repo: f.repo, NoFetch: true})
				_, rangeErr := RangeBeyondSessions(ctx, f.repo, 1)
				_, listErr := List(ctx, f.repo)
				for name, err := range map[string]error{"take": takeErr, "range": rangeErr, "list": listErr} {
					if err == nil || !strings.Contains(err.Error(), path) {
						t.Errorf("%s error = %v; want path %s", name, err, path)
					}
				}
			})
		}
	}
}

func TestSlotLimitTakeAndSessionRange(t *testing.T) {
	t.Setenv("HRN_HOME", t.TempDir())
	f := newFixture(t)
	f.write(f.repo, ".hrn/ws.json", `{"max_slots":2}`)
	path := filepath.Join(f.repo, ".hrn", "ws.json")
	ctx := context.Background()
	opt := TakeOptions{Repo: f.repo, Max: 3, NoFetch: true}
	var above *MaxError
	if _, err := Take(ctx, opt); !errors.As(err, &above) || !strings.Contains(err.Error(), "allows 2 slots") || !strings.Contains(err.Error(), path) {
		t.Errorf("above limit: %v", err)
	}
	opt.Max = 1
	first, err := Take(ctx, opt)
	if err != nil || first.N != 1 {
		t.Fatalf("narrowed take: %+v, %v", first, err)
	}
	opt.Max = 0
	second, err := Take(ctx, opt)
	if err != nil || second.N != 2 {
		t.Fatalf("default take: %+v, %v", second, err)
	}
	_, err = Take(ctx, opt)
	var full *NoSlotError
	if !errors.As(err, &full) || full.Max != 2 || !strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "allow more") {
		t.Errorf("full range: %v", err)
	}
	if _, err := os.Stat(filepath.Join(SlotsDir(f.repo), "3")); !os.IsNotExist(err) {
		t.Errorf("slot 3 must not exist: %v", err)
	}
	for _, repo := range []string{f.repo, first.Path} {
		if got, err := RangeBeyondSessions(ctx, repo, DefaultMax); err != nil || got != 2 {
			t.Errorf("session range = %d, %v; want 2", got, err)
		}
	}
	f.write(f.repo, ".hrn/ws.json", `{"max_slots":1}`)
	rows, err := List(ctx, f.repo)
	if err != nil || len(rows) != 2 || rows[1].N != 2 || rows[1].State != "taken" {
		t.Errorf("list above limit: %+v, %v", rows, err)
	}
	// A slot held above a lowered limit must still come back.
	if _, err := Free(ctx, second.Path); err != nil {
		t.Fatalf("free above limit: %v", err)
	}
	if _, err := Free(ctx, first.Path); err != nil {
		t.Fatal(err)
	}
}
