package version

import "testing"

func TestNormalized(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"v1.2.3", "1.2.3"},
		{" 1.2.3 ", "1.2.3"},
		{"v1.2.3-beta", "1.2.3"},
		{"v1.2.3+build5", "1.2.3"},
		{"", "0.0.0"},
	}

	for _, tt := range tests {
		got := Normalized(tt.in)
		if got != tt.want {
			t.Fatalf("Normalized(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCompare(t *testing.T) {
	tests := []struct {
		a    string
		b    string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		{"1.0.0", "1.0.1", -1},
		{"1.2.0", "1.10.0", -1},
		{"v1.2.3-beta", "1.2.3", 0},
	}

	for _, tt := range tests {
		got := Compare(tt.a, tt.b)
		if got != tt.want {
			t.Fatalf("Compare(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
