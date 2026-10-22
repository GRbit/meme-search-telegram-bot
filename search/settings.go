package search

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/i18n"
	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

const settingsPrefix = CallbackPrefix + "p:"

func isSettingsCommand(u *models.Update) bool {
	return isPrivateText(u) && commandName(u.Message.Text) == "settings"
}

// isPendingInput matches the tag list a user types after "Set tags". Other
// commands are not swallowed: /start or /settings still work mid-input.
func (m *Module) isPendingInput(u *models.Update) bool {
	if !isPrivateText(u) || !m.inputs.active(u.Message.From.ID, m.now()) {
		return false
	}
	cmd := commandName(u.Message.Text)
	return cmd == "" || cmd == "cancel"
}

func (m *Module) handleSettingsCommand(ctx context.Context, b *bot.Bot, u *models.Update) {
	msg := u.Message
	m.log.DebugContext(ctx, "search: settings opened", "update", "message", "user_id", msg.From.ID)
	loc := m.deps.I18n.Localizer(msg.From.LanguageCode)
	text, kb := m.settingsView(loc, m.loadPrefs(ctx, msg.From.ID))
	m.sendText(ctx, b, msg.Chat.ID, text, kb)
}

// foldersOffered reports whether settings show folders: the server can list
// them, or the user saved some before a downgrade and should still see them.
func (m *Module) foldersOffered(p prefs) (line, button bool) {
	button = m.caps().FolderList
	return button || len(p.Folders) > 0, button
}

// settingsView renders the settings message and its keyboard.
func (m *Module) settingsView(loc *i18n.Localizer, p prefs) (string, *models.InlineKeyboardMarkup) {
	mode := m.effectiveMode(p)
	tags := loc.T("search.tags_none", nil)
	switch {
	case len(p.Tags) > 1 && p.TagMatch == memesearch.TagMatchAll:
		tags = loc.T("search.tags_all_of", map[string]any{"Tags": strings.Join(p.Tags, ", ")})
	case len(p.Tags) > 0:
		tags = strings.Join(p.Tags, ", ")
	}
	folderLine, folderButton := m.foldersOffered(p)
	folders := loc.T("search.folders_all", nil)
	if len(p.Folders) > 0 {
		folders = strings.Join(p.Folders, ", ")
	}
	text := loc.T("search.settings", map[string]any{"Mode": modeName(loc, mode), "Tags": tags,
		"ShowFolders": folderLine, "Folders": folders})
	modeBtn := func(md memesearch.Mode) models.InlineKeyboardButton {
		name := modeName(loc, md)
		if md == mode {
			name = loc.T("search.settings_selected", map[string]any{"Name": name})
		}
		return models.InlineKeyboardButton{Text: name, CallbackData: settingsPrefix + "mode:" + string(md)}
	}
	tagRow := []models.InlineKeyboardButton{{Text: loc.T("search.settings_set_tags", nil), CallbackData: settingsPrefix + "tags"}}
	if m.caps().TagList {
		tagRow = append(tagRow, models.InlineKeyboardButton{Text: loc.T("search.settings_pick_tags", nil), CallbackData: tagPrefix + "p:0"})
	}
	if len(p.Tags) > 0 {
		tagRow = append(tagRow, models.InlineKeyboardButton{Text: loc.T("search.settings_clear_tags", nil), CallbackData: settingsPrefix + "clear"})
	}
	rows := [][]models.InlineKeyboardButton{{modeBtn(memesearch.ModeVector), modeBtn(memesearch.ModeKeyword)}, tagRow}
	if m.tagMatchOffered(p) {
		matchBtn := func(tm memesearch.TagMatch, id string) models.InlineKeyboardButton {
			name := loc.T(id, nil)
			if (p.TagMatch == memesearch.TagMatchAll) == (tm == memesearch.TagMatchAll) {
				name = loc.T("search.settings_selected", map[string]any{"Name": name})
			}
			return models.InlineKeyboardButton{Text: name, CallbackData: settingsPrefix + "match:" + string(tm)}
		}
		rows = append(rows, []models.InlineKeyboardButton{
			matchBtn(memesearch.TagMatchAny, "search.settings_match_any"), matchBtn(memesearch.TagMatchAll, "search.settings_match_all"),
		})
	}
	if folderButton {
		rows = append(rows, []models.InlineKeyboardButton{{Text: loc.T("search.settings_folders", nil), CallbackData: folderPrefix + "p:0"}})
	}
	rows = append(rows, []models.InlineKeyboardButton{{Text: loc.T("search.settings_reset", nil), CallbackData: settingsPrefix + "reset"}})
	return text, &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func modeName(loc *i18n.Localizer, md memesearch.Mode) string {
	if md == memesearch.ModeKeyword {
		return loc.T("search.mode_keyword", nil)
	}
	return loc.T("search.mode_vector", nil)
}

func (m *Module) effectiveMode(p prefs) memesearch.Mode {
	if p.Mode != "" {
		return p.Mode
	}
	return m.cfg.Mode
}

// handleSettingsCallback applies one settings button. Settings changes are
// free: the access middleware charges only searches.
func (m *Module) handleSettingsCallback(ctx context.Context, b *bot.Bot, cq *models.CallbackQuery) {
	switch {
	case strings.HasPrefix(cq.Data, folderPrefix):
		m.handlePicker(ctx, b, cq, m.folderPicker(m.userTarget()))
		return
	case strings.HasPrefix(cq.Data, tagPrefix):
		m.handlePicker(ctx, b, cq, m.tagPicker(m.userTarget()))
		return
	}
	user := cq.From.ID
	loc := m.deps.I18n.Localizer(cq.From.LanguageCode)
	action := strings.TrimPrefix(cq.Data, settingsPrefix)
	m.log.DebugContext(ctx, "search: settings button", "user_id", user, "action", action)
	p := m.loadPrefs(ctx, user)
	switch {
	case action == "tags":
		m.answerCallback(ctx, b, cq.ID, "")
		m.inputs.set(user, m.now())
		chat := user
		if cq.Message.Message != nil {
			chat = cq.Message.Message.Chat.ID
		}
		m.sendText(ctx, b, chat, loc.T("search.settings_tags_prompt",
			map[string]any{"Max": memesearch.MaxTags, "Len": memesearch.MaxTagLen}), nil)
		return
	case action == "clear":
		p.Tags = nil
	case action == "reset":
		p = prefs{}
	case strings.HasPrefix(action, "match:"):
		switch memesearch.TagMatch(strings.TrimPrefix(action, "match:")) {
		case memesearch.TagMatchAny:
			p.TagMatch = ""
		case memesearch.TagMatchAll:
			p.TagMatch = memesearch.TagMatchAll
		default:
			m.log.DebugContext(ctx, "search: unknown tag match ignored", "user_id", user, "action", action)
			m.answerCallback(ctx, b, cq.ID, "")
			return
		}
	case strings.HasPrefix(action, "mode:"):
		md := memesearch.Mode(strings.TrimPrefix(action, "mode:"))
		if md != memesearch.ModeVector && md != memesearch.ModeKeyword {
			m.log.DebugContext(ctx, "search: unknown settings mode ignored", "user_id", user, "mode", string(md))
			m.answerCallback(ctx, b, cq.ID, "")
			return
		}
		p.Mode = md
	default:
		m.log.DebugContext(ctx, "search: unknown settings action ignored", "user_id", user, "action", action)
		m.answerCallback(ctx, b, cq.ID, "")
		return
	}
	if err := m.savePrefs(ctx, user, p); err != nil {
		m.log.WarnContext(ctx, "search: save settings failed", "user_id", user, "err", err)
		m.answerCallback(ctx, b, cq.ID, loc.T("search.error", nil))
		return
	}
	m.answerCallback(ctx, b, cq.ID, "")
	m.showSettings(ctx, b, loc, cq, p)
}

// showSettings edits the settings message in place, or sends a new one when
// Telegram no longer gives access to it.
func (m *Module) showSettings(ctx context.Context, b *bot.Bot, loc *i18n.Localizer, cq *models.CallbackQuery, p prefs) {
	m.editSettings(ctx, b, cq, func() (string, *models.InlineKeyboardMarkup) { return m.settingsView(loc, p) })
}

// editSettings replaces the settings message (or a picker in its place) with view.
func (m *Module) editSettings(ctx context.Context, b *bot.Bot, cq *models.CallbackQuery, view func() (string, *models.InlineKeyboardMarkup)) {
	text, kb := view()
	prev := cq.Message.Message
	if prev == nil {
		m.sendText(ctx, b, cq.From.ID, text, kb)
		return
	}
	_, err := b.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID: prev.Chat.ID, MessageID: prev.ID, Text: text, ReplyMarkup: kb,
	})
	if err != nil {
		// "message is not modified" lands here too when a button repeats the current value.
		m.log.DebugContext(ctx, "search: edit settings message failed", "user_id", cq.From.ID, "err", err)
	}
}

