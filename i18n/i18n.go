// Package i18n builds one message bundle from the locale files of several modules.
//
// Each module embeds locales/<lang>.yaml and exposes it as an fs.FS whose root
// holds those files; message ids are prefixed with the module name, so bundles
// from different modules never collide.
package i18n

import (
	"errors"
	"io/fs"
	"log/slog"
)

// Bundle holds every loaded message for every language.
type Bundle struct{}

// NewBundle loads all *.yaml files from each locale FS; defaultLang is the
// fallback for unknown Telegram language codes and missing translations.
func NewBundle(defaultLang string, log *slog.Logger, locales ...fs.FS) (*Bundle, error) {
	return nil, errors.New("i18n: not implemented")
}

// Localizer returns a localizer for a Telegram language_code such as "ru" or "pt-br".
func (b *Bundle) Localizer(languageCode string) *Localizer {
	return &Localizer{}
}

// Localizer renders messages in one language.
type Localizer struct{}

// T renders message id with template data. A missing id renders as the id
// itself so a gap in a catalog is visible but never fatal.
func (l *Localizer) T(id string, data map[string]any) string {
	return id
}

// N renders a plural-aware message id for count; count is also available to
// the template as .Count.
func (l *Localizer) N(id string, count int, data map[string]any) string {
	return id
}
