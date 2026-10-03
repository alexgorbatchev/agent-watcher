//go:build darwin

package scanner

import "testing"

func TestParseComm(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"standard null terminated", []byte{'n', 'o', 'd', 'e', 0, 0, 0}, "node"},
		{"leading null", []byte{0, 'b', 'u', 'n', 0}, "bun"},
		{"no null full slice", []byte{'p', 'i'}, "pi"},
		{"empty slice", []byte{}, ""},
		{"all nulls", []byte{0, 0, 0, 0}, ""},
		{"claude binary", []byte{'c', 'l', 'a', 'u', 'd', 'e', 0}, "claude"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseComm(tt.in)
			if got != tt.want {
				t.Errorf("parseComm(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
