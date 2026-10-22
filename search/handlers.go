package search

import (
	"context"
	"errors"
	"slices"
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
	o := parseOverrides(msg.Text)
	q, ok := searchQuery(o.query)
	m.log.DebugContext(ctx, "search: text", "update", "message", "user_id", user, "query", q,
		"query_len", utf8.RuneCountInString(q), "tags", o.tags, "folders", o.folders, "all_folders", o.allFolders)
	if o.group != "" {
		// Group codes come from a group's pinned inline button; a private search has no group.
		m.log.DebugContext(ctx, "search: group code in private search ignored", "user_id", user, "code", o.group)
	}
	switch {
	case q == "":
		m.log.DebugContext(ctx, "search: filters without a query", "user_id", user)
		m.sendText(ctx, b, msg.Chat.ID, loc.T("search.filters_only", nil), nil)
		return
	case !ok:
		m.log.DebugContext(ctx, "search: query too long", "user_id", user)
		m.sendText(ctx, b, msg.Chat.ID, loc.T("search.query_too_long", map[string]any{"Max": memesearch.MaxQueryLen}), nil)
		return
	}
	folders, key, data, reason := m.acceptFilters(ctx, o)
	if key != "" {
		m.log.DebugContext(ctx, "search: query filters refused", "user_id", user, "reason", reason,
			"tags", len(o.tags), "folders", o.folders)
		m.sendText(ctx, b, msg.Chat.ID, loc.T(key, data), nil)
		return
	}
	o.folders = folders
	m.runSearch(ctx, b, loc, user, msg.Chat.ID, q, m.privateScope(m.searchPrefs(ctx, user), o), nil)
}

// runSearch searches q within sc and sends page 1 to chat; text searches and
// the buttons that rerun a search all end here.
// exclude (or nil) is dropped from the results with its copies: "Similar"
// must not show the meme it started from.
func (m *Module) runSearch(ctx context.Context, b *bot.Bot, loc *i18n.Localizer, user, chat int64, q string, sc scope, exclude *memesearch.Meme) {
	m.log.DebugContext(ctx, "search: scope", "user_id", user, "mode", string(sc.mode), "tags", sc.tags,
		"folders", sc.folders)
	found, next, err := m.find(ctx, user, q, &sc, memesearch.MaxLimit, 0)
	if err != nil {
		m.sendText(ctx, b, chat, m.searchError(ctx, loc, user, err, sc.folders), nil)
		return
	}
	m.showResults(ctx, b, loc, user, chat, q, sc, found, next, exclude)
}

// showResults stores a session for found and sends page 1; q and sc are what
// "Search everywhere" and "Similar" rerun.
func (m *Module) showResults(ctx context.Context, b *bot.Bot, loc *i18n.Localizer, user, chat int64, q string, sc scope,
	found []memesearch.Meme, next int, exclude *memesearch.Meme,
) {
	var seed []memesearch.Meme
	if exclude != nil {
		seed = append(seed, *exclude)
	}
	found, dropped := newDupKeys(seed...).filter(found)
	m.log.DebugContext(ctx, "search: results", "user_id", user, "count", len(found), "duplicates_dropped", dropped,
		"folders_applied", sc.foldersApplied)
	if next > 0 && m.deps.Trusted != nil && !m.deps.Trusted(user) {
		m.log.DebugContext(ctx, "search: paging past the first results is for trusted users", "user_id", user)
		next = 0
	}
	s := &session{owner: user, chat: chat, memes: found, nextOffset: next, created: m.now(), query: q, scope: sc, exclude: exclude}
	if len(found) == 0 {
		if !sc.filtered() {
			m.sendText(ctx, b, chat, loc.T("search.no_results", map[string]any{"Query": q}), nil)
			return
		}
		// Kept only for the "Search everywhere" button.
		tok := m.pages.put(s, m.now())
		m.sendText(ctx, b, chat, loc.T("search.no_results_scoped",
			map[string]any{"Query": q, "Scope": m.describeScope(loc, sc)}), m.everywhereKeyboard(loc, tok))
		return
	}
	tok := m.pages.put(s, m.now())
	m.log.DebugContext(ctx, "search: session stored", "user_id", user, "pages",
		pageCount(len(found), m.cfg.ResultsPerPage), "sessions", m.pages.len())
	m.sendPage(ctx, b, loc, s, tok, 1)
}

