package mediacache

import (
	"testing"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

// Newer servers report each meme's folder (relative to the memes root, which
// LibraryDir is), so a name shared by several folders needs no download.

func inFolder(id int64, folder, name string) memesearch.Meme {
	return memesearch.Meme{ID: id, Folder: folder, Filename: name, MediaType: "image/jpeg"}
}

func TestLookupByFolderSettlesNameCollision(t *testing.T) {
	e := newEnv(t, Config{})
	e.file("a/x.jpg", "first")
	e.file("b/x.jpg", "second")
	e.scan()
	e.setFileID("a/x.jpg", "fid-a", "uniq-a")
	e.setFileID("b/x.jpg", "fid-b", "uniq-b")
	if got, ok := e.c.Lookup(t.Context(), inFolder(1, "b", "x.jpg")); !ok || got != "fid-b" {
		t.Fatalf("Lookup with folder = %q %v, want fid-b", got, ok)
	}
	// An older server sends no folder: the name stays ambiguous, as before.
	if got, ok := e.c.Lookup(t.Context(), inFolder(2, "", "x.jpg")); ok {
		t.Fatalf("Lookup without folder = %q, want a miss", got)
	}
}

func TestLookupByNestedFolder(t *testing.T) {
	e := newEnv(t, Config{})
	e.file("telegram/alice/x.jpg", "a")
	e.file("telegram/bob/x.jpg", "b")
	e.scan()
	e.setFileID("telegram/bob/x.jpg", "fid-b", "uniq-b")
	if got, ok := e.c.Lookup(t.Context(), inFolder(1, "telegram/bob", "x.jpg")); !ok || got != "fid-b" {
		t.Fatalf("Lookup = %q %v", got, ok)
	}
}

func TestResolveByFolderSkipsTheDownload(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID})
	e.file("a/x.jpg", "first")
	e.file("b/x.jpg", "second")
	e.scan()
	e.run()
	if id := e.resolve(inFolder(7, "b", "x.jpg")); id != "fid-1" {
		t.Fatalf("file_id = %q", id)
	}
	if s := e.sender.sent(); len(s) != 1 || s[0].caption != "b/x.jpg" || s[0].sum != sum("second") {
		t.Fatalf("sent = %+v", s)
	}
	if n := e.api.calls(); n != 0 {
		t.Fatalf("content downloads = %d, want 0", n)
	}
}

func TestPrepareByFolderReadsTheFile(t *testing.T) {
	e := newEnv(t, Config{})
	e.file("a/x.jpg", "first")
	e.file("b/x.jpg", "second")
	e.scan()
	p := e.prepare(inFolder(7, "b", "x.jpg"))
	if string(p.Data) != "second" || p.LibraryPath != "b/x.jpg" {
		t.Fatalf("photo = %q from %q", p.Data, p.LibraryPath)
	}
	if n := e.api.calls(); n != 0 {
		t.Fatalf("content downloads = %d, want 0", n)
	}
}

// LibraryDir may be mounted at another level than meme-search's memes root;
// then the folder path finds nothing and the name lookup still works.
func TestFolderMismatchFallsBackToName(t *testing.T) {
	e := newEnv(t, Config{})
	e.file("c/y.jpg", "y")
	e.scan()
	e.setFileID("c/y.jpg", "fid-y", "uniq-y")
	if got, ok := e.c.Lookup(t.Context(), inFolder(1, "memes/c", "y.jpg")); !ok || got != "fid-y" {
		t.Fatalf("Lookup = %q %v", got, ok)
	}
}

func TestFeedWithFolderKnowsCollidingNames(t *testing.T) {
	ls := newListServer(t)
	e, _ := newFeedEnv(t, Config{}, ls, listed())
	e.file("a/x.jpg", "first")
	e.file("b/other.jpg", "other")
	e.scan()
	ls.add(memesearch.Meme{ID: 1, Folder: "b", Filename: "x.jpg", MediaType: "image/jpeg"})
	e.c.pollFeed(t.Context())
	if len(e.c.rescan) != 1 {
		t.Fatal("b/x.jpg is not indexed, but a/x.jpg hid it from the rescan check")
	}
}

// With a folder the index does not have at all, LibraryDir sits at another
// level: the name match counts, or every poll would rescan.
func TestFeedWithUnknownFolderTrustsTheName(t *testing.T) {
	ls := newListServer(t, memesearch.Meme{ID: 1, Folder: "memes/a", Filename: "x.jpg", MediaType: "image/jpeg"})
	e, _ := newFeedEnv(t, Config{}, ls, listed())
	e.file("a/x.jpg", "first")
	e.scan()
	e.c.pollFeed(t.Context())
	if len(e.c.rescan) != 0 {
		t.Fatal("rescan requested for a file the index has under another root")
	}
}
