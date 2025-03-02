// Package sandbox implements the runtime provisioner for runtimes in sandboxed processes.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"

	"github.com/oasisprotocol/oasis-core/go/common"
	cmnBackoff "github.com/oasisprotocol/oasis-core/go/common/backoff"
	"github.com/oasisprotocol/oasis-core/go/common/logging"
	"github.com/oasisprotocol/oasis-core/go/common/node"
	"github.com/oasisprotocol/oasis-core/go/common/pubsub"
	"github.com/oasisprotocol/oasis-core/go/common/version"
	"github.com/oasisprotocol/oasis-core/go/runtime/host"
	"github.com/oasisprotocol/oasis-core/go/runtime/host/protocol"
	"github.com/oasisprotocol/oasis-core/go/runtime/host/sandbox/process"
)

var errRuntimeNotReady = errors.New("runtime is not yet ready")

const (
	runtimeConnectTimeout      = 5 * time.Second
	runtimeInitTimeout         = 1 * time.Second
	runtimeExtendedInitTimeout = 120 * time.Second
	runtimeInterruptTimeout    = 1 * time.Second
	resetTickerTimeout         = 15 * time.Minute

	bindHostSocketPath = "/host.sock"

	ctrlChannelBufferSize = 16
)

// Config contains the sandbox provisioner configuration options.
type Config struct {
	// GetSandboxConfig is a function that generates the sandbox configuration. In case it is not
	// specified a default function is used.
	GetSandboxConfig func(cfg host.Config, socketPath, runtimeDir string) (process.Config, error)

	// HostInfo provides information about the host environment.
	HostInfo *protocol.HostInfo

	// HostInitializer is a function that additionally initializes the runtime host. In case it is
	// not specified a default function is used.
	HostInitializer func(context.Context, *HostInitializerParams) (*host.StartedEvent, error)

	// Logger is an optional logger to use with this provisioner. In case it is not specified a
	// default logger will be created.
	Logger *logging.Logger

	// SandboxBinaryPath is the path to the sandbox support binary.
	SandboxBinaryPath string

	// InsecureNoSandbox disables the sandbox and runs the runtime binary directly.
	InsecureNoSandbox bool
}

// HostInitializerParams contains parameters for the HostInitializer function.
type HostInitializerParams struct {
	Runtime    host.Runtime
	Version    version.Version
	Process    process.Process
	Connection protocol.Connection

	NotifyUpdateCapabilityTEE <-chan struct{}
}

type provisioner struct {
	cfg Config
}

// Implements host.Provisioner.
func (p *provisioner) NewRuntime(cfg host.Config) (host.Runtime, error) {
	id := cfg.Bundle.Manifest.ID

	r := &sandboxedRuntime{
		cfg:                         p.cfg,
		rtCfg:                       cfg,
		id:                          id,
		stopCh:                      make(chan struct{}),
		ctrlCh:                      make(chan interface{}, ctrlChannelBufferSize),
		notifier:                    pubsub.NewBroker(false),
		notifyUpdateCapabilityTEECh: make(chan struct{}, 1),
		logger:                      p.cfg.Logger.With("runtime_id", id),
	}

	return r, nil
}

// Implements host.Provisioner.
func (p *provisioner) Name() string {
	return "sandbox"
}

// abortRequest is a request to the runtime manager goroutine to abort the runtime.
// In case of failures or if force flag is set, the runtime is restarted.
type abortRequest struct {
	ch    chan<- error
	force bool
}

type sandboxedRuntime struct {
	sync.RWMutex

	cfg   Config
	rtCfg host.Config
	id    common.Namespace

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	ctrlCh    chan interface{}

	process  process.Process
	conn     protocol.Connection
	notifier *pubsub.Broker

	notifyUpdateCapabilityTEECh chan struct{}
	capabilityTEE               *node.CapabilityTEE

	rtVersion *version.Version

	logger *logging.Logger
}

