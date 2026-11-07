// Package archive lists the contents of ZIP, 7z, RAR and TAR(.gz) files read over HTTP Range.
//
// ZIP and 7z keep their directory at the end of the file, so only a few blocks are read.
// TAR is skipped header-to-header with seeks. RAR and compressed TAR need sequential reads;
// those stop at a configurable byte budget.
package archive

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/bodgit/sevenzip"
	"github.com/nwaples/rardecode/v2"
)

// Entry is one file or folder in an archive.
type Entry struct {
	Name     string `json:"name"` // full path inside the archive, "/" separated
	Size     int64  `json:"size"`
	Packed   int64  `json:"packed,omitempty"`
	Modified string `json:"modified,omitempty"`
	Dir      bool   `json:"dir"`
}

// Listing is the result of reading an archive.
type Listing struct {
	Format    string  `json:"format"`
	Entries   []Entry `json:"entries"`
	Truncated bool    `json:"truncated"`
	Encrypted bool    `json:"encrypted,omitempty"`
}

// ErrBudget means a sequential format needed more bytes than allowed.
var ErrBudget = errors.New("budget exceeded")

// MaxEntries bounds the listing size.
const MaxEntries = 20000

// Format detects the archive format from the file name ("" if unsupported).
func Format(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.HasSuffix(n, ".zip"), strings.HasSuffix(n, ".cbz"):
		return "zip"
	case strings.HasSuffix(n, ".7z"), strings.HasSuffix(n, ".cb7"):
		return "7z"
	case strings.HasSuffix(n, ".rar"), strings.HasSuffix(n, ".cbr"):
		return "rar"
	case strings.HasSuffix(n, ".tar"):
		return "tar"
	case strings.HasSuffix(n, ".tar.gz"), strings.HasSuffix(n, ".tgz"):
		return "tgz"
	}
	return ""
}

// Sequential reports whether a format must be read from the start.
func Sequential(format string) bool { return format == "rar" || format == "tgz" }

// budgetReader fails once more than max bytes were read (seeks are free).
type budgetReader struct {
	r    *io.SectionReader
	read int64
	max  int64
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.read >= b.max {
		return 0, ErrBudget
	}
	if rem := b.max - b.read; int64(len(p)) > rem {
		p = p[:rem]
	}
	n, err := b.r.Read(p)
	b.read += int64(n)
	return n, err
}

func (b *budgetReader) Seek(off int64, whence int) (int64, error) { return b.r.Seek(off, whence) }

