package search

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/i18n"
	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

const inlineCacheSeconds = 30

// loadingStartParam is the /start payload of the "memes are loading" button;
// handleCommand ignores payloads, so it greets like a plain /start.
const loadingStartParam = "loading"

func isPrivateText(u *models.Update) bool {
	return u.Message != nil && u.Message.From != nil && u.Message.Chat.Type == models.ChatTypePrivate &&
		u.Message.Text != ""
}

// isTextSearch matches private non-command text; blank text is left alone so
// a sibling module (or nobody) gets it.
func isTextSearch(u *models.Update) bool {
	return isPrivateText(u) && !strings.HasPrefix(u.Message.Text, "/") && strings.TrimSpace(u.Message.Text) != ""
}

// commandName returns "start" for "/start", "/start payload" and "/start@bot".
func commandName(text string) string {
	if !strings.HasPrefix(text, "/") {
		return ""
	}
	word, _, _ := strings.Cut(text[1:], " ")
	word, _, _ = strings.Cut(word, "@")
	return word
}

func isCommand(u *models.Update) bool {
	if !isPrivateText(u) {
		return false
	}
	switch commandName(u.Message.Text) {
	case "start", "help":
		return true
	}
	return false
}

func isOwnCallback(u *models.Update) bool {
	return u.CallbackQuery != nil && strings.HasPrefix(u.CallbackQuery.Data, CallbackPrefix)
}

func isInline(u *models.Update) bool {
	return u.InlineQuery != nil && u.InlineQuery.From != nil
}

// botName caches the bot's username: it never changes while the bot runs.
type botName struct {
	mu   sync.Mutex
	name string
}

func (n *botName) get(ctx context.Context, b *bot.Bot) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.name != "" {
		return n.name, nil
	}
	me, err := b.GetMe(ctx)
	if err != nil {
		return "", err
	}
	n.name = me.Username
	return n.name, nil
}

func (m *Module) handleCommand(ctx context.Context, b *bot.Bot, u *models.Update) {
	msg := u.Message
	cmd := commandName(msg.Text)
	m.log.DebugContext(ctx, "search: command", "update", "message", "user_id", msg.From.ID, "command", cmd)
	name, err := m.me.get(ctx, b)
	if err != nil {
		m.log.WarnContext(ctx, "search: getMe failed", "err", err)
	}
	loc := m.deps.I18n.Localizer(msg.From.LanguageCode)
	m.sendText(ctx, b, msg.Chat.ID, loc.T("search."+cmd, map[string]any{"BotUsername": name, "Inline": m.deps.Media.UploadsEnabled()}), nil)
}

func (m *Module) handleText(ctx context.Context, b *bot.Bot, u *models.Update) {
	msg := u.Message
	user := msg.From.ID
	loc := m.deps.I18n.Localizer(msg.From.LanguageCode)
	q, ok := searchQuery(msg.Text)
	m.log.DebugContext(ctx, "search: text", "update", "message", "user_id", user, "query", q,
		"query_len", utf8.RuneCountInString(q))
	if !ok {
		m.log.DebugContext(ctx, "search: query too long", "user_id", user)
		m.sendText(ctx, b, msg.Chat.ID, loc.T("search.query_too_long", map[string]any{"Max": memesearch.MaxQueryLen}), nil)
		return
	}
	p := m.loadPrefs(ctx, user)
	opts := memesearch.SearchOptions{Mode: m.effectiveMode(p), Limit: memesearch.MaxLimit, Tags: p.Tags}
	m.log.DebugContext(ctx, "search: scope", "user_id", user, "mode", string(opts.Mode), "tags", opts.Tags)
	found, err := m.deps.API.Search(ctx, q, opts)
	if err != nil {
		m.sendText(ctx, b, msg.Chat.ID, m.searchError(ctx, loc, user, err), nil)
		return
	}
	m.log.DebugContext(ctx, "search: results", "user_id", user, "count", len(found))
	if len(found) == 0 {
		m.sendText(ctx, b, msg.Chat.ID, loc.T("search.no_results", map[string]any{"Query": q}), nil)
		return
	}
	s := &session{owner: user, chat: msg.Chat.ID, memes: found, created: m.now()}
	tok := m.pages.put(s, m.now())
	m.log.DebugContext(ctx, "search: session stored", "user_id", user, "pages",
		pageCount(len(found), m.cfg.ResultsPerPage), "sessions", m.pages.len())
	m.sendPage(ctx, b, loc, s, tok, 1)
}

// searchError logs err and returns the text to show the user.
func (m *Module) searchError(ctx context.Context, loc *i18n.Localizer, user int64, err error) string {
	if errors.Is(err, memesearch.ErrRateLimited) {
		m.log.WarnContext(ctx, "search: API rate limited", "user_id", user, "err", err)
		return loc.T("search.busy", nil)
	}
	m.log.WarnContext(ctx, "search: API search failed", "user_id", user, "err", err)
	return loc.T("search.error", nil)
}

// sendPage sends one page as album plus a control message, whose "More"
// button exists only while further pages do: albums cannot carry keyboards.
func (m *Module) sendPage(ctx context.Context, b *bot.Bot, loc *i18n.Localizer, s *session, tok string, page int) {
	per := m.cfg.ResultsPerPage
	pages := pageCount(len(s.memes), per)
	items := pageSlice(s.memes, page, per)
	m.log.DebugContext(ctx, "search: sending page", "user_id", s.owner, "page", page, "pages", pages, "items", len(items))
	if m.deliver(ctx, b, s.chat, items) == 0 {
		m.sendText(ctx, b, s.chat, loc.T("search.error", nil), nil)
		return
	}
	var kb models.ReplyMarkup
	if page < pages {
		kb = &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{
			{Text: loc.T("search.more", nil), CallbackData: moreData(tok, page+1)},
		}}}
	}
	m.sendText(ctx, b, s.chat, loc.T("search.page", map[string]any{"Page": page, "Pages": pages}), kb)
}

