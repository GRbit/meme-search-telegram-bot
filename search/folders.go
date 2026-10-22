package search

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/i18n"
	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

const (
	// listTTL keeps picker pages and dir: checks off the API's 60/min listing
	// limit while a user clicks around; new folders show up a few minutes late.
	listTTL = 5 * time.Minute
	// folderPageSize keeps the picker within one phone screen.
	folderPageSize = 8
	folderPrefix   = settingsPrefix + "f:"
)

// listCache holds one server listing (folders or tags) for listTTL.
type listCache[T any] struct {
	mu    sync.Mutex
	items []T
	at    time.Time
	have  bool
}

// get returns the cached list, refreshing it after listTTL. A failed refresh
// keeps serving the stale list: a picker that worked a minute ago should not
// vanish over one API hiccup.
func (c *listCache[T]) get(ctx context.Context, now time.Time, fetch func(context.Context) ([]T, error)) ([]T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.have && now.Sub(c.at) < listTTL {
		return c.items, nil
	}
	items, err := fetch(ctx)
	if err != nil {
		if c.have {
			return c.items, nil
		}
		return nil, err
	}
	c.items, c.at, c.have = items, now, true
	return items, nil
}

func (m *Module) caps() memesearch.Capabilities {
	if m.srv == nil {
		return memesearch.Capabilities{}
	}
	return m.srv.Capabilities()
}

// folders returns the server's folder list, or ok false when the server has
// none to give (stock server, or the listing failed).
func (m *Module) folders(ctx context.Context) ([]memesearch.Folder, bool) {
	if m.srv == nil || !m.caps().FolderList {
		return nil, false
	}
	list, err := m.folderList.get(ctx, m.now(), m.srv.Folders)
	if err != nil {
		m.log.WarnContext(ctx, "search: folder list failed", "err", err)
		return nil, false
	}
	m.log.DebugContext(ctx, "search: folder list", "count", len(list))
	return list, true
}

// resolveFolders maps typed dir: names to the server's exact names (folder
// names are case-sensitive, people type them however). Without a list every
// name passes through and the server judges it.
func (m *Module) resolveFolders(ctx context.Context, typed []string) (exact, unknown []string) {
	if len(typed) == 0 {
		return nil, nil
	}
	list, ok := m.folders(ctx)
	if !ok {
		return typed, nil
	}
	for _, t := range typed {
		name, found := matchFolder(list, t)
		if !found {
			unknown = append(unknown, t)
			continue
		}
		exact = appendNew(exact, name)
	}
	return exact, unknown
}

// matchFolder prefers an exact match and falls back to a unique
// case-insensitive one; two folders differing only in case stay ambiguous.
func matchFolder(list []memesearch.Folder, typed string) (string, bool) {
	var fold []string
	for _, f := range list {
		if f.Name == typed {
			return f.Name, true
		}
		if strings.EqualFold(f.Name, typed) {
			fold = append(fold, f.Name)
		}
	}
	if len(fold) == 1 {
		return fold[0], true
	}
	return "", false
}

// find runs one search within sc. Folders are sent only through Server, whose
// answer says whether they were applied; sc.foldersApplied records that.
func (m *Module) find(ctx context.Context, user int64, q string, sc *scope, limit int) ([]memesearch.Meme, error) {
	opts := sc.options(limit)
	if m.srv == nil {
		sc.foldersApplied = false
		return m.deps.API.Search(ctx, q, opts)
	}
	opts.Folders = sc.folders
	page, err := m.srv.SearchPage(ctx, q, opts)
	if err != nil {
		return nil, err
	}
	sc.foldersApplied = len(sc.folders) > 0 && !slices.Contains(page.Ignored, memesearch.ParamFolder)
	if len(sc.folders) > 0 && !sc.foldersApplied {
		// The client has already turned FolderFilter off; this says which user saw unfiltered results.
		m.log.WarnContext(ctx, "search: server ignored the folder filter, results are from all folders",
			"user_id", user, "folders", sc.folders)
	}
	return page.Memes, nil
}

// invalidFolder reports the server's 422 for a folder name it does not know.
func invalidFolder(err error) bool {
	var apiErr *memesearch.APIError
	return errors.As(err, &apiErr) && apiErr.Code == "invalid_folder"
}

func folderToggleData(idx int, name string) string {
	return folderPrefix + "t:" + strconv.Itoa(idx) + ":" + nameHash(name)
}

// nameHash ties a toggle button to the name it showed: buttons carry an index
// (names can be 300 characters, callback data only 64 bytes), and the list
// may have changed by the time the button is pressed.
func nameHash(name string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return fmt.Sprintf("%08x", h.Sum32())
}

