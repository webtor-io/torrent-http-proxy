package services

import "testing"

func TestLimiterPathUsesModTailForLightArtifacts(t *testing.T) {
	l := &SessionLimiter{lightExts: parseLightExts(defaultLightExts)}
	cases := []struct {
		name string
		src  *Source
		want string
	}{
		{"plain file", &Source{Path: "/Dir/S01E01.mkv"}, "/Dir/S01E01.mkv"},
		{"opensubtitles track", &Source{Path: "/Dir/S01E01.mkv", Mod: &Mod{Type: "vi", Path: "/opensubtitles/7891996.vtt"}}, "/Dir/S01E01.mkv~vi/opensubtitles/7891996.vtt"},
		{"another track differs", &Source{Path: "/Dir/S01E01.mkv", Mod: &Mod{Type: "vi", Path: "/opensubtitles/8691402.vtt"}}, "/Dir/S01E01.mkv~vi/opensubtitles/8691402.vtt"},
		{"srt2vtt", &Source{Path: "/Dir/sub.srt", Mod: &Mod{Type: "vtt", Path: "/sub.vtt"}}, "/Dir/sub.srt~vtt/sub.vtt"},
		{"hls manifest is light", &Source{Path: "/Dir/S01E01.mkv", Mod: &Mod{Type: "hls", Path: "/index.m3u8"}}, "/Dir/S01E01.mkv~hls/index.m3u8"},
		{"hls segment keeps the source path", &Source{Path: "/Dir/S01E01.mkv", Mod: &Mod{Type: "hls", Path: "/seg-00042.ts"}}, "/Dir/S01E01.mkv"},
		{"mod without tail", &Source{Path: "/Dir/S01E01.mkv", Mod: &Mod{Type: "cp", Path: ""}}, "/Dir/S01E01.mkv"},
	}
	for _, c := range cases {
		if got := l.limiterPath(c.src); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestLimiterPathMakesTrackLight(t *testing.T) {
	l := &SessionLimiter{lightExts: parseLightExts(defaultLightExts), bigFileThreshold: 1}
	src := &Source{InfoHash: "h", Path: "/Dir/S01E01.mkv", Mod: &Mod{Type: "vi", Path: "/opensubtitles/1.vtt"}}
	if l.isBigFile(src.InfoHash, l.limiterPath(src)) {
		t.Fatal("a subtitle track fetched through ~vi must not count as a big file")
	}
	if !l.isBigFile(src.InfoHash, l.limiterPath(&Source{InfoHash: "h", Path: "/Dir/S01E01.mkv", Mod: &Mod{Type: "hls", Path: "/seg.ts"}})) {
		t.Fatal("an HLS segment must still count against the big-files cap")
	}
}
