// Package module defines the contract every handler module of this bot implements.
//
// A host (this repo's cmd/bot, or another bot that embeds these packages) owns
// the *bot.Bot and decides which modules to enable; a module only attaches its
// own narrowly matched handlers, so several modules can share one bot.
package module

import "github.com/go-telegram/bot"

// Module attaches its handlers to a bot.
//
// Implementations register handlers with RegisterHandlerMatchFunc and narrow
// match functions; they never use WithDefaultHandler, which would steal updates
// from sibling modules.
type Module interface {
	Register(b *bot.Bot)
}
