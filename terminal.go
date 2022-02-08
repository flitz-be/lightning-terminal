package terminal

import (
	"context"
	"crypto/tls"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	restProxy "github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/jessevdk/go-flags"
	"github.com/lightninglabs/faraday/frdrpc"
	"github.com/lightninglabs/lightning-terminal/litrpc"
	"github.com/lightninglabs/lightning-terminal/session"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/loopd"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/pool"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/autopilotrpc"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/verrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnrpc/watchtowerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/wtclientrpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/rpcperms"
	"github.com/lightningnetwork/lnd/signal"
	grpcProxy "github.com/mwitkow/grpc-proxy/proxy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon.v2"
)

const (
	defaultServerTimeout  = 10 * time.Second
	defaultConnectTimeout = 15 * time.Second
	defaultStartupTimeout = 5 * time.Second
)

// restRegistration is a function type that represents a REST proxy
// registration.
type restRegistration func(context.Context, *restProxy.ServeMux, string,
	[]grpc.DialOption) error

var (
	// maxMsgRecvSize is the largest message our REST proxy will receive. We
	// set this to 200MiB atm.
	maxMsgRecvSize = grpc.MaxCallRecvMsgSize(1 * 1024 * 1024 * 200)

	// appBuildFS is an in-memory file system that contains all the static
	// HTML/CSS/JS files of the UI. It is compiled into the binary with the
	// go 1.16 embed directive below. Because the path is relative to the
	// root package, all assets will have a path prefix of /app/build/ which
	// we'll strip by giving a sub directory to the HTTP server.
	//
	//go:embed app/build/*
	appBuildFS embed.FS

	// appFilesDir is the sub directory of the above build directory which
	// we pass to the HTTP server.
	appFilesDir = "app/build"

	// appFilesPrefix is the path prefix the static assets of the UI are
	// exposed under. This variable can be overwritten during build time if
	// a different deployment path should be used.
	appFilesPrefix = ""

	// patternRESTRequest is the regular expression that matches all REST
	// URIs that are currently used by lnd, faraday, loop and pool.
	patternRESTRequest = regexp.MustCompile(`^/v\d/.*`)

	// lndRESTRegistrations is the list of all lnd REST handler registration
	// functions we want to call when creating our REST proxy. We include
	// all lnd subserver packages here, even though some might not be active
	// in a remote lnd node. That will result in an "UNIMPLEMENTED" error
	// instead of a 404 which should be an okay tradeoff vs. connecting
	// first and querying all enabled subservers to dynamically populate
	// this list.
	lndRESTRegistrations = []restRegistration{
		lnrpc.RegisterLightningHandlerFromEndpoint,
		lnrpc.RegisterWalletUnlockerHandlerFromEndpoint,
		autopilotrpc.RegisterAutopilotHandlerFromEndpoint,
		chainrpc.RegisterChainNotifierHandlerFromEndpoint,
		invoicesrpc.RegisterInvoicesHandlerFromEndpoint,
		routerrpc.RegisterRouterHandlerFromEndpoint,
		signrpc.RegisterSignerHandlerFromEndpoint,
		verrpc.RegisterVersionerHandlerFromEndpoint,
		walletrpc.RegisterWalletKitHandlerFromEndpoint,
		watchtowerrpc.RegisterWatchtowerHandlerFromEndpoint,
		wtclientrpc.RegisterWatchtowerClientHandlerFromEndpoint,
	}

	// minimalCompatibleVersion is the minimal lnd version that is required
	// to run LiT in remote mode.
	minimalCompatibleVersion = &verrpc.Version{
		AppMajor: 0,
		AppMinor: 13,
		AppPatch: 3,
		BuildTags: []string{
			"signrpc", "walletrpc", "chainrpc", "invoicesrpc",
		},
	}
)

// LightningTerminal is the main grand unified binary instance. Its task is to
// start an lnd node then start and register external subservers to it.
type LightningTerminal struct {
	cfg *Config

	defaultImplCfg *lnd.ImplementationCfg

	// lndInterceptorChain is a reference to lnd's interceptor chain that
	// guards all incoming calls. This is only set in integrated mode!
	lndInterceptorChain *rpcperms.InterceptorChain

	wg         sync.WaitGroup
	lndErrChan chan error

	lndClient   *lndclient.GrpcLndServices
	basicClient lnrpc.LightningClient

	faradayServer  *frdrpc.RPCServer
	faradayStarted bool

	loopServer  *loopd.Daemon
	loopStarted bool

	poolServer  *pool.Server
	poolStarted bool

	rpcProxy   *rpcProxy
	httpServer *http.Server

	sessionDB        *session.DB
	sessionServer    *session.Server
	sessionRpcServer *sessionRpcServer

	restHandler http.Handler
	restCancel  func()
}

// New creates a new instance of the lightning-terminal daemon.
func New() *LightningTerminal {
	return &LightningTerminal{
		lndErrChan: make(chan error, 1),
	}
}

