package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/creasty/defaults"
	"github.com/google/uuid"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/events"
	"github.com/pterodactyl/wings/internal/networkpolicy"
	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/server/filesystem"
	"github.com/pterodactyl/wings/system"
)

// Server is the high level definition for a server instance being controlled
// by Wings.
type Server struct {
	// Internal mutex used to block actions that need to occur sequentially, such as
	// writing the configuration to the disk.
	sync.RWMutex
	ctx       context.Context
	ctxCancel *context.CancelFunc

	emitterLock sync.Mutex
	powerLock   *system.Locker
	syncMu      sync.Mutex

	// Maintains the configuration for the server. This is the data that gets returned by the Panel
	// such as build settings and container images.
	cfg    Configuration
	client remote.Client

	// The crash handler for this server instance.
	crasher CrashHandler

	resources   ResourceUsage
	Environment environment.ProcessEnvironment `json:"-"`

	fs             *filesystem.Filesystem
	fileOperations *FileOperationManager

	// Events emitted by the server instance.
	emitter *events.Bus

	// Defines the process configuration for the server instance. This is dynamically
	// fetched from the Pterodactyl Server instance each time the server process is
	// started, and then cached here.
	procConfig *remote.ProcessConfiguration

	// Tracks the installation process for this server and prevents a server from running
	// two installer processes at the same time. This also allows us to cancel a running
	// installation process, for example when a server is deleted from the panel while the
	// installer process is still running.
	installing   *system.AtomicBool
	transferring *system.AtomicBool
	restoring    *system.AtomicBool

	// The console throttler instance used to control outputs.
	throttler    *ConsoleThrottle
	throttleOnce sync.Once

	// Tracks open websocket connections for the server.
	wsBag       *WebsocketBag
	wsBagLocker sync.Mutex
	sftpBag     *system.ContextBag

	sinks map[system.SinkName]*system.SinkPool
}

// New returns a new server instance with a context and all of the default
// values set on the struct.
func New(client remote.Client) (*Server, error) {
	ctx, cancel := context.WithCancel(context.Background())
	s := Server{
		ctx:          ctx,
		ctxCancel:    &cancel,
		client:       client,
		installing:   system.NewAtomicBool(false),
		transferring: system.NewAtomicBool(false),
		restoring:    system.NewAtomicBool(false),
		powerLock:    system.NewLocker(),
		sinks: map[system.SinkName]*system.SinkPool{
			system.LogSink:     system.NewSinkPool(),
			system.InstallSink: system.NewSinkPool(),
		},
	}
	if err := defaults.Set(&s); err != nil {
		return nil, errors.Wrap(err, "server: could not set default values for struct")
	}
	if err := defaults.Set(&s.cfg); err != nil {
		return nil, errors.Wrap(err, "server: could not set defaults for server configuration")
	}

	s.resources.State = system.NewAtomicString(environment.ProcessOfflineState)
	s.fileOperations = NewFileOperationManager(&s)
	return &s, nil
}

// CleanupForDestroy stops all running background tasks for this server that are
// using the context on the server struct. This will cancel any running install
// processes for the server as well.
func (s *Server) CleanupForDestroy() {
	s.CtxCancel()
	if s.fileOperations != nil {
		s.fileOperations.CancelAll()
	}
	s.Events().Destroy()
	s.DestroyAllSinks()
	s.Websockets().CancelAll()
	s.powerLock.Destroy()
}

// FileOperations returns the server-isolated manager for cancellable filesystem work.
func (s *Server) FileOperations() *FileOperationManager {
	return s.fileOperations
}

// ID returns the UUID for the server instance.
func (s *Server) ID() string {
	return s.Config().GetUuid()
}

// Id returns the UUID for the server instance. This function is deprecated
// in favor of Server.ID().
//
// Deprecated
func (s *Server) Id() string {
	return s.ID()
}

// Cancels the context assigned to this server instance. Assuming background tasks
// are using this server's context for things, all of the background tasks will be
// stopped as a result.
func (s *Server) CtxCancel() {
	if s.ctxCancel != nil {
		(*s.ctxCancel)()
	}
}

