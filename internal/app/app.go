// Package app wires the homeserver together and runs it.
//
// It is the seam tests use: [New] builds everything (database, services, HTTP handlers)
// without binding a socket, [App.Handler] hands out the client-server handler for an
// in-process test server, and [App.Run] binds the real listeners and blocks until the
// context is cancelled, shutting the listeners down gracefully.
package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/enimatek-nl/qwatrix/internal/accounts"
	"github.com/enimatek-nl/qwatrix/internal/config"
	"github.com/enimatek-nl/qwatrix/internal/csapi"
	"github.com/enimatek-nl/qwatrix/internal/devicelists"
	"github.com/enimatek-nl/qwatrix/internal/directory"
	"github.com/enimatek-nl/qwatrix/internal/events"
	"github.com/enimatek-nl/qwatrix/internal/fedapi"
	"github.com/enimatek-nl/qwatrix/internal/httpx"
	"github.com/enimatek-nl/qwatrix/internal/identity"
	"github.com/enimatek-nl/qwatrix/internal/keyserver"
	"github.com/enimatek-nl/qwatrix/internal/matrix"
	"github.com/enimatek-nl/qwatrix/internal/media"
	"github.com/enimatek-nl/qwatrix/internal/notifier"
	"github.com/enimatek-nl/qwatrix/internal/presence"
	"github.com/enimatek-nl/qwatrix/internal/pushrules"
	"github.com/enimatek-nl/qwatrix/internal/readstate"
	"github.com/enimatek-nl/qwatrix/internal/roomserver"
	"github.com/enimatek-nl/qwatrix/internal/store"
	"github.com/enimatek-nl/qwatrix/internal/syncapi"
	"github.com/enimatek-nl/qwatrix/internal/todevice"
	"github.com/enimatek-nl/qwatrix/internal/uia"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

// App is a built homeserver: services plus the HTTP surface in front of them.
type App struct {
	cfg      *config.Config
	store    *store.Store
	accounts *accounts.Service
	// identity is the server's signing key pair, events is the event domain, notifier is
	// the wake-up bus shared by the writers and /sync, and rooms and sync are the two
	// services the client API is mostly built on.
	identity *identity.Identity
	events   *events.Service
	notifier *notifier.Notifier
	rooms    *roomserver.Service
	sync     *syncapi.Service
	handler  http.Handler
	// federation is the server-server API, served on its own TLS listener because it is a
	// different protocol with a different way of proving who is asking.
	federation http.Handler
	// outbox delivers this server's queued peer copies. It is a ledger rather than an inline send so
	// that an unreachable peer can neither fail a local user's message nor make it vanish.
	outbox *fedapi.Outbox
	// closed guards Close so a test cleanup and a deferred call cannot double-close.
	closed bool
}

// Rooms exposes the room server to in-process tests and to the federation listener.
func (a *App) Rooms() *roomserver.Service { return a.rooms }

// Sync exposes the sync service.
func (a *App) Sync() *syncapi.Service { return a.sync }

// Events exposes the event domain.
func (a *App) Events() *events.Service { return a.events }

// Identity exposes the signing identity.
func (a *App) Identity() *identity.Identity { return a.identity }

// Notifier exposes the wake-up bus, so that a test can be sure a write woke the pollers
// rather than waiting on a timeout.
func (a *App) Notifier() *notifier.Notifier { return a.notifier }

