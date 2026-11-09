// Package media probes files with ffprobe and serves HLS remux/transcode sessions with ffmpeg.
package media

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Stream is a subset of ffprobe stream info.
type Stream struct {
	Index         int    `json:"index"`
	CodecType     string `json:"codec_type"`
	CodecName     string `json:"codec_name"`
	Profile       string `json:"profile,omitempty"`
	Width         int    `json:"width,omitempty"`
	Height        int    `json:"height,omitempty"`
	PixFmt        string `json:"pix_fmt,omitempty"`
	Channels      int    `json:"channels,omitempty"`
	SampleRate    string `json:"sample_rate,omitempty"`
	Language      string `json:"language,omitempty"`
	Title         string `json:"title,omitempty"`
	ColorTransfer string `json:"color_transfer,omitempty"`
	// End is when the stream's last packet ends on the playback timeline (0 = unknown).
	End float64 `json:"end,omitempty"`
}

// VRInfo is the result of VR detection.
type VRInfo struct {
	Projection string `json:"projection"` // "", "180", "360"
	Stereo     string `json:"stereo"`     // "mono", "sbs", "tb"
	Source     string `json:"source"`     // "metadata", "filename", ""
}

// Probe is what the player needs to decide how to play a file.
type Probe struct {
	Format   string   `json:"format"`
	Duration float64  `json:"duration"`
	Bitrate  int64    `json:"bitrate"`
	Streams  []Stream `json:"streams"`
	Video    *Stream  `json:"video,omitempty"`
	Audio    *Stream  `json:"audio,omitempty"`
	VR       VRInfo   `json:"vr"`
	V        int      `json:"v,omitempty"` // probeVersion that produced this result
}

// probeVersion is bumped when Probe gains fields older cached results lack (2: stream end).
const probeVersion = 2

type ffprobeOut struct {
	Streams []struct {
		Index         int               `json:"index"`
		CodecType     string            `json:"codec_type"`
		CodecName     string            `json:"codec_name"`
		Profile       string            `json:"profile"`
		Width         int               `json:"width"`
		Height        int               `json:"height"`
		PixFmt        string            `json:"pix_fmt"`
		Channels      int               `json:"channels"`
		SampleRate    string            `json:"sample_rate"`
		ColorTransfer string            `json:"color_transfer"`
		StartTime     string            `json:"start_time"`
		Duration      string            `json:"duration"`
		Tags          map[string]string `json:"tags"`
		Disposition   map[string]int    `json:"disposition"`
		SideDataList  []map[string]any  `json:"side_data_list"`
	} `json:"streams"`
	Format struct {
		FormatName string            `json:"format_name"`
		StartTime  string            `json:"start_time"`
		Duration   string            `json:"duration"`
		BitRate    string            `json:"bit_rate"`
		Tags       map[string]string `json:"tags"`
	} `json:"format"`
}

// Prober runs ffprobe and caches results by Dropbox file id + rev.
type Prober struct {
	DB      *sql.DB
	FFprobe string

	probes    singleflight.Group
	slotsOnce sync.Once
	slots     chan struct{}
}

const (
	probeTimeout          = 45 * time.Second
	maxConcurrentProbeRun = 4
)

// Available reports whether ffprobe is configured.
func (p *Prober) Available() bool { return p.FFprobe != "" }