// Returns a context instance for the server. This should be used to allow background
// tasks to be canceled if the server is removed. It will only be canceled when the
// application is stopped or if the server gets deleted.
func (s *Server) Context() context.Context {
	return s.ctx
}

// Returns all of the environment variables that should be assigned to a running
// server instance.
func (s *Server) GetEnvironmentVariables() []string {
	out := []string{
		// TODO: allow this to be overridden by the user.
		fmt.Sprintf("TZ=%s", config.Get().System.Timezone),
		fmt.Sprintf("STARTUP=%s", s.Config().Invocation),
		fmt.Sprintf("SERVER_MEMORY=%d", s.MemoryLimit()),
		fmt.Sprintf("SERVER_IP=%s", s.Config().Allocations.DefaultMapping.Ip),
		fmt.Sprintf("SERVER_PORT=%d", s.Config().Allocations.DefaultMapping.Port),
	}

eloop:
	for k := range s.Config().EnvVars {
		// Don't allow any environment variables that we have already set above.
		for _, e := range out {
			if strings.HasPrefix(e, strings.ToUpper(k)+"=") {
				continue eloop
			}
		}

		out = append(out, fmt.Sprintf("%s=%s", strings.ToUpper(k), s.Config().EnvVars.Get(k)))
	}

	return out
}

func (s *Server) Log() *log.Entry {
	return log.WithField("server", s.ID())
}

// Sync syncs the state of the server on the Panel with Wings. This ensures that
// we're always using the state of the server from the Panel and allows us to
// not require successful API calls to Wings to do things.
//
// This also means mass actions can be performed against servers on the Panel
// and they will automatically sync with Wings when the server is started.
func (s *Server) Sync() error {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	id := s.ID()
	cfg, err := s.client.GetServerConfiguration(s.Context(), id)
	if err != nil {
		if err := remote.AsRequestError(err); err != nil && err.StatusCode() == http.StatusNotFound {
			return &serverDoesNotExist{}
		}

		return errors.WithStackIf(err)
	}

	if err := s.syncWithConfiguration(cfg, id); err != nil {
		return errors.WithStackIf(err)
	}

	// Update the disk space limits for the server whenever the configuration for
	// it changes.
	s.fs.SetDiskLimit(s.DiskSpace())

	if err := s.syncWithEnvironment(false); err != nil {
		return err
	}

	// If the server is suspended immediately disconnect all open websocket connections
	// and any connected SFTP clients. We don't need to worry about revoking any JWTs
	// here since they'll be blocked from re-connecting to the websocket anyways. This
	// just forces the client to disconnect and attempt to reconnect (rather than waiting
	// on them to send a message and hit that disconnect logic).
	if s.IsSuspended() {
		s.Websockets().CancelAll()
		s.Sftp().CancelAll()
	}

	return nil
}

// SyncWithConfiguration accepts a configuration object for a server and will
// sync all of the values with the existing server state. If an environment is
// already present, it must accept the staged network policy and allocation
// snapshot before any server configuration is published.
func (s *Server) SyncWithConfiguration(cfg remote.ServerConfigurationResponse) error {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	trustedID := ""
	if s.Environment != nil {
		trustedID = s.ID()
	}
	return s.syncWithConfiguration(cfg, trustedID)
}

type networkPolicyConfigError struct {
	cause         error
	configuration *Configuration
}

func (e *networkPolicyConfigError) Error() string {
	return "invalid network_policy configuration: " + e.cause.Error()
}

func (e *networkPolicyConfigError) Unwrap() error {
	return e.cause
}

func (s *Server) syncWithConfiguration(cfg remote.ServerConfigurationResponse, trustedID string) error {
	c, err := stageConfiguration(cfg.Settings, trustedID)
	if err != nil {
		var policyErr *networkPolicyConfigError
		if errors.As(err, &policyErr) && s.Environment != nil {
			return s.rejectNetworkPolicy(policyErr)
		}

		return errors.WithStackIf(err)
	}

	if s.Environment != nil {
		settings := environment.Settings{
			Mounts:      s.Environment.Config().Mounts(),
			Allocations: c.Allocations,
			Limits:      c.Build,
			Labels:      c.Labels,
		}
		if e, ok := s.Environment.(networkPolicyEnvironment); ok {
			if err := e.SetNetworkSettings(s.Context(), settings, c.NetworkPolicy); err != nil {
				return err
			}
		} else if c.NetworkPolicy != nil {
			return errors.New("network_policy is unsupported by this environment")
		}
	}

	s.commitConfiguration(c, cfg.ProcessConfiguration)
	return nil
}

