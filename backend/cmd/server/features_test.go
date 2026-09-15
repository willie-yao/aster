package main

import (
	"testing"
)

func TestFormatPricingRate(t *testing.T) {
	for _, test := range []struct {
		value string
		want  string
	}{
		{value: "3.000000", want: "3.00"},
		{value: "0.305", want: "0.31"},
		{value: "0.304", want: "0.30"},
	} {
		if got := formatPricingRate(test.value); got != test.want {
			t.Errorf("formatPricingRate(%q) = %q, want %q", test.value, got, test.want)
		}
	}
}
