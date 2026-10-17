// Command bot runs the meme-search Telegram bot.
//
// It is wiring only and doubles as the example in docs/embedding.md: if it
// grows past wiring, logic has leaked out of a module.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/go-telegram/bot"

	// TZ must work for a bare binary on a host without zoneinfo, not only in the image.
	_ "time/tzdata"

	"github.com/grbit/meme-search-telegram-bot/access"
	"github.com/grbit/meme-search-telegram-bot/config"
	"github.com/grbit/meme-search-telegram-bot/i18n"
	"github.com/grbit/meme-search-telegram-bot/mediacache"
	"github.com/grbit/meme-search-telegram-bot/memesearch"
	"github.com/grbit/meme-search-telegram-bot/module"
	"github.com/grbit/meme-search-telegram-bot/saver"
	"github.com/grbit/meme-search-telegram-bot/search"
	"github.com/grbit/meme-search-telegram-bot/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Environ(), exeDir())
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	for _, line := range strings.Split(cfg.Summary(), "\n") {
		log.Info("config", "setting", line)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	db, err := store.Open(ctx, cfg.DBPath, log)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	bundle, err := i18n.NewBundle(cfg.DefaultLang, log, search.LocaleFS(), saver.LocaleFS(), access.LocaleFS())
	if err != nil {
		return err
	}
	api, err := memesearch.New(cfg.MemeSearch, nil, log.With("module", "memesearch"))
	if err != nil {
		return err
	}
	b, err := bot.New(cfg.TelegramToken, bot.WithErrorsHandler(func(err error) {
		log.Warn("telegram", "err", err)
	}))
	if err != nil {
		return fmt.Errorf("telegram: %w", err)
	}

	acc, err := access.New(ctx, cfg.Access, access.Deps{DB: db, I18n: bundle, Log: log.With("module", "access")})
	if err != nil {
		return err
	}
	media, err := mediacache.New(ctx, cfg.Media, mediacache.Deps{DB: db, API: api, Sender: b, Log: log.With("module", "mediacache")})
	if err != nil {
		return err
	}
	srch, err := search.New(cfg.Search, search.Deps{
		API: api, Media: media, I18n: bundle, Log: log.With("module", "search"),
		Middlewares: []bot.Middleware{acc.Middleware(search.Chargeable)},
	})
	if err != nil {
		return err
	}
	modules := []module.Module{srch}
	if cfg.SaverEnabled {
		sv, err := saver.New(cfg.Saver, saver.Deps{Permit: acc, Known: media, I18n: bundle, Log: log.With("module", "saver")})
		if err != nil {
			return err
		}
		modules = append(modules, sv)
	}
	for _, m := range modules {
		m.Register(b)
	}

	go func() {
		if err := media.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("media cache stopped", "err", err)
		}
	}()

	if !media.UploadsEnabled() {
		log.Info("inline mode off: set CACHE_CHAT_ID to a private channel where the bot is admin")
	}
	log.Info("bot started", "inline", media.UploadsEnabled(), "saver", cfg.SaverEnabled)
	b.Start(ctx)
	log.Info("bot stopped")
	return nil
}

// exeDir is the base for relative default paths (data/, memes/, config.yml),
// so a bare binary keeps its state next to itself wherever it is started from.
func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
}
