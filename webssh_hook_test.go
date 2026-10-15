//go:build webssh

package main

import "testing"

func TestSplitOrigins(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"*.example.com", []string{"*.example.com"}},
		{" a.com ,  b.com ,", []string{"a.com", "b.com"}},
	} {
		got := splitOrigins(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("splitOrigins(%q) = %v, want %v", tc.in, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("splitOrigins(%q) = %v, want %v", tc.in, got, tc.want)
			}
		}
	}
}