// Probe returns cached info or runs ffprobe against url.
func (p *Prober) Probe(ctx context.Context, fileID, rev, name, url string) (*Probe, error) {
	if pr, ok := p.cachedProbe(ctx, fileID, rev); ok {
		return pr, nil
	}
	if !p.Available() {
		pr := &Probe{VR: DetectVRName(name)}
		return pr, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if fileID == "" || rev == "" {
		return p.probeWithTimeout(ctx, fileID, rev, name, url)
	}

	ch := p.probes.DoChan(fileID+"\x00"+rev, func() (any, error) {
		opCtx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		if pr, ok := p.cachedProbe(opCtx, fileID, rev); ok {
			return pr, nil
		}
		return p.probeWithTimeout(opCtx, fileID, rev, name, url)
	})
	select {
	case result := <-ch:
		if result.Err != nil {
			return nil, result.Err
		}
		pr, ok := result.Val.(*Probe)
		if !ok || pr == nil {
			return nil, fmt.Errorf("ffprobe 공유 결과 형식 오류")
		}
		return pr, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *Prober) cachedProbe(ctx context.Context, fileID, rev string) (*Probe, bool) {
	if fileID == "" || rev == "" {
		return nil, false
	}
	var data string
	if err := p.DB.QueryRowContext(ctx, `SELECT data FROM media_probe WHERE file_id = ? AND rev = ?`, fileID, rev).Scan(&data); err != nil {
		return nil, false
	}
	var pr Probe
	if json.Unmarshal([]byte(data), &pr) != nil || pr.V < probeVersion {
		return nil, false
	}
	return &pr, true
}

func (p *Prober) probeWithTimeout(ctx context.Context, fileID, rev, name, url string) (*Probe, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	p.slotsOnce.Do(func() { p.slots = make(chan struct{}, maxConcurrentProbeRun) })
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	cmd := exec.CommandContext(ctx, p.FFprobe, "-v", "error", "-hide_banner",
		"-probesize", "8M", "-analyzeduration", "10M",
		"-print_format", "json", "-show_format", "-show_streams", url)
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("ffprobe 실패: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("ffprobe 실행 실패: %w", err)
	}
	pr, err := parseProbe(out, name)
	if err != nil {
		return nil, err
	}
	if fileID != "" && rev != "" {
		b, _ := json.Marshal(pr)
		_, _ = p.DB.ExecContext(ctx, `INSERT OR REPLACE INTO media_probe(file_id, rev, data, created_at) VALUES(?,?,?,?)`, fileID, rev, string(b), time.Now().Unix())
	}
	return pr, nil
}

func parseProbe(out []byte, name string) (*Probe, error) {
	var f ffprobeOut
	if err := json.Unmarshal(out, &f); err != nil {
		return nil, fmt.Errorf("ffprobe 출력 해석 실패: %w", err)
	}
	pr := &Probe{Format: f.Format.FormatName, V: probeVersion}
	pr.Duration, _ = strconv.ParseFloat(f.Format.Duration, 64)
	pr.Bitrate, _ = strconv.ParseInt(f.Format.BitRate, 10, 64)
	fstart, _ := strconv.ParseFloat(f.Format.StartTime, 64)
	var meta VRInfo
	for _, s := range f.Streams {
		st := Stream{Index: s.Index, CodecType: s.CodecType, CodecName: s.CodecName, Profile: s.Profile, Width: s.Width, Height: s.Height,
			PixFmt: s.PixFmt, Channels: s.Channels, SampleRate: s.SampleRate, ColorTransfer: s.ColorTransfer,
			Language: s.Tags["language"], Title: s.Tags["title"], End: streamEnd(s.StartTime, s.Duration, s.Tags, fstart)}
		pr.Streams = append(pr.Streams, st)
		switch s.CodecType {
		case "video":
			if s.Disposition["attached_pic"] == 1 {
				continue
			}
			if pr.Video == nil {
				v := st
				pr.Video = &v
				meta = vrFromSideData(s.SideDataList, s.Tags)
			}
		case "audio":
			if pr.Audio == nil {
				a := st
				pr.Audio = &a
			}
		}
	}
	if meta.Projection == "" {
		meta = vrFromTags(f.Format.Tags)
	}
	if meta.Projection != "" {
		meta.Source = "metadata"
		pr.VR = meta
	} else {
		pr.VR = DetectVRName(name)
	}
	return pr, nil
}

// streamEnd returns when a stream ends relative to the file start, which is the timeline the
// HLS output uses (-copyts -start_at_zero). Most containers report a duration from the stream
// start; Matroska only has a DURATION tag, which is the absolute end. 0 means unknown.
func streamEnd(start, duration string, tags map[string]string, fileStart float64) float64 {
	st, err1 := strconv.ParseFloat(start, 64)
	if d, err := strconv.ParseFloat(duration, 64); err == nil && err1 == nil && d > 0 {
		return st - fileStart + d
	}
	for k, v := range tags {
		if !strings.HasPrefix(strings.ToUpper(k), "DURATION") {
			continue
		}
		var h, m int
		var sec float64
		if _, err := fmt.Sscanf(v, "%d:%d:%f", &h, &m, &sec); err == nil {
			if end := float64(h*3600+m*60) + sec - fileStart; end > 0 {
				return end
			}
		}
	}
	return 0
}

// vrFromSideData reads ffprobe "Spherical Mapping" and "Stereo 3D" side data.
func vrFromSideData(sd []map[string]any, tags map[string]string) VRInfo {
	var v VRInfo
	v.Stereo = "mono"
	for _, d := range sd {
		t, _ := d["side_data_type"].(string)
		switch {
		case strings.Contains(t, "Spherical"):
			proj, _ := d["projection"].(string)
			if strings.Contains(proj, "equirectangular") {
				v.Projection = "360"
				// Cropped equirect (bounds) covering half the sphere is VR180.
				if l, ok := d["bound_left"].(float64); ok && l > 0 {
					v.Projection = "180"
				}
			}
			if strings.Contains(proj, "half") || strings.Contains(proj, "180") {
				v.Projection = "180"
			}
		case strings.Contains(t, "Stereo"):
			typ, _ := d["type"].(string)
			switch {
			case strings.Contains(typ, "side by side"):
				v.Stereo = "sbs"
			case strings.Contains(typ, "top and bottom"), strings.Contains(typ, "top bottom"):
				v.Stereo = "tb"
			}
		}
	}
	if m := tags["stereo_mode"]; m != "" {
		switch {
		case strings.Contains(m, "left_right") || strings.Contains(m, "side_by_side"):
			v.Stereo = "sbs"
		case strings.Contains(m, "top_bottom") || strings.Contains(m, "bottom_top"):
			v.Stereo = "tb"
		}
	}
	return v
}

func vrFromTags(tags map[string]string) VRInfo {
	for k, v := range tags {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "spherical") && strings.EqualFold(v, "true") {
			return VRInfo{Projection: "360", Stereo: "mono"}
		}
	}
	return VRInfo{}
}

var (
	re180 = regexp.MustCompile(`(?i)(^|[^0-9a-z])(vr)?180([^0-9]|$)`)
	re360 = regexp.MustCompile(`(?i)(^|[^0-9a-z])(vr)?360([^0-9]|$)`)
	reSBS = regexp.MustCompile(`(?i)(^|[^a-z])(sbs|lr|3dh|side[-_ ]?by[-_ ]?side)([^a-z]|$)`)
	reTB  = regexp.MustCompile(`(?i)(^|[^a-z])(tb|ou|tab|3dv|over[-_ ]?under|top[-_ ]?bottom)([^a-z]|$)`)
)

// DetectVRName guesses VR layout from file name keywords such as 180, 360, _LR, _TB, SBS.
func DetectVRName(name string) VRInfo {
	base := strings.TrimSuffix(path.Base(name), path.Ext(name))
	v := VRInfo{Stereo: "mono"}
	switch {
	case re180.MatchString(base):
		v.Projection = "180"
	case re360.MatchString(base):
		v.Projection = "360"
	}
	switch {
	case reSBS.MatchString(base):
		v.Stereo = "sbs"
	case reTB.MatchString(base):
		v.Stereo = "tb"
	}
	if v.Projection == "" {
		return VRInfo{Stereo: "mono"}
	}
	if v.Projection == "180" && v.Stereo == "mono" {
		// VR180 files are almost always side-by-side.
		v.Stereo = "sbs"
	}
	v.Source = "filename"
	return v
}