// Implements host.Runtime.
func (r *sandboxedRuntime) ID() common.Namespace {
	return r.id
}

// GetInfo implements host.Runtime.
func (r *sandboxedRuntime) GetActiveVersion() (*version.Version, error) {
	r.RLock()
	defer r.RUnlock()

	if r.conn == nil {
		return nil, errRuntimeNotReady
	}
	return r.rtVersion, nil
}

// Implements host.Runtime.
func (r *sandboxedRuntime) GetInfo(ctx context.Context) (*protocol.RuntimeInfoResponse, error) {
	conn, err := r.getConnection(ctx)
	if err != nil {
		return nil, err
	}

	return conn.GetInfo()
}

// Implements host.Runtime.
func (r *sandboxedRuntime) GetCapabilityTEE() (*node.CapabilityTEE, error) {
	r.RLock()
	defer r.RUnlock()

	if r.conn == nil {
		return nil, errRuntimeNotReady
	}
	return r.capabilityTEE, nil
}

// Implements host.Runtime.
func (r *sandboxedRuntime) Call(ctx context.Context, body *protocol.Body) (*protocol.Body, error) {
	conn, err := r.getConnection(ctx)
	if err != nil {
		return nil, err
	}

	// Take care to release lock before calling into the runtime as otherwise this could lead to a
	// deadlock in case the runtime makes a call that acquires the cross node lock and at the same
	// time SetVersion is being called to update the version with the cross node lock acquired.

	return conn.Call(ctx, body)
}

func (r *sandboxedRuntime) getConnection(ctx context.Context) (protocol.Connection, error) {
	var conn protocol.Connection
	getConnFn := func() error {
		r.RLock()
		defer r.RUnlock()

		if r.conn == nil {
			return errRuntimeNotReady
		}
		conn = r.conn

		return nil
	}
	// Retry call in case the runtime is not yet ready.
	err := backoff.Retry(getConnFn, backoff.WithContext(cmnBackoff.NewExponentialBackOff(), ctx))
	if err != nil {
		return nil, err
	}

	return conn, nil
}

// Implements host.Runtime.
func (r *sandboxedRuntime) UpdateCapabilityTEE() {
	select {
	case r.notifyUpdateCapabilityTEECh <- struct{}{}:
	default:
	}
}

// Implements host.Runtime.
func (r *sandboxedRuntime) WatchEvents() (<-chan *host.Event, pubsub.ClosableSubscription) {
	typedCh := make(chan *host.Event)
	sub := r.notifier.Subscribe()
	sub.Unwrap(typedCh)

	return typedCh, sub
}

// Implements host.Runtime.
func (r *sandboxedRuntime) Start() {
	r.startOnce.Do(func() {
		go r.manager()
	})
}

