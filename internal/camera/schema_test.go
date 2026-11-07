package camera

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// setPath builds a partial config {"a":{"b":v}} for path "a.b".
func setPath(path string, v any) []byte {
	parts := strings.Split(path, ".")
	b, _ := json.Marshal(map[string]any{parts[0]: map[string]any{parts[1]: v}})
	return b
}

// TestSchemaMatchesValidate checks that every schema field exists in Config
// and that its range or values are exactly what Validate accepts.
func TestSchemaMatchesValidate(t *testing.T) {
	doc := Schema()
	if err := Validate(doc.Defaults, nil); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
	seen := map[string]bool{}
	for _, fd := range doc.Fields {
		seen[fd.Path] = true
		try := func(v any) error {
			c, err := Merge(doc.Defaults, setPath(fd.Path, v))
			if err != nil {
				t.Fatalf("%s: merge %v: %v", fd.Path, v, err)
			}
			return Validate(c, nil)
		}
		switch fd.Type {
		case "int", "float":
			step := 1.0
			if fd.Type == "float" {
				step = 0.001
			}
			for _, v := range []float64{*fd.Min, *fd.Max} {
				if err := try(v); err != nil {
					t.Errorf("%s: %v rejected: %v", fd.Path, v, err)
				}
			}
			for _, v := range []float64{*fd.Min - step, *fd.Max + step} {
				if err := try(v); err == nil {
					t.Errorf("%s: %v accepted, schema range is [%v, %v]", fd.Path, v, *fd.Min, *fd.Max)
				}
			}
		case "enum":
			for _, v := range fd.Values {
				if err := try(v); err != nil {
					t.Errorf("%s: %q rejected: %v", fd.Path, v, err)
				}
			}
			if err := try("nonsense"); err == nil {
				t.Errorf("%s: unknown value accepted", fd.Path)
			}
		case "size":
			for _, v := range fd.Sizes {
				if err := try(v); err != nil {
					t.Errorf("%s: %v rejected: %v", fd.Path, v, err)
				}
			}
			if err := try([2]int{100, 100}); err == nil {
				t.Errorf("%s: unknown size accepted", fd.Path)
			}
		case "bool":
			if err := try(true); err != nil {
				t.Errorf("%s: %v", fd.Path, err)
			}
		default:
			t.Errorf("%s: unknown type %q", fd.Path, fd.Type)
		}
	}
	// Every leaf of Config must be described.
	var m map[string]map[string]any
	b, _ := json.Marshal(doc.Defaults)
	_ = json.Unmarshal(b, &m)
	for sec, kv := range m {
		for k := range kv {
			if p := fmt.Sprintf("%s.%s", sec, k); !seen[p] {
				t.Errorf("config field %s missing from Schema()", p)
			}
		}
	}
}
