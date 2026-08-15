package tools

import (
	"encoding/json"
	"testing"
)

func TestFlexInt_UnmarshalJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
		err  bool
	}{
		{"number", `5`, 5, false},
		{"quoted number", `"5"`, 5, false},
		{"quoted with spaces", `" 200 "`, 200, false},
		{"zero", `0`, 0, false},
		{"quoted zero", `"0"`, 0, false},
		{"negative number", `-3`, -3, false},
		{"quoted negative", `"-3"`, -3, false},
		{"float", `5.0`, 5, false},
		{"quoted float", `"5.0"`, 5, false},
		{"null", `null`, 0, false},
		{"empty string", `""`, 0, false},
		{"non-numeric", `"abc"`, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var n FlexInt
			err := json.Unmarshal([]byte(tc.in), &n)
			if tc.err {
				if err == nil {
					t.Fatalf("expected error for %q", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.in, err)
			}
			if n.Int() != tc.want {
				t.Errorf("FlexInt(%q) = %d, want %d", tc.in, n.Int(), tc.want)
			}
		})
	}
}

func TestFlexInt_InStruct(t *testing.T) {
	var args struct {
		Lines       FlexInt `json:"lines"`
		ContextLine FlexInt `json:"context_lines"`
	}
	// Mixed numeric and string-encoded values.
	if err := json.Unmarshal([]byte(`{"lines":"2000","context_lines":5}`), &args); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if args.Lines.Int() != 2000 || args.ContextLine.Int() != 5 {
		t.Errorf("got lines=%d context_lines=%d, want 2000/5", args.Lines.Int(), args.ContextLine.Int())
	}
}
