package mediacache

import (
	"slices"
	"testing"
)

func TestNameDate(t *testing.T) {
	for name, want := range map[string]string{
		"2025-05-31_MrSparkline_28566_10798c1de486dabee59adb09b94429bb.jpg": "2025-05-31",
		"2026-10-18_dzrmemes_25_0123456789abcdef0123.jpg":                   "2026-10-18",
		"meme from 2024-02-29.png":                                          "2024-02-29",
		"2023-02-29_then_2022-12-01":                                        "2022-12-01", // first one is not a calendar date
		"2025-13-01_x.jpg":                                                  "",
		"x12025-05-31.jpg":                                                  "2025-05-31",
		"20250531.jpg":                                                      "",
		"2025-5-31.jpg":                                                     "",
		"cat.jpg":                                                           "",
		"":                                                                  "",
	} {
		if got := nameDate(name); got != want {
			t.Errorf("nameDate(%q) = %q, want %q", name, got, want)
		}
	}
}

func warmupCaptions(e *env) []string {
	var out []string
	for _, s := range e.sender.sent() {
		out = append(out, s.caption)
	}
	return out
}

func TestWarmupNewestNameDateFirst(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID})
	want := []string{
		"a/2026-10-18_y_2_ab.jpg",
		"c/2026-10-18_a.jpg", // same date: by rel_path
		"a/2025-05-31_x_1_cd.jpg",
		"a/2025-02-30_bad_2024-12-01.jpg",
		"a/aaa.jpg", // no date: after every dated file, by rel_path
		"b/nodate.jpg",
	}
	for i, rel := range []string{want[4], want[2], want[5], want[0], want[3], want[1]} {
		e.file(rel, string(rune('a'+i)))
	}
	e.runFull()
	eventually(t, "all warmup uploads", func() bool { return len(e.sender.sent()) == len(want) })
	if got := warmupCaptions(e); !slices.Equal(got, want) {
		t.Fatalf("upload order:\n got %q\nwant %q", got, want)
	}
}

// Files found by a later scan or saved meanwhile go ahead of older files that
// are already queued.
func TestNewerFilesJumpQueuedOlderWarmup(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID, UploadsPerMin: 1})
	e.file("a/2020-01-01_old.jpg", "old1")
	e.file("a/2020-01-02_old.jpg", "old2")
	e.run()
	if _, err := e.c.scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first upload", func() bool { return len(e.sender.sent()) == 1 })
	e.file("a/2026-01-01_new.jpg", "new")
	if _, err := e.c.scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := e.c.AddLibraryFile(t.Context(), e.file("a/2026-10-19_saved.jpg", "saved")); err != nil {
		t.Fatal(err)
	}
	want := []string{"a/2020-01-02_old.jpg", "a/2026-10-19_saved.jpg", "a/2026-01-01_new.jpg", "a/2020-01-01_old.jpg"}
	if got := e.drain(len(want)); !slices.Equal(got, want) {
		t.Fatalf("upload order:\n got %q\nwant %q", got, want)
	}
}

// Uploads someone waits for rank before every warmup file, whatever its date.
func TestJobOrderRanksWaitedUploadsFirst(t *testing.T) {
	meme := &job{memeID: 1, done: make(chan result, 1)}
	newer := &job{relPath: "z/2026-01-01.jpg", date: "2026-01-01"}
	older := &job{relPath: "a/2020-01-01.jpg", date: "2020-01-01"}
	undated := &job{relPath: "a/x.jpg"}
	for _, c := range []struct {
		a, b *job
		want bool
	}{
		{meme, newer, true}, {newer, meme, false}, {meme, meme, false},
		{newer, older, true}, {older, newer, false}, {older, undated, true}, {undated, older, false},
		{&job{relPath: "a/x.jpg"}, &job{relPath: "b/x.jpg"}, true},
	} {
		if got := jobBefore(c.a, c.b); got != c.want {
			t.Errorf("jobBefore(%q, %q) = %v, want %v", c.a.relPath, c.b.relPath, got, c.want)
		}
	}
}
