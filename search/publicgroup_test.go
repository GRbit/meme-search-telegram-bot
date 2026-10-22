package search

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

// publicGroupEnv has userID public (or trusted), a group default and a
// group code set by admin 7 with tag "code".
func publicGroupEnv(t *testing.T, trusted bool) (*env, *fakeAPI, string, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	api := &fakeAPI{results: memeRange(1)}
	srv := newServer(api, "x", "defdir")
	e := newEnv(t, api, &fakeMedia{uploads: true, known: map[int64]string{1: "f1"}},
		withServer(srv), withGroupScope("#def dir:defdir"),
		withLog(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		func(_ *Config, d *Deps) { d.Trusted = func(int64) bool { return trusted } })
	e.tg.admins = []int64{7}
	e.process(groupCmd("/memescope set #code", 7))
	return e, api, scopeCode(t, e), &buf
}

func TestPublicGroupInlineIgnoresTypedFilters(t *testing.T) {
	e, api, code, buf := publicGroupEnv(t, false)
	e.process(inlineFromChat("#dog dir:x funny", "supergroup"))
	o := api.opts[0]
	if api.searches[0] != "funny" || !slices.Equal(o.Tags, []string{"def"}) || !slices.Equal(o.Folders, []string{"defdir"}) {
		t.Fatalf("search %q opts %+v: typed filters must give way to INLINE_GROUP_SCOPE", api.searches[0], o)
	}
	if !strings.Contains(buf.String(), "ignored_tags=1") || !strings.Contains(buf.String(), "ignored_folders=1") {
		t.Fatalf("no debug line with counts:\n%s", buf.String())
	}
	// dir:* is a typed filter too: the group default folders stay.
	e.process(inlineFromChat("dir:* funny", "group"))
	if !slices.Equal(api.opts[1].Folders, []string{"defdir"}) {
		t.Fatalf("dir:* applied: %+v", api.opts[1])
	}
	// The pinned button's code keeps working.
	e.process(inlineFromChat(code+" #dog funny", "supergroup"))
	if !slices.Equal(api.opts[2].Tags, []string{"code"}) {
		t.Fatalf("group code: %+v", api.opts[2])
	}
	// Nothing but filters: an empty query, not searched and not charged.
	n := api.searchCount()
	e.process(inlineFromChat("#dog", "supergroup"))
	if api.searchCount() != n || Chargeable(inlineFromChat("#dog", "supergroup")) {
		t.Fatal("a filters-only query was searched or charged")
	}
}

func TestPublicInlineOutsideGroupsKeepsFilters(t *testing.T) {
	e, api, _, _ := publicGroupEnv(t, false)
	for _, chatType := range []string{"private", "sender", ""} {
		e.process(inlineFromChat("#dog funny", chatType))
		if o := api.opts[len(api.opts)-1]; !slices.Equal(o.Tags, []string{"dog"}) {
			t.Fatalf("chat type %q: %+v", chatType, o)
		}
	}
	e.process(textUpdate("#dog funny"))
	if o := api.opts[len(api.opts)-1]; !slices.Equal(o.Tags, []string{"dog"}) {
		t.Fatalf("private search: %+v", o)
	}
}

func TestTrustedGroupInlineKeepsFilters(t *testing.T) {
	e, api, _, _ := publicGroupEnv(t, true)
	e.process(inlineFromChat("#dog dir:x funny", "supergroup"))
	if o := api.opts[0]; !slices.Equal(o.Tags, []string{"dog"}) || !slices.Equal(o.Folders, []string{"x"}) {
		t.Fatalf("trusted user: %+v", o)
	}
}
