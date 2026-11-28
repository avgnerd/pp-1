package media

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/dvfs/dropbox-vfs-manager/internal/db"
	"github.com/dvfs/dropbox-vfs-manager/internal/testutil"
)

// A trailing digit boundary let resolution and frame-rate suffixes through: 360p, 360x240 and
// 180fps are ordinary video names, not VR layouts.
func TestDetectVRNameRejectsResolutionAndFrameRateSuffixes(t *testing.T) {
	notVR := []string{
		"movie.360p.mp4",
		"360x240.mp4",
		"video_180fps.mp4",
		"movie.1080p.mkv",
		"ep180x.mp4",
	}
	for _, name := range notVR {
		if got := DetectVRName(name); got.Projection != "" || got.Stereo != "mono" {
			t.Errorf("%s: got %+v, want no VR", name, got)
		}
	}
	stillVR := map[string]VRInfo{
		"trip_360.mp4":         {Projection: "360", Stereo: "mono", Source: "filename"},
		"concert_VR180_LR.mp4": {Projection: "180", Stereo: "sbs", Source: "filename"},
		"show 360 TB.mkv":      {Projection: "360", Stereo: "tb", Source: "filename"},
		"beach_180_sbs.mp4":    {Projection: "180", Stereo: "sbs", Source: "filename"},
		"walk.360.3dh.mp4":     {Projection: "360", Stereo: "sbs", Source: "filename"},
	}
	for name, want := range stillVR {
		if got := DetectVRName(name); got != want {
			t.Errorf("%s: got %+v, want %+v", name, got, want)
		}
	}
}

// A Dropbox file id and rev survive a rename, so a filename-derived VR layout cached under them is
// stale. Reading it back must use the current name, without rewriting the stored row.
func TestCachedProbeRecomputesFilenameVRFromCurrentName(t *testing.T) {
	d, err := db.OpenPath(filepath.Join(testutil.TempDir(t), "probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	stored := Probe{
		Format:   "mov,mp4",
		Duration: 10,
		VR:       VRInfo{Projection: "360", Stereo: "mono", Source: "filename"},
		V:        probeVersion,
	}
	writeProbeRow(t, d, "id:renamed", "rev1", stored)
	p := &Prober{DB: d, FFprobe: ""}

	pr, err := p.Probe(context.Background(), "id:renamed", "rev1", "trip_180_LR.mkv", "")
	if err != nil {
		t.Fatal(err)
	}
	want := VRInfo{Projection: "180", Stereo: "sbs", Source: "filename"}
	if pr.VR != want {
		t.Fatalf("renamed file VR = %+v, want %+v from the current name", pr.VR, want)
	}
	if reread := readProbeRow(t, d, "id:renamed", "rev1"); reread.VR != stored.VR {
		t.Fatalf("cached row VR = %+v after a rename read, want it untouched (%+v)", reread.VR, stored.VR)
	}

	// VR that came from ffprobe metadata describes the content, not the name, and must survive
	// any rename.
	stored.VR = VRInfo{Projection: "360", Stereo: "mono", Source: "metadata"}
	writeProbeRow(t, d, "id:renamed", "rev1", stored)
	pr, err = p.Probe(context.Background(), "id:renamed", "rev1", "trip_180_LR.mkv", "")
	if err != nil {
		t.Fatal(err)
	}
	if pr.VR != stored.VR {
		t.Fatalf("metadata VR = %+v after a rename, want %+v preserved", pr.VR, stored.VR)
	}
}

// Version 2 stored metadata VR after the old parser let a format-level spherical tag replace a
// stream's sbs/tb layout with mono. Those rows are wrong rather than merely incomplete, so the
// version bump must reject them while preserving rows produced by the fixed parser at version 3.
func TestCachedProbeRejectsVersion2MetadataRow(t *testing.T) {
	d, err := db.OpenPath(filepath.Join(testutil.TempDir(t), "probe-version.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	stored := Probe{
		Format:   "mov,mp4",
		Duration: 10,
		VR:       VRInfo{Projection: "360", Stereo: "sbs", Source: "metadata"},
		V:        2,
	}
	writeProbeRow(t, d, "id:versioned", "rev1", stored)
	p := &Prober{DB: d, FFprobe: ""}
	if pr, ok := p.cachedProbe(context.Background(), "id:versioned", "rev1", "movie.mp4"); ok {
		t.Fatalf("V=2 cached probe was accepted: %+v", pr)
	}

	stored.V = 3
	writeProbeRow(t, d, "id:versioned", "rev1", stored)
	pr, ok := p.cachedProbe(context.Background(), "id:versioned", "rev1", "movie.mp4")
	if !ok {
		t.Fatal("V=3 cached probe was rejected")
	}
	if pr.V != 3 || pr.VR != stored.VR {
		t.Fatalf("V=3 cached probe = %+v, want V=3 and VR %+v preserved", pr, stored.VR)
	}
}

func writeProbeRow(t *testing.T, d *sql.DB, fileID, rev string, pr Probe) {
	t.Helper()
	b, err := json.Marshal(pr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT OR REPLACE INTO media_probe(file_id, rev, data, created_at) VALUES(?,?,?,?)`,
		fileID, rev, string(b), time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
}

func readProbeRow(t *testing.T, d *sql.DB, fileID, rev string) Probe {
	t.Helper()
	var data string
	if err := d.QueryRow(`SELECT data FROM media_probe WHERE file_id = ? AND rev = ?`, fileID, rev).Scan(&data); err != nil {
		t.Fatal(err)
	}
	var pr Probe
	if err := json.Unmarshal([]byte(data), &pr); err != nil {
		t.Fatal(err)
	}
	return pr
}

// A format-level spherical tag only says the file is 360 video. Stream side data is the only
// source for the stereo layout, so merging the tag must not reset an sbs/tb pair to mono.
func TestParseProbeKeepsStreamStereoWhenFormatIsSpherical(t *testing.T) {
	cases := []struct {
		name string
		json string
		want VRInfo
	}{
		{
			name: "spherical tag plus side-by-side side data",
			json: `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","width":3840,"height":1920,
				"start_time":"0.000000","duration":"10.000000",
				"side_data_list":[{"side_data_type":"Stereo 3D","type":"side by side"}]}],
				"format":{"format_name":"mov,mp4","duration":"10.000000","bit_rate":"1000",
				"tags":{"spherical":"true"}}}`,
			want: VRInfo{Projection: "360", Stereo: "sbs", Source: "metadata"},
		},
		{
			name: "spherical tag plus top-and-bottom side data",
			json: `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","width":3840,"height":1920,
				"start_time":"0.000000","duration":"10.000000",
				"side_data_list":[{"side_data_type":"Stereo 3D","type":"top and bottom"}]}],
				"format":{"format_name":"mov,mp4","duration":"10.000000","bit_rate":"1000",
				"tags":{"spherical":"true"}}}`,
			want: VRInfo{Projection: "360", Stereo: "tb", Source: "metadata"},
		},
		{
			name: "spherical tag without stream side data",
			json: `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","width":3840,"height":1920,
				"start_time":"0.000000","duration":"10.000000"}],
				"format":{"format_name":"mov,mp4","duration":"10.000000","bit_rate":"1000",
				"tags":{"spherical":"true"}}}`,
			want: VRInfo{Projection: "360", Stereo: "mono", Source: "metadata"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr, err := parseProbe([]byte(tc.json), "movie.mp4")
			if err != nil {
				t.Fatal(err)
			}
			if pr.VR != tc.want {
				t.Fatalf("VR = %+v, want %+v", pr.VR, tc.want)
			}
		})
	}
}
