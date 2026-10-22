package search

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/i18n"
	"github.com/grbit/meme-search-telegram-bot/memesearch"
	"github.com/grbit/meme-search-telegram-bot/store"
)

// groupScope is the meme scope a group's admins set with /memescope.
//
// Telegram never tells a bot which chat an inline query comes from, so the
// scope travels as "g:<code>" in the query text, prefilled by a button the
// admins pin. Codes are not secrets: memes are not private, a spoofed code
// only changes the spoofer's own results.
type groupScope struct {
	chatID  int64
	code    string
	tags    []string
	folders []string
	// allFolders searches every folder: it beats the user's saved folders and
	// INLINE_GROUP_SCOPE, which an empty folder list would not.
	allFolders bool
}

const (
	groupCodePrefix = "g:"
	groupCodeLen    = 6
	// groupCodeAlphabet skips look-alikes (l, 1, o, 0) for codes read off a screen.
	groupCodeAlphabet = "abcdefghijkmnpqrstuvwxyz23456789"
)

func newGroupCode() string {
	var b [groupCodeLen]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails on supported platforms
	for i := range b {
		b[i] = groupCodeAlphabet[int(b[i])%len(groupCodeAlphabet)]
	}
	return string(b[:])
}

func (m *Module) loadGroup(ctx context.Context, where string, arg any) (*groupScope, error) {
	g := &groupScope{}
	var tags, folders string
	err := m.deps.DB.QueryRowContext(ctx, `SELECT chat_id, code, tags, folders, all_folders FROM search_group_scopes WHERE `+where, arg).
		Scan(&g.chatID, &g.code, &tags, &folders, &g.allFolders)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("search: read group scope: %w", err)
	}
	if err := json.Unmarshal([]byte(tags), &g.tags); err != nil {
		return nil, fmt.Errorf("search: group scope tags: %w", err)
	}
	if err := json.Unmarshal([]byte(folders), &g.folders); err != nil {
		return nil, fmt.Errorf("search: group scope folders: %w", err)
	}
	return g, nil
}

func (m *Module) groupByChat(ctx context.Context, chat int64) (*groupScope, error) {
	return m.loadGroup(ctx, "chat_id = ?", chat)
}

func (m *Module) groupByCode(ctx context.Context, code string) (*groupScope, error) {
	return m.loadGroup(ctx, "code = ?", code)
}

// saveGroup stores the scope of chat and returns its code. An existing code is
// kept, so a pinned button survives every later "set" and "reset".
// allFolders is stored only without specific folders: the two exclude each other.
func (m *Module) saveGroup(ctx context.Context, chat int64, tags, folders []string, allFolders bool, setBy int64) (string, error) {
	allFolders = allFolders && len(folders) == 0
	tj, err := json.Marshal(nonNil(tags))
	if err != nil {
		return "", err
	}
	fj, err := json.Marshal(nonNil(folders))
	if err != nil {
		return "", err
	}
	var code string
	// A fresh code can collide with another group's; a few tries make that vanishingly unlikely.
	for range 5 {
		err = store.WriteTx(ctx, m.deps.DB, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, `INSERT INTO search_group_scopes (chat_id, code, tags, folders, all_folders, set_by, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (chat_id) DO UPDATE SET tags = excluded.tags, folders = excluded.folders,
					all_folders = excluded.all_folders, set_by = excluded.set_by, updated_at = excluded.updated_at
				RETURNING code`, chat, newGroupCode(), string(tj), string(fj), allFolders, setBy, m.now().Unix()).Scan(&code)
		})
		if err == nil {
			return code, nil
		}
	}
	return "", fmt.Errorf("search: save group scope: %w", err)
}

