package retry

import "testing"

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
	}{
		{value: "0", want: 0},
		{value: "3", want: 3},
	} {
		got, err := Parse(tc.value)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.value, err)
		}
		if got != tc.want {
			t.Fatalf("Parse(%q) = %d, want %d", tc.value, got, tc.want)
		}
	}
	if _, err := Parse("many"); err == nil {
		t.Fatal("invalid retry count was accepted")
	}
}