// Implements host.Runtime.
func (r *sandboxedRuntime) Abort(ctx context.Context, force bool) error {
	// Send internal request to the manager goroutine.
	ch := make(chan error, 1)
	select {
	case r.ctrlCh <- &abortRequest{ch: ch, force: force}:
	case <-ctx.Done():
		return ctx.Err()
	}

	// Wait for response from the manager goroutine.
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Implements host.Runtime.
func (r *sandboxedRuntime) Stop() {
	r.stopOnce.Do(func() {
		close(r.stopCh)
	})
}

// Implements host.EmitEvent.
func (r *sandboxedRuntime) EmitEvent(ev *host.Event) {
	r.notifier.Broadcast(ev)
}

func (r *sandboxedRuntime) startProcess() (err error) {
	// Create a temporary directory.
	runtimeDir, err := os.MkdirTemp("", "oasis-runtime")
	if err != nil {
		return fmt.Errorf("failed to create temporary directory: %w", err)
	}
	// We can remove the worker directory after the worker has been started as it
	// has been mounted into the sandbox and is no longer needed.
	defer os.RemoveAll(runtimeDir)

	// Create unix socket.
	hostSocket := filepath.Join(runtimeDir, "host.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: hostSocket})
	if err != nil {
		return fmt.Errorf("failed to create host socket: %w", err)
	}

	// Since we only accept a single connection, we should close the listener
	// in any case.
	defer listener.Close()

	// Create the sandbox as configured.
	var p process.Process
	var ok bool
	defer func() {
		// Make sure the process gets killed in case of errors.
		if !ok && p != nil {
			p.Kill()
		}
	}()

	switch r.cfg.InsecureNoSandbox {
	case true:
		// No sandbox.
		r.logger.Warn("starting an UNSANDBOXED runtime")

		cfg, cErr := r.cfg.GetSandboxConfig(r.rtCfg, hostSocket, runtimeDir)
		if cErr != nil {
			return fmt.Errorf("failed to configure process: %w", cErr)
		}

		p, err = process.NewNaked(cfg)
		if err != nil {
			return fmt.Errorf("failed to spawn process: %w", err)
		}
	case false:
		// With sandbox.
		cfg, cErr := r.cfg.GetSandboxConfig(r.rtCfg, bindHostSocketPath, runtimeDir)
		if cErr != nil {
			return fmt.Errorf("failed to configure sandbox: %w", cErr)
		}

		if cfg.BindRW == nil {
			cfg.BindRW = make(map[string]string)
		}
		cfg.BindRW[hostSocket] = bindHostSocketPath

		p, err = process.NewBubbleWrap(cfg)
		if err != nil {
			return fmt.Errorf("failed to spawn sandbox: %w", err)
		}
	}

	// Wait for the runtime to connect.
	r.logger.Info("waiting for runtime to connect",
		"pid", p.GetPID(),
	)

	// Spawn goroutine that waits for a connection to be established.
	connCh := make(chan interface{})
	go func() {
		lerr := listener.SetDeadline(time.Now().Add(runtimeConnectTimeout))
		if lerr != nil {
			connCh <- lerr
			return
		}
		conn, lerr := listener.Accept()
		if lerr != nil {
			connCh <- lerr
			return
		}

		connCh <- conn
		close(connCh)
	}()

	var conn net.Conn
	select {
	case res := <-connCh:
		// Got a connection or timed out while accepting a connection.
		switch r := res.(type) {
		case error:
			return fmt.Errorf("error while accepting runtime connection: %w", r)
		case net.Conn:
			conn = r
		default:
			panic("invalid type")
		}
	case <-p.Wait():
		// Runtime has terminated before a connection was accepted.
		r.logger.Debug("runtime process exited unexpectedly",
			"pid", p.GetPID(),
			"err", p.Error(),
		)

		return fmt.Errorf("terminated while waiting for runtime to connect")
	}

	// Initialize the connection.
	r.logger.Info("runtime connected",
		"pid", p.GetPID(),
	)

	pc, err := protocol.NewConnection(r.logger, r.id, r.rtCfg.MessageHandler)
	if err != nil {
		return fmt.Errorf("failed to create connection: %w", err)
	}
	defer func() {
		// Make sure the connection gets cleaned up in case of errors.
		if !ok {
			pc.Close()
		}
	}()

	// Create a context that gets cancelled if runtime is stopped.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-ctx.Done():
		case <-r.stopCh:
			cancel()
		}
	}()
	defer cancel()

	// Populate the runtime-specific parts of host information.
	hi := r.cfg.HostInfo.Clone()
	hi.LocalConfig = r.rtCfg.LocalConfig

	// Perform common host initialization.
	var rtVersion *version.Version
	initCtx, cancelInit := context.WithTimeout(ctx, runtimeInitTimeout)
	defer cancelInit()
	if rtVersion, err = pc.InitHost(initCtx, conn, hi); err != nil {
		return fmt.Errorf("failed to initialize connection: %w", err)
	}

	// Make sure the version matches what is configured in the bundle.
	if bndVersion := r.rtCfg.Bundle.Manifest.Version; *rtVersion != bndVersion {
		return fmt.Errorf("version mismatch (runtime reported: %s bundle: %s)", *rtVersion, bndVersion)
	}

	hp := &HostInitializerParams{
		Runtime:                   r,
		Version:                   *rtVersion,
		Process:                   p,
		Connection:                pc,
		NotifyUpdateCapabilityTEE: r.notifyUpdateCapabilityTEECh,
	}

	// Perform configuration-specific host initialization.
	exInitCtx, cancelExInit := context.WithTimeout(ctx, runtimeExtendedInitTimeout)
	defer cancelExInit()
	ev, err := r.cfg.HostInitializer(exInitCtx, hp)
	if err != nil {
		return fmt.Errorf("failed to initialize connection: %w", err)
	}

	ok = true
	r.process = p
	r.Lock()
	r.conn = pc
	r.capabilityTEE = ev.CapabilityTEE
	r.rtVersion = rtVersion
	r.Unlock()

	// Notify subscribers that a runtime has been started.
	r.notifier.Broadcast(&host.Event{Started: ev})

	return nil
}