// searchError logs err and returns the text to show the user.
func (m *Module) searchError(ctx context.Context, loc *i18n.Localizer, user int64, err error, folders []string) string {
	if invalidFolder(err) {
		// The folder list was unavailable or stale, so the server had the last word.
		m.log.DebugContext(ctx, "search: server rejected a folder", "user_id", user, "folders", folders, "err", err)
		return loc.T("search.folder_unknown", map[string]any{"Folders": strings.Join(folders, ", ")})
	}
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
	all, next := s.snapshot()
	pages := pageCount(len(all), per)
	items := pageSlice(all, page, per)
	m.log.DebugContext(ctx, "search: sending page", "user_id", s.owner, "page", page, "pages", pages, "items", len(items))
	if m.deliver(ctx, b, s.chat, items) == 0 {
		m.sendText(ctx, b, s.chat, loc.T("search.error", nil), nil)
		return
	}
	pagesLabel := strconv.Itoa(pages)
	if next > 0 {
		pagesLabel += "+"
	}
	lines := []string{loc.T("search.page", map[string]any{"Page": page, "Pages": pagesLabel})}
	if page == 1 {
		if len(s.scope.folders) > 0 && !s.scope.foldersApplied {
			lines = append([]string{loc.T("search.folders_unsupported", nil)}, lines...)
		}
		if s.scope.wantsAllTags() && !s.scope.tagMatchApplied {
			lines = append([]string{loc.T("search.tag_match_unsupported", nil)}, lines...)
		}
		if !m.isDefaultScope(s.scope) {
			lines = append([]string{loc.T("search.scope_line", map[string]any{"Scope": m.describeScope(loc, s.scope)})}, lines...)
		}
	}
	var kb models.ReplyMarkup
	if rows := m.pageKeyboard(loc, s, tok, page, true); len(rows) > 0 {
		kb = &models.InlineKeyboardMarkup{InlineKeyboard: rows}
	}
	m.sendText(ctx, b, s.chat, strings.Join(lines, "\n"), kb)
}

// similarPerRow keeps "Similar N" buttons readable on a phone.
const similarPerRow = 5

