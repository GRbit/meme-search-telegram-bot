package module

import (
	"context"
	"log/slog"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/i18n"
)

// CommandScope is where a command is offered in Telegram's command menu.
type CommandScope int

// Command scopes: private chats with the bot, and all groups and supergroups.
const (
	ScopePrivate CommandScope = iota
	ScopeGroups
)

// Command is one slash command a module handles.
type Command struct {
	Name          string // without "/", lowercase letters, digits and "_"
	DescriptionID string // i18n message id of the menu text
	Scope         CommandScope
}

// Commander is implemented by modules that handle slash commands, so a host
// can list them in Telegram's command menu. It is optional.
type Commander interface {
	Commands() []Command
}

// SetCommands publishes the commands of mods that implement Commander as the
// bot's command menu: per scope, one default list and one per language of
// bundle. Errors are logged at Warn and never returned: the menu is a
// convenience, and commands work without it.
//
// A scope without commands is left alone rather than cleared, so a host
// combining these modules with its own can set that scope itself.
func SetCommands(ctx context.Context, b *bot.Bot, bundle *i18n.Bundle, log *slog.Logger, mods ...Module) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	byScope := map[CommandScope][]Command{}
	seen := map[CommandScope]map[string]bool{ScopePrivate: {}, ScopeGroups: {}}
	for _, m := range mods {
		c, ok := m.(Commander)
		if !ok {
			continue
		}
		for _, cmd := range c.Commands() {
			if seen[cmd.Scope][cmd.Name] {
				continue
			}
			seen[cmd.Scope][cmd.Name] = true
			byScope[cmd.Scope] = append(byScope[cmd.Scope], cmd)
		}
	}
	scopes := []struct {
		scope CommandScope
		tg    models.BotCommandScope
		name  string
	}{
		{ScopePrivate, &models.BotCommandScopeAllPrivateChats{}, "private"},
		{ScopeGroups, &models.BotCommandScopeAllGroupChats{}, "groups"},
	}
	// "" is the list for users whose language has no list of its own.
	langs := append([]string{""}, bundle.Languages()...)
	for _, sc := range scopes {
		cmds := byScope[sc.scope]
		if len(cmds) == 0 {
			continue
		}
		for _, lang := range langs {
			loc := bundle.Localizer(lang)
			list := make([]models.BotCommand, len(cmds))
			for i, c := range cmds {
				list[i] = models.BotCommand{Command: c.Name, Description: loc.T(c.DescriptionID, nil)}
			}
			_, err := b.SetMyCommands(ctx, &bot.SetMyCommandsParams{Commands: list, Scope: sc.tg, LanguageCode: lang})
			if err != nil {
				log.WarnContext(ctx, "telegram: set command menu failed", "scope", sc.name, "lang", lang, "err", err)
				continue
			}
			log.DebugContext(ctx, "telegram: command menu set", "scope", sc.name, "lang", lang, "commands", len(list))
		}
	}
}
