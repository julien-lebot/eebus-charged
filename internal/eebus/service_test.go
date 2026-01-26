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
		{
			name:     "no changes",
			input:    "0123456789abcdef",
			expected: "0123456789abcdef",
		},
		{
			name:     "with dashes",
			input:    "01-23-45-67-89-ab-cd-ef",
			expected: "0123456789abcdef",
		},
		{
			name:     "with spaces",
			input:    "01 23 45 67 89 ab cd ef",
			expected: "0123456789abcdef",
		},
		{
			name:     "mixed",
			input:    "01-23 45-67 89 ab-cd-ef",
			expected: "0123456789abcdef",
		},
		{
			name:     "empty",
			input:    "",
			expected: "",
		},
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
	input := "01-23-45-67-89-ab-cd-ef-01-23-45-67-89-ab-cd-ef-01-23-45-67-89-ab-cd-ef-01-23-45-67-89-ab-cd-ef"
	// A reasonably long SKI-like string to make allocations measurable
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		normalizeSkI(input)
	}
}