// pageKeyboard builds a control message's buttons: "Similar N" for every
// described meme of the page (N is its position in the album), "More" while
// pages remain, and "Search everywhere" on page 1 of a filtered search.
func (m *Module) pageKeyboard(loc *i18n.Localizer, s *session, tok string, page int, withMore bool) [][]models.InlineKeyboardButton {
	per := m.cfg.ResultsPerPage
	var rows [][]models.InlineKeyboardButton
	var row []models.InlineKeyboardButton
	all, next := s.snapshot()
	for i, mm := range pageSlice(all, page, per) {
		// Without a description there is nothing to search by.
		if strings.TrimSpace(mm.Description) == "" {
			continue
		}
		row = append(row, models.InlineKeyboardButton{
			Text: loc.T("search.similar", map[string]any{"N": i + 1}), CallbackData: similarData(tok, (page-1)*per+i),
		})
		if len(row) == similarPerRow {
			rows, row = append(rows, row), nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	if withMore && (page < pageCount(len(all), per) || next > 0) {
		rows = append(rows, []models.InlineKeyboardButton{{Text: loc.T("search.more", nil), CallbackData: moreData(tok, page+1)}})
	}
	if page == 1 && s.scope.filtered() {
		rows = append(rows, m.everywhereKeyboard(loc, tok).InlineKeyboard...)
	}
	return rows
}

// handleSimilar searches by the description of one meme of a session, within
// the session's tags; vector mode, because a description is prose, not keywords.
func (m *Module) handleSimilar(ctx context.Context, b *bot.Bot, cq *models.CallbackQuery) {
	loc := m.deps.I18n.Localizer(cq.From.LanguageCode)
	tok, idx, ok := parseSimilar(cq.Data)
	var s *session
	if ok {
		s = m.pages.get(tok, m.now())
	}
	switch {
	case s == nil || idx >= len(snapshotMemes(s)):
		m.log.DebugContext(ctx, "search: similar on expired session", "user_id", cq.From.ID, "data", cq.Data)
		m.answerCallback(ctx, b, cq.ID, loc.T("search.expired", nil))
		return
	case s.owner != cq.From.ID:
		m.log.DebugContext(ctx, "search: foreign similar refused", "user_id", cq.From.ID, "owner", s.owner)
		m.answerCallback(ctx, b, cq.ID, loc.T("search.not_owner", nil))
		return
	}
	src := snapshotMemes(s)[idx]
	q := truncateRunes(strings.TrimSpace(src.Description), memesearch.MaxQueryLen)
	if q == "" {
		m.log.DebugContext(ctx, "search: similar without description", "user_id", cq.From.ID, "meme_id", src.ID)
		m.answerCallback(ctx, b, cq.ID, loc.T("search.no_description", nil))
		return
	}
	m.answerCallback(ctx, b, cq.ID, "")
	sc := s.scope
	sc.mode = memesearch.ModeVector
	if found, done := m.similarByID(ctx, b, loc, cq.From.ID, s.chat, src, sc); done {
		if found != nil {
			m.showResults(ctx, b, loc, cq.From.ID, s.chat, q, sc, found, 0, &src)
		}
		return
	}
	m.log.DebugContext(ctx, "search: similar by description", "user_id", cq.From.ID, "meme_id", src.ID,
		"query_len", utf8.RuneCountInString(q))
	m.runSearch(ctx, b, loc, cq.From.ID, s.chat, q, sc, &src)
}

// similarByID asks the server for memes similar to src itself, which is exact
// and costs no re-embedding. done false means: use the description query.
// done with nil found means the user was already told about an error.
//
// The endpoint has no tag or folder filter, so a filtered session keeps the
// description query, which applies its scope; filtering 20 neighbours
// locally could leave nothing.
func (m *Module) similarByID(ctx context.Context, b *bot.Bot, loc *i18n.Localizer, user, chat int64,
	src memesearch.Meme, sc scope,
) (found []memesearch.Meme, done bool) {
	switch {
	case m.srv == nil || !m.caps().Similar:
		m.log.DebugContext(ctx, "search: similar by id unavailable", "user_id", user)
		return nil, false
	case len(sc.tags) > 0 || len(sc.folders) > 0:
		m.log.DebugContext(ctx, "search: similar by id skipped, scope is filtered", "user_id", user,
			"tags", sc.tags, "folders", sc.folders)
		return nil, false
	}
	found, err := m.srv.Similar(ctx, src.ID, memesearch.MaxLimit)
	switch {
	case errors.Is(err, memesearch.ErrUnsupported), errors.Is(err, memesearch.ErrNotFound):
		// An older server (the client turned the capability off) or a meme gone since the search.
		m.log.DebugContext(ctx, "search: similar by id failed, falling back", "user_id", user, "meme_id", src.ID, "err", err)
		return nil, false
	case err != nil:
		m.sendText(ctx, b, chat, m.searchError(ctx, loc, user, err, nil), nil)
		return nil, true
	case len(found) == 0:
		// No embedding yet; the description may still find something.
		m.log.DebugContext(ctx, "search: similar by id empty, falling back", "user_id", user, "meme_id", src.ID)
		return nil, false
	}
	m.log.DebugContext(ctx, "search: similar by id", "user_id", user, "meme_id", src.ID, "count", len(found))
	return found, true
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func (m *Module) everywhereKeyboard(loc *i18n.Localizer, tok string) *models.InlineKeyboardMarkup {
	return &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{
		{{Text: loc.T("search.everywhere", nil), CallbackData: everywherePrefix + tok}},
	}}
}

// handleEverywhere reruns a session's query without tags and folders.
func (m *Module) handleEverywhere(ctx context.Context, b *bot.Bot, cq *models.CallbackQuery) {
	loc := m.deps.I18n.Localizer(cq.From.LanguageCode)
	tok := strings.TrimPrefix(cq.Data, everywherePrefix)
	s := m.pages.get(tok, m.now())
	switch {
	case s == nil:
		m.log.DebugContext(ctx, "search: everywhere on expired session", "user_id", cq.From.ID)
		m.answerCallback(ctx, b, cq.ID, loc.T("search.expired", nil))
		return
	case s.owner != cq.From.ID:
		m.log.DebugContext(ctx, "search: foreign everywhere refused", "user_id", cq.From.ID, "owner", s.owner)
		m.answerCallback(ctx, b, cq.ID, loc.T("search.not_owner", nil))
		return
	}
	m.answerCallback(ctx, b, cq.ID, "")
	m.log.DebugContext(ctx, "search: search everywhere", "user_id", cq.From.ID, "query", s.query)
	m.runSearch(ctx, b, loc, cq.From.ID, s.chat, s.query, scope{mode: s.scope.mode}, nil)
}

func (m *Module) handleCallback(ctx context.Context, b *bot.Bot, u *models.Update) {
	cq := u.CallbackQuery
	loc := m.deps.I18n.Localizer(cq.From.LanguageCode)
	m.log.DebugContext(ctx, "search: callback", "update", "callback_query", "user_id", cq.From.ID, "data", cq.Data)
	switch {
	case strings.HasPrefix(cq.Data, settingsPrefix):
		m.handleSettingsCallback(ctx, b, cq)
		return
	case strings.HasPrefix(cq.Data, groupPickPrefix):
		m.handleGroupPick(ctx, b, cq)
		return
	case strings.HasPrefix(cq.Data, everywherePrefix):
		m.handleEverywhere(ctx, b, cq)
		return
	case strings.HasPrefix(cq.Data, similarPrefix):
		m.handleSimilar(ctx, b, cq)
		return
	}
	tok, page, ok := parseMore(cq.Data)
	if !ok {
		m.log.DebugContext(ctx, "search: callback ignored", "user_id", cq.From.ID, "data", cq.Data)
		m.answerCallback(ctx, b, cq.ID, "")
		return
	}
	s := m.pages.get(tok, m.now())
	if s == nil {
		m.log.DebugContext(ctx, "search: session expired", "user_id", cq.From.ID, "page", page)
		m.answerCallback(ctx, b, cq.ID, loc.T("search.expired", nil))
		return
	}
	if s.owner != cq.From.ID {
		m.log.DebugContext(ctx, "search: foreign paging refused", "user_id", cq.From.ID, "owner", s.owner)
		m.answerCallback(ctx, b, cq.ID, loc.T("search.not_owner", nil))
		return
	}
	if popup := m.ensurePage(ctx, loc, s, page); popup != "" {
		m.answerCallback(ctx, b, cq.ID, popup)
		return
	}
	m.answerCallback(ctx, b, cq.ID, "")
	if prev := cq.Message.Message; prev != nil {
		// Only "More" goes: the old page keeps its Similar buttons.
		rows := m.pageKeyboard(loc, s, tok, page-1, false)
		if rows == nil {
			rows = [][]models.InlineKeyboardButton{}
		}
		_, err := b.EditMessageReplyMarkup(ctx, &bot.EditMessageReplyMarkupParams{
			ChatID: prev.Chat.ID, MessageID: prev.ID,
			ReplyMarkup: &models.InlineKeyboardMarkup{InlineKeyboard: rows},
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
	o := parseOverrides(iq.Query)
	q, ok := o.searchable()
	m.log.DebugContext(ctx, "search: inline", "update", "inline_query", "user_id", user, "query", q,
		"query_len", utf8.RuneCountInString(q), "chat_type", iq.ChatType, "tags", o.tags, "folders", o.folders)
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
		o = m.dropPublicGroupFilters(ctx, user, iq.ChatType, o)
		// Unknown typed folders are passed on: the server's 422 gives an empty grid, which is all
		// inline can say anyway.
		if folders, unknown := m.resolveFolders(ctx, o.folders); len(unknown) == 0 {
			o.folders = folders
		}
		sc := m.inlineScope(m.searchPrefs(ctx, user), o, m.inlineGroup(ctx, user, o.group), iq.ChatType)
		m.log.DebugContext(ctx, "search: inline scope", "user_id", user, "tags", sc.tags, "folders", sc.folders)
		found, _, err := m.find(ctx, user, q, &sc, m.cfg.InlineResults, 0)
		switch {
		case invalidFolder(err):
			m.log.DebugContext(ctx, "search: inline folder rejected by the server", "user_id", user, "folders", sc.folders)
		case err != nil:
			// Inline answers have no room for an explanation; an empty grid it is.
			m.log.WarnContext(ctx, "search: inline search failed", "user_id", user, "err", err)
		}
		found, dropped := newDupKeys().filter(found)
		if dropped > 0 {
			m.log.DebugContext(ctx, "search: inline duplicates dropped", "user_id", user, "duplicates_dropped", dropped)
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

// ensurePage makes page available, fetching the next server page when the
// fetched memes ran out and the server said there are more. It returns the
// popup text when the page cannot be shown. Paging stays free even when it
// calls the API: it continues a search that was already charged.
func (m *Module) ensurePage(ctx context.Context, loc *i18n.Localizer, s *session, page int) string {
	per := m.cfg.ResultsPerPage
	// Held across the request so two quick presses do not fetch the same offset twice.
	s.mu.Lock()
	defer s.mu.Unlock()
	if pageSlice(s.memes, page, per) != nil {
		return ""
	}
	if s.nextOffset == 0 || page != pageCount(len(s.memes), per)+1 {
		m.log.DebugContext(ctx, "search: page not available", "user_id", s.owner, "page", page)
		return loc.T("search.expired", nil)
	}
	sc := s.scope
	found, next, err := m.find(ctx, s.owner, s.query, &sc, memesearch.MaxLimit, s.nextOffset)
	switch {
	case errors.Is(err, memesearch.ErrUnsupported):
		s.nextOffset = 0
		return loc.T("search.no_more", nil)
	case err != nil:
		return m.searchError(ctx, loc, s.owner, err, sc.folders)
	}
	shown := s.memes
	if s.exclude != nil {
		shown = append(slices.Clone(shown), *s.exclude)
	}
	// Against everything already in the session: the library may also change between
	// requests and shift results across pages.
	fresh, dropped := newDupKeys(shown...).filter(found)
	s.memes = append(s.memes, fresh...)
	added := len(fresh)
	m.log.DebugContext(ctx, "search: fetched more results", "user_id", s.owner, "offset", s.nextOffset,
		"count", len(found), "added", added, "duplicates_dropped", dropped, "next_offset", next)
	// s.mu is held: the store lock nests inside it, never the other way round.
	m.pages.grow(s, added, m.now())
	s.nextOffset = next
	if pageSlice(s.memes, page, per) == nil {
		return loc.T("search.no_more", nil)
	}
	return ""
}

// dropPublicGroupFilters removes the #tag, dir: and dir:* words a public user
// typed into an inline query in a group: there the group's code and
// INLINE_GROUP_SCOPE decide what is shown. The words are already out of the
// query text, and the over-limit refusal still counts them, so Chargeable
// (which cannot see tiers) and the handler keep agreeing.
func (m *Module) dropPublicGroupFilters(ctx context.Context, user int64, chatType string, o overrides) overrides {
	if m.deps.Trusted == nil || m.deps.Trusted(user) || !isGroupChatType(chatType) {
		return o
	}
	if len(o.tags) == 0 && len(o.folders) == 0 && !o.allFolders {
		return o
	}
	m.log.DebugContext(ctx, "search: typed filters ignored for a public user in a group", "user_id", user,
		"ignored_tags", len(o.tags), "ignored_folders", len(o.folders), "ignored_all_folders", o.allFolders)
	o.tags, o.folders, o.allFolders = nil, nil, false
	return o
}