// Run starts everything and then blocks until either the application is shut
// down or a critical error happens.
func (g *LightningTerminal) Run() error {
	// Hook interceptor for os signals.
	shutdownInterceptor, err := signal.Intercept()
	if err != nil {
		return fmt.Errorf("could not intercept signals: %v", err)
	}

	cfg, err := loadAndValidateConfig(shutdownInterceptor)
	if err != nil {
		return fmt.Errorf("could not load config: %w", err)
	}
	g.cfg = cfg
	g.defaultImplCfg = g.cfg.Lnd.ImplementationConfig(shutdownInterceptor)

	// Create the instances of our subservers now so we can hook them up to
	// lnd once it's fully started.
	bufRpcListener := bufconn.Listen(100)
	g.faradayServer = frdrpc.NewRPCServer(g.cfg.faradayRpcConfig)
	g.loopServer = loopd.New(g.cfg.Loop, nil)
	g.poolServer = pool.NewServer(g.cfg.Pool)
	g.rpcProxy = newRpcProxy(
		g.cfg, g, getAllMethodPermissions(), bufRpcListener,
	)

	// Create an instance of the local Terminal Connect session store DB.
	networkDir := path.Join(g.cfg.LitDir, g.cfg.Network)
	g.sessionDB, err = session.NewDB(networkDir, session.DBFilename)
	if err != nil {
		return fmt.Errorf("error creating session DB: %v", err)
	}

	// Create the gRPC server that handles adding/removing sessions and the
	// actual mailbox server that spins up the Terminal Connect server
	// interface.
	g.sessionServer = session.NewServer(
		func(opts ...grpc.ServerOption) *grpc.Server {
			allOpts := []grpc.ServerOption{
				grpc.CustomCodec(grpcProxy.Codec()), // nolint: staticcheck,
				grpc.UnknownServiceHandler(
					grpcProxy.TransparentHandler(
						g.rpcProxy.director,
					),
				),
			}
			allOpts = append(allOpts, opts...)
			mailboxGrpcServer := grpc.NewServer(allOpts...)

			_ = g.RegisterGrpcSubserver(mailboxGrpcServer)

			return mailboxGrpcServer
		},
	)
	g.sessionRpcServer = &sessionRpcServer{
		basicAuth:     g.rpcProxy.basicAuth,
		db:            g.sessionDB,
		sessionServer: g.sessionServer,
		quit:          make(chan struct{}),
	}

	// Now start up all previously created sessions.
	sessions, err := g.sessionDB.ListSessions()
	if err != nil {
		return fmt.Errorf("error listing sessions: %v", err)
	}
	for _, sess := range sessions {
		if err := g.sessionRpcServer.resumeSession(sess); err != nil {
			return fmt.Errorf("error resuming sesion: %v", err)
		}
	}

	// Overwrite the loop and pool daemon's user agent name so it sends
	// "litd" instead of "loopd" and "poold" respectively.
	loop.AgentName = "litd"
	pool.SetAgentName("litd")

	// Call the "real" main in a nested manner so the defers will properly
	// be executed in the case of a graceful shutdown.
	readyChan := make(chan struct{})
	bufReadyChan := make(chan struct{})
	unlockChan := make(chan struct{})
	macChan := make(chan []byte, 1)

	if g.cfg.LndMode == ModeIntegrated {
		lisCfg := lnd.ListenerCfg{
			RPCListeners: []*lnd.ListenerWithSignal{{
				Listener: &onDemandListener{
					addr: g.cfg.Lnd.RPCListeners[0],
				},
				Ready: readyChan,
			}, {
				Listener: bufRpcListener,
				Ready:    bufReadyChan,
				MacChan:  macChan,
			}},
		}

		implCfg := &lnd.ImplementationCfg{
			GrpcRegistrar:       g,
			RestRegistrar:       g,
			ExternalValidator:   g,
			DatabaseBuilder:     g.defaultImplCfg.DatabaseBuilder,
			WalletConfigBuilder: g,
			ChainControlBuilder: g.defaultImplCfg.ChainControlBuilder,
		}

		g.wg.Add(1)
		go func() {
			defer g.wg.Done()

			err := lnd.Main(
				g.cfg.Lnd, lisCfg, implCfg, shutdownInterceptor,
			)
			if e, ok := err.(*flags.Error); err != nil &&
				(!ok || e.Type != flags.ErrHelp) {

				log.Errorf("Error running main lnd: %v", err)
				g.lndErrChan <- err
				return
			}

			close(g.lndErrChan)
		}()
	} else {
		close(unlockChan)
		close(readyChan)
		close(bufReadyChan)

		_ = g.RegisterGrpcSubserver(g.rpcProxy.grpcServer)
	}

	// We'll also create a REST proxy that'll convert any REST calls to gRPC
	// calls and forward them to the internal listener.
	if g.cfg.EnableREST {
		if err := g.createRESTProxy(); err != nil {
			return fmt.Errorf("error creating REST proxy: %v", err)
		}
	}

	// Wait for lnd to be started up so we know we have a TLS cert.
	select {
	// If lnd needs to be unlocked we get the signal that it's ready to do
	// so. We then go ahead and start the UI so we can unlock it there as
	// well.
	case <-unlockChan:

	// If lnd is running with --noseedbackup and doesn't need unlocking, we
	// get the ready signal immediately.
	case <-readyChan:

	case err := <-g.lndErrChan:
		return err

	case <-shutdownInterceptor.ShutdownChannel():
		return errors.New("shutting down")
	}

	// We now know that starting lnd was successful. If we now run into an
	// error, we must shut down lnd correctly.
	defer func() {
		err := g.shutdown()
		if err != nil {
			log.Errorf("Error shutting down: %v", err)
		}
	}()

	// Now start the RPC proxy that will handle all incoming gRPC, grpc-web
	// and REST requests. We also start the main web server that dispatches
	// requests either to the static UI file server or the RPC proxy. This
	// makes it possible to unlock lnd through the UI.
	if err := g.rpcProxy.Start(); err != nil {
		return fmt.Errorf("error starting lnd gRPC proxy server: %v",
			err)
	}
	if err := g.startMainWebServer(); err != nil {
		return fmt.Errorf("error starting UI HTTP server: %v", err)
	}

	// Now that we have started the main UI web server, show some useful
	// information to the user so they can access the web UI easily.
	if err := g.showStartupInfo(); err != nil {
		return fmt.Errorf("error displaying startup info: %v", err)
	}

	// Wait for lnd to be unlocked, then start all clients.
	select {
	case <-readyChan:

	case err := <-g.lndErrChan:
		return err

	case <-shutdownInterceptor.ShutdownChannel():
		return errors.New("shutting down")
	}

	// If we're in integrated mode, we'll need to wait for lnd to send the
	// macaroon after unlock before going any further.
	if g.cfg.LndMode == ModeIntegrated {
		<-bufReadyChan
		g.cfg.lndAdminMacaroon = <-macChan
	}

	err = g.startSubservers()
	if err != nil {
		log.Errorf("Could not start subservers: %v", err)
		return err
	}

	// Now block until we receive an error or the main shutdown signal.
	select {
	case err := <-g.loopServer.ErrChan:
		// Loop will shut itself down if an error happens. We don't need
		// to try to stop it again.
		g.loopStarted = false
		log.Errorf("Received critical error from loop, shutting down: "+
			"%v", err)

	case err := <-g.lndErrChan:
		if err != nil {
			log.Errorf("Received critical error from lnd, "+
				"shutting down: %v", err)
		}

	case <-shutdownInterceptor.ShutdownChannel():
		log.Infof("Shutdown signal received")
	}

	return nil
}

