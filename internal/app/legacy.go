package app

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

// legacyStorageName names a storage target imported from deprecated settings.
const legacyStorageName = "Imported storage"

// legacyImport imports the deprecated environment variables and <data_dir>/config.json
// into the database, once per source: a source is imported only while its setting has
// no stored value, and every source is marked so later starts ignore it. Each
// deprecated source that is set is reported with a warning.
type legacyImport struct {
	logger      *slog.Logger
	legacy      *config.Legacy
	settings    *settings.Service
	targets     *targets.Service
	connections *connections.Service
	auth        *auth.Service
	store       *store.SQLiteStore

	imported []string
}

// run performs the import.
func (l *legacyImport) run(ctx context.Context) error {
	lg := l.legacy
	for _, p := range lg.Problems {
		l.logger.Warn("ignoring an unreadable deprecated setting", slog.String("problem", p))
	}
	res, err := l.settings.Import(ctx, lg.Settings)
	if err != nil {
		return err
	}
	l.imported = append(l.imported, res.Imported...)

	if err := l.importStorage(ctx); err != nil {
		return err
	}
	if err := l.importMongoURI(ctx); err != nil {
		return err
	}
	if err := l.importAPIKey(ctx); err != nil {
		return err
	}
	// Jobs of older releases without a connection string of their own used the
	// server-wide default; that is only known while it is being imported.
	defaultURI := ""
	if slices.Contains(l.imported, lg.MongoURISource) {
		defaultURI = lg.MongoURI
	}
	if _, err := l.store.MigrateLegacyJobURIs(ctx, defaultURI); err != nil {
		return fmt.Errorf("migrate legacy job connection strings: %w", err)
	}
	l.report()
	return nil
}

// importStorage creates the storage target described by deprecated settings as the
// default when no target exists yet.
func (l *legacyImport) importStorage(ctx context.Context) error {
	ls := l.legacy.Storage
	if ls == nil || l.settings.WasImported(ls.Source()) {
		return nil
	}
	list, err := l.targets.List(ctx)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		in := targets.Input{Name: legacyStorageName, Type: ls.Target.Type, Local: ls.Target.Local, S3: ls.Target.S3, IsDefault: true}
		if ls.Target.Local != nil {
			in.Name = targets.DefaultLocalName
			// Earlier builds resolved a relative path against the working directory of
			// the process; keep that meaning and store it absolute.
			abs, err := filepath.Abs(ls.Target.Local.Path)
			if err != nil {
				return fmt.Errorf("resolve legacy local storage path: %w", err)
			}
			in.Local = &models.LocalTarget{Path: abs}
		}
		t, err := l.targets.Create(ctx, in)
		if err != nil {
			l.logger.Warn("could not import the deprecated storage settings; configure a storage target in the dashboard",
				slog.Any("sources", ls.Sources), slog.Any("error", err))
		} else {
			l.logger.Info("imported the deprecated storage settings as the default storage target",
				slog.String("storage_target_id", t.ID), slog.String("location", t.Location()))
			l.imported = append(l.imported, ls.Sources...)
		}
	}
	return l.settings.MarkImported(ctx, ls.Source())
}

// importMongoURI creates the "default" connection when no connection exists yet.
func (l *legacyImport) importMongoURI(ctx context.Context) error {
	lg := l.legacy
	if lg.MongoURI == "" || l.settings.WasImported(lg.MongoURISource) {
		return nil
	}
	created, err := l.connections.EnsureDefault(ctx, lg.MongoURI)
	if err != nil {
		l.logger.Warn("could not import the deprecated MongoDB connection string", slog.String("source", lg.MongoURISource), slog.Any("error", err))
	} else if created {
		l.logger.Info("created connection \"default\" from a deprecated setting", slog.String("source", lg.MongoURISource))
	}
	// Recorded as imported even without a new connection: its jobs are migrated next.
	l.imported = append(l.imported, lg.MongoURISource)
	return l.settings.MarkImported(ctx, lg.MongoURISource)
}

// importAPIKey stores the former static API key as an API key.
func (l *legacyImport) importAPIKey(ctx context.Context) error {
	lg := l.legacy
	if lg.APIKey == "" || l.settings.WasImported(lg.APIKeySource) {
		return nil
	}
	if _, err := l.auth.ImportAPIKey(ctx, lg.APIKey); err != nil {
		l.logger.Warn("could not import the deprecated static API key", slog.String("source", lg.APIKeySource), slog.Any("error", err))
	} else {
		l.imported = append(l.imported, lg.APIKeySource)
	}
	return l.settings.MarkImported(ctx, lg.APIKeySource)
}

// report warns about every deprecated source that is still set.
func (l *legacyImport) report() {
	for _, src := range l.legacy.Present {
		if slices.Contains(l.imported, src) {
			l.logger.Warn(src+" is deprecated and was imported into the database; remove it (manage the setting in the dashboard)",
				slog.String("source", src))
			continue
		}
		l.logger.Warn(src+" is deprecated and ignored; manage the setting in the dashboard and remove it",
			slog.String("source", src))
	}
	if l.legacy.File != "" {
		l.logger.Warn("the legacy configuration file is ignored after its one-time import; remove it",
			slog.String("path", l.legacy.File))
	}
}