func (m *Module) handleCallback(ctx context.Context, b *bot.Bot, u *models.Update) {
	cq := u.CallbackQuery
	loc := m.deps.I18n.Localizer(cq.From.LanguageCode)
	m.log.DebugContext(ctx, "search: callback", "update", "callback_query", "user_id", cq.From.ID, "data", cq.Data)
	if strings.HasPrefix(cq.Data, settingsPrefix) {
		m.handleSettingsCallback(ctx, b, cq)
		return
	}
	tok, page, ok := parseMore(cq.Data)
	if !ok {
		// Includes srch:s: until "similar" buttons exist.
		m.log.DebugContext(ctx, "search: callback ignored", "user_id", cq.From.ID, "data", cq.Data)
		m.answerCallback(ctx, b, cq.ID, "")
		return
	}
	s := m.pages.get(tok, m.now())
	if s == nil || pageSlice(s.memes, page, m.cfg.ResultsPerPage) == nil {
		m.log.DebugContext(ctx, "search: session expired", "user_id", cq.From.ID, "page", page)
		m.answerCallback(ctx, b, cq.ID, loc.T("search.expired", nil))
		return
	}
	if s.owner != cq.From.ID {
		m.log.DebugContext(ctx, "search: foreign paging refused", "user_id", cq.From.ID, "owner", s.owner)
		m.answerCallback(ctx, b, cq.ID, loc.T("search.not_owner", nil))
		return
	}
	m.answerCallback(ctx, b, cq.ID, "")
	if prev := cq.Message.Message; prev != nil {
		_, err := b.EditMessageReplyMarkup(ctx, &bot.EditMessageReplyMarkupParams{
			ChatID: prev.Chat.ID, MessageID: prev.ID,
			ReplyMarkup: &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}},
		})
		if err != nil {
			m.log.WarnContext(ctx, "search: remove More button failed", "user_id", cq.From.ID, "err", err)
		}
	}
	m.sendPage(ctx, b, loc, s, tok, page)
}

func (m *Module) handleInline(ctx context.Context, b *bot.Bot, u *models.Update) {
	iq := u.InlineQuery
	user := iq.From.ID
	q, ok := searchQuery(iq.Query)
	m.log.DebugContext(ctx, "search: inline", "update", "inline_query", "user_id", user, "query", q,
		"query_len", utf8.RuneCountInString(q))
	// Telegram sends a query per keystroke; only the one the user stopped at
	// is worth an API call. Access already charged it in the middleware, at
	// arrival, exactly as without the debounce.
	if m.cfg.InlineDebounce > 0 {
		seq, latest := m.waitLatest(ctx, user, iq.ID)
		if !latest {
			return
		}
		defer m.inline.finish(user, seq)
	}
	results := []models.InlineQueryResult{}
	var misses []memesearch.Meme
	if ok {
		found, err := m.deps.API.Search(ctx, q, memesearch.SearchOptions{Mode: m.cfg.Mode, Limit: m.cfg.InlineResults})
		if err != nil {
			// Inline answers have no room for an explanation; an empty grid it is.
			m.log.WarnContext(ctx, "search: inline search failed", "user_id", user, "err", err)
		}
		for _, meme := range found {
			fid, hit := m.deps.Media.Lookup(ctx, meme)
			if !hit {
				misses = append(misses, meme)
				continue
			}
			results = append(results, &models.InlineQueryResultCachedPhoto{
				ID: strconv.FormatInt(meme.ID, 10), PhotoFileID: fid,
			})
		}
		if len(misses) > 0 {
			// Never wait: the uploads serve the user's next keystroke, not this one.
			m.deps.Media.EnqueueFor(user, misses...)
		}
		m.log.DebugContext(ctx, "search: inline results", "user_id", user, "count", len(found),
			"cache_hits", len(results), "cache_misses", len(misses))
	}
	params := &bot.AnswerInlineQueryParams{
		InlineQueryID: iq.ID, Results: results, CacheTime: inlineCacheSeconds, IsPersonal: true,
	}
	if len(misses) > 0 {
		// Without the button an all-miss answer is a blank grid with no
		// explanation. cache_time 1 because go-telegram/bot drops 0, and a
		// cached incomplete answer would hide the memes once they are uploaded.
		loc := m.deps.I18n.Localizer(iq.From.LanguageCode)
		params.CacheTime = 1
		params.Button = &models.InlineQueryResultsButton{
			Text: loc.T("search.inline_loading", nil), StartParameter: loadingStartParam,
		}
		m.log.DebugContext(ctx, "search: inline loading button", "user_id", user, "cache_misses", len(misses))
	}
	_, err := b.AnswerInlineQuery(ctx, params)
	if err != nil {
		m.log.WarnContext(ctx, "search: answer inline query failed", "user_id", user, "err", err)
	}
}

func (m *Module) sendText(ctx context.Context, b *bot.Bot, chat int64, text string, kb models.ReplyMarkup) {
	if _, err := b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chat, Text: text, ReplyMarkup: kb}); err != nil {
		m.log.WarnContext(ctx, "search: send message failed", "chat_id", chat, "err", err)
	}
}

func (m *Module) answerCallback(ctx context.Context, b *bot.Bot, id, text string) {
	if _, err := b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: id, Text: text}); err != nil {
		m.log.WarnContext(ctx, "search: answer callback failed", "err", err)
	}
}