// startSubservers creates an internal connection to lnd and then starts all
// embedded daemons as external subservers that hook into the same gRPC and REST
// servers that lnd started.
func (g *LightningTerminal) startSubservers() error {
	var (
		insecure      bool
		clientOptions []lndclient.BasicClientOption
	)

	host, network, tlsPath, macPath, macData := g.cfg.lndConnectParams()
	clientOptions = append(clientOptions, lndclient.MacaroonData(
		hex.EncodeToString(macData),
	))
	clientOptions = append(
		clientOptions, lndclient.MacFilename(path.Base(macPath)),
	)

	// If we're in integrated mode, we can retrieve the macaroon string
	// from lnd directly, rather than grabbing it from disk.
	if g.cfg.LndMode == ModeIntegrated {
		// Set to true in integrated mode, since we will not require tls
		// when communicating with lnd via a bufconn.
		insecure = true
		clientOptions = append(clientOptions, lndclient.Insecure())
	}

	// The main RPC listener of lnd might need some time to start, it could
	// be that we run into a connection refused a few times. We use the
	// basic client connection to find out if the RPC server is started yet
	// because that doesn't do anything else than just connect. We'll check
	// if lnd is also ready to be used in the next step.
	err := wait.NoError(func() error {
		// Create an lnd client now that we have the full configuration.
		// We'll need a basic client and a full client because not all
		// subservers have the same requirements.
		var err error
		g.basicClient, err = lndclient.NewBasicClient(
			host, tlsPath, path.Dir(macPath), string(network),
			clientOptions...,
		)
		return err
	}, defaultStartupTimeout)
	if err != nil {
		return err
	}

	// Now we know that the connection itself is ready. But we also need to
	// wait for two things: The chain notifier to be ready and the lnd
	// wallet being fully synced to its chain backend. The chain notifier
	// will always be ready first so if we instruct the lndclient to wait
	// for the wallet sync, we should be fully ready to start all our
	// subservers. This will just block until lnd signals readiness. But we
	// still want to react to shutdown requests, so we need to listen for
	// those.
	ctxc, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Make sure the context is canceled if the user requests shutdown.
	go func() {
		select {
		// Client requests shutdown, cancel the wait.
		case <-interceptor.ShutdownChannel():
			cancel()

		// The check was completed and the above defer canceled the
		// context. We can just exit the goroutine, nothing more to do.
		case <-ctxc.Done():
		}
	}()

	g.lndClient, err = lndclient.NewLndServices(
		&lndclient.LndServicesConfig{
			LndAddress:            host,
			Network:               network,
			TLSPath:               tlsPath,
			Insecure:              insecure,
			CustomMacaroonPath:    macPath,
			CustomMacaroonHex:     hex.EncodeToString(macData),
			BlockUntilChainSynced: true,
			BlockUntilUnlocked:    true,
			CallerCtx:             ctxc,
			CheckVersion:          minimalCompatibleVersion,
		},
	)
	if err != nil {
		return err
	}

	// In the integrated mode, we received an admin macaroon once lnd was
	// ready. We can now bake a "super macaroon" that contains all
	// permissions of all daemons that we can use for any internal calls.
	if g.cfg.LndMode == ModeIntegrated {
		// Create a super macaroon that can be used to control lnd,
		// faraday, loop, and pool, all at the same time.
		ctx := context.Background()
		superMacaroon, err := bakeSuperMacaroon(
			ctx, g.basicClient, 0, getAllPermissions(), nil,
		)
		if err != nil {
			return err
		}

		g.rpcProxy.superMacaroon = superMacaroon
	}

	// If we're in integrated and stateless init mode, we won't create
	// macaroon files in any of the subserver daemons.
	createDefaultMacaroons := true
	if g.cfg.LndMode == ModeIntegrated && g.lndInterceptorChain != nil &&
		g.lndInterceptorChain.MacaroonService() != nil {

		// If the wallet was initialized in stateless mode, we don't
		// want any macaroons lying around on the filesystem. In that
		// case only the UI will be able to access any of the integrated
		// daemons. In all other cases we want default macaroons so we
		// can use the CLI tools to interact with loop/pool/faraday.
		macService := g.lndInterceptorChain.MacaroonService()
		createDefaultMacaroons = !macService.StatelessInit
	}

	// Both connection types are ready now, let's start our subservers if
	// they should be started locally as an integrated service.
	if !g.cfg.faradayRemote {
		err = g.faradayServer.StartAsSubserver(
			g.lndClient.LndServices, createDefaultMacaroons,
		)
		if err != nil {
			return err
		}
		g.faradayStarted = true
	}

	if !g.cfg.loopRemote {
		err = g.loopServer.StartAsSubserver(
			g.lndClient, createDefaultMacaroons,
		)
		if err != nil {
			return err
		}
		g.loopStarted = true
	}

	if !g.cfg.poolRemote {
		err = g.poolServer.StartAsSubserver(
			g.basicClient, g.lndClient, createDefaultMacaroons,
		)
		if err != nil {
			return err
		}
		g.poolStarted = true
	}

	return nil
}

