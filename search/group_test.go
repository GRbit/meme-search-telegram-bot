package search

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"
)

const groupID = -100500

func groupCmd(text string, from int64) *models.Update {
	return &models.Update{Message: &models.Message{
		ID: 3, Text: text,
		From: &models.User{ID: from, LanguageCode: "en", Username: "u"},
		Chat: models.Chat{ID: groupID, Type: models.ChatTypeSupergroup, Title: "Memes fans"},
	}}
}

// scopeCode returns the g:<code> the last scope button prefills.
func scopeCode(t *testing.T, e *env) string {
	t.Helper()
	msg := e.lastSent(t, "sendMessage")
	var kb models.InlineKeyboardMarkup
	if err := json.Unmarshal([]byte(msg.fields["reply_markup"]), &kb); err != nil {
		t.Fatalf("keyboard %q: %v", msg.fields["reply_markup"], err)
	}
	btn := kb.InlineKeyboard[0][0]
	if btn.SwitchInlineQueryCurrentChat == nil || !strings.HasPrefix(*btn.SwitchInlineQueryCurrentChat, "g:") {
		t.Fatalf("button = %+v", btn)
	}
	return strings.TrimSpace(*btn.SwitchInlineQueryCurrentChat)
}

func groupEnv(t *testing.T, api *fakeAPI) *env {
	e := newEnv(t, api, &fakeMedia{uploads: true, known: map[int64]string{1: "f1"}})
	e.tg.admins = []int64{7, 8}
	return e
}

func TestGroupScopeSetByAdminAndUsedInline(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	e := groupEnv(t, api)
	e.process(groupCmd("/memescope@meme_test_bot set #cat #dog", 8))
	code := scopeCode(t, e)
	if txt := e.lastSent(t, "sendMessage").fields["text"]; !strings.Contains(txt, "cat, dog") {
		t.Fatalf("button message = %q", txt)
	}
	withSavedTags(e, "mine")
	e.process(inlineUpdate(code + " funny"))
	if api.searches[0] != "funny" || !slices.Equal(api.opts[0].Tags, []string{"cat", "dog"}) {
		t.Fatalf("searches = %q opts = %+v", api.searches, api.opts)
	}
	e.process(inlineUpdate(code + " #q funny"))
	if !slices.Equal(api.opts[1].Tags, []string{"q"}) {
		t.Fatalf("query tags must beat the group code: %+v", api.opts[1])
	}
	if Chargeable(inlineUpdate(code)) {
		t.Fatal("a bare group code is charged")
	}
}

func TestGroupScopeNeedsAdmin(t *testing.T) {
	e := groupEnv(t, &fakeAPI{})
	e.process(groupCmd("/memescope set #cat", 42))
	if txt := e.lastSent(t, "sendMessage").fields["text"]; !strings.Contains(txt, "admin") {
		t.Fatalf("reply = %q", txt)
	}
	e.process(groupCmd("/memescope", 42))
	if txt := e.lastSent(t, "sendMessage").fields["text"]; !strings.Contains(txt, "no meme scope") {
		t.Fatalf("show = %q", txt)
	}
}

// Anonymous admins post as the group itself.
func TestGroupScopeAnonymousAdmin(t *testing.T) {
	e := groupEnv(t, &fakeAPI{})
	u := groupCmd("/memescope set #cat", 1087968824)
	u.Message.SenderChat = &models.Chat{ID: groupID, Type: models.ChatTypeSupergroup}
	e.process(u)
	scopeCode(t, e)
	if n := len(e.tg.byMethod("getChatAdministrators")); n != 0 {
		t.Fatalf("getChatAdministrators called %d times for an anonymous admin", n)
	}
}

func TestGroupScopeResetOffAndRepost(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	e := groupEnv(t, api)
	e.process(groupCmd("/memescope set #cat", 7))
	code := scopeCode(t, e)
	e.process(groupCmd("/memescope reset", 7))
	e.process(inlineUpdate(code + " a"))
	if len(api.opts[0].Tags) != 0 {
		t.Fatalf("reset kept tags: %+v", api.opts[0])
	}
	e.process(groupCmd("/memescope set #dog", 7))
	if got := scopeCode(t, e); got != code {
		t.Fatalf("code changed on set: %q -> %q (pinned buttons would break)", code, got)
	}
	e.process(groupCmd("/memescope button", 7))
	if got := scopeCode(t, e); got != code {
		t.Fatalf("repost code = %q", got)
	}
	e.process(groupCmd("/memescope off", 7))
	withSavedTags(e, "mine")
	e.process(inlineUpdate(code + " b"))
	if !slices.Equal(api.opts[1].Tags, []string{"mine"}) {
		t.Fatalf("code still works after off: %+v", api.opts[1])
	}
}

func TestGroupScopeRejectsWordsAndOtherBots(t *testing.T) {
	e := groupEnv(t, &fakeAPI{})
	e.process(groupCmd("/memescope set cats only", 7))
	if txt := e.lastSent(t, "sendMessage").fields["text"]; !strings.Contains(txt, "#tag") {
		t.Fatalf("reply = %q", txt)
	}
	n := len(e.tg.byMethod("sendMessage"))
	e.process(groupCmd("/memescope@other_bot set #cat", 7))
	if len(e.tg.byMethod("sendMessage")) != n {
		t.Fatal("answered a command for another bot")
	}
}

func TestUnknownGroupCodeIgnored(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	e := groupEnv(t, api)
	e.process(inlineUpdate("g:zzzzzz funny"))
	if api.searches[0] != "funny" || len(api.opts[0].Tags) != 0 {
		t.Fatalf("searches = %q opts = %+v", api.searches, api.opts)
	}
}

func TestBotRemovedFromGroupDropsScope(t *testing.T) {
	api := &fakeAPI{results: memeRange(1)}
	e := groupEnv(t, api)
	e.process(groupCmd("/memescope set #cat", 7))
	code := scopeCode(t, e)
	e.process(&models.Update{MyChatMember: &models.ChatMemberUpdated{
		Chat: models.Chat{ID: groupID, Type: models.ChatTypeSupergroup},
		From: models.User{ID: 7},
		NewChatMember: models.ChatMember{Type: models.ChatMemberTypeBanned,
			Banned: &models.ChatMemberBanned{Status: models.ChatMemberTypeBanned, User: &models.User{ID: 999}}},
	}})
	e.process(inlineUpdate(code + " x"))
	if len(api.opts[0].Tags) != 0 {
		t.Fatalf("scope kept after the bot was removed: %+v", api.opts[0])
	}
}
