package mediacache

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

func (e *env) setFileID(rel, fid, uniq string) {
	e.t.Helper()
	if err := e.c.setLibraryFileID(e.t.Context(), rel, fid, uniq); err != nil {
		e.t.Fatal(err)
	}
}

// file_id depends on content, not path: a renamed folder must not send its
// whole content to the cache channel again.
func TestRenamedFolderKeepsFileIDs(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID})
	e.file("tg-grbit/1.jpg", "one")
	e.file("tg-grbit/2.jpg", "two")
	e.scan()
	e.setFileID("tg-grbit/1.jpg", "fid-1", "uniq-1")
	e.setFileID("tg-grbit/2.jpg", "fid-2", "uniq-2")
	if err := os.Rename(filepath.Join(e.lib, "tg-grbit"), filepath.Join(e.lib, "tg_grbit")); err != nil {
		t.Fatal(err)
	}
	e.run()
	st, err := e.c.scan(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.pending != 0 || st.added != 2 || st.removed != 2 {
		t.Fatalf("stats = %+v, want 2 added, 2 removed, nothing pending", st)
	}
	for rel, fid := range map[string]string{"tg_grbit/1.jpg": "fid-1", "tg_grbit/2.jpg": "fid-2"} {
		if r := e.libRow(rel); r.fileID != fid || r.fileUniqueID != "uniq"+fid[3:] {
			t.Fatalf("%s = %+v, want %s", rel, r, fid)
		}
	}
	if fid, ok := e.c.Lookup(t.Context(), memesearch.Meme{ID: 7, Filename: "2.jpg"}); !ok || fid != "fid-2" {
		t.Fatalf("Lookup by name = %q, %v", fid, ok)
	}
	time.Sleep(5 * time.Millisecond)
	if n := len(e.sender.sent()); n != 0 {
		t.Fatalf("uploads to the cache chat = %d, want 0", n)
	}
}

func TestAddLibraryFileInheritsFileIDOfSameContent(t *testing.T) {
	e := newEnv(t, Config{CacheChatID: chatID})
	e.file("a/1.jpg", "one")
	e.scan()
	e.setFileID("a/1.jpg", "fid-1", "uniq-1")
	if err := e.c.AddLibraryFile(t.Context(), e.file("b/copy.jpg", "one")); err != nil {
		t.Fatal(err)
	}
	if r := e.libRow("b/copy.jpg"); r.fileID != "fid-1" {
		t.Fatalf("copy = %+v", r)
	}
	if n := e.c.queue.Len(0) + e.c.queue.Len(1); n != 0 {
		t.Fatalf("queued uploads = %d, want 0", n)
	}
}

// A changed file still loses its own file_id, but takes the one of another
// row that already has the new content.
func TestChangedFileInheritsFromRowWithNewContent(t *testing.T) {
	e := newEnv(t, Config{})
	e.file("a/1.jpg", "one")
	e.file("a/2.jpg", "two")
	e.scan()
	e.setFileID("a/1.jpg", "fid-1", "uniq-1")
	e.setFileID("a/2.jpg", "fid-2", "uniq-2")
	e.file("a/2.jpg", "one")
	e.scan()
	if r := e.libRow("a/2.jpg"); r.fileID != "fid-1" || r.fileUniqueID != "uniq-1" {
		t.Fatalf("changed row = %+v", r)
	}
}

// A failure is not inherited: one more attempt is cheap, and a copied file is
// worth retrying if the failure was a fluke.
func TestFailureIsNotInherited(t *testing.T) {
	e := newEnv(t, Config{})
	e.file("a/1.jpg", "one")
	e.scan()
	if err := e.c.markLibraryFailed(t.Context(), "a/1.jpg", "too big"); err != nil {
		t.Fatal(err)
	}
	e.file("b/1.jpg", "one")
	e.scan()
	if r := e.libRow("b/1.jpg"); r.failed || r.fileID != "" {
		t.Fatalf("copy = %+v", r)
	}
}
