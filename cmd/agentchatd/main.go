// Command agentchatd runs the OpenFlock server.
package main

import (
	// reminders resolve wall times in IANA zones; the container image has no tzdata
	_ "time/tzdata"

	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/presmihaylov/agentchat/models"
	"github.com/presmihaylov/agentchat/pkg/envx"
	"github.com/presmihaylov/agentchat/services/api"
	"github.com/presmihaylov/agentchat/services/auth"
	"github.com/presmihaylov/agentchat/services/embed"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// accessConfig reads the Cloudflare Access service token. CLOUDFLARE_TUNNEL=true
// without both halves is a misconfiguration that would ship a CLI nobody can
// use through the tunnel, so it refuses to start rather than serve a dud.
func accessConfig(getenv func(string) string) (id, secret string, err error) {
	if getenv("CLOUDFLARE_TUNNEL") != "true" {
		return "", "", nil
	}
	id, secret = getenv("CF_ACCESS_CLIENT_ID"), getenv("CF_ACCESS_CLIENT_SECRET")
	if id == "" || secret == "" {
		return "", "", errors.New("CLOUDFLARE_TUNNEL=true needs CF_ACCESS_CLIENT_ID and CF_ACCESS_CLIENT_SECRET")
	}
	return id, secret, nil
}

// authConfig reads the human-login knobs. Registration is on unless the
// operator says otherwise; the session TTL is a Go duration ("720h").
func authConfig(getenv func(string) string) (registration bool, ttl time.Duration, err error) {
	registration = true
	if v := envx.GetFrom(getenv, "REGISTRATION_ENABLED"); v != "" {
		registration, err = strconv.ParseBool(v)
		if err != nil {
			return false, 0, fmt.Errorf("OPENFLOCK_REGISTRATION_ENABLED: %w", err)
		}
	}
	ttl = 720 * time.Hour
	if v := envx.GetFrom(getenv, "SESSION_TTL"); v != "" {
		ttl, err = time.ParseDuration(v)
		if err != nil || ttl <= 0 {
			return false, 0, fmt.Errorf("OPENFLOCK_SESSION_TTL must be a positive duration like 720h, got %q", v)
		}
	}
	return registration, ttl, nil
}

// authProviders builds the login registry. Password is always on; Clerk
// joins only on a Clerk deployment (CLERK_SECRET_KEY set, design section 11).
func authProviders(store auth.PasswordStore, registration bool, getenv func(string) string) *auth.Registry {
	providers := []auth.Provider{auth.NewPasswordProvider(store, registration)}
	if k := getenv("CLERK_SECRET_KEY"); k != "" {
		providers = append(providers, auth.NewClerkProvider(k))
	}
	return auth.NewRegistry(providers...)
}

// parseFlags reads the command line. migrateTo is nil unless -migrate-to was
// given; version 0 is not a valid target, so a plain zero cannot stand in for
// "absent".
func parseFlags(args []string) (migrateTo *uint, err error) {
	fs := flag.NewFlagSet("agentchatd", flag.ContinueOnError)
	v := fs.Uint("migrate-to", 0, "move the schema to this migration version and exit without serving (rollback step, see docs/PROD.md)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	var set bool
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == "migrate-to" })
	if !set {
		return nil, nil
	}
	if *v == 0 {
		return nil, errors.New("-migrate-to needs a version of 1 or more")
	}
	return v, nil
}