// New builds the homeserver described by cfg. The database is opened and migrated, so a
// failure here is a start-up failure and never a request-time surprise.
func New(cfg *config.Config) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}
	if err := ensureDataDir(cfg); err != nil {
		return nil, err
	}

	st, err := store.Open(cfg.DatabaseFile())
	if err != nil {
		return nil, err
	}

	acc := accounts.New(st, cfg)

	// The signing identity is loaded (or created) before anything can build an event, and
	// the notifier is shared by the writers and the long-pollers: one process, one stream.
	serverIdentity, err := identity.Load(cfg.SigningKeyFile(), cfg.ServerName)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	eventService := events.New(cfg, serverIdentity)
	changes := notifier.New()
	rooms := roomserver.New(st, cfg, eventService, changes)
	presenceService := presence.New(st, cfg, changes)
	readState := readstate.New(st, cfg, changes)
	keyService := keyserver.New(st, cfg, changes)
	directoryService := directory.New(st, cfg)
	pushRuleService := pushrules.New(st, changes, pushrules.Options{ServerName: cfg.ServerName})
	// The unread badge is measured from receipts but decided by push rules, because "unread"
	// means "worth reading", which is a question the reader answers and not the server.
	readState.WithBadgeClassifier(pushRuleService)
	deviceListService := devicelists.New(st, changes, devicelists.Options{ServerName: cfg.ServerName})
	keyService.WithDeviceListAnnouncer(func(ctx context.Context, userID string) error {
		return deviceListService.Announce(ctx, userID, false)
	})
	acc.WithDeviceObserver(func(userID string) {
		if err := deviceListService.Announce(context.Background(), userID, false); err != nil {
			slog.Warn("could not announce a new device", "user_id", userID, "error", err)
		}
	})
	rooms.WithDeviceListAnnouncer(deviceListService)
	rooms.WithPresenceObserver(presenceService)
	toDeviceService := todevice.New(st, changes, todevice.Options{ServerName: cfg.ServerName})
	mediaService, err := media.New(st, media.Options{
		Dir:               filepath.Join(cfg.DataDir, "media"),
		ServerName:        cfg.ServerName,
		MaxBytes:          cfg.MediaSizeLimitBytes,
		ThumbnailsEnabled: cfg.ThumbnailsEnabled,
	})
	if err != nil {
		return nil, fmt.Errorf("opening the media repository: %w", err)
	}
	acc.WithProfileObserver(func(ctx context.Context, localpart, displayName, avatarURL string) {
		if err := directoryService.PublishProfile(ctx, localpart, displayName, avatarURL); err != nil {
			slog.Warn("could not publish a profile to the user directory",
				"localpart", localpart, "error", err)
		}
	})
	rooms.WithNamePublisher(directoryService)
	syncService := syncapi.New(st, cfg, rooms, presenceService, readState, pushRuleService, toDeviceService, deviceListService, changes)

	app := &App{cfg: cfg, store: st, accounts: acc, identity: serverIdentity, events: eventService, notifier: changes, rooms: rooms, sync: syncService}
	app.handler = httpx.Chain(
		csapi.NewMux(csapi.Deps{
			Config: cfg,
			Store:  st,
			PublicFederation: func(w http.ResponseWriter, r *http.Request) {
				// The server-server API is built after the client API, so this is read at request time.
				if app.federation == nil {
					httpx.RespondError(w, r, matrix.NewUnknown("This homeserver does not serve the federation API"))
					return
				}
				app.federation.ServeHTTP(w, r)
			},
			Accounts:      acc,
			UIA:           uia.NewManager(),
			SharedSecrets: accounts.NewNonceStore(),
			Rooms:         rooms,
			Keys:          keyService,
			Directory:     directoryService,
			Sync:          syncService,
			Presence:      presenceService,
			ReadState:     readState,
			PushRules:     pushRuleService,
			ToDevice:      toDeviceService,
			Media:         mediaService,
			DeviceLists:   deviceListService,
		}),
		httpx.Recoverer,
		httpx.LogRequests,
		httpx.NoStore,
		httpx.CORS,
	)
	// The outbound federation client is this server's signing identity on the wire. It doubles as
	// the way the verifier obtains a peer's keys: to decide whether a peer is who it claims, this
	// server asks that peer's own server, over a connection it verifies.
	peer, err := fedapi.NewPeer(fedapi.PeerOptions{
		ServerName:            spec.ServerName(cfg.ServerName),
		KeyID:                 string(serverIdentity.KeyID),
		PrivateKey:            serverIdentity.PrivateKey,
		TrustedCACertFile:     cfg.TrustedCACertFile,
		InsecureSkipTLSVerify: cfg.FederationInsecureSkipTLSVerify,
		UserAgent:             "qwatrix/" + cfg.Version,
	})
	if err != nil {
		return nil, fmt.Errorf("building the federation peer client: %w", err)
	}
	if cfg.FederationInsecureSkipTLSVerify {
		slog.Warn("federation is configured NOT to verify peer TLS certificates; "+
			"anybody able to intercept a peer connection can then publish a signing key as that peer. "+
			"This is for test networks and must not be used in a deployment",
			"server_name", cfg.ServerName)
	}
	// A deployment that is not serving federation does not queue deliveries either: the events would
	// accumulate against a worker that was never going to run, which is storage spent on a message
	// nobody will ever be told about.
	if cfg.FederationAddr == "" {
		rooms.WithoutFederationOutbound()
	}
	app.outbox = fedapi.NewOutbox(peer, st, spec.ServerName(cfg.ServerName))

	app.federation = fedapi.New(serverIdentity, fedapi.Options{
		ServerName: spec.ServerName(cfg.ServerName),
		Version:    "qwatrix/" + cfg.Version,
		Join:       rooms,
		Peer:       peer,
		Devices:    keyService,
		Media:      mediaService,
	})
	// A client asking about a user who lives elsewhere has to be answered by that user's own server,
	// so the key server is given the transport that can ask. Without it, /keys/query for a remote user
	// answers "no devices" — which a client reads as a true statement about a stranger rather than as
	// this server shrugging, and it will not ask again.
	keyService.WithRemoteKeySource(fedapi.NewRemoteDevices(peer))
	// An image hosted elsewhere is not this server's file, and a client cannot fetch it from the
	// origin on its own behalf: a Matrix client asks its own homeserver, and this server has to be
	// willing to go and ask. Without this, every image, avatar and file from another server in a
	// federated room is a grey square with no error anywhere — the file exists, two hops away, and
	// nothing the user can do makes it appear.
	if cfg.FederationAddr != "" {
		mediaService.WithRemoteFetcher(media.RemoteFetcherFunc(
			func(ctx context.Context, origin string, path string) (*media.RemoteMedia, error) {
				fetched, err := peer.GetSignedFile(ctx, spec.ServerName(origin), path)
				if err != nil {
					return nil, err
				}
				return &media.RemoteMedia{
					ContentType: fetched.ContentType,
					FileName:    fetched.FileName,
					Body:        fetched.Body,
					Size:        fetched.Size,
				}, nil
			}))
	}
	// Joining a room this server does not hold is the one room operation that cannot be answered
	// from the local database, so the room server is given the federation transport for it. Left
	// unset, a room this server has never heard of simply does not exist — which is the correct
	// answer for a homeserver that does not federate.
	rooms.WithFederationJoiner(fedapi.NewRemoteJoiner(fedapi.RemoteJoinOptions{
		ServerName: spec.ServerName(cfg.ServerName),
		Peer:       peer,
		Events:     eventService,
		KeyRing:    fedapi.NewKeyRing(peer, serverIdentity),
	}))
	return app, nil
}

