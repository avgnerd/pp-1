// Package pathutil normalises virtual paths and validates mount points.
package pathutil

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode"
)

// Reserved first segments cannot be used as mount roots because the SPA and API own them.
var Reserved = []string{"admin", "api", "login", "search", "download", "assets", "robots.txt", "favicon.ico", "favicon.svg", "index.html"}

// PathError is a user-facing invalid-path error (maps to HTTP 400).
type PathError struct{ Msg string }

func (e *PathError) Error() string { return e.Msg }

// Clean turns any user-supplied path into "/a/b" form. It rejects ".." segments instead of
// resolving them so callers cannot escape a mount, and rejects control characters.
func Clean(p string) (string, error) {
	p = strings.ReplaceAll(p, "\\", "/")
	segs := strings.Split(p, "/")
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		switch s {
		case "", ".":
			continue
		case "..":
			return "", &PathError{"경로에 '..'를 사용할 수 없습니다"}
		}
		for _, r := range s {
			if unicode.IsControl(r) {
				return "", &PathError{"경로에 제어 문자를 사용할 수 없습니다"}
			}
		}
		out = append(out, s)
	}
	return "/" + strings.Join(out, "/"), nil
}

// NormalizeMount validates a mount path: absolute, not root, no reserved first segment,
// segments without characters Dropbox and URLs handle badly.
func NormalizeMount(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("마운트 경로를 입력하세요 (예: /path/to)")
	}
	if !strings.HasPrefix(p, "/") {
		return "", errors.New("마운트 경로는 /로 시작해야 합니다 (예: /path/to)")
	}
	c, err := Clean(p)
	if err != nil {
		return "", err
	}
	if c == "/" {
		return "", errors.New("루트(/)는 마운트 경로로 쓸 수 없습니다")
	}
	for _, s := range strings.Split(c[1:], "/") {
		if strings.ContainsAny(s, `?#%*:<>|"`) {
			return "", fmt.Errorf("마운트 경로 '%s'에 ? # %% * : < > | \" 문자는 쓸 수 없습니다", s)
		}
		if strings.TrimSpace(s) != s {
			return "", errors.New("마운트 경로 구간 앞뒤에 공백을 둘 수 없습니다")
		}
	}
	first := strings.ToLower(strings.SplitN(c[1:], "/", 2)[0])
	for _, r := range Reserved {
		if first == r {
			return "", fmt.Errorf("'/%s'는 시스템이 사용하는 경로라 마운트할 수 없습니다", first)
		}
	}
	if len(c) > 512 {
		return "", errors.New("마운트 경로가 너무 깁니다")
	}
	return c, nil
}

// NormalizeRoot validates a Dropbox source folder ("/" for the whole account).
func NormalizeRoot(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "/", nil
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return Clean(p)
}

// HasPrefix reports whether p equals prefix or is inside it, comparing whole segments
// case-insensitively. HasPrefix("/a/b", "/a") is true; HasPrefix("/ab", "/a") is false.
func HasPrefix(p, prefix string) bool {
	lp, lx := strings.ToLower(p), strings.ToLower(prefix)
	if lx == "/" {
		return true
	}
	return lp == lx || strings.HasPrefix(lp, lx+"/")
}

// MountsConflict reports whether two mounts are equal or nested (either direction).
func MountsConflict(a, b string) bool {
	return HasPrefix(a, b) || HasPrefix(b, a)
}

// Join joins a root and a relative path into a clean absolute path.
func Join(root, rel string) string {
	j := path.Join("/", root, rel)
	return j
}

// Rel returns p relative to prefix ("/" when equal). It assumes HasPrefix(p, prefix).
func Rel(p, prefix string) string {
	if prefix == "/" {
		return p
	}
	segments := strings.Count(prefix[1:], "/") + 1
	start := 0
	for i := 0; i < segments; i++ {
		slash := strings.IndexByte(p[start+1:], '/')
		if slash < 0 {
			return "/"
		}
		start += slash + 1
	}
	return p[start:]
}

// Parent returns the parent of a clean absolute path ("/" for "/").
func Parent(p string) string {
	if p == "/" {
		return "/"
	}
	return path.Dir(p)
}

// ValidName checks a single file/folder name for rename.
func ValidName(n string) error {
	if n == "" || n == "." || n == ".." {
		return errors.New("이름을 입력하세요")
	}
	if strings.ContainsAny(n, "/\\") {
		return errors.New("이름에 / 또는 \\를 쓸 수 없습니다")
	}
	if strings.TrimSpace(n) != n {
		return errors.New("이름 앞뒤에 공백을 둘 수 없습니다")
	}
	for _, r := range n {
		if unicode.IsControl(r) {
			return errors.New("이름에 제어 문자를 쓸 수 없습니다")
		}
	}
	if len(n) > 255 {
		return errors.New("이름이 너무 깁니다")
	}
	return nil
}
