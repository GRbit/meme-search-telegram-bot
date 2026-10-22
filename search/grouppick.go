package search

import (
	"context"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/i18n"
)

// groupPickPrefix starts the buttons of a group's scope picker ("/memescope pick").
const groupPickPrefix = CallbackPrefix + "gp:"

// groupTarget keeps a picker's selection in the scope of chat. Saving keeps
// the group's code, so a pinned button stays valid while admins click.
func (m *Module) groupTarget(chat int64) pickTarget {
	return pickTarget{
		prefix: groupPickPrefix,
		load: func(ctx context.Context, _ *models.CallbackQuery) (prefs, error) {
			g, err := m.groupByChat(ctx, chat)
			if err != nil || g == nil {
				return prefs{}, err
			}
			return prefs{Tags: g.tags, Folders: g.folders}, nil
		},
		save: func(ctx context.Context, cq *models.CallbackQuery, p prefs) error {
			_, err := m.saveGroup(ctx, chat, p.Tags, p.Folders, cq.From.ID)
			return err
		},
		back: func(ctx context.Context, b *bot.Bot, loc *i18n.Localizer, cq *models.CallbackQuery, p prefs) {
			m.editSettings(ctx, b, cq, func() (string, *models.InlineKeyboardMarkup) { return m.groupPickView(loc, p) })
		},
	}
}

func (m *Module) groupScopeText(loc *i18n.Localizer, p prefs) string {
	desc := m.describeScope(loc, scope{mode: m.cfg.Mode, tags: p.Tags, folders: p.Folders})
	if desc == "" {
		desc = loc.T("search.group_everything", nil)
	}
	return desc
}

// groupPickView is the picker's start: the current scope and a button per
// list the server can give.
func (m *Module) groupPickView(loc *i18n.Localizer, p prefs) (string, *models.InlineKeyboardMarkup) {
	caps := m.caps()
	var row []models.InlineKeyboardButton
	if caps.TagList {
		row = append(row, models.InlineKeyboardButton{Text: loc.T("search.settings_pick_tags", nil), CallbackData: groupPickPrefix + "tg:p:0"})
	}
	if caps.FolderList {
		row = append(row, models.InlineKeyboardButton{Text: loc.T("search.settings_folders", nil), CallbackData: groupPickPrefix + "f:p:0"})
	}
	rows := [][]models.InlineKeyboardButton{row,
		{{Text: loc.T("search.group_pick_done", nil), CallbackData: groupPickPrefix + "done"}}}
	return loc.T("search.group_pick", map[string]any{"Scope": m.groupScopeText(loc, p)}),
		&models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// startGroupPick answers "/memescope pick" from an admin with the picker message.
func (m *Module) startGroupPick(ctx context.Context, b *bot.Bot, loc *i18n.Localizer, chat int64) {
	caps := m.caps()
	if !caps.TagList && !caps.FolderList {
		m.log.DebugContext(ctx, "search: group picker without server lists", "chat_id", chat)
		m.sendText(ctx, b, chat, loc.T("search.group_pick_unsupported", nil), nil)
		return
	}
	g, err := m.groupByChat(ctx, chat)
	if err != nil {
		m.log.WarnContext(ctx, "search: read group scope failed", "chat_id", chat, "err", err)
		m.sendText(ctx, b, chat, loc.T("search.error", nil), nil)
		return
	}
	var p prefs
	if g != nil {
		p = prefs{Tags: g.tags, Folders: g.folders}
	}
	text, kb := m.groupPickView(loc, p)
	m.sendText(ctx, b, chat, text, kb)
}

// handleGroupPick runs a group picker button. Every press is checked against
// the group's admins: the picker message is visible to the whole group.
func (m *Module) handleGroupPick(ctx context.Context, b *bot.Bot, cq *models.CallbackQuery) {
	loc := m.deps.I18n.Localizer(cq.From.LanguageCode)
	msg := cq.Message.Message
	if msg == nil || !isGroupChatType(string(msg.Chat.Type)) {
		m.log.DebugContext(ctx, "search: group picker button outside a group ignored", "user_id", cq.From.ID)
		m.answerCallback(ctx, b, cq.ID, "")
		return
	}
	chat := msg.Chat.ID
	admin, err := m.isChatAdmin(ctx, b, chat, cq.From.ID)
	switch {
	case err != nil:
		m.log.WarnContext(ctx, "search: admin check failed", "chat_id", chat, "err", err)
		m.answerCallback(ctx, b, cq.ID, loc.T("search.group_admin_check_failed", nil))
		return
	case !admin:
		m.log.DebugContext(ctx, "search: group picker press refused, not an admin", "chat_id", chat, "user_id", cq.From.ID)
		m.answerCallback(ctx, b, cq.ID, loc.T("search.group_not_admin", nil))
		return
	}
	action := strings.TrimPrefix(cq.Data, groupPickPrefix)
	m.log.DebugContext(ctx, "search: group picker button", "chat_id", chat, "user_id", cq.From.ID, "action", action)
	t := m.groupTarget(chat)
	switch {
	case strings.HasPrefix(action, "f:"):
		m.handlePicker(ctx, b, cq, m.folderPicker(t))
	case strings.HasPrefix(action, "tg:"):
		m.handlePicker(ctx, b, cq, m.tagPicker(t))
	case action == "done":
		p, err := t.load(ctx, cq)
		if err != nil {
			m.log.WarnContext(ctx, "search: read group scope failed", "chat_id", chat, "err", err)
			m.answerCallback(ctx, b, cq.ID, loc.T("search.error", nil))
			return
		}
		m.answerCallback(ctx, b, cq.ID, "")
		m.editSettings(ctx, b, cq, func() (string, *models.InlineKeyboardMarkup) {
			// An empty keyboard removes the buttons; nil would keep them.
			return loc.T("search.group_pick_saved", map[string]any{"Scope": m.groupScopeText(loc, p)}),
				&models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}}
		})
	default:
		m.answerCallback(ctx, b, cq.ID, "")
	}
}