// RegisterGrpcSubserver is a callback on the lnd.SubserverConfig struct that is
// called once lnd has initialized its main gRPC server instance. It gives the
// daemons (or external subservers) the possibility to register themselves to
// the same server instance.
//
// NOTE: This is part of the lnd.GrpcRegistrar interface.
func (g *LightningTerminal) RegisterGrpcSubserver(server *grpc.Server) error {
	if err := g.defaultImplCfg.RegisterGrpcSubserver(server); err != nil {
		return err
	}

	// In remote mode the "director" of the RPC proxy will act as a catch-
	// all for any gRPC request that isn't known because we didn't register
	// any server for it. The director will then forward the request to the
	// remote service.
	if !g.cfg.faradayRemote {
		frdrpc.RegisterFaradayServerServer(server, g.faradayServer)
	}

	if !g.cfg.loopRemote {
		looprpc.RegisterSwapClientServer(server, g.loopServer)
	}

	if !g.cfg.poolRemote {
		poolrpc.RegisterTraderServer(server, g.poolServer)
	}

	litrpc.RegisterSessionsServer(server, g.sessionRpcServer)

	return nil
}

// RegisterRestSubserver is a callback on the lnd.SubserverConfig struct that is
// called once lnd has initialized its main REST server instance. It gives the
// daemons (or external subservers) the possibility to register themselves to
// the same server instance.
//
// NOTE: This is part of the lnd.RestRegistrar interface.
func (g *LightningTerminal) RegisterRestSubserver(ctx context.Context,
	mux *restProxy.ServeMux, endpoint string,
	dialOpts []grpc.DialOption) error {

	err := g.defaultImplCfg.RegisterRestSubserver(
		ctx, mux, endpoint, dialOpts,
	)
	if err != nil {
		return err
	}

	err = frdrpc.RegisterFaradayServerHandlerFromEndpoint(
		ctx, mux, endpoint, dialOpts,
	)
	if err != nil {
		return err
	}

	err = looprpc.RegisterSwapClientHandlerFromEndpoint(
		ctx, mux, endpoint, dialOpts,
	)
	if err != nil {
		return err
	}

	return poolrpc.RegisterTraderHandlerFromEndpoint(
		ctx, mux, endpoint, dialOpts,
	)
}

