package handler

import (
	"reflect"
	"testing"
)

func TestSanitizeProviderPriorities(t *testing.T) {
	tests := []struct {
		name string
		in   []uint
		want []uint
	}{
		{"nil to empty", nil, []uint{}},
		{"empty stays empty", []uint{}, []uint{}},
		{"dedupe keeps first position", []uint{3, 3, 1}, []uint{3, 1}},
		{"drops zero id", []uint{0, 5}, []uint{5}},
		{"preserves order", []uint{1, 2, 3}, []uint{1, 2, 3}},
		{"only zeros to empty", []uint{0, 0}, []uint{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeProviderPriorities(tt.in)
			if got == nil {
				t.Fatal("expected non-nil result")
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("sanitizeProviderPriorities(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