// folderPickerView renders one page of the folder picker.
func (m *Module) folderPickerView(loc *i18n.Localizer, p prefs, list []memesearch.Folder, page int, own string) (string, *models.InlineKeyboardMarkup) {
	pages := max(pageCount(len(list), folderPageSize), 1)
	page = min(max(page, 0), pages-1)
	selected := loc.T("search.folders_all", nil)
	if len(p.Folders) > 0 {
		selected = strings.Join(p.Folders, ", ")
	}
	text := loc.T("search.folder_picker", map[string]any{"Max": memesearch.MaxFolders, "Selected": selected})
	var rows [][]models.InlineKeyboardButton
	start := page * folderPageSize
	for i := start; i < min(start+folderPageSize, len(list)); i++ {
		f := list[i]
		label := fmt.Sprintf("%s (%d)", f.Name, f.MemeCount)
		if slices.Contains(p.Folders, f.Name) {
			label = loc.T("search.settings_selected", map[string]any{"Name": label})
		}
		rows = append(rows, []models.InlineKeyboardButton{{Text: label, CallbackData: folderToggleData(i, f.Name)}})
	}
	if pages > 1 {
		var nav []models.InlineKeyboardButton
		if page > 0 {
			nav = append(nav, models.InlineKeyboardButton{Text: "<", CallbackData: folderPrefix + "p:" + strconv.Itoa(page-1)})
		}
		if page < pages-1 {
			nav = append(nav, models.InlineKeyboardButton{Text: ">", CallbackData: folderPrefix + "p:" + strconv.Itoa(page+1)})
		}
		rows = append(rows, nav)
	}
	shortcuts := []models.InlineKeyboardButton{{Text: loc.T("search.folder_picker_all", nil), CallbackData: folderPrefix + "all"}}
	if own != "" {
		shortcuts = append(shortcuts, models.InlineKeyboardButton{Text: loc.T("search.folder_picker_mine", nil), CallbackData: folderPrefix + "mine"})
	}
	rows = append(rows, shortcuts,
		[]models.InlineKeyboardButton{{Text: loc.T("search.folder_picker_back", nil), CallbackData: folderPrefix + "back"}})
	return text, &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// ownFolder returns the user's save folder when the server has it.
func (m *Module) ownFolder(u *models.User, list []memesearch.Folder) string {
	if m.deps.OwnFolder == nil {
		return ""
	}
	name := m.deps.OwnFolder(u)
	if name == "" || !slices.ContainsFunc(list, func(f memesearch.Folder) bool { return f.Name == name }) {
		return ""
	}
	return name
}

// handleFolderCallback runs one folder picker button. Like other settings it
// is free and edits the settings message in place.
func (m *Module) handleFolderCallback(ctx context.Context, b *bot.Bot, cq *models.CallbackQuery) {
	user := cq.From.ID
	loc := m.deps.I18n.Localizer(cq.From.LanguageCode)
	action := strings.TrimPrefix(cq.Data, folderPrefix)
	list, ok := m.folders(ctx)
	if !ok {
		m.log.DebugContext(ctx, "search: folder picker without a folder list", "user_id", user, "action", action)
		m.answerCallback(ctx, b, cq.ID, loc.T("search.folders_unavailable", nil))
		return
	}
	p := m.loadPrefs(ctx, user)
	own := m.ownFolder(&cq.From, list)
	page := 0
	switch {
	case strings.HasPrefix(action, "p:"):
		page, _ = strconv.Atoi(strings.TrimPrefix(action, "p:"))
		m.log.DebugContext(ctx, "search: folder picker page", "user_id", user, "page", page)
		m.answerCallback(ctx, b, cq.ID, "")
		m.editSettings(ctx, b, cq, func() (string, *models.InlineKeyboardMarkup) {
			return m.folderPickerView(loc, p, list, page, own)
		})
		return
	case action == "back":
		m.log.DebugContext(ctx, "search: folder picker closed", "user_id", user)
		m.answerCallback(ctx, b, cq.ID, "")
		m.showSettings(ctx, b, loc, cq, p)
		return
	case action == "all":
		p.Folders = nil
	case action == "mine":
		if own == "" {
			m.answerCallback(ctx, b, cq.ID, "")
			return
		}
		p.Folders = []string{own}
	case strings.HasPrefix(action, "t:"):
		idxStr, hash, _ := strings.Cut(strings.TrimPrefix(action, "t:"), ":")
		idx, err := strconv.Atoi(idxStr)
		if err != nil || idx < 0 || idx >= len(list) || nameHash(list[idx].Name) != hash {
			m.log.DebugContext(ctx, "search: stale folder button", "user_id", user, "data", cq.Data)
			m.answerCallback(ctx, b, cq.ID, loc.T("search.folder_list_changed", nil))
			m.editSettings(ctx, b, cq, func() (string, *models.InlineKeyboardMarkup) {
				return m.folderPickerView(loc, p, list, 0, own)
			})
			return
		}
		name := list[idx].Name
		page = idx / folderPageSize
		switch {
		case slices.Contains(p.Folders, name):
			p.Folders = slices.DeleteFunc(slices.Clone(p.Folders), func(f string) bool { return f == name })
		case len(p.Folders) >= memesearch.MaxFolders:
			m.log.DebugContext(ctx, "search: folder limit reached", "user_id", user)
			m.answerCallback(ctx, b, cq.ID, loc.T("search.folder_too_many", map[string]any{"Max": memesearch.MaxFolders}))
			return
		default:
			p.Folders = append(slices.Clone(p.Folders), name)
		}
	default:
		m.log.DebugContext(ctx, "search: unknown folder action ignored", "user_id", user, "action", action)
		m.answerCallback(ctx, b, cq.ID, "")
		return
	}
	if err := m.savePrefs(ctx, user, p); err != nil {
		m.log.WarnContext(ctx, "search: save folders failed", "user_id", user, "err", err)
		m.answerCallback(ctx, b, cq.ID, loc.T("search.error", nil))
		return
	}
	m.log.DebugContext(ctx, "search: folders saved", "user_id", user, "folders", p.Folders)
	m.answerCallback(ctx, b, cq.ID, "")
	m.editSettings(ctx, b, cq, func() (string, *models.InlineKeyboardMarkup) {
		return m.folderPickerView(loc, p, list, page, own)
	})
}
