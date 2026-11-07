package pathutil

import "testing"

func TestMountsConflict(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"/path/to", "/path/to", true},
		{"/path/to", "/PATH/To", true},
		{"/path/to", "/path", true},
		{"/path/to", "/path/to/x", true},
		{"/path/to", "/path/to2", false},
		{"/path/to", "/path/t", false},
		{"/a", "/b", false},
		{"/media/movies", "/media/anime", false},
	}
	for _, c := range cases {
		if got := MountsConflict(c.a, c.b); got != c.want {
			t.Errorf("MountsConflict(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestNormalizeMount(t *testing.T) {
	ok := map[string]string{
		"/path/to":       "/path/to",
		"/path//to/":     "/path/to",
		"/영화/2024":       "/영화/2024",
		" /media/x ":     "/media/x",
		"/path/./to":     "/path/to",
	}
	for in, want := range ok {
		got, err := NormalizeMount(in)
		if err != nil || got != want {
			t.Errorf("NormalizeMount(%q)=%q,%v want %q", in, got, err, want)
		}
	}
	bad := []string{"", "path/to", "/", "/../x", "/a/../../b", "/admin", "/API/x", "/search", "/assets/x", "/robots.txt", "/a?b", "/a#b", "/a\x00b", "/a/ b"}
	for _, in := range bad {
		if _, err := NormalizeMount(in); err == nil {
			t.Errorf("NormalizeMount(%q) should fail", in)
		}
	}
}

func TestCleanRejectsTraversal(t *testing.T) {
	if _, err := Clean("/a/../../etc"); err == nil {
		t.Fatal("expected error")
	}
	if c, _ := Clean(`\a\b`); c != "/a/b" {
		t.Fatal(c)
	}
}

func TestRel(t *testing.T) {
	if Rel("/path/to", "/path/to") != "/" || Rel("/path/to/x/y", "/path/to") != "/x/y" {
		t.Fatal("Rel")
	}
	if Join("/Videos", "/x/y") != "/Videos/x/y" || Join("/", "/") != "/" {
		t.Fatal("Join")
	}
}
