package search

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

const foldersNote = "cannot filter by folder"

func TestFolderFilterAppliedOnNewServer(t *testing.T) {
	api := &fakeAPI{results: memeRange(3)}
	srv := newServer(api, "tg-alice", "work/reactions")
	e := newEnv(t, api, &fakeMedia{uploads: true}, withServer(srv))
	e.process(textUpdate("cat dir:Work/Reactions"))
	if len(api.opts) != 1 || !slices.Equal(api.opts[0].Folders, []string{"work/reactions"}) {
		t.Fatalf("opts = %+v", api.opts)
	}
	ctl := e.lastSent(t, "sendMessage")
	if txt := ctl.fields["text"]; strings.Contains(txt, foldersNote) || !strings.Contains(txt, "folders: work/reactions") {
		t.Fatalf("control = %q", txt)
	}
	if everywhereData(t, ctl) == "" {
		t.Fatal("an applied folder filter must offer Search everywhere")
	}
	e.process(callbackUpdate(userID, everywhereData(t, ctl)))
	if len(api.opts) != 2 || len(api.opts[1].Folders) != 0 {
		t.Fatalf("everywhere rerun opts = %+v", api.opts)
	}
}

func TestFolderFilterIgnoredByOldServer(t *testing.T) {
	api := &fakeAPI{results: memeRange(3)}
	srv := newServer(api, "x")
	srv.ignore = []memesearch.Param{memesearch.ParamFolder}
	h := &levelCounter{}
	e := newEnv(t, api, &fakeMedia{uploads: true}, withServer(srv), withLog(h))
	e.process(textUpdate("cat dir:x"))
	if !slices.Equal(api.opts[0].Folders, []string{"x"}) {
		t.Fatalf("opts = %+v", api.opts)
	}
	ctl := e.lastSent(t, "sendMessage")
	if !strings.Contains(ctl.fields["text"], foldersNote) {
		t.Fatalf("control = %q", ctl.fields["text"])
	}
	if everywhereData(t, ctl) != "" {
		t.Fatal("an unapplied folder filter offers Search everywhere")
	}
	if h.warnings() == 0 {
		t.Fatal("an ignored filter must be logged at Warn")
	}
}

// A stock memesearch.Client has no folder filter: folders are never sent.
func TestFolderFilterWithoutServer(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	e := newEnv(t, api, &fakeMedia{uploads: true})
	e.process(textUpdate("cat dir:x"))
	if len(api.opts[0].Folders) != 0 || !strings.Contains(e.lastSent(t, "sendMessage").fields["text"], foldersNote) {
		t.Fatalf("opts = %+v text = %q", api.opts, e.lastSent(t, "sendMessage").fields["text"])
	}
}

func TestUnknownFolderIsNotSearched(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	e := newEnv(t, api, &fakeMedia{uploads: true}, withServer(newServer(api, "tg-alice")))
	e.process(textUpdate("cat dir:nope"))
	if api.searchCount() != 0 {
		t.Fatal("unknown folder searched")
	}
	if txt := e.lastSent(t, "sendMessage").fields["text"]; !strings.Contains(txt, "nope") || !strings.Contains(txt, "/settings") {
		t.Fatalf("reply = %q", txt)
	}
}

func TestInvalidFolderFromServer(t *testing.T) {
	api := &fakeAPI{err: &memesearch.APIError{Status: 422, Code: "invalid_folder", Message: "bad"}}
	srv := newServer(api)
	srv.foldersErr = errors.New("down") // no list to check against: the server decides
	e := newEnv(t, api, &fakeMedia{uploads: true}, withServer(srv))
	e.process(textUpdate("cat dir:nope"))
	if api.searchCount() != 1 {
		t.Fatalf("searches = %d", api.searchCount())
	}
	if txt := e.lastSent(t, "sendMessage").fields["text"]; !strings.Contains(txt, "nope") {
		t.Fatalf("reply = %q", txt)
	}
}

func TestSettingsFolderButtonOnlyWithFolderList(t *testing.T) {
	api := &fakeAPI{}
	srv := newServer(api, "a")
	e := newEnv(t, api, &fakeMedia{uploads: true}, withServer(srv))
	e.process(textUpdate("/settings"))
	msg := e.lastSent(t, "sendMessage")
	if !hasButton(buttons(t, msg), "srch:p:f:p:0") || !strings.Contains(msg.fields["text"], "Folders: all") {
		t.Fatalf("settings = %q %v", msg.fields["text"], buttons(t, msg))
	}
	srv.caps.FolderList = false
	e.process(textUpdate("/settings"))
	msg = e.lastSent(t, "sendMessage")
	if hasButton(buttons(t, msg), "srch:p:f:p:0") || strings.Contains(msg.fields["text"], "Folders") {
		t.Fatalf("old server settings = %q %v", msg.fields["text"], buttons(t, msg))
	}
}

func pickerButton(t *testing.T, c call, label string) string {
	t.Helper()
	for _, b := range buttons(t, c) {
		if strings.Contains(b[0], label) {
			return b[1]
		}
	}
	t.Fatalf("no button %q in %v", label, buttons(t, c))
	return ""
}