// ValidateMacaroon extracts the macaroon from the context's gRPC metadata,
// checks its signature, makes sure all specified permissions for the called
// method are contained within and finally ensures all caveat conditions are
// met. A non-nil error is returned if any of the checks fail.
//
// NOTE: This is part of the lnd.ExternalValidator interface.
func (g *LightningTerminal) ValidateMacaroon(ctx context.Context,
	requiredPermissions []bakery.Op, fullMethod string) error {

	macHex, err := macaroons.RawMacaroonFromContext(ctx)
	if err != nil {
		return err
	}

	// If we're in integrated mode, we're using a super macaroon internally,
	// which we can just pass straight to lnd for validation. But the user
	// might still be using a specific macaroon, which should be handled the
	// same as before.
	isSuperMacaroon := macHex == g.rpcProxy.superMacaroon
	if g.cfg.LndMode == ModeIntegrated && isSuperMacaroon {
		macBytes, err := hex.DecodeString(macHex)
		if err != nil {
			return err
		}

		// If we haven't connected to lnd yet, we can't check the super
		// macaroon. The user will need to wait a bit.
		if g.lndClient == nil {
			return fmt.Errorf("cannot validate macaroon, not yet " +
				"connected to lnd, please wait")
		}

		// Convert permissions to the form that lndClient will accept.
		permissions := make(
			[]lndclient.MacaroonPermission, len(requiredPermissions),
		)
		for idx, perm := range requiredPermissions {
			permissions[idx] = lndclient.MacaroonPermission{
				Entity: perm.Entity,
				Action: perm.Action,
			}
		}

		res, err := g.lndClient.Client.CheckMacaroonPermissions(
			ctx, macBytes, permissions, fullMethod,
		)
		if !res {
			return fmt.Errorf("macaroon is not valid, returned %v",
				res)
		}

		return err
	}

	// Validate all macaroons for services that are running in the local
	// process. Calls that we proxy to a remote host don't need to be
	// checked as they'll have their own interceptor.
	switch {
	case isFaradayURI(fullMethod):
		// In remote mode we just pass through the request, the remote
		// daemon will check the macaroon.
		if g.cfg.faradayRemote {
			return nil
		}

		if !g.faradayStarted {
			return fmt.Errorf("faraday is not yet ready for " +
				"requests, lnd possibly still starting or " +
				"syncing")
		}

		err = g.faradayServer.ValidateMacaroon(
			ctx, requiredPermissions, fullMethod,
		)
		if err != nil {
			return &proxyErr{
				proxyContext: "faraday",
				wrapped: fmt.Errorf("invalid macaroon: %v",
					err),
			}
		}

	case isLoopURI(fullMethod):
		// In remote mode we just pass through the request, the remote
		// daemon will check the macaroon.
		if g.cfg.loopRemote {
			return nil
		}

		if !g.loopStarted {
			return fmt.Errorf("loop is not yet ready for " +
				"requests, lnd possibly still starting or " +
				"syncing")
		}

		err = g.loopServer.ValidateMacaroon(
			ctx, requiredPermissions, fullMethod,
		)
		if err != nil {
			return &proxyErr{
				proxyContext: "loop",
				wrapped: fmt.Errorf("invalid macaroon: %v",
					err),
			}
		}

	case isPoolURI(fullMethod):
		// In remote mode we just pass through the request, the remote
		// daemon will check the macaroon.
		if g.cfg.poolRemote {
			return nil
		}

		if !g.poolStarted {
			return fmt.Errorf("pool is not yet ready for " +
				"requests, lnd possibly still starting or " +
				"syncing")
		}

		err = g.poolServer.ValidateMacaroon(
			ctx, requiredPermissions, fullMethod,
		)
		if err != nil {
			return &proxyErr{
				proxyContext: "pool",
				wrapped: fmt.Errorf("invalid macaroon: %v",
					err),
			}
		}

	case isLitURI(fullMethod):
		wrap := fmt.Errorf("invalid basic auth")
		_, err := g.rpcProxy.convertBasicAuth(ctx, fullMethod, wrap)
		if err != nil {
			return &proxyErr{
				proxyContext: "lit",
				wrapped: fmt.Errorf("invalid auth: %v",
					err),
			}
		}
	}

	// Because lnd will spin up its own gRPC server with macaroon
	// interceptors if it is running in this process, it will check its
	// macaroons there. If lnd is running remotely, that process will check
	// the macaroons. So we don't need to worry about anything other than
	// the subservers that are running in the local process.
	return nil
}

// Permissions returns all permissions for which the external validator of the
// terminal is responsible.
//
// NOTE: This is part of the lnd.ExternalValidator interface.
func (g *LightningTerminal) Permissions() map[string][]bakery.Op {
	return getSubserverPermissions()
}

// BuildWalletConfig is responsible for creating or unlocking and then
// fully initializing a wallet.
//
// NOTE: This is only implemented in order for us to intercept the setup call
// and store a reference to the interceptor chain.
//
// NOTE: This is part of the lnd.WalletConfigBuilder interface.
func (g *LightningTerminal) BuildWalletConfig(ctx context.Context,
	dbs *lnd.DatabaseInstances, interceptorChain *rpcperms.InterceptorChain,
	grpcListeners []*lnd.ListenerWithSignal) (*chainreg.PartialChainControl,
	*btcwallet.Config, func(), error) {

	g.lndInterceptorChain = interceptorChain

	return g.defaultImplCfg.WalletConfigBuilder.BuildWalletConfig(
		ctx, dbs, interceptorChain, grpcListeners,
	)
}

// shutdown stops all subservers that were started and attached to lnd.
func (g *LightningTerminal) shutdown() error {
	var returnErr error

	if g.faradayStarted {
		if err := g.faradayServer.Stop(); err != nil {
			log.Errorf("Error stopping faraday: %v", err)
			returnErr = err
		}
	}

	if g.loopStarted {
		g.loopServer.Stop()
		if err := <-g.loopServer.ErrChan; err != nil {
			log.Errorf("Error stopping loop: %v", err)
			returnErr = err
		}
	}

	if g.poolStarted {
		if err := g.poolServer.Stop(); err != nil {
			log.Errorf("Error stopping pool: %v", err)
			returnErr = err
		}
	}

	g.sessionRpcServer.stop()
	if err := g.sessionDB.Close(); err != nil {
		log.Errorf("Error closing session DB: %v", err)
		returnErr = err
	}
	g.sessionServer.Stop()

	if g.lndClient != nil {
		g.lndClient.Close()
	}

	if g.restCancel != nil {
		g.restCancel()
	}

	if g.rpcProxy != nil {
		if err := g.rpcProxy.Stop(); err != nil {
			log.Errorf("Error stopping lnd proxy: %v", err)
			returnErr = err
		}
	}

	if g.httpServer != nil {
		if err := g.httpServer.Close(); err != nil {
			log.Errorf("Error stopping UI server: %v", err)
			returnErr = err
		}
	}

	// In case the error wasn't thrown by lnd, make sure we stop it too.
	interceptor.RequestShutdown()

	g.wg.Wait()

	// The lnd error channel is only used if we are actually running lnd in
	// the same process.
	if g.cfg.LndMode == ModeIntegrated {
		err := <-g.lndErrChan
		if err != nil {
			log.Errorf("Error stopping lnd: %v", err)
			returnErr = err
		}
	}

	return returnErr
}

