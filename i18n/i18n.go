// Package i18n builds one message bundle from the locale files of several modules.
//
// Each module embeds locales/<lang>.yaml and exposes it as an fs.FS whose root
// holds those files; message ids are prefixed with the module name, so bundles
// from different modules never collide.
package i18n

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"

	goi18n "github.com/nicksnyder/go-i18n/v2/i18n"
	"go.yaml.in/yaml/v3"
	"golang.org/x/text/language"
)

// Bundle holds every loaded message for every language.
type Bundle struct {
	b           *goi18n.Bundle
	defaultLang string
	log         *slog.Logger
}

// NewBundle loads all *.yaml files from each locale FS; defaultLang is the
// fallback for unknown Telegram language codes and missing translations.
//
// The same message id in two files of one language is an error: go-i18n would
// silently keep the last one, hiding a module prefix clash.
func NewBundle(defaultLang string, log *slog.Logger, locales ...fs.FS) (*Bundle, error) {
	if defaultLang == "" {
		return nil, errors.New("i18n: default language is empty")
	}
	tag, err := language.Parse(defaultLang)
	if err != nil {
		return nil, fmt.Errorf("i18n: default language %q: %w", defaultLang, err)
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	b := goi18n.NewBundle(tag)
	b.RegisterUnmarshalFunc("yaml", yaml.Unmarshal)

	seen := map[string]string{} // "<lang> <id>" -> file that defined it
	for i, fsys := range locales {
		names, err := fs.Glob(fsys, "*.yaml")
		if err != nil {
			return nil, fmt.Errorf("i18n: locale fs #%d: %w", i, err)
		}
		for _, name := range names {
			where := fmt.Sprintf("locale fs #%d %s", i, name)
			data, err := fs.ReadFile(fsys, name)
			if err != nil {
				return nil, fmt.Errorf("i18n: read %s: %w", where, err)
			}
			mf, err := b.ParseMessageFileBytes(data, name)
			if err != nil {
				return nil, fmt.Errorf("i18n: parse %s: %w", where, err)
			}
			for _, m := range mf.Messages {
				key := mf.Tag.String() + " " + m.ID
				if prev, ok := seen[key]; ok {
					return nil, fmt.Errorf("i18n: message %q defined in both %s and %s", m.ID, prev, where)
				}
				seen[key] = where
			}
			log.Debug("i18n: loaded locale file", "file", where, "lang", mf.Tag.String(), "messages", len(mf.Messages))
		}
	}
	return &Bundle{b: b, defaultLang: tag.String(), log: log}, nil
}

// Localizer returns a localizer for a Telegram language_code such as "ru" or "pt-br".
//
// Empty, unparsable and unsupported codes all get the default language.
func (b *Bundle) Localizer(languageCode string) *Localizer {
	return &Localizer{
		l:    goi18n.NewLocalizer(b.b, languageCode, b.defaultLang),
		lang: languageCode,
		log:  b.log,
	}
}

// Localizer renders messages in one language.
type Localizer struct {
	l    *goi18n.Localizer
	lang string
	log  *slog.Logger
}

// T renders message id with template data. A missing id renders as the id
// itself so a gap in a catalog is visible but never fatal.
func (l *Localizer) T(id string, data map[string]any) string {
	return l.render(&goi18n.LocalizeConfig{MessageID: id, TemplateData: data})
}

// N renders a plural-aware message id for count; count is also available to
// the template as .Count.
func (l *Localizer) N(id string, count int, data map[string]any) string {
	td := make(map[string]any, len(data)+1)
	maps.Copy(td, data)
	td["Count"] = count
	return l.render(&goi18n.LocalizeConfig{MessageID: id, TemplateData: td, PluralCount: count})
}

func (l *Localizer) render(lc *goi18n.LocalizeConfig) string {
	msg, tag, err := l.l.LocalizeWithTag(lc)
	if tag == language.Und {
		l.log.Debug("i18n: message not found", "id", lc.MessageID, "lang", l.lang, "err", err)
		return lc.MessageID
	}
	if err != nil {
		// Found, but only in the default language or with a template error:
		// go-i18n still returns the best text it could render.
		l.log.Debug("i18n: message rendered with fallback", "id", lc.MessageID, "lang", l.lang, "used", tag.String(), "err", err)
		if msg == "" {
			return lc.MessageID
		}
	}
	return msg
}
