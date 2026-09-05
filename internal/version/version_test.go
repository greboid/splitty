package version

import "testing"

func TestString(t *testing.T) {
	tests := []struct {
		name string
		v    Version
		want string
	}{
		{"short hash", Version{Revision: "abc1234"}, "abc1234"},
		{"long hash is abbreviated", Version{Revision: "56705db0123456789abcdef"}, "56705db"},
		{"dirty", Version{Revision: "56705db0123456789abcdef", Dirty: true}, "56705db-dirty"},
		{"no revision", Version{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.v.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}
