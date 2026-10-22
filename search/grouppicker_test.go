package search

import (
	"slices"
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"
)

func groupCallback(from int64, data string) *models.Update {
	return &models.Update{CallbackQuery: &models.CallbackQuery{
		ID: "gcb", Data: data, From: models.User{ID: from, LanguageCode: "en"},
		Message: models.MaybeInaccessibleMessage{
			Type:    models.MaybeInaccessibleMessageTypeMessage,
			Message: &models.Message{ID: 88, Chat: models.Chat{ID: groupID, Type: models.ChatTypeSupergroup}},
		},
	}}
}

func groupPickerEnv(t *testing.T) (*env, *fakeServer) {
	t.Helper()
	api := &fakeAPI{results: memeRange(1)}
	srv := newServer(api, "f1", "f2").withTags("cat", "dog")
	e := newEnv(t, api, &fakeMedia{uploads: true, known: map[int64]string{1: "f1"}}, withServer(srv))
	e.tg.admins = []int64{7, 8}
	return e, srv
}

func TestGroupPickerOpensForAdmins(t *testing.T) {
	e, _ := groupPickerEnv(t)
	e.process(groupCmd("/memescope pick", 99))
	if txt := e.lastSent(t, "sendMessage").fields["text"]; !strings.Contains(txt, "Only group admins") {
		t.Fatalf("non-admin reply = %q", txt)
	}
	e.process(groupCmd("/memescope pick", 8))
	bs := buttons(t, e.lastSent(t, "sendMessage"))
	for _, d := range []string{"srch:gp:tg:p:0", "srch:gp:f:p:0", "srch:gp:done"} {
		if !hasButton(bs, d) {
			t.Fatalf("no %s in %v", d, bs)
		}
	}
}

func TestGroupPickerSetsScope(t *testing.T) {
	e, srv := groupPickerEnv(t)
	e.process(groupCallback(8, "srch:gp:tg:p:0"))
	e.process(groupCallback(8, pickerButton(t, e.lastSent(t, "editMessageText"), "dog (2)")))
	e.process(groupCallback(8, "srch:gp:f:p:0"))
	e.process(groupCallback(8, pickerButton(t, e.lastSent(t, "editMessageText"), "f2 (2)")))
	g, err := e.mod.groupByChat(t.Context(), groupID)
	if err != nil || g == nil || !slices.Equal(g.tags, []string{"dog"}) || !slices.Equal(g.folders, []string{"f2"}) {
		t.Fatalf("group scope = %+v err = %v", g, err)
	}
	e.process(groupCallback(8, pickerButton(t, e.lastSent(t, "editMessageText"), "Back")))
	if txt := e.lastSent(t, "editMessageText").fields["text"]; !strings.Contains(txt, "dog") || !strings.Contains(txt, "f2") {
		t.Fatalf("parent view = %q", txt)
	}
	e.process(groupCallback(8, "srch:gp:done"))
	done := e.lastSent(t, "editMessageText")
	if len(buttons(t, done)) != 0 || !strings.Contains(done.fields["text"], "dog") {
		t.Fatalf("done = %q %v", done.fields["text"], buttons(t, done))
	}
	// The scope works through the group's code like one set by command.
	e.process(groupCmd("/memescope button", 8))
	e.process(inlineUpdate(scopeCode(t, e) + " funny"))
	last := srv.opts[len(srv.opts)-1]
	if !slices.Equal(last.Tags, []string{"dog"}) || !slices.Equal(last.Folders, []string{"f2"}) {
		t.Fatalf("inline opts = %+v", last)
	}
}

func TestGroupPickerRefusesNonAdminPress(t *testing.T) {
	e, _ := groupPickerEnv(t)
	e.process(groupCallback(99, "srch:gp:tg:p:0"))
	if txt := e.lastSent(t, "answerCallbackQuery").fields["text"]; !strings.Contains(txt, "Only group admins") {
		t.Fatalf("popup = %q", txt)
	}
	e.process(groupCallback(99, pickToggleData(groupPickPrefix+"tg:", 0, "cat")))
	if g, _ := e.mod.groupByChat(t.Context(), groupID); g != nil {
		t.Fatalf("non-admin changed the scope: %+v", g)
	}
	if len(e.tg.byMethod("editMessageText")) != 0 {
		t.Fatal("non-admin press edited the picker")
	}
}

func TestGroupPickerOldServer(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	e := newEnv(t, api, &fakeMedia{uploads: true})
	e.tg.admins = []int64{8}
	e.process(groupCmd("/memescope pick", 8))
	if txt := e.lastSent(t, "sendMessage").fields["text"]; !strings.Contains(txt, "/memescope set") {
		t.Fatalf("reply = %q", txt)
	}
}

// A press from a private chat with group picker data must not touch anything.
func TestGroupPickerIgnoredOutsideGroups(t *testing.T) {
	e, _ := groupPickerEnv(t)
	e.process(callbackUpdate(8, pickToggleData(groupPickPrefix+"tg:", 0, "cat")))
	if g, _ := e.mod.groupByChat(t.Context(), chatID); g != nil {
		t.Fatalf("scope saved for a private chat: %+v", g)
	}
}