// startMainWebServer creates the main web HTTP server that delegates requests
// between the embedded HTTP server and the RPC proxy. An incoming request will
// go through the following chain of components:
//
//    Request on port 8443       <------------------------------------+
//        |                                 converted gRPC request    |
//        v                                                           |
//    +---+----------------------+ other  +----------------+          |
//    | Main web HTTP server     +------->+ Embedded HTTP  |          |
//    +---+----------------------+____+   +----------------+          |
//        |                           |                               |
//        v any RPC or grpc-web call  |  any REST call                |
//    +---+----------------------+    |->+----------------+           |
//    | grpc-web proxy           |       + grpc-gateway   +-----------+
//    +---+----------------------+       +----------------+
//        |
//        v native gRPC call with basic auth
//    +---+----------------------+
//    | interceptors             |
//    +---+----------------------+
//        |
//        v native gRPC call with macaroon
//    +---+----------------------+
//    | gRPC server              |
//    +---+----------------------+
//        |
//        v unknown authenticated call, gRPC server is just a wrapper
//    +---+----------------------+
//    | director                 |
//    +---+----------------------+
//        |
//        v authenticated call
//    +---+----------------------+ call to lnd or integrated daemon
//    | lnd (remote or local)    +---------------+
//    | faraday remote           |               |
//    | loop remote              |    +----------v----------+
//    | pool remote              |    | lnd local subserver |
//    +--------------------------+    |  - faraday          |
//                                    |  - loop             |
//                                    |  - pool             |
//                                    +---------------------+
//
func (g *LightningTerminal) startMainWebServer() error {
	// Initialize the in-memory file server from the content compiled by
	// the go:embed directive. Since everything's relative to the root dir,
	// we need to create an FS of the sub directory app/build.
	buildDir, err := fs.Sub(appBuildFS, appFilesDir)
	if err != nil {
		return err
	}
	staticFileServer := http.FileServer(&ClientRouteWrapper{
		assets: http.FS(buildDir),
	})

	// Both gRPC (web) and static file requests will come into through the
	// main UI HTTP server. We use this simple switching handler to send the
	// requests to the correct implementation.
	httpHandler := func(resp http.ResponseWriter, req *http.Request) {
		// If this is some kind of gRPC, gRPC Web or REST call that
		// should go to lnd or one of the daemons, pass it to the proxy
		// that handles all those calls.
		if g.rpcProxy.isHandling(resp, req) {
			return
		}

		// REST requests aren't that easy to identify, we have to look
		// at the URL itself. If this is a REST request, we give it
		// directly to our REST handler which will then forward it to
		// us again but converted to a gRPC request.
		if g.cfg.EnableREST && isRESTRequest(req) {
			log.Infof("Handling REST request: %s", req.URL.Path)
			g.restHandler.ServeHTTP(resp, req)

			return
		}

		// If we got here, it's a static file the browser wants, or
		// something we don't know in which case the static file server
		// will answer with a 404.
		log.Infof("Handling static file request: %s", req.URL.Path)

		// Add 1-year cache header for static files. React uses content-
		// based hashes in file names, so when any file is updated, the
		// url will change causing the browser cached version to be
		// invalidated.
		var re = regexp.MustCompile(`^/(static|fonts|icons)/.*`)
		if re.MatchString(req.URL.Path) {
			resp.Header().Set("Cache-Control", "max-age=31536000")
		}

		// Transfer static files using gzip to save up to 70% of
		// bandwidth.
		gzipHandler := makeGzipHandler(staticFileServer.ServeHTTP)
		gzipHandler(resp, req)
	}

	// Create and start our HTTPS server now that will handle both gRPC web
	// and static file requests.
	g.httpServer = &http.Server{
		// To make sure that long-running calls and indefinitely opened
		// streaming connections aren't terminated by the internal
		// proxy, we need to disable all timeouts except the one for
		// reading the HTTP headers. That timeout shouldn't be removed
		// as we would otherwise be prone to the slowloris attack where
		// an attacker takes too long to send the headers and uses up
		// connections that way. Once the headers are read, we either
		// know it's a static resource and can deliver that very cheaply
		// or check the authentication for other calls.
		WriteTimeout:      0,
		IdleTimeout:       0,
		ReadTimeout:       0,
		ReadHeaderTimeout: defaultServerTimeout,
		Handler:           http.HandlerFunc(httpHandler),
	}
	httpListener, err := net.Listen("tcp", g.cfg.HTTPSListen)
	if err != nil {
		return fmt.Errorf("unable to listen on %v: %v",
			g.cfg.HTTPSListen, err)
	}
	tlsConfig, err := buildTLSConfigForHttp2(g.cfg)
	if err != nil {
		return fmt.Errorf("unable to create TLS config: %v", err)
	}
	tlsListener := tls.NewListener(httpListener, tlsConfig)

	g.wg.Add(1)
	go func() {
		defer g.wg.Done()

		log.Infof("Listening for http_tls on: %v", tlsListener.Addr())
		err := g.httpServer.Serve(tlsListener)
		if err != nil && err != http.ErrServerClosed {
			log.Errorf("http_tls server error: %v", err)
		}
	}()

	// We only enable an additional HTTP only listener if the user
	// explicitly sets a value.
	if g.cfg.HTTPListen != "" {
		insecureListener, err := net.Listen("tcp", g.cfg.HTTPListen)
		if err != nil {
			return fmt.Errorf("unable to listen on %v: %v",
				g.cfg.HTTPListen, err)
		}

		g.wg.Add(1)
		go func() {
			defer g.wg.Done()

			log.Infof("Listening for http on: %v",
				insecureListener.Addr())
			err := g.httpServer.Serve(insecureListener)
			if err != nil && err != http.ErrServerClosed {
				log.Errorf("http server error: %v", err)
			}
		}()
	}

	return nil
}

