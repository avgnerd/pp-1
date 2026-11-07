package pathutil

import "testing"

func TestRelPreservesOriginalPathAcrossCaseFoldLengthChanges(t *testing.T) {
	cases := []struct {
		name   string
		p      string
		prefix string
		want   string
	}{
		{name: "kelvin mount from ascii scope", p: "/k", prefix: "/K", want: "/"},
		{name: "kelvin descendant from ascii scope", p: "/k/child", prefix: "/K", want: "/child"},
		{name: "ascii mount from kelvin scope", p: "/K", prefix: "/k", want: "/"},
		{name: "ascii descendant from kelvin scope", p: "/K/child", prefix: "/k", want: "/child"},
		{name: "turkish dotted I exact", p: "/İ", prefix: "/i", want: "/"},
		{name: "turkish dotted I descendant", p: "/İ/child", prefix: "/i", want: "/child"},
		{name: "turkish dotted I reverse exact", p: "/i", prefix: "/İ", want: "/"},
		{name: "ordinary exact", p: "/path/to", prefix: "/path/to", want: "/"},
		{name: "ordinary descendant", p: "/path/to/child", prefix: "/path/to", want: "/child"},
		{name: "root", p: "/anything", prefix: "/", want: "/anything"},
		{name: "trailing slash exact", p: "/path/to/", prefix: "/path/to/", want: "/"},
		{name: "control byte preserved", p: "/path/\x01", prefix: "/path", want: "/\x01"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !HasPrefix(tc.p, tc.prefix) {
				t.Fatalf("HasPrefix(%q, %q) = false; case does not exercise Rel", tc.p, tc.prefix)
			}
			if got := Rel(tc.p, tc.prefix); got != tc.want {
				t.Fatalf("Rel(%q, %q) = %q, want %q", tc.p, tc.prefix, got, tc.want)
			}
		})
	}
}