func (r *sandboxedRuntime) handleAbortRequest(rq *abortRequest) error {
	r.logger.Warn("interrupting runtime")

	// First attempt to gracefully interrupt the runtime by sending a request.
	ctx, cancel := context.WithTimeout(context.Background(), runtimeInterruptTimeout)
	defer cancel()

	response, err := r.conn.Call(ctx, &protocol.Body{RuntimeAbortRequest: &protocol.Empty{}})
	if err == nil && response.RuntimeAbortResponse != nil && !rq.force {
		// Successful response, and no force restart required.
		return nil
	}

	r.logger.Warn("restarting runtime", "force_restart", rq.force, "abort_err", err, "abort_resp", response)

	// Failed to gracefully interrupt the runtime. Kill the runtime and it will be automatically
	// restarted by the manager after it dies.
	r.process.Kill()

	// Wait for the runtime to terminate. We do this here so that the response to the interrupt
	// request is only sent after the new runtime has been respawned and is ready to use.
	select {
	case <-r.process.Wait():
	case <-r.stopCh:
		return context.Canceled
	}

	r.logger.Warn("runtime terminated due to restart request")

	// Remove the process so it will be respanwed (it would be respawned either way, but with an
	// additional "unexpected termination" message).
	r.conn.Close()
	r.process = nil
	r.Lock()
	r.conn = nil
	r.capabilityTEE = nil
	r.rtVersion = nil
	r.Unlock()

	// Notify subscribers that the runtime has stopped.
	r.notifier.Broadcast(&host.Event{Stopped: &host.StoppedEvent{}})

	return nil
}

