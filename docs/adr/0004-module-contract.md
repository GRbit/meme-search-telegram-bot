# 4. Modules attach to a host-owned bot

Every handler module implements `module.Module` (`Register(*bot.Bot)`), takes explicit `Config` and `Deps`, keeps
no package-level state and never reads the environment, so another bot (a planned hub combining this with the
transcriber bot) can import any subset. Match functions are narrow, callback data and table names carry a module
prefix, locale files are embedded per module with prefixed message ids.
