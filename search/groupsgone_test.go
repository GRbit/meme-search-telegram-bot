package search

import (
	"slices"
	"testing"

	"github.com/grbit/meme-search-telegram-bot/store"
)

// Group scopes were dropped: a "g:" word is ordinary query text everywhere.
func TestGroupCodeIsQueryText(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	e := newEnv(t, api, &fakeMedia{uploads: true, known: map[int64]string{1: "f1"}})
	e.process(inlineUpdate("g:abcdef funny"))
	e.process(textUpdate("g:abcdef funny"))
	if !slices.Equal(api.searches, []string{"g:abcdef funny", "g:abcdef funny"}) {
		t.Fatalf("searches = %q", api.searches)
	}
	if !Chargeable(inlineUpdate("g:abcdef")) {
		t.Fatal("a lone g: word is a query now and is charged")
	}
}

func TestMemescopeIsGone(t *testing.T) {
	e := newEnv(t, &fakeAPI{}, &fakeMedia{uploads: true})
	e.tg.admins = []int64{7}
	e.process(groupUpdate("/memescope set #cat", 7))
	e.process(groupUpdate("/memescope pick", 7))
	if n := len(e.tg.byMethod("sendMessage")); n != 0 {
		t.Fatalf("/memescope answered %d messages", n)
	}
}

// A database from before the removal loses the group scope table; earlier
// migration steps stay as released.
func TestMigrationDropsGroupScopes(t *testing.T) {
	db := testDB(t)
	if err := store.Migrate(t.Context(), db, "search", migrations[:3]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO search_group_scopes (chat_id, code, set_by, updated_at) VALUES (-1, 'abc', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	newEnv(t, &fakeAPI{}, &fakeMedia{}, func(_ *Config, d *Deps) { d.DB = db })
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_master WHERE name = 'search_group_scopes'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("table still there: n=%d err=%v", n, err)
	}
}
