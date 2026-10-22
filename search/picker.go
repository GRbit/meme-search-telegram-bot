package search

import (
	"context"
	"fmt"
	"hash/fnv"
	"slices"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/i18n"
	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

const (
	// pickerPageSize keeps a picker within one phone screen.
	pickerPageSize = 8
	folderPrefix   = settingsPrefix + "f:"
	tagPrefix      = settingsPrefix + "tg:"
)

// pickTarget is where a picker's selection lives: a user's settings or a
// group's scope.
type pickTarget struct {
	prefix string // settingsPrefix or groupPickPrefix; pickers add "f:" or "tg:"
	load   func(ctx context.Context, cq *models.CallbackQuery) (prefs, error)
	save   func(ctx context.Context, cq *models.CallbackQuery, p prefs) error
	// back shows the view the picker was opened from.
	back func(ctx context.Context, b *bot.Bot, loc *i18n.Localizer, cq *models.CallbackQuery, p prefs)
	// ownFolder offers "Only mine": a group has no save folder of its own.
	ownFolder bool
}

func (m *Module) userTarget() pickTarget {
	return pickTarget{
		prefix: settingsPrefix,
		load: func(ctx context.Context, cq *models.CallbackQuery) (prefs, error) {
			return m.loadPrefs(ctx, cq.From.ID), nil
		},
		save: func(ctx context.Context, cq *models.CallbackQuery, p prefs) error {
			return m.savePrefs(ctx, cq.From.ID, p)
		},
		back:      m.showSettings,
		ownFolder: true,
	}
}

type pickItem struct {
	name  string
	count int
}

// picker is one multi-select list (folders, tags) over a server listing, for
// /settings or a group's scope.
type picker struct {
	kind   string // for logs
	prefix string
	max    int
	// message ids: title (Max, Selected), the button that clears the selection,
	// the popup at max, the popup when the list is unavailable.
	titleID, noneID, clearID, tooManyID, unavailableID string
	get                                                func(prefs) []string
	set                                                func(*prefs, []string)
	items                                              func(ctx context.Context) ([]pickItem, bool)
	// own returns the "Only mine" choice, "" for none; nil means no such button.
	own    func(u *models.User, items []pickItem) string
	target pickTarget
}

func (m *Module) folderPicker(t pickTarget) picker {
	var own func(*models.User, []pickItem) string
	if t.ownFolder {
		own = m.ownFolder
	}
	return picker{
		target: t,
		kind:   "folders", prefix: t.prefix + "f:", max: memesearch.MaxFolders,
		titleID: "search.folder_picker", noneID: "search.folders_all", clearID: "search.folder_picker_all",
		tooManyID: "search.folder_too_many", unavailableID: "search.folders_unavailable",
		get: func(p prefs) []string { return p.Folders },
		set: func(p *prefs, v []string) { p.Folders = v },
		items: func(ctx context.Context) ([]pickItem, bool) {
			list, ok := m.folders(ctx)
			out := make([]pickItem, len(list))
			for i, f := range list {
				out[i] = pickItem{f.Name, f.MemeCount}
			}
			return out, ok
		},
		own: own,
	}
}

func (m *Module) tagPicker(t pickTarget) picker {
	return picker{
		target: t,
		kind:   "tags", prefix: t.prefix + "tg:", max: memesearch.MaxTags,
		titleID: "search.tag_picker", noneID: "search.tags_none", clearID: "search.tag_picker_none",
		tooManyID: "search.settings_tags_too_many_short", unavailableID: "search.tags_unavailable",
		get: func(p prefs) []string { return p.Tags },
		set: func(p *prefs, v []string) { p.Tags = v },
		items: func(ctx context.Context) ([]pickItem, bool) {
			list, ok := m.tags(ctx)
			out := make([]pickItem, len(list))
			for i, t := range list {
				out[i] = pickItem{t.Name, t.MemeCount}
			}
			return out, ok
		},
	}
}

// ownFolder returns the user's save folder when the server has it.
func (m *Module) ownFolder(u *models.User, items []pickItem) string {
	if m.deps.OwnFolder == nil {
		return ""
	}
	name := m.deps.OwnFolder(u)
	if name == "" || !slices.ContainsFunc(items, func(it pickItem) bool { return it.name == name }) {
		return ""
	}
	return name
}

func pickToggleData(prefix string, idx int, name string) string {
	return prefix + "t:" + strconv.Itoa(idx) + ":" + nameHash(name)
}

func folderToggleData(idx int, name string) string { return pickToggleData(folderPrefix, idx, name) }

// nameHash ties a toggle button to the name it showed: buttons carry an index
// (folder names can be 300 characters, callback data only 64 bytes), and the
// list may have changed by the time the button is pressed.
func nameHash(name string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return fmt.Sprintf("%08x", h.Sum32())
}

// view renders one page of the picker.
func (pk picker) view(loc *i18n.Localizer, p prefs, items []pickItem, page int, own string) (string, *models.InlineKeyboardMarkup) {
	pages := max(pageCount(len(items), pickerPageSize), 1)
	page = min(max(page, 0), pages-1)
	chosen := pk.get(p)
	selected := loc.T(pk.noneID, nil)
	if len(chosen) > 0 {
		selected = strings.Join(chosen, ", ")
	}
	text := loc.T(pk.titleID, map[string]any{"Max": pk.max, "Selected": selected})
	var rows [][]models.InlineKeyboardButton
	start := page * pickerPageSize
	for i := start; i < min(start+pickerPageSize, len(items)); i++ {
		it := items[i]
		label := fmt.Sprintf("%s (%d)", it.name, it.count)
		if slices.Contains(chosen, it.name) {
			label = loc.T("search.settings_selected", map[string]any{"Name": label})
		}
		rows = append(rows, []models.InlineKeyboardButton{{Text: label, CallbackData: pickToggleData(pk.prefix, i, it.name)}})
	}
	if pages > 1 {
		var nav []models.InlineKeyboardButton
		if page > 0 {
			nav = append(nav, models.InlineKeyboardButton{Text: "<", CallbackData: pk.prefix + "p:" + strconv.Itoa(page-1)})
		}
		if page < pages-1 {
			nav = append(nav, models.InlineKeyboardButton{Text: ">", CallbackData: pk.prefix + "p:" + strconv.Itoa(page+1)})
		}
		rows = append(rows, nav)
	}
	shortcuts := []models.InlineKeyboardButton{{Text: loc.T(pk.clearID, nil), CallbackData: pk.prefix + "all"}}
	if own != "" {
		shortcuts = append(shortcuts, models.InlineKeyboardButton{Text: loc.T("search.folder_picker_mine", nil), CallbackData: pk.prefix + "mine"})
	}
	rows = append(rows, shortcuts,
		[]models.InlineKeyboardButton{{Text: loc.T("search.folder_picker_back", nil), CallbackData: pk.prefix + "back"}})
	return text, &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// handlePicker runs one picker button. Like other settings it is free and
// edits the settings message in place.
func (m *Module) handlePicker(ctx context.Context, b *bot.Bot, cq *models.CallbackQuery, pk picker) {
	user := cq.From.ID
	loc := m.deps.I18n.Localizer(cq.From.LanguageCode)
	action := strings.TrimPrefix(cq.Data, pk.prefix)
	items, ok := pk.items(ctx)
	if !ok {
		m.log.DebugContext(ctx, "search: picker without a list", "user_id", user, "picker", pk.kind, "action", action)
		m.answerCallback(ctx, b, cq.ID, loc.T(pk.unavailableID, nil))
		return
	}
	p, err := pk.target.load(ctx, cq)
	if err != nil {
		m.log.WarnContext(ctx, "search: picker load failed", "user_id", user, "picker", pk.kind, "err", err)
		m.answerCallback(ctx, b, cq.ID, loc.T("search.error", nil))
		return
	}
	own := ""
	if pk.own != nil {
		own = pk.own(&cq.From, items)
	}
	render := func(page int) {
		m.editSettings(ctx, b, cq, func() (string, *models.InlineKeyboardMarkup) { return pk.view(loc, p, items, page, own) })
	}
	page := 0
	chosen := pk.get(p)
	switch {
	case strings.HasPrefix(action, "p:"):
		page, _ = strconv.Atoi(strings.TrimPrefix(action, "p:"))
		m.log.DebugContext(ctx, "search: picker page", "user_id", user, "picker", pk.kind, "page", page)
		m.answerCallback(ctx, b, cq.ID, "")
		render(page)
		return
	case action == "back":
		m.log.DebugContext(ctx, "search: picker closed", "user_id", user, "picker", pk.kind)
		m.answerCallback(ctx, b, cq.ID, "")
		pk.target.back(ctx, b, loc, cq, p)
		return
	case action == "all":
		chosen = nil
	case action == "mine":
		if own == "" {
			m.answerCallback(ctx, b, cq.ID, "")
			return
		}
		chosen = []string{own}
	case strings.HasPrefix(action, "t:"):
		idxStr, hash, _ := strings.Cut(strings.TrimPrefix(action, "t:"), ":")
		idx, err := strconv.Atoi(idxStr)
		if err != nil || idx < 0 || idx >= len(items) || nameHash(items[idx].name) != hash {
			m.log.DebugContext(ctx, "search: stale picker button", "user_id", user, "picker", pk.kind, "data", cq.Data)
			m.answerCallback(ctx, b, cq.ID, loc.T("search.folder_list_changed", nil))
			render(0)
			return
		}
		name := items[idx].name
		page = idx / pickerPageSize
		switch {
		case slices.Contains(chosen, name):
			chosen = slices.DeleteFunc(slices.Clone(chosen), func(x string) bool { return x == name })
		case len(chosen) >= pk.max:
			m.log.DebugContext(ctx, "search: picker limit reached", "user_id", user, "picker", pk.kind)
			m.answerCallback(ctx, b, cq.ID, loc.T(pk.tooManyID, map[string]any{"Max": pk.max}))
			return
		default:
			chosen = append(slices.Clone(chosen), name)
		}
	default:
		m.log.DebugContext(ctx, "search: unknown picker action ignored", "user_id", user, "picker", pk.kind, "action", action)
		m.answerCallback(ctx, b, cq.ID, "")
		return
	}
	pk.set(&p, chosen)
	if err := pk.target.save(ctx, cq, p); err != nil {
		m.log.WarnContext(ctx, "search: save picker selection failed", "user_id", user, "picker", pk.kind, "err", err)
		m.answerCallback(ctx, b, cq.ID, loc.T("search.error", nil))
		return
	}
	m.log.DebugContext(ctx, "search: picker selection saved", "user_id", user, "picker", pk.kind, "selected", chosen)
	m.answerCallback(ctx, b, cq.ID, "")
	render(page)
}
