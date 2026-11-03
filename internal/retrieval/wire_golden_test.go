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

func TestKnowledgeWireGolden(t *testing.T) {
	values := map[string]any{"scope": &Scope{}, "entry": &Entry{}, "import": &Import{}, "version": &Version{}, "admission": &Admission{}, "search": &Search{}, "packet": &Packet{}, "change": &Change{}, "active": &Active{}, "record": &Record{}}
	for name, ptr := range values {
		t.Run(name, func(t *testing.T) {
			filledWire(reflect.ValueOf(ptr).Elem())
			got, err := json.Marshal(ptr)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", "wire", name+".json")
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
