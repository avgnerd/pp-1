package media

import (
	"testing"
)

// argValue returns the argument that follows name, or "" when name is not present.
func argValue(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

// FFmpeg n9 measures t in force_key_frames from the run's first output frame, even with
// -ss 24 -copyts -start_at_zero, and n_forced also starts at zero for every run. A seek run
// therefore needs the same expression as a run from the first segment. Adding startNum skipped
// every forced keyframe until t caught up: real ffmpeg wrote a 10 s first segment rather than
// 4 s. TestTranscodeSeekKeepsFourSecondCadence checks that cadence against the muxer.
func TestTranscodeForceKeyFramesIsRunRelative(t *testing.T) {
	cases := []struct {
		name     string
		n        int
		startNum int
	}{
		{name: "run from the first segment", n: 0, startNum: 0},
		{name: "seek restart at segment 6", n: 6, startNum: 6},
		{name: "seek restart at segment 24", n: 24, startNum: 24},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session{mgr: &Manager{}, src: Source{Mode: ModeTranscode, Height: 1080, SrcH: 2160}}
			want := "expr:gte(t,n_forced*4)"
			if got := argValue(s.Args("http://example.invalid/in.mkv", tc.n, tc.startNum, 0), "-force_key_frames"); got != want {
				t.Fatalf("-force_key_frames = %q, want %q", got, want)
			}
		})
	}
}

// libx264 with yuv420p exits 1 on an odd dimension. A run that does not downscale used to pass the
// source size straight through, and an odd requested height reached the scale filter unchanged.
func TestTranscodeFilterKeepsDimensionsEven(t *testing.T) {
	cases := []struct {
		name   string
		srcH   int
		height int
		want   string
	}{
		{name: "odd source, no downscale", srcH: 481, height: 1080,
			want: "scale=trunc(iw/2)*2:trunc(ih/2)*2,format=yuv420p"},
		{name: "even source, no downscale", srcH: 720, height: 1080,
			want: "scale=trunc(iw/2)*2:trunc(ih/2)*2,format=yuv420p"},
		{name: "unknown source height, downscale guard", srcH: 0, height: 1080,
			want: "scale=-2:'min(1080,trunc(ih/2)*2)',format=yuv420p"},
		{name: "odd requested height, downscale", srcH: 1080, height: 481,
			want: "scale=-2:'min(480,ih)',format=yuv420p"},
		{name: "even requested height, downscale", srcH: 1080, height: 480,
			want: "scale=-2:'min(480,ih)',format=yuv420p"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session{mgr: &Manager{}, src: Source{Mode: ModeTranscode, Height: tc.height, SrcH: tc.srcH}}
			if got := argValue(s.Args("http://example.invalid/in.mkv", 0, 0, 0), "-vf"); got != tc.want {
				t.Fatalf("-vf = %q, want %q", got, tc.want)
			}
		})
	}
}
