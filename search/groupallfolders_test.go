package search

import (
	"slices"
	"strings"
	"testing"

	"github.com/grbit/meme-search-telegram-bot/store"
)

// allFoldersEnv: a trusted user with saved folder "mine", INLINE_GROUP_SCOPE
// folder "defdir", and admin 7.
func allFoldersEnv(t *testing.T) (*env, *fakeAPI) {
	t.Helper()
	api := &fakeAPI{results: memeRange(1)}
	srv := newServer(api, "defdir", "mine", "x")
	e := newEnv(t, api, &fakeMedia{uploads: true, known: map[int64]string{1: "f1"}},
		withServer(srv), withGroupScope("dir:defdir"))
	e.tg.admins = []int64{7}
	e.process(callbackUpdate(userID, folderToggleData(1, "mine")))
	return e, api
}

func groupFlag(t *testing.T, e *env) (folders []string, all bool) {
	t.Helper()
	g, err := e.mod.groupByChat(t.Context(), groupID)
	if err != nil || g == nil {
		t.Fatalf("group scope = %+v err = %v", g, err)
	}
	return g.folders, g.allFolders
}

func TestGroupAllFoldersFlag(t *testing.T) {
	e, api := allFoldersEnv(t)
	e.process(groupCmd("/memescope set #cat dir:*", 7))
	if txt := e.lastSent(t, "sendMessage").fields["text"]; !strings.Contains(txt, "folders: all") {
		t.Fatalf("button message = %q", txt)
	}
	if folders, all := groupFlag(t, e); !all || len(folders) != 0 {
		t.Fatalf("folders %v all %v", folders, all)
	}
	code := scopeCode(t, e)
	e.process(inlineFromChat(code+" funny", "supergroup"))
	if o := api.opts[0]; len(o.Folders) != 0 || !slices.Equal(o.Tags, []string{"cat"}) {
		t.Fatalf("all folders must beat saved and default folders: %+v", o)
	}
	// A trusted user's own dir: is still the most specific.
	e.process(inlineFromChat(code+" dir:x funny", "supergroup"))
	if o := api.opts[1]; !slices.Equal(o.Folders, []string{"x"}) {
		t.Fatalf("typed folder: %+v", o)
	}

	e.process(groupCmd("/memescope set dir:*", 7)) // alone is a valid scope now
	if _, all := groupFlag(t, e); !all {
		t.Fatal("dir:* alone refused")
	}
	e.process(groupCmd("/memescope set dir:x", 7))
	if folders, all := groupFlag(t, e); all || !slices.Equal(folders, []string{"x"}) {
		t.Fatalf("specific folders must clear the flag: %v %v", folders, all)
	}
	e.process(groupCmd("/memescope set dir:*", 7))
	e.process(groupCmd("/memescope reset", 7))
	if _, all := groupFlag(t, e); all {
		t.Fatal("reset kept the flag")
	}
}

func TestGroupPickerAllFolders(t *testing.T) {
	e, _ := allFoldersEnv(t)
	e.process(groupCallback(7, "srch:gp:f:p:0"))
	e.process(groupCallback(7, pickerButton(t, e.lastSent(t, "editMessageText"), "x (3)")))
	e.process(groupCallback(7, pickerButton(t, e.lastSent(t, "editMessageText"), "All folders")))
	if folders, all := groupFlag(t, e); !all || len(folders) != 0 {
		t.Fatalf("after All folders: %v %v", folders, all)
	}
	e.process(groupCallback(7, pickerButton(t, e.lastSent(t, "editMessageText"), "x (3)")))
	if folders, all := groupFlag(t, e); all || !slices.Equal(folders, []string{"x"}) {
		t.Fatalf("after picking x: %v %v", folders, all)
	}
	// A user's "All folders" stays what it was: no saved folders.
	e.process(callbackUpdate(userID, "srch:p:f:all"))
	if p := e.mod.loadPrefs(t.Context(), userID); len(p.Folders) != 0 {
		t.Fatalf("user prefs = %+v", p)
	}
}

// Rows saved before the flag existed load as "no flag".
func TestGroupScopesSurviveAllFoldersMigration(t *testing.T) {
	db := testDB(t)
	if err := store.Migrate(t.Context(), db, "search", migrations[:3]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO search_group_scopes (chat_id, code, tags, folders, set_by, updated_at)
		VALUES (?, 'abcdef', '["cat"]', '["x"]', 7, 1)`, groupID); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, &fakeAPI{}, &fakeMedia{uploads: true}, func(_ *Config, d *Deps) { d.DB = db })
	if folders, all := groupFlag(t, e); all || !slices.Equal(folders, []string{"x"}) {
		t.Fatalf("old row: %v %v", folders, all)
	}
}
