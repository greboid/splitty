// Command splitpayments is a self-hosted Splitwise alternative: groups,
// expenses in five split modes, settle-up payments, balances, an activity
// feed and LLM-powered receipt scanning.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/csmith/envflag/v2"
	"github.com/csmith/slogflags"

	"github.com/greboid/splitpayments/internal/activity"
	"github.com/greboid/splitpayments/internal/auth"
	"github.com/greboid/splitpayments/internal/config"
	"github.com/greboid/splitpayments/internal/database"
	"github.com/greboid/splitpayments/internal/expense"
	"github.com/greboid/splitpayments/internal/group"
	"github.com/greboid/splitpayments/internal/receipt"
	"github.com/greboid/splitpayments/internal/render"
	"github.com/greboid/splitpayments/internal/server"
	"github.com/greboid/splitpayments/internal/user"
	"github.com/greboid/splitpayments/web"
)

func main() {
	cfg := config.Register(flag.CommandLine)
	slogflags.Logger(slogflags.WithSetDefault(true))
	envflag.Parse()

	if err := run(cfg); err != nil {
		slog.Error("shutting down", "error", err)
		os.Exit(1)
	}
}

func run(cfg *config.Config) error {
	driver, err := database.ResolveDriver(cfg.DBDriver, cfg.Database)
	if err != nil {
		return err
	}
	// The SQLite file's parent directory must exist before it is opened; a
	// Postgres DSN is not a path.
	if driver == "sqlite" {
		if dir := filepath.Dir(cfg.Database); dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
	}

	db, err := database.Open(driver, cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := database.Migrate(db, driver); err != nil {
		return err
	}

	// Templates: embedded, or the -static-dir sibling override in development.
	templateFS := web.Templates()
	if cfg.StaticDir != "" {
		templateFS = os.DirFS(filepath.Join(filepath.Dir(cfg.StaticDir), "templates"))
	}
	scanEnabled := cfg.ReceiptAPIURL != "" &&
		(cfg.ReceiptModel != "" || (cfg.ReceiptAPIWorkflow != "" &&
			(cfg.ReceiptAPIFormat == receipt.FormatGWTYPE2 || cfg.ReceiptAPIFormat == receipt.FormatGWTYPE3)))
	r, err := render.New(templateFS, cfg.Currency, cfg.CurrencySymbol, scanEnabled, web.AssetVersion())
	if err != nil {
		return err
	}

	users := user.NewStore(db)
	groups := group.NewStore(db)
	expenses := expense.NewStore(db)
	activityStore := activity.NewStore(db)
	sessions := &auth.SessionStore{DB: db, Lifetime: cfg.SessionLifetime, Secure: cfg.CookieSecure}
	invites := &auth.InviteStore{DB: db, Lifetime: cfg.InviteLifetime}

	authHandlers := &auth.Handlers{
		Users:    users,
		Sessions: sessions,
		Invites:  invites,
		Groups:   groups,
		Activity: activityStore,
		Render:   r,
	}
	groupHandlers := &group.Handlers{
		Store:    groups,
		Users:    users,
		Expenses: expenses,
		Activity: activityStore,
		Invites:  invites,
		Render:   r,
	}
	expenseHandlers := &expense.Handlers{
		Store:    expenses,
		Users:    users,
		Activity: activityStore,
		Render:   r,
	}
	activityHandlers := &activity.Handler{Store: activityStore, Render: r}

	var receiptHandlers *receipt.Handlers
	if scanEnabled {
		receiptClient := receipt.NewClient(cfg.ReceiptAPIURL, cfg.ReceiptAPIKey, cfg.ReceiptModel, cfg.ReceiptAPIFormat)
		receiptClient.Workflow = cfg.ReceiptAPIWorkflow
		receiptClient.Args = cfg.ReceiptAPIArgs
		receiptHandlers = &receipt.Handlers{
			Store:    receipt.NewStore(db),
			Client:   receiptClient,
			Expenses: expenses,
		}
		attrs := []any{"format", receiptClient.Format}
		if cfg.ReceiptModel != "" {
			attrs = append(attrs, "model", cfg.ReceiptModel)
		}
		if cfg.ReceiptAPIWorkflow != "" {
			attrs = append(attrs, "workflow", cfg.ReceiptAPIWorkflow)
		}
		slog.Info("receipt scanning enabled", attrs...)
	} else {
		// Serving previously-stored receipts still works even if scanning
		// is disabled now.
		receiptHandlers = &receipt.Handlers{Store: receipt.NewStore(db), Expenses: expenses}
	}

	srv := &server.Server{
		Cfg:       *cfg,
		Users:     users,
		Sessions:  sessions,
		AuthH:     authHandlers,
		Groups:    groups,
		GroupH:    groupHandlers,
		Expenses:  expenses,
		ExpenseH:  expenseHandlers,
		Activity:  activityStore,
		ActivityH: activityHandlers,
		Render:    r,
		ReceiptH:  receiptHandlers,
	}

	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpServer.Shutdown(shutdownCtx)
	}()

	slog.Info("listening", "address", cfg.Listen, "currency", cfg.Currency, "driver", driver)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