// createRESTProxy creates a grpc-gateway based REST proxy that takes any call
// identified as a REST call, converts it to a gRPC request and forwards it to
// our local main server for further triage/forwarding.
func (g *LightningTerminal) createRESTProxy() error {
	// The default JSON marshaler of the REST proxy only sets OrigName to
	// true, which instructs it to use the same field names as specified in
	// the proto file and not switch to camel case. What we also want is
	// that the marshaler prints all values, even if they are falsey.
	customMarshalerOption := restProxy.WithMarshalerOption(
		restProxy.MIMEWildcard, &restProxy.JSONPb{
			MarshalOptions: protojson.MarshalOptions{
				UseProtoNames:   true,
				EmitUnpopulated: true,
			},
		},
	)

	// For our REST dial options, we increase the max message size that
	// we'll decode to allow clients to hit endpoints which return more data
	// such as the DescribeGraph call. We set this to 200MiB atm. Should be
	// the same value as maxMsgRecvSize in lnd/cmd/lncli/main.go.
	restDialOpts := []grpc.DialOption{
		// We are forwarding the requests directly to the address of our
		// own local listener. To not need to mess with the TLS
		// certificate (which might be tricky if we're using Let's
		// Encrypt), we just skip the certificate verification.
		// Injecting a malicious hostname into the listener address will
		// result in an error on startup so this should be quite safe.
		grpc.WithTransportCredentials(credentials.NewTLS(
			&tls.Config{InsecureSkipVerify: true},
		)),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(1 * 1024 * 1024 * 200),
		),
	}

	// We use our own RPC listener as the destination for our REST proxy.
	// If the listener is set to listen on all interfaces, we replace it
	// with localhost, as we cannot dial it directly.
	restProxyDest := toLocalAddress(g.cfg.HTTPSListen)

	// Now start the REST proxy for our gRPC server above. We'll ensure
	// we direct LND to connect to its loopback address rather than a
	// wildcard to prevent certificate issues when accessing the proxy
	// externally.
	restMux := restProxy.NewServeMux(customMarshalerOption)
	ctx, cancel := context.WithCancel(context.Background())
	g.restCancel = cancel

	// Enable WebSocket and CORS support as well. A request will pass
	// through the following chain:
	// req ---> CORS handler --> WS proxy ---> REST proxy --> gRPC endpoint
	// where gRPC endpoint is our main HTTP(S) listener again.
	restHandler := lnrpc.NewWebSocketProxy(
		restMux, log, g.cfg.Lnd.WSPingInterval, g.cfg.Lnd.WSPongWait,
		lnrpc.LndClientStreamingURIs,
	)
	g.restHandler = allowCORS(restHandler, g.cfg.RestCORS)

	// First register all lnd handlers. This will make it possible to speak
	// REST over the main RPC listener port in both remote and integrated
	// mode. In integrated mode the user can still use the --lnd.restlisten
	// to spin up an extra REST listener that also offers the same
	// functionality, but is no longer required. In remote mode REST will
	// only be enabled on the main HTTP(S) listener.
	for _, registrationFn := range lndRESTRegistrations {
		err := registrationFn(ctx, restMux, restProxyDest, restDialOpts)
		if err != nil {
			return fmt.Errorf("error registering REST handler: %v",
				err)
		}
	}

	// Now register all handlers for faraday, loop and pool.
	err := g.RegisterRestSubserver(
		ctx, restMux, restProxyDest, restDialOpts,
	)
	if err != nil {
		return fmt.Errorf("error registering REST handler: %v", err)
	}

	return nil
}

// bakeSuperMacaroon uses the lnd client to bake a macaroon that can include
// permissions for multiple daemons.
func bakeSuperMacaroon(ctx context.Context, lnd lnrpc.LightningClient,
	rootKeyID uint64, perms []bakery.Op, caveats []macaroon.Caveat) (string,
	error) {

	if lnd == nil {
		return "", errors.New("lnd not yet connected")
	}

	req := &lnrpc.BakeMacaroonRequest{
		Permissions: make(
			[]*lnrpc.MacaroonPermission, len(perms),
		),
		AllowExternalPermissions: true,
		RootKeyId:                rootKeyID,
	}
	for idx, perm := range perms {
		req.Permissions[idx] = &lnrpc.MacaroonPermission{
			Entity: perm.Entity,
			Action: perm.Action,
		}
	}

	res, err := lnd.BakeMacaroon(ctx, req)
	if err != nil {
		return "", err
	}

	macBytes, err := hex.DecodeString(res.Macaroon)
	if err != nil {
		return "", err
	}

	var mac macaroon.Macaroon
	if err := mac.UnmarshalBinary(macBytes); err != nil {
		return "", err
	}

	for _, caveat := range caveats {
		if err := mac.AddFirstPartyCaveat(caveat.Id); err != nil {
			return "", err
		}
	}

	macBytes, err = mac.MarshalBinary()
	if err != nil {
		return "", err
	}

	return hex.EncodeToString(macBytes), err
}

