package retrieval

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func filledWire(v reflect.Value) {
	if v.Type() == reflect.TypeOf(time.Time{}) {
		v.Set(reflect.ValueOf(time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)))
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		filledWire(v.Elem())
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			filledWire(v.Field(i))
		}
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		filledWire(v.Index(0))
	case reflect.String:
		v.SetString("fixture")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int64:
		v.SetInt(7)
	case reflect.Float64:
		v.SetFloat(1.25)
	}
}

func TestKnowledgeWireMatchesTheGoldenJSON(t *testing.T) {
	values := map[string]any{"scope": &Scope{}, "entry": &Entry{}, "import": &Import{}, "version": &Version{}, "admission": &Admission{}, "search": &Search{}, "packet": &Packet{}, "change": &Change{}, "active": &Active{}, "record": &Record{}}
	for name, value := range values {
		for _, zero := range []bool{false, true} {
			ptr := reflect.New(reflect.TypeOf(value).Elem()).Interface()
			fixtureName := name
			if zero {
				fixtureName += ".zero"
			} else {
				filledWire(reflect.ValueOf(ptr).Elem())
			}
			t.Run(fixtureName, func(t *testing.T) {
				got, err := json.Marshal(ptr)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join("testdata", "wire", fixtureName+".json")
				if os.Getenv("HRN_UPDATE_WIRE_FIXTURES") == "1" {
					if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, got, 0644); err != nil {
						t.Fatal(err)
					}
				}
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("wire mismatch: got %s, want %s", got, want)
				}
				out := reflect.New(reflect.TypeOf(ptr).Elem()).Interface()
				dec := json.NewDecoder(bytes.NewReader(want))
				dec.DisallowUnknownFields()
				if err := dec.Decode(out); err != nil {
					t.Fatal(err)
				}
				if err := dec.Decode(new(any)); err != io.EOF {
					t.Fatalf("trailing data: %v", err)
				}
				if !reflect.DeepEqual(ptr, out) {
					t.Fatalf("round trip differs: %#v %#v", ptr, out)
				}
			})
		}
	}
}

func TestPacketRankingProvenance(t *testing.T) {
	var p Packet
	if err := json.Unmarshal([]byte(`{"ranking":"current"}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.VersionRanking != "" || p.RankingMismatch {
		t.Fatalf("legacy packet: %+v", p)
	}
	for _, mismatch := range []bool{false, true} {
		p.VersionRanking, p.RankingMismatch = "older", mismatch
		data, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["version_ranking"]) != `"older"` || (len(fields["ranking_mismatch"]) > 0) != mismatch {
			t.Fatalf("ranking fields: %s", data)
		}
		var got Packet
		if err := json.Unmarshal(data, &got); err != nil || got.VersionRanking != "older" || got.RankingMismatch != mismatch {
			t.Fatalf("round trip: %+v %v", got, err)
		}
	}
	p.VersionRanking = ""
	data, err := json.Marshal(p)
	if err != nil || bytes.Contains(data, []byte(`"version_ranking"`)) {
		t.Fatalf("empty ranking: %s %v", data, err)
	}
}