func stageConfiguration(settings json.RawMessage, trustedID string) (*Configuration, error) {
	c := &Configuration{
		CrashDetectionEnabled: config.Get().System.CrashDetection.CrashDetectionEnabled,
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(settings, &fields); err != nil {
		return c, err
	}

	rawPolicy, hasPolicy := fields["network_policy"]
	if hasPolicy {
		fields["network_policy"] = json.RawMessage("null")
	}
	staged, err := json.Marshal(fields)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(staged, c); err != nil {
		return c, err
	}
	if trustedID != "" {
		trusted, err := uuid.Parse(trustedID)
		if err != nil || trusted.String() != trustedID {
			return c, errors.New("trusted server UUID is not canonical")
		}

		incoming, err := uuid.Parse(c.Uuid)
		if err != nil || incoming.String() != c.Uuid || incoming != trusted {
			return c, errors.New("configuration UUID does not match the trusted server UUID")
		}
	}

	if hasPolicy && !bytes.Equal(bytes.TrimSpace(rawPolicy), []byte("null")) {
		var policy networkpolicy.Policy
		if err := networkpolicy.Decode(rawPolicy, &policy); err != nil {
			return c, &networkPolicyConfigError{cause: err, configuration: c}
		}

		limits := config.Get().Docker.NetworkPolicy
		ceiling := networkpolicy.Bandwidth{Upload: limits.MaxUploadBPS, Download: limits.MaxDownloadBPS}
		if err := policy.Validate(c.Allocations.Mappings, ceiling); err != nil {
			return c, &networkPolicyConfigError{cause: err, configuration: c}
		}

		c.NetworkPolicy = &policy
	}

	return c, nil
}

func (s *Server) rejectNetworkPolicy(policyErr *networkPolicyConfigError) error {
	e, ok := s.Environment.(networkPolicyEnvironment)
	if !ok {
		return policyErr
	}
	if err := e.RejectNetworkPolicy(s.Context(), policyErr); err != nil {
		return err
	}

	return policyErr
}

func (s *Server) commitConfiguration(c *Configuration, process *remote.ProcessConfiguration) {
	s.cfg.mu.Lock()
	defer s.cfg.mu.Unlock()

	// Keep the mutex: readers may already be waiting on it.
	s.cfg.Uuid = c.Uuid
	s.cfg.Meta = c.Meta
	s.cfg.Suspended = c.Suspended
	s.cfg.Invocation = c.Invocation
	s.cfg.SkipEggScripts = c.SkipEggScripts
	s.cfg.EnvVars = c.EnvVars
	s.cfg.Labels = c.Labels
	s.cfg.Allocations = c.Allocations
	s.cfg.NetworkPolicy = c.NetworkPolicy
	s.cfg.Build = c.Build
	s.cfg.CrashDetectionEnabled = c.CrashDetectionEnabled
	s.cfg.Mounts = c.Mounts
	s.cfg.Egg = c.Egg
	s.cfg.Container = c.Container

	s.Lock()
	s.procConfig = process
	s.Unlock()
}

// Reads the log file for a server up to a specified number of bytes.
func (s *Server) ReadLogfile(len int) ([]string, error) {
	return s.Environment.Readlog(len)
}

// Initializes a server instance. This will run through and ensure that the environment
// for the server is setup, and that all of the necessary files are created.
func (s *Server) CreateEnvironment() error {
	// Ensure the data directory exists before getting too far through this process.
	if err := s.EnsureDataDirectoryExists(); err != nil {
		return err
	}

	cfg := config.Get()
	if cfg.System.MachineID.Enable {
		// Hytale wants a machine-id in order to encrypt tokens for the server. So
		// write a machine-id file for the server that contains the server's UUID
		// without any dashes.
		p := filepath.Join(cfg.System.MachineID.Directory, s.ID())
		machineID := append(bytes.ReplaceAll([]byte(s.ID()), []byte{'-'}, []byte{}), '\n')
		if err := os.WriteFile(p, machineID, 0o644); err != nil {
			return fmt.Errorf("failed to write machine-id (at '%s') for server '%s': %w", p, s.ID(), err)
		}
	}

	return s.Environment.Create()
}

// Checks if the server is marked as being suspended or not on the system.
func (s *Server) IsSuspended() bool {
	return s.Config().Suspended
}

func (s *Server) ProcessConfiguration() *remote.ProcessConfiguration {
	s.RLock()
	defer s.RUnlock()

	return s.procConfig
}

// Filesystem returns an instance of the filesystem for this server.
func (s *Server) Filesystem() *filesystem.Filesystem {
	return s.fs
}

// EnsureDataDirectoryExists ensures that the data directory for the server
// instance exists.
func (s *Server) EnsureDataDirectoryExists() error {
	if _, err := os.Lstat(s.fs.Path()); err != nil {
		if os.IsNotExist(err) {
			s.Log().Debug("server: creating root directory and setting permissions")
			if err := os.MkdirAll(s.fs.Path(), 0o700); err != nil {
				return errors.WithStack(err)
			}
			if err := s.fs.Chown("/"); err != nil {
				s.Log().WithField("error", err).Warn("server: failed to chown server data directory")
			}
		} else {
			return errors.WrapIf(err, "server: failed to stat server root directory")
		}
	}
	return nil
}

// OnStateChange sets the state of the server internally. This function handles crash detection as
// well as reporting to event listeners for the server.
func (s *Server) OnStateChange() {
	prevState := s.resources.State.Load()

	st := s.Environment.State()
	// Update the currently tracked state for the server.
	s.resources.State.Store(st)

	// Emit the event to any listeners that are currently registered.
	if prevState != s.Environment.State() {
		s.Log().WithField("status", st).Debug("saw server status change event")
		s.Events().Publish(StatusEvent, st)
	}

	// Reset the resource usage to 0 when the process fully stops so that all the UI
	// views in the Panel correctly display 0.
	if st == environment.ProcessOfflineState {
		s.resources.Reset()
		s.Events().Publish(StatsEvent, s.Proc())
	}

	// If server was in an online state, and is now in an offline state we should handle
	// that as a crash event. In that scenario, check the last crash time, and the crash
	// counter.
	//
	// In the event that we have passed the thresholds, don't do anything, otherwise
	// automatically attempt to start the process back up for the user. This is done in a
	// separate thread as to not block any actions currently taking place in the flow
	// that called this function.
	if (prevState == environment.ProcessStartingState || prevState == environment.ProcessRunningState) &&
		s.Environment.State() == environment.ProcessOfflineState {
		s.Log().Info("detected server as entering a crashed state; running crash handler")

		go func(server *Server) {
			if err := server.handleServerCrash(); err != nil {
				if IsTooFrequentCrashError(err) {
					server.Log().Info("did not restart server after crash; occurred too soon after the last")
				} else {
					s.PublishConsoleOutputFromDaemon("Server crash was detected but an error occurred while handling it.")
					server.Log().WithField("error", err).Error("failed to handle server crash")
				}
			}
		}(s)
	}
}

// IsRunning determines if the server state is running or not. This is different
// from the environment state, it is simply the tracked state from this daemon
// instance, and not the response from Docker.
func (s *Server) IsRunning() bool {
	st := s.Environment.State()

	return st == environment.ProcessRunningState || st == environment.ProcessStartingState
}

// APIResponse is a type returned when requesting details about a single server
// instance on Wings. This includes the information needed by the Panel in order
// to show resource utilization and the current state on this system.
type APIResponse struct {
	State         string        `json:"state"`
	IsSuspended   bool          `json:"is_suspended"`
	Utilization   ResourceUsage `json:"utilization"`
	Configuration Configuration `json:"configuration"`
}

// ToAPIResponse returns the server struct as an API object that can be consumed
// by callers.
func (s *Server) ToAPIResponse() APIResponse {
	return APIResponse{
		State:         s.Environment.State(),
		IsSuspended:   s.IsSuspended(),
		Utilization:   s.Proc(),
		Configuration: *s.Config(),
	}
}
