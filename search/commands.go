package search

import "github.com/grbit/meme-search-telegram-bot/module"

// Commands lists the module's slash commands for the host's command menu.
//
// /memescope is offered only with inline mode: without it a group scope has
// no search button to pin. /cancel is left out: it only ends a tag prompt.
func (m *Module) Commands() []module.Command {
	cmds := []module.Command{
		{Name: "start", DescriptionID: "search.cmd_start", Scope: module.ScopePrivate},
		{Name: "help", DescriptionID: "search.cmd_help", Scope: module.ScopePrivate},
		{Name: "settings", DescriptionID: "search.cmd_settings", Scope: module.ScopePrivate},
	}
	if m.deps.Media.UploadsEnabled() {
		cmds = append(cmds, module.Command{Name: groupCommand, DescriptionID: "search.cmd_memescope", Scope: module.ScopeGroups})
		for _, sub := range groupSubcommands {
			cmds = append(cmds, module.Command{Name: groupCommand + "_" + sub, DescriptionID: "search.cmd_memescope_" + sub,
				Scope: module.ScopeGroups})
		}
	}
	return cmds
}