func TestFolderPicker(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	names := []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "b9", "tg-ann"}
	srv := newServer(api, names...)
	e := newEnv(t, api, &fakeMedia{uploads: true}, withServer(srv), func(_ *Config, d *Deps) {
		d.OwnFolder = func(u *models.User) string {
			if u.ID == userID {
				return "tg-ann"
			}
			return ""
		}
	})
	e.process(callbackUpdate(userID, "srch:p:f:p:0"))
	view := e.lastSent(t, "editMessageText")
	if strings.Contains(view.fields["reply_markup"], "b9") {
		t.Fatal("first page shows more than one page of folders")
	}
	e.process(callbackUpdate(userID, pickerButton(t, view, "a2 (2)")))
	view = e.lastSent(t, "editMessageText")
	if !strings.Contains(pickerButton(t, view, "a2 (2)"), "srch:p:f:t:") || !strings.Contains(view.fields["text"], "a2") {
		t.Fatalf("after toggle = %q %v", view.fields["text"], buttons(t, view))
	}
	e.process(callbackUpdate(userID, pickerButton(t, view, ">")))
	view = e.lastSent(t, "editMessageText")
	e.process(callbackUpdate(userID, pickerButton(t, view, "b9")))
	e.process(textUpdate("cat"))
	if !slices.Equal(api.opts[0].Folders, []string{"a2", "b9"}) {
		t.Fatalf("opts = %+v", api.opts)
	}
	// Toggling a selected folder removes it.
	e.process(callbackUpdate(userID, pickerButton(t, e.lastSent(t, "editMessageText"), "b9")))
	e.process(textUpdate("cat"))
	if !slices.Equal(api.opts[1].Folders, []string{"a2"}) {
		t.Fatalf("opts = %+v", api.opts)
	}
	e.process(callbackUpdate(userID, pickerButton(t, e.lastSent(t, "editMessageText"), "Only mine")))
	e.process(textUpdate("cat"))
	if !slices.Equal(api.opts[2].Folders, []string{"tg-ann"}) {
		t.Fatalf("opts = %+v", api.opts)
	}
	e.process(callbackUpdate(userID, pickerButton(t, e.lastSent(t, "editMessageText"), "All folders")))
	e.process(textUpdate("cat"))
	if len(api.opts[3].Folders) != 0 {
		t.Fatalf("opts = %+v", api.opts)
	}
	e.process(callbackUpdate(userID, pickerButton(t, e.lastSent(t, "editMessageText"), "Back")))
	if txt := e.lastSent(t, "editMessageText").fields["text"]; !strings.Contains(txt, "Search settings") {
		t.Fatalf("back = %q", txt)
	}
}

func TestFolderPickerStaleIndex(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	srv := newServer(api, "a", "b")
	e := newEnv(t, api, &fakeMedia{uploads: true}, withServer(srv))
	e.process(callbackUpdate(userID, "srch:p:f:p:0"))
	data := pickerButton(t, e.lastSent(t, "editMessageText"), "b (2)")
	srv.folders = []memesearch.Folder{{Name: "a"}, {Name: "c"}}
	e.clock.Advance(10 * time.Minute) // past the list cache
	e.process(callbackUpdate(userID, data))
	e.process(textUpdate("cat"))
	if len(api.opts[0].Folders) != 0 {
		t.Fatalf("stale button selected %v", api.opts[0].Folders)
	}
}

func TestFolderPickerLimit(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	var names []string
	for i := range memesearch.MaxFolders + 1 {
		names = append(names, "f"+string(rune('a'+i)))
	}
	srv := newServer(api, names...)
	e := newEnv(t, api, &fakeMedia{uploads: true}, withServer(srv))
	for i := range memesearch.MaxFolders + 1 {
		e.process(callbackUpdate(userID, folderToggleData(i, names[i])))
	}
	e.process(textUpdate("cat"))
	if len(api.opts[0].Folders) != memesearch.MaxFolders {
		t.Fatalf("folders = %v", api.opts[0].Folders)
	}
}

func TestFolderListCached(t *testing.T) {
	api := &fakeAPI{}
	srv := newServer(api, "a")
	e := newEnv(t, api, &fakeMedia{uploads: true}, withServer(srv))
	e.process(callbackUpdate(userID, "srch:p:f:p:0"))
	e.process(callbackUpdate(userID, "srch:p:f:p:0"))
	if srv.folderCalls != 1 {
		t.Fatalf("folder calls = %d", srv.folderCalls)
	}
	e.clock.Advance(10 * time.Minute)
	e.process(callbackUpdate(userID, "srch:p:f:p:0"))
	if srv.folderCalls != 2 {
		t.Fatalf("folder calls after TTL = %d", srv.folderCalls)
	}
}

func TestInlineSendsFolders(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	srv := newServer(api, "a", "b")
	e := newEnv(t, api, &fakeMedia{uploads: true, known: map[int64]string{1: "f1"}}, withServer(srv))
	e.process(callbackUpdate(userID, folderToggleData(1, "b")))
	e.process(inlineUpdate("funny"))
	e.process(inlineUpdate("funny dir:A"))
	if !slices.Equal(api.opts[0].Folders, []string{"b"}) || !slices.Equal(api.opts[1].Folders, []string{"a"}) {
		t.Fatalf("opts = %+v", api.opts)
	}
}

func TestGroupScopeFolders(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	srv := newServer(api, "work/reactions")
	e := newEnv(t, api, &fakeMedia{uploads: true, known: map[int64]string{1: "f1"}}, withServer(srv))
	e.tg.admins = []int64{7}
	e.process(groupCmd("/memescope set dir:Work/Reactions", 7))
	txt := e.lastSent(t, "sendMessage").fields["text"]
	if strings.Contains(txt, foldersNote) || !strings.Contains(txt, "work/reactions") {
		t.Fatalf("button message = %q", txt)
	}
	e.process(groupCmd("/memescope set dir:nope", 7))
	if txt := e.lastSent(t, "sendMessage").fields["text"]; !strings.Contains(txt, "nope") {
		t.Fatalf("unknown folder reply = %q", txt)
	}
}