func (m *Module) deleteGroup(ctx context.Context, chat int64) error {
	return store.WriteTx(ctx, m.deps.DB, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM search_group_scopes WHERE chat_id = ?`, chat)
		return err
	})
}

// groupSubcommands also exist as commands of their own ("/memescope_set"),
// which Telegram's menu can list; "/memescope set" stays an alias.
var groupSubcommands = []string{"set", "pick", "reset", "button", "off"}

const groupCommand = "memescope"

// groupSubcommand maps a command name to its subcommand: "" for /memescope
// itself, ok false for anything else.
func groupSubcommand(name string) (sub string, ok bool) {
	if name == groupCommand {
		return "", true
	}
	sub, found := strings.CutPrefix(name, groupCommand+"_")
	return sub, found && slices.Contains(groupSubcommands, sub)
}

func isGroupScopeCommand(u *models.Update) bool {
	msg := u.Message
	if msg == nil || msg.Text == "" || !isGroupChatType(string(msg.Chat.Type)) {
		return false
	}
	_, ok := groupSubcommand(commandName(msg.Text))
	return ok
}

// isBotRemoved matches the bot leaving or being kicked from a chat.
func isBotRemoved(u *models.Update) bool {
	if u.MyChatMember == nil {
		return false
	}
	t := u.MyChatMember.NewChatMember.Type
	return t == models.ChatMemberTypeLeft || t == models.ChatMemberTypeBanned
}

func (m *Module) handleBotRemoved(ctx context.Context, _ *bot.Bot, u *models.Update) {
	chat := u.MyChatMember.Chat.ID
	m.log.DebugContext(ctx, "search: bot removed from chat, dropping its scope", "update", "my_chat_member",
		"chat_id", chat, "status", string(u.MyChatMember.NewChatMember.Type))
	if err := m.deleteGroup(ctx, chat); err != nil {
		m.log.WarnContext(ctx, "search: drop group scope failed", "chat_id", chat, "err", err)
	}
}

// commandTarget returns "bot" for "/cmd@bot args", or "".
func commandTarget(text string) string {
	word, _, _ := strings.Cut(text, " ")
	_, target, _ := strings.Cut(word, "@")
	return target
}

func (m *Module) handleGroupScope(ctx context.Context, b *bot.Bot, u *models.Update) {
	msg := u.Message
	chat := msg.Chat.ID
	var from int64
	if msg.From != nil {
		from = msg.From.ID
	}
	if target := commandTarget(msg.Text); target != "" {
		name, err := m.me.get(ctx, b)
		if err != nil {
			m.log.WarnContext(ctx, "search: getMe failed", "err", err)
			return
		}
		if !strings.EqualFold(target, name) {
			m.log.DebugContext(ctx, "search: command for another bot ignored", "chat_id", chat, "target", target)
			return
		}
	}
	lang := ""
	if msg.From != nil {
		lang = msg.From.LanguageCode
	}
	loc := m.deps.I18n.Localizer(lang)
	_, args, _ := strings.Cut(strings.TrimSpace(msg.Text), " ")
	sub, _ := groupSubcommand(commandName(msg.Text))
	rest := strings.TrimSpace(args)
	if sub == "" {
		sub, rest, _ = strings.Cut(rest, " ")
		sub = strings.ToLower(sub)
	}
	m.log.DebugContext(ctx, "search: group scope command", "update", "message", "chat_id", chat, "user_id", from,
		"sub", sub)
	reply := func(key string, data map[string]any) { m.sendText(ctx, b, chat, loc.T(key, data), nil) }

	if sub == "" || sub == "show" {
		g, err := m.groupByChat(ctx, chat)
		switch {
		case err != nil:
			m.log.WarnContext(ctx, "search: read group scope failed", "chat_id", chat, "err", err)
			reply("search.error", nil)
		case g == nil:
			reply("search.group_none", nil)
		default:
			m.postGroupButton(ctx, b, loc, g)
		}
		return
	}
	switch sub {
	case "set", "reset", "button", "off", "pick":
	default:
		reply("search.group_usage", nil)
		return
	}
	admin, err := m.isGroupAdmin(ctx, b, msg)
	if err != nil {
		m.log.WarnContext(ctx, "search: admin check failed", "chat_id", chat, "err", err)
		reply("search.group_admin_check_failed", nil)
		return
	}
	if !admin {
		m.log.DebugContext(ctx, "search: group scope change refused, not an admin", "chat_id", chat, "user_id", from)
		reply("search.group_not_admin", nil)
		return
	}
	switch sub {
	case "pick":
		m.startGroupPick(ctx, b, loc, chat)
	case "set":
		o := parseOverrides(rest)
		if o.query != "" || (len(o.tags) == 0 && len(o.folders) == 0 && !o.allFolders) {
			reply("search.group_filters_only", map[string]any{"Max": memesearch.MaxTags})
			return
		}
		folders, key, data, reason := m.acceptFilters(ctx, o)
		if key != "" {
			m.log.DebugContext(ctx, "search: group scope filters refused", "chat_id", chat, "reason", reason,
				"tags", len(o.tags), "folders", o.folders)
			reply(key, data)
			return
		}
		// As in a search, a named folder wins over dir:*.
		m.setGroupAndPost(ctx, b, loc, chat, o.tags, folders, o.allFolders && len(folders) == 0, from)
	case "reset":
		m.setGroupAndPost(ctx, b, loc, chat, nil, nil, false, from)
	case "button":
		g, err := m.groupByChat(ctx, chat)
		switch {
		case err != nil:
			m.log.WarnContext(ctx, "search: read group scope failed", "chat_id", chat, "err", err)
			reply("search.error", nil)
		case g == nil:
			reply("search.group_none", nil)
		default:
			m.postGroupButton(ctx, b, loc, g)
		}
	case "off":
		if err := m.deleteGroup(ctx, chat); err != nil {
			m.log.WarnContext(ctx, "search: drop group scope failed", "chat_id", chat, "err", err)
			reply("search.error", nil)
			return
		}
		m.log.DebugContext(ctx, "search: group scope off", "chat_id", chat, "user_id", from)
		reply("search.group_off", nil)
	}
}

func (m *Module) setGroupAndPost(ctx context.Context, b *bot.Bot, loc *i18n.Localizer, chat int64, tags, folders []string,
	allFolders bool, by int64,
) {
	code, err := m.saveGroup(ctx, chat, tags, folders, allFolders, by)
	if err != nil {
		m.log.WarnContext(ctx, "search: save group scope failed", "chat_id", chat, "err", err)
		m.sendText(ctx, b, chat, loc.T("search.error", nil), nil)
		return
	}
	m.log.DebugContext(ctx, "search: group scope saved", "chat_id", chat, "user_id", by, "tags", tags, "folders", folders,
		"all_folders", allFolders)
	m.postGroupButton(ctx, b, loc, &groupScope{chatID: chat, code: code, tags: tags, folders: folders, allFolders: allFolders})
}

// postGroupButton posts the message admins pin: its button prefills
// "@bot g:<code> " in the chat's input field.
func (m *Module) postGroupButton(ctx context.Context, b *bot.Bot, loc *i18n.Localizer, g *groupScope) {
	if !m.deps.Media.UploadsEnabled() {
		m.sendText(ctx, b, g.chatID, loc.T("search.group_inline_off", nil), nil)
		return
	}
	desc := m.groupScopeText(loc, prefs{Tags: g.tags, Folders: g.folders, AllFolders: g.allFolders})
	text := loc.T("search.group_button", map[string]any{"Scope": desc})
	if len(g.folders) > 0 && !m.caps().FolderFilter {
		text += "\n" + loc.T("search.folders_unsupported", nil)
	}
	prefill := groupCodePrefix + g.code + " "
	kb := &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{
		{Text: loc.T("search.group_search", nil), SwitchInlineQueryCurrentChat: &prefill},
	}}}
	m.sendText(ctx, b, g.chatID, text, kb)
}

// groupAnonymousAdmin is Telegram's placeholder sender of anonymous admins.
const groupAnonymousAdmin = 1087968824

// isGroupAdmin checks the sender against getChatAdministrators. Anonymous
// admins send as the group itself (sender_chat), which only admins can do.
func (m *Module) isGroupAdmin(ctx context.Context, b *bot.Bot, msg *models.Message) (bool, error) {
	if msg.SenderChat != nil && msg.SenderChat.ID == msg.Chat.ID {
		return true, nil
	}
	if msg.From == nil {
		return false, nil
	}
	return m.isChatAdmin(ctx, b, msg.Chat.ID, msg.From.ID)
}

// isChatAdmin checks user against getChatAdministrators. The anonymous-admin
// placeholder alone proves nothing: anyone's anonymous post would carry it.
func (m *Module) isChatAdmin(ctx context.Context, b *bot.Bot, chat, user int64) (bool, error) {
	if user == groupAnonymousAdmin {
		return false, nil
	}
	admins, err := b.GetChatAdministrators(ctx, &bot.GetChatAdministratorsParams{ChatID: chat})
	if err != nil {
		return false, err
	}
	for _, a := range admins {
		switch {
		case a.Owner != nil && a.Owner.User != nil && a.Owner.User.ID == user:
			return true, nil
		case a.Administrator != nil && a.Administrator.User.ID == user:
			return true, nil
		}
	}
	return false, nil
}

// inlineGroup resolves a g:<code> of an inline query; unknown codes are
// ignored so a stale pinned button still searches with personal settings.
func (m *Module) inlineGroup(ctx context.Context, user int64, code string) *groupScope {
	if code == "" {
		return nil
	}
	g, err := m.groupByCode(ctx, code)
	switch {
	case err != nil:
		m.log.WarnContext(ctx, "search: read group scope failed", "user_id", user, "err", err)
	case g == nil:
		m.log.DebugContext(ctx, "search: unknown group code ignored", "user_id", user, "code", code)
	default:
		m.log.DebugContext(ctx, "search: group code applied", "user_id", user, "chat_id", g.chatID)
	}
	return g
}
