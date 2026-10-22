package search

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/configerr"
	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

func inlineFromChat(q, chatType string) *models.Update {
	u := inlineUpdate(q)
	u.InlineQuery.ChatType = chatType
	return u
}

func withGroupScope(s string) envOpt {
	return func(c *Config, _ *Deps) { c.InlineGroupScope = s }
}

func TestInlineUsesSavedTagsButKeepsMode(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	e := newEnv(t, api, &fakeMedia{uploads: true, known: map[int64]string{1: "f1"}})
	e.process(callbackUpdate(userID, "srch:p:mode:keyword"))
	withSavedTags(e, "cat")
	e.process(inlineUpdate("funny"))
	if len(api.opts) != 1 || !slices.Equal(api.opts[0].Tags, []string{"cat"}) || api.opts[0].Mode != memesearch.ModeVector {
		t.Fatalf("opts = %+v", api.opts)
	}
}

func TestInlineOverridesStripped(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	e := newEnv(t, api, &fakeMedia{uploads: true, known: map[int64]string{1: "f1"}})
	withSavedTags(e, "cat")
	e.process(inlineUpdate("#dog dir:x funny"))
	if api.searches[0] != "funny" || !slices.Equal(api.opts[0].Tags, []string{"dog"}) {
		t.Fatalf("searches = %q opts = %+v", api.searches, api.opts)
	}
	if Chargeable(inlineUpdate("#dog")) {
		t.Fatal("filters-only inline query is charged")
	}
}

// Precedence, most specific first: query, saved settings, group default.
func TestInlineGroupScopeDefault(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	e := newEnv(t, api, &fakeMedia{uploads: true, known: map[int64]string{1: "f1"}}, withGroupScope("#reaction #work"))
	for _, tc := range []struct {
		chatType string
		want     []string
	}{
		{"supergroup", []string{"reaction", "work"}},
		{"group", []string{"reaction", "work"}},
		{"private", nil},
		{"sender", nil},
	} {
		api.opts = nil
		e.process(inlineFromChat("x", tc.chatType))
		if !slices.Equal(api.opts[0].Tags, tc.want) {
			t.Errorf("%s: tags = %q, want %q", tc.chatType, api.opts[0].Tags, tc.want)
		}
	}
	withSavedTags(e, "mine")
	api.opts = nil
	e.process(inlineFromChat("x", "group"))
	if !slices.Equal(api.opts[0].Tags, []string{"mine"}) {
		t.Fatalf("saved tags must beat the group default: %q", api.opts[0].Tags)
	}
	api.opts = nil
	e.process(inlineFromChat("#q x", "group"))
	if !slices.Equal(api.opts[0].Tags, []string{"q"}) {
		t.Fatalf("query tags must beat everything: %q", api.opts[0].Tags)
	}
}

func TestInlineGroupScopeValidation(t *testing.T) {
	for _, bad := range []string{"words #x", "#1 #2 #3 #4 #5 #6 #7 #8 #9 #10 #11", "#123456789012345678901",
		manyDirs(memesearch.MaxFolders + 1), "dir:" + strings.Repeat("f", memesearch.MaxFolderLen+1)} {
		fields, _ := configerr.Fields((Config{InlineGroupScope: bad}).Validate())
		if len(fields) != 1 || fields[0].Name != "InlineGroupScope" {
			t.Errorf("%q: fields = %+v", bad, fields)
		}
	}
	for _, ok := range []string{"", "#a", "#a dir:memes", "dir:*"} {
		if err := (Config{InlineGroupScope: ok}).Validate(); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
}

func manyDirs(n int) string {
	var parts []string
	for i := range n {
		parts = append(parts, fmt.Sprintf("dir:f%d", i))
	}
	return strings.Join(parts, " ")
}