// allowCORS wraps the given http.Handler with a function that adds the
// Access-Control-Allow-Origin header to the response.
func allowCORS(handler http.Handler, origins []string) http.Handler {
	allowHeaders := "Access-Control-Allow-Headers"
	allowMethods := "Access-Control-Allow-Methods"
	allowOrigin := "Access-Control-Allow-Origin"

	// If the user didn't supply any origins that means CORS is disabled
	// and we should return the original handler.
	if len(origins) == 0 {
		return handler
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		// Skip everything if the browser doesn't send the Origin field.
		if origin == "" {
			handler.ServeHTTP(w, r)
			return
		}

		// Set the static header fields first.
		w.Header().Set(
			allowHeaders,
			"Content-Type, Accept, Grpc-Metadata-Macaroon",
		)
		w.Header().Set(allowMethods, "GET, POST, DELETE")

		// Either we allow all origins or the incoming request matches
		// a specific origin in our list of allowed origins.
		for _, allowedOrigin := range origins {
			if allowedOrigin == "*" || origin == allowedOrigin {
				// Only set allowed origin to requested origin.
				w.Header().Set(allowOrigin, origin)

				break
			}
		}

		// For a pre-flight request we only need to send the headers
		// back. No need to call the rest of the chain.
		if r.Method == "OPTIONS" {
			return
		}

		// Everything's prepared now, we can pass the request along the
		// chain of handlers.
		handler.ServeHTTP(w, r)
	})
}

// showStartupInfo shows useful information to the user to easily access the
// web UI that was just started.
func (g *LightningTerminal) showStartupInfo() error {
	info := struct {
		mode    string
		status  string
		alias   string
		version string
		webURI  string
	}{
		mode:    g.cfg.LndMode,
		status:  "locked",
		alias:   g.cfg.Lnd.Alias,
		version: build.Version(),
		webURI: fmt.Sprintf("https://%s", strings.ReplaceAll(
			strings.ReplaceAll(
				g.cfg.HTTPSListen, "0.0.0.0", "localhost",
			), "[::]", "localhost",
		)),
	}

	// In remote mode we try to query the info.
	if g.cfg.LndMode == ModeRemote {
		// We try to query GetInfo on the remote node to find out the
		// alias. But the wallet might be locked.
		host, network, tlsPath, macPath, _ := g.cfg.lndConnectParams()
		basicClient, err := lndclient.NewBasicClient(
			host, tlsPath, path.Dir(macPath), string(network),
			lndclient.MacFilename(path.Base(macPath)),
		)
		if err != nil {
			return fmt.Errorf("error querying remote node: %v", err)
		}

		ctx := context.Background()
		res, err := basicClient.GetInfo(ctx, &lnrpc.GetInfoRequest{})
		if err != nil {
			if !lndclient.IsUnlockError(err) {
				return fmt.Errorf("error querying remote "+
					"node : %v", err)
			}

			// Node is locked.
			info.status = "locked"
			info.alias = "???? (node is locked)"
			info.version = "???? (node is locked)"
		} else {
			info.status = "online"
			info.alias = res.Alias
			info.version = res.Version
		}
	}

	// In integrated mode, we can derive the state from our configuration.
	if g.cfg.LndMode == ModeIntegrated {
		// If the integrated node is running with no seed backup, the
		// wallet cannot be locked and the node is online right away.
		if g.cfg.Lnd.NoSeedBackup {
			info.status = "online"
		}
	}

	// If there's an additional HTTP listener, list it as well.
	listenAddr := g.cfg.HTTPSListen
	if g.cfg.HTTPListen != "" {
		host := toLocalAddress(g.cfg.HTTPListen)
		info.webURI = fmt.Sprintf("%s or http://%s", info.webURI, host)
		listenAddr = fmt.Sprintf("%s, %s", listenAddr, g.cfg.HTTPListen)
	}

	str := "" +
		"----------------------------------------------------------\n" +
		" Lightning Terminal (LiT) by Lightning Labs               \n" +
		"                                                          \n" +
		" Operating mode      %s                                   \n" +
		" Node status         %s                                   \n" +
		" Alias               %s                                   \n" +
		" Version             %s                                   \n" +
		" Web interface       %s (open %s in your browser)         \n" +
		"----------------------------------------------------------\n"
	fmt.Printf(str, info.mode, info.status, info.alias, info.version,
		listenAddr, info.webURI)

	return nil
}

// ClientRouteWrapper is a wrapper around a FileSystem which properly handles
// URL routes that are defined in the client app but unknown to the backend
// http server
type ClientRouteWrapper struct {
	assets http.FileSystem
}

// Open intercepts requests to open files. If the file does not exist and there
// is no file extension, then assume this is a client side route and return the
// contents of index.html
func (i *ClientRouteWrapper) Open(name string) (http.File, error) {
	localName := name

	// The file prefix can be overwritten during build time.
	if appFilesPrefix != "" {
		localName = strings.Replace(name, appFilesPrefix, "/", 1)
	}
	localName = strings.ReplaceAll(localName, "//", "/")
	ret, err := i.assets.Open(localName)
	if !os.IsNotExist(err) || filepath.Ext(localName) != "" {
		return ret, err
	}

	return i.assets.Open("/index.html")
}

// toLocalAddress converts an address that is meant as a wildcard listening
// address ("0.0.0.0" or "[::]") into an address that can be dialed (localhost).
func toLocalAddress(listenerAddress string) string {
	addr := strings.ReplaceAll(listenerAddress, "0.0.0.0", "localhost")
	return strings.ReplaceAll(addr, "[::]", "localhost")
}

// isRESTRequest determines if a request is a REST request by checking that the
// URI starts with /vX/ where X is a single digit number. This is currently true
// for all REST URIs of lnd, faraday, loop and pool as they all either start
// with /v1/ or /v2/.
func isRESTRequest(req *http.Request) bool {
	return patternRESTRequest.MatchString(req.URL.Path)
}