// DrainFederationOutbox attempts the queued peer copies now and reports how many reached a peer.
//
// A production server does not need this — the delivery worker runs on a tick — but a test cannot be
// served by a goroutine it has no influence over, and "the message arrived eventually" is not an
// assertion anybody wants to debug at three in the morning.
func (a *App) DrainFederationOutbox(ctx context.Context) (int, error) {
	if a.outbox == nil {
		return 0, nil
	}
	return a.outbox.DrainOnce(ctx)
}

// Handler returns the client-server HTTP handler, including middleware.
func (a *App) Handler() http.Handler { return a.handler }

// FederationHandler returns the server-server HTTP handler. It is served over TLS on its own
// port, and is exposed separately so a test can drive it without a listener.
func (a *App) FederationHandler() http.Handler { return a.federation }

// Accounts exposes the accounts service to the composition root (and to the federation
// package, which needs to know who is asking).
func (a *App) Accounts() *accounts.Service { return a.accounts }

// Store exposes the data store to the composition root.
func (a *App) Store() *store.Store { return a.store }

// Close releases resources. It is safe to call more than once.
func (a *App) Close() error {
	if a.closed {
		return nil
	}
	a.closed = true
	if a.store != nil {
		if err := a.store.Close(); err != nil {
			return fmt.Errorf("closing database: %w", err)
		}
	}
	return nil
}

