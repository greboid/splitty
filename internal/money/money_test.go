package money

import "testing"

func TestParseAndFormat(t *testing.T) {
	cases := []struct {
		in   string
		want Amount
		str  string
	}{
		{"0", 0, "£0.00"},
		{"12", 1200, "£12.00"},
		{"12.5", 1250, "£12.50"},
		{"12.34", 1234, "£12.34"},
		{"-3.99", -399, "-£3.99"},
		{"+1.10", 110, "£1.10"},
		{" 4.20 ", 420, "£4.20"},
		{"1,234.56", 123456, "£1234.56"},
		{"0.01", 1, "£0.01"},
		{"-0.01", -1, "-£0.01"},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Parse(%q) = %d, want %d", c.in, got, c.want)
		}
		if f := Format(got, "£"); f != c.str {
			t.Errorf("Format(%d) = %q, want %q", got, f, c.str)
		}
	}
	bad := []string{"", "abc", "1.234", "99.999", "- 1", "..", "1..2"}
	for _, s := range bad {
		if v, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) expected error, got %d", s, v)
		}
	}
	if FormatSigned(150, "£") != "+£1.50" {
		t.Errorf("FormatSigned pos: %q", FormatSigned(150, "£"))
	}
	if FormatSigned(-150, "£") != "-£1.50" {
		t.Errorf("FormatSigned neg: %q", FormatSigned(-150, "£"))
	}
}

func TestAllocateEven(t *testing.T) {
	cases := []struct {
		total Amount
		n     int
		want  []Amount
	}{
		{100, 3, []Amount{34, 33, 33}},
		{100, 4, []Amount{25, 25, 25, 25}},
		{1, 3, []Amount{1, 0, 0}},
		{-100, 3, []Amount{-34, -33, -33}},
		{0, 2, []Amount{0, 0}},
		{101, 1, []Amount{101}},
	}
	for _, c := range cases {
		got := Allocate(c.total, c.n)
		if len(got) != len(c.want) {
			t.Fatalf("Allocate(%d,%d) length %d", c.total, c.n, len(got))
		}
		var sum Amount
		for i := range got {
			sum += got[i]
			if got[i] != c.want[i] {
				t.Errorf("Allocate(%d,%d)[%d] = %d, want %d", c.total, c.n, i, got[i], c.want[i])
			}
		}
		if sum != c.total {
			t.Errorf("Allocate(%d,%d) sums to %d", c.total, c.n, sum)
		}
	}
	if got := Allocate(100, 0); got != nil {
		t.Errorf("Allocate with n=0 should be nil, got %v", got)
	}
}

func TestAllocateByWeights(t *testing.T) {
	cases := []struct {
		name    string
		total   Amount
		weights []int64
		want    []Amount
	}{
		{"proportional", 100, []int64{1, 1, 1}, []Amount{34, 33, 33}},
		{"weights", 1000, []int64{1, 3}, []Amount{250, 750}},
		{"remainder exact", 10, []int64{1, 1, 1}, []Amount{4, 3, 3}},
		{"negative total", -1000, []int64{1, 3}, []Amount{-250, -750}},
		{"uneven", 999, []int64{2, 1}, []Amount{666, 333}},
		{"zero weight participant", 300, []int64{2, 0, 1}, []Amount{200, 0, 100}},
	}
	for _, c := range cases {
		got := AllocateByWeights(c.total, c.weights)
		if len(got) != len(c.want) {
			t.Fatalf("%s: length %d", c.name, len(got))
		}
		var sum Amount
		for i := range got {
			sum += got[i]
			if got[i] != c.want[i] {
				t.Errorf("%s: [%d] = %d, want %d", c.name, i, got[i], c.want[i])
			}
		}
		if sum != c.total {
			t.Errorf("%s: sums to %d, want %d", c.name, sum, c.total)
		}
	}
	if got := AllocateByWeights(100, []int64{0, 0}); got != nil {
		t.Errorf("zero weights should be nil, got %v", got)
	}
	if got := AllocateByWeights(100, []int64{1, -1}); got != nil {
		t.Errorf("negative weights should be nil, got %v", got)
	}
}
