// Command fakedropbox runs the in-memory Dropbox API fake for demos and end-to-end tests.
//
// Point data/config.json at it:
//
//	"dropbox_api_base": "http://127.0.0.1:9900",
//	"dropbox_content_base": "http://127.0.0.1:9900",
//	"dropbox_authorize_url": "http://127.0.0.1:9900/oauth2/authorize",
//	"dropbox_token_url": "http://127.0.0.1:9900/oauth2/token"
//
// Every app key is its own account, pre-filled with sample folders when -seed is set.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/dropbox/fake"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9900", "listen address")
	seed := flag.Int("seed", 3, "number of app keys (app1..appN) to pre-fill with sample files")
	ffmpeg := flag.String("ffmpeg", "ffmpeg", "ffmpeg used to generate sample media (skipped if missing)")
	flag.Parse()

	s := fake.New()
	s.BaseURL = "http://" + *addr
	samples := makeSamples(*ffmpeg)
	now := time.Now()
	for i := 1; i <= *seed; i++ {
		a := s.Account(fmt.Sprintf("app%d", i))
		a.Put("/문서/readme.txt", []byte("안녕하세요. 가짜 Dropbox 계정입니다.\nhello world\n"), now.Add(-48*time.Hour))
		a.Put("/문서/notes.md", []byte("# 메모\n\n- 항목 1\n- 항목 2\n"), now.Add(-24*time.Hour))
		a.Put("/문서/report.docx", []byte("fake docx"), now.Add(-12*time.Hour))
		a.Put("/문서/sheet.xlsx", []byte("fake xlsx"), now.Add(-11*time.Hour))
		for j := 1; j <= 40; j++ {
			a.Put(fmt.Sprintf("/사진/2024/IMG_%04d.jpg", j), samples["jpg"], now.Add(-time.Duration(j)*time.Hour))
		}
		a.Mkdir("/빈 폴더")
		for name, data := range samples {
			if name == "jpg" {
				continue
			}
			a.Put("/미디어/sample."+name, data, now)
		}
		a.Put("/미디어/vr_360_sample.mp4", samples["mp4"], now)
	}
	log.Printf("fake Dropbox on http://%s (app1..app%d seeded)", *addr, *seed)
	log.Fatal(http.ListenAndServe(*addr, s.Handler()))
}

// makeSamples builds small media files with ffmpeg; it falls back to tiny placeholders.
func makeSamples(ffmpeg string) map[string][]byte {
	out := map[string][]byte{"jpg": tinyJPEG()}
	if _, err := exec.LookPath(ffmpeg); err != nil {
		return out
	}
	dir, err := os.MkdirTemp("", "fakedbx")
	if err != nil {
		return out
	}
	defer os.RemoveAll(dir)
	gen := func(name string, args ...string) {
		p := filepath.Join(dir, name)
		cmd := exec.Command(ffmpeg, append(append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...), p)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			log.Printf("sample %s: %v %s", name, err, stderr.String())
			return
		}
		if b, err := os.ReadFile(p); err == nil {
			out[filepath.Ext(name)[1:]] = b
		}
	}
	src := []string{"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30:duration=20", "-f", "lavfi", "-i", "sine=frequency=440:duration=20"}
	gen("sample.mp4", append(src, "-c:v", "libx264", "-pix_fmt", "yuv420p", "-preset", "ultrafast", "-c:a", "aac", "-shortest", "-movflags", "+faststart")...)
	gen("sample.mkv", append(src, "-c:v", "libx264", "-pix_fmt", "yuv420p", "-preset", "ultrafast", "-c:a", "aac", "-shortest")...)
	gen("sample.wmv", append(src, "-c:v", "wmv2", "-c:a", "wmav2", "-shortest")...)
	gen("sample.mp3", "-f", "lavfi", "-i", "sine=frequency=330:duration=15", "-c:a", "libmp3lame")
	gen("sample.jpg", "-f", "lavfi", "-i", "testsrc2=size=1600x1067", "-frames:v", "1")
	return out
}

func tinyJPEG() []byte {
	return []byte{0xFF, 0xD8, 0xFF, 0xD9}
}
