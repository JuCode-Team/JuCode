package region

import "testing"

func TestParseUnits(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int64
	}{
		{"0", 0}, {"1", 1_000_000}, {"1.234567", 1_234_567}, {"-2.5", -2_500_000},
	} {
		got, err := ParseUnits(tc.value)
		if err != nil || got != tc.want {
			t.Fatalf("ParseUnits(%q) = %d, %v; want %d", tc.value, got, err, tc.want)
		}
	}
	if _, err := ParseUnits("0.0000001"); err == nil {
		t.Fatal("expected precision error")
	}
}

func TestFormatUnits(t *testing.T) {
	if got := FormatUnits(1_234_567); got != "1.234567" {
		t.Fatalf("got %q", got)
	}
}

func TestParseUnitsRounded(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int64
	}{
		{"0.00006408", 64},
		{"0.00000049", 0},
		{"0.00000050", 1},
		{"1.23456789", 1_234_568},
	} {
		got, err := ParseUnitsRounded(tc.value)
		if err != nil || got != tc.want {
			t.Fatalf("ParseUnitsRounded(%q) = %d, %v; want %d", tc.value, got, err, tc.want)
		}
	}
}
