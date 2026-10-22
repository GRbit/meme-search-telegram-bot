package search

import "github.com/grbit/meme-search-telegram-bot/module"

// Commands lists the module's slash commands for the host's command menu.
//
// All are private-chat commands; /cancel is left out: it only ends a tag prompt.
func (m *Module) Commands() []module.Command {
	return []module.Command{
		{Name: "start", DescriptionID: "search.cmd_start", Scope: module.ScopePrivate},
		{Name: "help", DescriptionID: "search.cmd_help", Scope: module.ScopePrivate},
		{Name: "settings", DescriptionID: "search.cmd_settings", Scope: module.ScopePrivate},
	}
}