func run() error {
	migrateTo, err := parseFlags(os.Args[1:])
	if err != nil {
		return err
	}
	dbURL := envx.Get("DB_URL")
	if dbURL == "" {
		return errors.New("OPENFLOCK_DB_URL is required")
	}
	if migrateTo != nil {
		got, err := models.MigrateTo(context.Background(), dbURL, *migrateTo)
		if err != nil {
			return err
		}
		fmt.Printf("schema at version %d\n", got)
		return nil
	}
	port := envx.Get("PORT")
	if port == "" {
		port = "8090"
	}
	publicURL := envx.Get("PUBLIC_URL")
	if publicURL == "" {
		publicURL = "http://localhost:" + port
	}

	if old := envx.LegacyInUse(os.Getenv, "DB_URL", "PORT", "PUBLIC_URL", "TRUST_PROXY",
		"REGISTRATION_ENABLED", "SESSION_TTL"); len(old) > 0 {
		slog.Warn("using deprecated AGENTCHAT_* environment variables; rename them to OPENFLOCK_*", "names", old)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := models.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer store.Close()

	var embedder api.Embedder
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		embedder = embed.NewOpenAI(key)
		go embed.NewWorker(store, embedder).Run(ctx)
		slog.Info("semantic search enabled", "model", embed.Model)
	} else {
		slog.Warn("OPENAI_API_KEY not set; semantic search disabled")
	}

	accessID, accessSecret, err := accessConfig(os.Getenv)
	if err != nil {
		return err
	}
	if accessID != "" {
		slog.Info("Cloudflare Access service token will be baked into /cli.sh")
	}

	registration, sessionTTL, err := authConfig(os.Getenv)
	if err != nil {
		return err
	}
	if !registration {
		slog.Info("self-service registration disabled")
	}

	server := api.New(store, api.Config{
		PublicURL:           publicURL,
		Embedder:            embedder,
		TrustProxy:          envx.Get("TRUST_PROXY") == "true",
		AccessClientID:      accessID,
		AccessClientSecret:  accessSecret,
		Providers:           authProviders(store, registration, os.Getenv),
		SessionTTL:          sessionTTL,
		RegistrationEnabled: registration,
	})

	// avatars and logos uploaded before resized copies existed get them once;
	// a later boot finds nothing to do
	go func() {
		done, skipped, err := store.BackfillAvatarVariants(ctx)
		if err != nil {
			slog.Error("avatar variant backfill failed", "err", err)
		} else if done+skipped > 0 {
			slog.Info("avatar variants backfilled", "resized", done, "unsupported", skipped)
		}
	}()

	// unreferenced uploads (posted but never attached) and dead login
	// sessions get swept periodically
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if n, err := store.DeleteOrphanAttachments(ctx); err != nil {
					slog.Error("orphan attachment sweep failed", "err", err)
				} else if n > 0 {
					slog.Info("swept orphan attachments", "count", n)
				}
				if n, err := store.SweepSessions(ctx); err != nil {
					slog.Error("session sweep failed", "err", err)
				} else if n > 0 {
					slog.Info("swept expired sessions", "count", n)
				}
			}
		}
	}()

	// passive presence: a participant whose heartbeat stopped goes offline
	// without any request, so a sweeper announces that transition as an event
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := store.SweepPresence(ctx); err != nil {
					slog.Error("presence sweep failed", "err", err)
				}
				if _, err := store.SweepDeliveries(ctx); err != nil {
					slog.Error("delivery sweep failed", "err", err)
				}
				if err := store.SweepCalls(ctx); err != nil {
					slog.Error("call sweep failed", "err", err)
				}
			}
		}
	}()

	// reminders (task 22): fire whatever is due on boot and every few seconds.
	// next_fire_at moves in the same tx as the event, so a restart neither
	// double-fires nor skips.
	go func() {
		fire := func() {
			if n, err := store.FireDueReminders(ctx, time.Now()); err != nil {
				slog.Error("reminder tick failed", "err", err)
			} else if n > 0 {
				slog.Info("reminders fired", "n", n)
			}
		}
		fire()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				fire()
			}
		}
	}()

	httpServer := &http.Server{
		Addr:              ":" + port,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// long-polls watch this context so they end promptly on SIGTERM
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	slog.Info("agentchatd listening", "addr", httpServer.Addr, "public_url", publicURL)
	if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// ListenAndServe returns the moment Shutdown starts; wait for the actual
	// drain or in-flight responses get connection-reset on process exit
	<-shutdownDone
	return nil
}