func (r *sandboxedRuntime) manager() {
	var ticker *backoff.Ticker

	defer func() {
		r.logger.Warn("terminating runtime")

		if ticker != nil {
			ticker.Stop()
			ticker = nil
		}
		if r.process != nil {
			r.conn.Close()
			r.process.Kill()
			<-r.process.Wait()
			r.process = nil

			r.Lock()
			r.conn = nil
			r.capabilityTEE = nil
			r.Unlock()
		}

		// Notify subscribers that the runtime has stopped.
		r.notifier.Broadcast(&host.Event{Stopped: &host.StoppedEvent{}})
	}()

	// Subscribe to own events to make sure the cached CapabilityTEE remains up-to-date.
	evCh, evSub := r.WatchEvents()
	defer evSub.Close()

	var attempt int
	for {
		// Make sure to restart the process if terminated.
		if r.process == nil {
			firstTickCh := make(chan struct{}, 1)
			if ticker == nil {
				// Initialize a ticker for restarting the process. We use a separate channel
				// to restart the process immediately on the first run, as we don't want to wait
				// for the first tick.
				ticker = backoff.NewTicker(cmnBackoff.NewExponentialBackOff())
				firstTickCh <- struct{}{}
				attempt = 0
			}

			select {
			case <-r.stopCh:
				r.logger.Warn("termination requested")
				return
			case <-firstTickCh:
			case <-ticker.C:
			}

			attempt++
			r.logger.Info("starting runtime",
				"attempt", attempt,
			)

			if err := r.startProcess(); err != nil {
				r.logger.Error("failed to start runtime",
					"err", err,
				)

				// Notify subscribers that a runtime has failed to start.
				r.notifier.Broadcast(&host.Event{
					FailedToStart: &host.FailedToStartEvent{
						Error: err,
					},
				})

				continue
			}
		}

		// Wait for either the runtime or the runtime manager to terminate.
		select {
		case grq := <-r.ctrlCh:
			switch rq := grq.(type) {
			case *abortRequest:
				// Request to abort the runtime.
				rq.ch <- r.handleAbortRequest(rq)
				close(rq.ch)
			default:
				r.logger.Error("received unknown request type",
					"request_type", fmt.Sprintf("%T", rq),
				)
			}
		case <-r.stopCh:
			r.logger.Warn("termination requested")
			return
		case <-r.process.Wait():
			// Process has terminated.
			r.logger.Error("runtime process has terminated unexpectedly",
				"err", r.process.Error(),
			)

			r.conn.Close()
			r.process = nil
			r.Lock()
			r.conn = nil
			r.capabilityTEE = nil
			r.rtVersion = nil
			r.Unlock()

			// Notify subscribers that the runtime has stopped.
			r.notifier.Broadcast(&host.Event{Stopped: &host.StoppedEvent{}})
		case <-time.After(resetTickerTimeout):
			// Reset the ticker if things work smoothly. Otherwise, keep on using the old ticker as
			// it can happen that the runtime constantly terminates after a successful start.
			if ticker != nil {
				ticker.Stop()
				ticker = nil
			}
		case ev := <-evCh:
			// Update runtime's CapabilityTEE in case this is an update event.
			if ue := ev.Updated; ue != nil {
				r.Lock()
				r.capabilityTEE = ue.CapabilityTEE
				r.Unlock()
			}
		}
	}
}

// New creates a new runtime provisioner that uses a local process sandbox.
func New(cfg Config) (host.Provisioner, error) {
	// Use a default Logger if none was provided.
	if cfg.Logger == nil {
		cfg.Logger = logging.GetLogger("runtime/host/sandbox")
	}
	// Use a default GetSandboxConfig if none was provided.
	if cfg.GetSandboxConfig == nil {
		cfg.GetSandboxConfig = func(hostCfg host.Config, socketPath, runtimeDir string) (process.Config, error) {
			logWrapper := host.NewRuntimeLogWrapper(
				cfg.Logger,
				"runtime_id", hostCfg.Bundle.Manifest.ID,
				"runtime_name", hostCfg.Bundle.Manifest.Name,
			)
			return process.Config{
				Path: hostCfg.Bundle.Path,
				Env: map[string]string{
					"OASIS_WORKER_HOST": socketPath,
				},
				SandboxBinaryPath: cfg.SandboxBinaryPath,
				Stdout:            logWrapper,
				Stderr:            logWrapper,
			}, nil
		}
	}
	// Make sure host environment information was provided in HostInfo.
	if cfg.HostInfo == nil {
		return nil, fmt.Errorf("no host information provided")
	}
	// Use a default HostInitializer if none was provided.
	if cfg.HostInitializer == nil {
		cfg.HostInitializer = func(ctx context.Context, hp *HostInitializerParams) (*host.StartedEvent, error) {
			return &host.StartedEvent{
				Version: hp.Version,
			}, nil
		}
	}
	return &provisioner{cfg: cfg}, nil
}
