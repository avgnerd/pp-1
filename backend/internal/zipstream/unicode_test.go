package zipstream

import (
	"context"
	"testing"
	"time"
	"unicode/utf8"
)

// A folder name whose Unicode lower-case form has a different UTF-8 byte length must still yield
// entry names relative to that folder. pathutil.Rel derives the offset from segment count for this
// reason; slicing at len(strings.ToLower(dbx)) cuts inside a character or past the path end.
func TestExpandEntryNamesSurviveCaseFoldLengthChange(t *testing.T) {
	cases := []struct {
		name string
		root string
		file string
		want string
	}{
		{"lowercase grows three code points", "/\u023E\u023E\u023E", "f", "m/f"},
		{"lowercase shrinks KELVIN SIGN", "/\u212A", "hello.txt", "m/hello.txt"},
		{"lowercase shrinks capital sharp s", "/\u1E9E", "hello.txt", "m/hello.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, fs := newTestServiceAt(t, "zip-unicode", tc.root)
			fs.Account("zip-unicode").Put(tc.root+"/"+tc.file, []byte("x"), time.Unix(1700000000, 0).UTC())
			files, err := Expand(context.Background(), svc, []string{"/m"}, 10)
			if err != nil {
				t.Fatalf("expand: %v", err)
			}
			if len(files) != 1 {
				t.Fatalf("got %d entries, want 1", len(files))
			}
			if !utf8.ValidString(files[0].ZipName) {
				t.Fatalf("entry name %q is not valid UTF-8", files[0].ZipName)
			}
			if files[0].ZipName != tc.want {
				t.Fatalf("entry name = %q, want %q", files[0].ZipName, tc.want)
			}
		})
	}
}
