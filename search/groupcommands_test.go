package search

import (
	"slices"
	"strings"
	"testing"
)

// Every subcommand also exists as its own command, so Telegram's menu can
// list them; "/memescope set ..." keeps working.
func TestSeparateGroupCommands(t *testing.T) {
	srv := newServer(&fakeAPI{results: memeRange(1)}, "x").withTags("cat")
	e := newEnv(t, srv.fakeAPI, &fakeMedia{uploads: true, known: map[int64]string{1: "f1"}}, withServer(srv))
	e.tg.admins = []int64{7}
	last := func() string { return e.lastSent(t, "sendMessage").fields["text"] }

	e.process(groupCmd("/memescope", 7))
	if !strings.Contains(last(), "/memescope_set") {
		t.Fatalf("no-scope reply must name the new commands: %q", last())
	}
	e.process(groupCmd("/memescope_set #cat dir:x", 7))
	if g, _ := e.mod.groupByChat(t.Context(), groupID); g == nil || !slices.Equal(g.tags, []string{"cat"}) || !slices.Equal(g.folders, []string{"x"}) {
		t.Fatalf("after /memescope_set: %+v", g)
	}
	e.process(groupCmd("/memescope_set@meme_test_bot #dog", 7))
	if g, _ := e.mod.groupByChat(t.Context(), groupID); !slices.Equal(g.tags, []string{"dog"}) {
		t.Fatalf("addressed /memescope_set: %+v", g)
	}
	n := len(e.tg.byMethod("sendMessage"))
	e.process(groupCmd("/memescope_set@otherbot #owl", 7))
	if len(e.tg.byMethod("sendMessage")) != n {
		t.Fatal("a command for another bot was answered")
	}
	e.process(groupCmd("/memescope_set #owl", 99))
	if !strings.Contains(last(), "Only group admins") {
		t.Fatalf("non-admin reply = %q", last())
	}
	e.process(groupCmd("/memescope_button", 7))
	if scopeCode(t, e) == "" {
		t.Fatal("no button")
	}
	e.process(groupCmd("/memescope_pick", 7))
	if !hasButton(buttons(t, e.lastSent(t, "sendMessage")), "srch:gp:done") {
		t.Fatalf("picker = %q", last())
	}
	e.process(groupCmd("/memescope_reset", 7))
	if g, _ := e.mod.groupByChat(t.Context(), groupID); len(g.tags) != 0 {
		t.Fatalf("after reset: %+v", g)
	}
	e.process(groupCmd("/memescope_off", 7))
	if g, _ := e.mod.groupByChat(t.Context(), groupID); g != nil {
		t.Fatalf("after off: %+v", g)
	}
	e.process(groupCmd("/memescope oops", 7))
	if !strings.Contains(last(), "/memescope_pick") {
		t.Fatalf("usage must name the new commands: %q", last())
	}
	n = len(e.tg.byMethod("sendMessage"))
	e.process(groupCmd("/memescope_nope", 7))
	if len(e.tg.byMethod("sendMessage")) != n {
		t.Fatal("an unknown /memescope_ command was answered")
	}
}
