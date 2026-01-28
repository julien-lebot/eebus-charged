package eebus

import (
	"testing"
)

func TestNormalizeSkI(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"Normal", "00-11-22-33", "00112233"},
		{"WithSpaces", "00 11 22 33", "00112233"},
		{"Mixed", "00-11 22-33", "00112233"},
		{"Empty", "", ""},
		{"NoSeparators", "00112233", "00112233"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeSkI(tt.input); got != tt.expected {
				t.Errorf("normalizeSkI() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func BenchmarkNormalizeSkI(b *testing.B) {
	input := "00-11-22-33-44-55-66-77-88-99-aa-bb-cc-dd-ee-ff"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		normalizeSkI(input)
	}
}
