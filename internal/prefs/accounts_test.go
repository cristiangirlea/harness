package prefs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestAccountsPreferences(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	zero, negative, high := 0, -1, 91
	account := Account{Name: "team-a", Home: home}

	tooMany := make([]Account, 17)
	for i := range tooMany {
		tooMany[i] = Account{Name: fmt.Sprintf("seat-%d", i), Home: home}
	}
	valid := Prefs{Accounts: map[string][]Account{"claude": {account}, "codex": {account}}, AccountReservePercent: &zero}
	invalid := map[string]Prefs{
		"backend":          {Accounts: map[string][]Account{"api": {account}}},
		"name":             {Accounts: map[string][]Account{"claude": {{Name: "Bad", Home: home}}}},
		"long name":        {Accounts: map[string][]Account{"claude": {{Name: "abcdefghijklmnopqrstuvwxyz0123456", Home: home}}}},
		"duplicate":        {Accounts: map[string][]Account{"claude": {account, account}}},
		"relative":         {Accounts: map[string][]Account{"claude": {{Name: "team-a", Home: "relative"}}}},
		"nul":              {Accounts: map[string][]Account{"claude": {{Name: "team-a", Home: home + "\x00"}}}},
		"count":            {Accounts: map[string][]Account{"claude": tooMany}},
		"negative reserve": {Accounts: valid.Accounts, AccountReservePercent: &negative},
		"high reserve":     {Accounts: valid.Accounts, AccountReservePercent: &high},
		"no accounts":      {Accounts: map[string][]Account{"codex": {}}, AccountReservePercent: &zero},
	}
	checkInvalid := func(t *testing.T, bad Prefs) {
		if err := Save(dir, bad); err == nil {
			t.Fatal("Save accepted invalid preferences")
		}
		data, err := json.Marshal(bad)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "backends.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir); err == nil {
			t.Fatal("Load accepted invalid preferences")
		}
	}
	for name, bad := range invalid {
		t.Run(name, func(t *testing.T) { checkInvalid(t, bad) })
	}
	t.Run("Windows partial paths", func(t *testing.T) {
		if runtime.GOOS != "windows" {
			t.Skip("Windows path syntax")
		}
		for _, path := range []string{`C:x`, `\x`} {
			checkInvalid(t, Prefs{Accounts: map[string][]Account{"claude": {{Name: "team-a", Home: path}}}})
		}
	})
	if err := Save(dir, valid); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil || !reflect.DeepEqual(got, valid) || got.Reserve() != 0 || !reflect.DeepEqual(got.AccountsFor("codex"), []Account{account}) {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "backends.json"), []byte("{\"default\":\"claude\",\"enabled\":[\"claude\"]}"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err = Load(dir)
	if err != nil || got.Default() != "claude" || got.Reserve() != 10 || len(got.AccountsFor("claude")) != 0 {
		t.Fatalf("old file: %+v, %v", got, err)
	}
	for _, candidate := range []string{`C:\x`, "/x"} {
		valid.Accounts["claude"] = []Account{{Name: "native", Home: candidate}}
		if err := Save(dir, valid); (err == nil) != filepath.IsAbs(candidate) {
			t.Fatalf("absolute path validation: %v", err)
		}
	}
}