func (m *Module) handlePendingInput(ctx context.Context, b *bot.Bot, u *models.Update) {
	msg := u.Message
	user := msg.From.ID
	loc := m.deps.I18n.Localizer(msg.From.LanguageCode)
	if commandName(msg.Text) == "cancel" {
		m.inputs.clear(user)
		m.log.DebugContext(ctx, "search: tags input cancelled", "user_id", user)
		m.sendText(ctx, b, msg.Chat.ID, loc.T("search.settings_cancelled", nil), nil)
		return
	}
	tags, bad := parseTagList(msg.Text)
	switch bad {
	case tagsTooMany:
		m.log.DebugContext(ctx, "search: tags input rejected", "user_id", user, "reason", "too_many", "count", len(tags))
		m.sendText(ctx, b, msg.Chat.ID, loc.T("search.settings_tags_too_many", map[string]any{"Max": memesearch.MaxTags}), nil)
		return
	case tagsTooLong:
		m.log.DebugContext(ctx, "search: tags input rejected", "user_id", user, "reason", "too_long")
		m.sendText(ctx, b, msg.Chat.ID, loc.T("search.settings_tags_too_long",
			map[string]any{"Tag": longestTag(tags), "Len": memesearch.MaxTagLen}), nil)
		return
	}
	p := m.loadPrefs(ctx, user)
	p.Tags = tags
	if err := m.savePrefs(ctx, user, p); err != nil {
		m.log.WarnContext(ctx, "search: save tags failed", "user_id", user, "err", err)
		m.sendText(ctx, b, msg.Chat.ID, loc.T("search.error", nil), nil)
		return
	}
	m.inputs.clear(user)
	m.log.DebugContext(ctx, "search: tags saved", "user_id", user, "tags", tags)
	text, kb := m.settingsView(loc, p)
	m.sendText(ctx, b, msg.Chat.ID, text, kb)
}

type tagProblem int

const (
	tagsOK tagProblem = iota
	tagsTooMany
	tagsTooLong
)

// parseTagList splits a comma separated list, dropping a leading "#",
// blanks and repeats. Commas separate because tags may contain spaces.
func parseTagList(text string) ([]string, tagProblem) {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(text, ",") {
		t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(part), "#"))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out, tagProblemOf(out)
}

func longestTag(tags []string) string {
	best := ""
	for _, t := range tags {
		if utf8.RuneCountInString(t) > utf8.RuneCountInString(best) {
			best = t
		}
	}
	return best
}
