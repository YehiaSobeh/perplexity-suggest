package httpapi

import (
	"strings"
	"testing"
)

func TestValidateQuery(t *testing.T) {
	tests := []struct {
		name    string
		q       string
		wantErr bool
	}{
		{"empty", "", true},
		{"short ascii", "hello", false},
		{"exactly 200 runes", strings.Repeat("a", 200), false},
		{"201 runes", strings.Repeat("a", 201), true},
		{"cyrillic", "где купить", false},
		{"cjk", "東京で買う", false},
		{"emoji", "buy a 🌮 in tokyo", false},
		{"200 emojis (each rune)", strings.Repeat("🌮", 200), false},
		{"invalid utf-8", string([]byte{0xff, 0xfe, 0xfd}), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateQuery(tc.q)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateQuery(%q) err = %v, wantErr = %v", tc.q, err, tc.wantErr)
			}
		})
	}
}
