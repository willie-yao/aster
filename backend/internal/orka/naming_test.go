package orka

import "testing"

func TestSanitize_RFC1123(t *testing.T) {
	cases := map[string]string{
		"Foo_Bar.Baz":            "foo-bar-baz",
		"a//b__c":                "a-b-c",
		"-leading-and-trailing-": "leading-and-trailing",
		"UPPER Case":             "upper-case",
	}
	for in, want := range cases {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}
