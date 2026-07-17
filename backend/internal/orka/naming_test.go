package orka

import "testing"

func TestFailureHash_DeterministicAndMessageNormalized(t *testing.T) {
	a := FailureHash("[It] creates a cluster", "timed out   waiting\nfor node")
	b := FailureHash("[It] creates a cluster", "timed out waiting for node")
	if a != b {
		t.Fatalf("whitespace-only message differences must hash equal: %q != %q", a, b)
	}
	if got := FailureHash("[It] creates a cluster", "different error"); got == a {
		t.Fatalf("different messages must hash differently")
	}
	if len(a) != 12 { // 6 bytes hex-encoded
		t.Fatalf("hash length = %d, want 12", len(a))
	}
}

func TestTaskName_ShapeAndValidity(t *testing.T) {
	name := TaskName("2075040971715776512", "7d9bbb65bce4", "v1")
	if want := "az-2075040971715776512-7d9bbb65bce4-v1"; name != want {
		t.Fatalf("TaskName = %q, want %q", name, want)
	}
}

func TestPatternTaskName_Deterministic(t *testing.T) {
	a := PatternTaskName("periodic-a", "prompt", "v1")
	b := PatternTaskName("periodic-a", "prompt", "v1")
	if a != b || len(a) == 0 {
		t.Fatalf("PatternTaskName = %q and %q, want equal non-empty names", a, b)
	}
	if c := PatternTaskName("periodic-a", "different", "v1"); c == a {
		t.Fatalf("different prompts must have different names: %q", c)
	}
	if c := PatternTaskName("periodic-b", "prompt", "v1"); c == a {
		t.Fatalf("different jobs must have different names: %q", c)
	}
	if c := PatternTaskName("periodic-a", "prompt", "v2"); c == a {
		t.Fatalf("different versions must have different names: %q", c)
	}
}

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
