package tools

import (
	"bytes"
	"fmt"
	"strconv"
)

// FlexInt is an int that unmarshals from either a JSON number or a numeric
// string. Float-shaped values are truncated toward zero. Weaker tool-using
// models frequently encode numeric arguments as strings; accepting both keeps
// those tool calls from wasting the model's investigation budget. null and
// empty decode to 0.
type FlexInt int

// UnmarshalJSON implements json.Unmarshaler for the lenient int.
func (n *FlexInt) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*n = 0
		return nil
	}
	// Strip surrounding quotes if the value came as a JSON string.
	if len(b) >= 2 && b[0] == '"' && b[len(b)-1] == '"' {
		b = b[1 : len(b)-1]
	}
	s := string(bytes.TrimSpace(b))
	if s == "" {
		*n = 0
		return nil
	}
	if v, err := strconv.Atoi(s); err == nil {
		*n = FlexInt(v)
		return nil
	}
	// Tolerate float-shaped values like "5.0" by truncating toward zero.
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		*n = FlexInt(int(f))
		return nil
	}
	return fmt.Errorf("invalid integer %q", s)
}

// Int returns the value as a plain int.
func (n FlexInt) Int() int { return int(n) }