// List reads an archive. budget bounds bytes read for sequential formats.
func List(format string, ra io.ReaderAt, size, budget int64) (*Listing, error) {
	l := &Listing{Format: format}
	add := func(e Entry) bool {
		if len(l.Entries) >= MaxEntries {
			l.Truncated = true
			return false
		}
		e.Name = strings.TrimPrefix(strings.ReplaceAll(e.Name, "\\", "/"), "/")
		if e.Name == "" {
			return true
		}
		l.Entries = append(l.Entries, e)
		return true
	}
	switch format {
	case "zip":
		zr, err := zip.NewReader(ra, size)
		if err != nil {
			return nil, fmt.Errorf("ZIP 파일을 읽을 수 없습니다: %w", err)
		}
		for _, f := range zr.File {
			if f.Flags&0x1 != 0 {
				l.Encrypted = true
			}
			if !add(Entry{Name: f.Name, Size: int64(f.UncompressedSize64), Packed: int64(f.CompressedSize64),
				Modified: f.Modified.UTC().Format(time.RFC3339), Dir: strings.HasSuffix(f.Name, "/")}) {
				break
			}
		}
	case "7z":
		zr, err := sevenzip.NewReader(ra, size)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "password") {
				l.Encrypted = true
				return l, nil
			}
			return nil, fmt.Errorf("7z 파일을 읽을 수 없습니다: %w", err)
		}
		for _, f := range zr.File {
			fi := f.FileInfo()
			if !add(Entry{Name: f.Name, Size: int64(f.UncompressedSize), Modified: f.Modified.UTC().Format(time.RFC3339), Dir: fi.IsDir()}) {
				break
			}
		}
	case "tar", "tgz":
		br := &budgetReader{r: io.NewSectionReader(ra, 0, size), max: budget}
		var r io.Reader = br
		if format == "tar" {
			// Plain tar can skip file data with seeks, so the budget only covers headers.
			br.max = 1 << 62
			r = &seekCounter{br: br, limit: budget}
		} else {
			gz, err := gzip.NewReader(br)
			if err != nil {
				return nil, fmt.Errorf("gzip 파일을 읽을 수 없습니다: %w", err)
			}
			defer gz.Close()
			r = gz
		}
		tr := tar.NewReader(r)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				if errors.Is(err, ErrBudget) {
					l.Truncated = true
					break
				}
				return nil, fmt.Errorf("TAR 파일을 읽을 수 없습니다: %w", err)
			}
			if h.Typeflag == tar.TypeXGlobalHeader {
				continue
			}
			if !add(Entry{Name: h.Name, Size: h.Size, Modified: h.ModTime.UTC().Format(time.RFC3339), Dir: h.Typeflag == tar.TypeDir}) {
				break
			}
		}
	case "rar":
		br := &budgetReader{r: io.NewSectionReader(ra, 0, size), max: budget}
		rr, err := rardecode.NewReader(br)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "password") {
				l.Encrypted = true
				return l, nil
			}
			return nil, fmt.Errorf("RAR 파일을 읽을 수 없습니다: %w", err)
		}
		for {
			h, err := rr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				if errors.Is(err, ErrBudget) {
					l.Truncated = true
					break
				}
				if strings.Contains(strings.ToLower(err.Error()), "password") {
					l.Encrypted = true
					break
				}
				return nil, fmt.Errorf("RAR 파일을 읽을 수 없습니다: %w", err)
			}
			if h.Encrypted {
				l.Encrypted = true
			}
			if !add(Entry{Name: h.Name, Size: h.UnPackedSize, Packed: h.PackedSize, Modified: h.ModificationTime.UTC().Format(time.RFC3339), Dir: h.IsDir}) {
				break
			}
		}
	default:
		return nil, errors.New("지원하지 않는 압축 형식입니다")
	}
	addImplicitDirs(l)
	return l, nil
}

// seekCounter passes reads through while counting them against limit; Seek stays free.
type seekCounter struct {
	br    *budgetReader
	limit int64
	read  int64
}

func (s *seekCounter) Read(p []byte) (int, error) {
	if s.read >= s.limit {
		return 0, ErrBudget
	}
	n, err := s.br.Read(p)
	s.read += int64(n)
	return n, err
}

func (s *seekCounter) Seek(off int64, whence int) (int64, error) { return s.br.Seek(off, whence) }

// addImplicitDirs adds folder entries that are only implied by file paths, then sorts.
func addImplicitDirs(l *Listing) {
	have := map[string]bool{}
	for _, e := range l.Entries {
		have[strings.TrimSuffix(e.Name, "/")] = true
	}
	for _, e := range append([]Entry(nil), l.Entries...) {
		d := path.Dir(strings.TrimSuffix(e.Name, "/"))
		for d != "." && d != "/" && d != "" {
			if !have[d] {
				have[d] = true
				l.Entries = append(l.Entries, Entry{Name: d + "/", Dir: true})
			}
			d = path.Dir(d)
		}
	}
	for i := range l.Entries {
		if l.Entries[i].Dir && !strings.HasSuffix(l.Entries[i].Name, "/") {
			l.Entries[i].Name += "/"
		}
	}
	sort.Slice(l.Entries, func(i, j int) bool { return l.Entries[i].Name < l.Entries[j].Name })
}