// Run serves until ctx is cancelled, then shuts the listeners down within shutdownTimeout.
//
// The two listeners are siblings in one errgroup: either failing cancels the other, so a
// homeserver whose federation port could not bind does not linger as a client-only server
// with an operator believing otherwise.
func (a *App) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	clientLn, err := net.Listen("tcp", a.cfg.ClientAddr)
	if err != nil {
		return fmt.Errorf("listening for client traffic on %s: %w", a.cfg.ClientAddr, err)
	}
	slog.Info("client-server API listening",
		"addr", a.cfg.ClientAddr, "server_name", a.cfg.ServerName)

	group, ctx := errgroup.WithContext(ctx)
	clientSrv := &http.Server{
		Handler:           a.handler,
		ReadHeaderTimeout: 20 * time.Second,
		// No overall read/write timeout: /sync long-polls for up to the configured
		// maximum, and idle keep-alives are normal. Timeouts live per-handler.
		ReadTimeout:  0,
		WriteTimeout: 0,
	}

	group.Go(func() error {
		if err := clientSrv.Serve(clientLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("client-server listener: %w", err)
		}
		return nil
	})

	// The server-server API. Federation is HTTPS by specification, so the listener needs a
	// certificate: the configured pair when one was given, a self-signed one otherwise. A
	// peer will not trust the self-signed case, which is the right outcome — an untrusted
	// certificate is a refusal, and federation should refuse rather than pretend.
	if a.cfg.FederationAddr != "" {
		fedSrv, fedLn, err := a.listenFederation()
		if err != nil {
			return err
		}
		slog.Info("server-server API listening",
			"addr", a.cfg.FederationAddr, "server_name", a.cfg.ServerName)
		group.Go(func() error {
			if err := fedSrv.ServeTLS(fedLn, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("server-server listener: %w", err)
			}
			return nil
		})
		group.Go(func() error {
			<-ctx.Done()
			return fedSrv.Shutdown(context.WithoutCancel(ctx))
		})
	}

	// The federation delivery ledger, running beside the listeners rather than behind them: a peer that
	// is unreachable is a reason to keep a copy queued, not a reason for anything a local user does to
	// wait or to fail.
	// The condition is whether this server federates at all, not whether it was given a separate
	// address to federate on. With no federation address the server-server API is served from the
	// client listener and federation is still on — which is the default deployment — and a server
	// that only delivers when somebody configures a second port is a server that never invites,
	// never kicks and never bans anybody on another homeserver. The queue keeps the obligation
	// either way, so what a wrong condition here produces is silence rather than an error.
	if a.outbox != nil {
		group.Go(func() error { return a.outbox.Run(ctx) })
	}

	group.Go(func() error {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		return clientSrv.Shutdown(shutdownCtx)
	})

	return group.Wait()
}

// listenFederation builds the TLS listener and its server. The certificate is loaded onto the
// http.Server rather than passed to ServeTLS by name because Config.FederationTLS may have to
// create a self-signed pair, and a start-up failure there should be a returned error rather than a
// goroutine that dies after the client API is already serving.
func (a *App) listenFederation() (*http.Server, net.Listener, error) {
	certFile, keyFile, err := a.cfg.FederationTLS()
	if err != nil {
		return nil, nil, fmt.Errorf("preparing the federation certificate: %w", err)
	}
	ln, err := net.Listen("tcp", a.cfg.FederationAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("listening for federation traffic on %s: %w", a.cfg.FederationAddr, err)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		_ = ln.Close()
		return nil, nil, fmt.Errorf("loading the federation certificate %s: %w", certFile, err)
	}
	return &http.Server{
		Handler:           a.federation,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 20 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
	}, ln, nil
}

// ensureDataDir creates the directories the server owns. Media lives under DataDir, and
// creating it up front turns an unwritable path into a start-up error rather than a
// first-upload failure.
func ensureDataDir(cfg *config.Config) error {
	for _, dir := range []string{cfg.DataDir, cfg.MediaDir()} {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("creating data directory %q: %w", dir, err)
		}
	}
	// A database in a directory that does not exist is a confusing failure; the parent of
	// an explicit --db path is not necessarily DataDir.
	if parent := filepath.Dir(cfg.DatabaseFile()); parent != "" && parent != "." {
		if err := os.MkdirAll(parent, 0o750); err != nil {
			return fmt.Errorf("creating database directory %q: %w", parent, err)
		}
	}
	return nil
}
