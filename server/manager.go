package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/gammazero/workerpool"
	"github.com/google/uuid"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/environment/docker"
	"github.com/pterodactyl/wings/internal/networkpolicy"
	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/server/filesystem"
)

type Manager struct {
	mu            sync.RWMutex
	client        remote.Client
	servers       []*Server
	rejectInitial func(context.Context, string, *Configuration, error) error
}

type initialNetworkPolicyError struct {
	cause error
}

func (e *initialNetworkPolicyError) Error() string {
	return "initial network policy rejected: " + e.cause.Error()
}

func (e *initialNetworkPolicyError) Unwrap() error {
	return e.cause
}

// NewManager returns a new server manager instance. This will boot up all the
// servers that are currently present on the filesystem and set them into the
// manager.
func NewManager(ctx context.Context, client remote.Client) (*Manager, error) {
	m := NewEmptyManager(client)
	if err := m.init(ctx); err != nil {
		return nil, err
	}

	return m, nil
}

// NewEmptyManager returns a new empty manager collection without actually
// loading any of the servers from the disk. This allows the caller to set their
// own servers into the collection as needed.
func NewEmptyManager(client remote.Client) *Manager {
	return &Manager{client: client, rejectInitial: rejectInitialNetworkPolicy}
}

// Client returns the HTTP client interface that allows interaction with the
// Panel API.
func (m *Manager) Client() remote.Client {
	return m.client
}

// Len returns the count of servers stored in the manager instance.
func (m *Manager) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return len(m.servers)
}

// Keys returns all of the server UUIDs stored in the manager set.
func (m *Manager) Keys() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	keys := make([]string, len(m.servers))
	for i, s := range m.servers {
		keys[i] = s.ID()
	}

	return keys
}

// Put replaces all the current values in the collection with the value that
// is passed through.
func (m *Manager) Put(s []*Server) {
	m.mu.Lock()
	m.servers = s
	m.mu.Unlock()
}

// All returns all the items in the collection.
func (m *Manager) All() []*Server {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.servers
}

// Add adds an item to the collection store.
func (m *Manager) Add(s *Server) {
	m.mu.Lock()
	m.servers = append(m.servers, s)
	m.mu.Unlock()
}

// Get returns a single server instance and a boolean value indicating if it was
// found in the global collection or not.
func (m *Manager) Get(uuid string) (*Server, bool) {
	match := m.Find(func(server *Server) bool {
		return server.ID() == uuid
	})
	return match, match != nil
}

// Filter returns only those items matching the filter criteria.
func (m *Manager) Filter(filter func(match *Server) bool) []*Server {
	m.mu.RLock()
	defer m.mu.RUnlock()

	r := make([]*Server, 0)
	for _, v := range m.servers {
		if filter(v) {
			r = append(r, v)
		}
	}

	return r
}

// Find returns a single element from the collection matching the filter. If
// nothing is found a nil result is returned.
func (m *Manager) Find(filter func(match *Server) bool) *Server {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, v := range m.servers {
		if filter(v) {
			return v
		}
	}

	return nil
}

// Remove removes all items from the collection that match the filter function.
func (m *Manager) Remove(filter func(match *Server) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	r := make([]*Server, 0)
	for _, v := range m.servers {
		if !filter(v) {
			r = append(r, v)
		}
	}

	m.servers = r
}

// PersistStates writes the current environment states to the disk for each
// server. This is generally called at a specific interval defined in the root
// runner command to avoid hammering disk I/O when tons of server switch states
// at once. It is fine if this file falls slightly out of sync, it is just here
// to make recovering from an unexpected system reboot a little easier.
func (m *Manager) PersistStates() error {
	states := map[string]string{}
	for _, s := range m.All() {
		states[s.ID()] = s.Environment.State()
	}

	data, err := json.Marshal(states)
	if err != nil {
		return errors.WithStack(err)
	}
	if err := os.WriteFile(config.Get().System.GetStatesPath(), data, 0o644); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// ReadStates returns the state of the servers.
func (m *Manager) ReadStates() (map[string]string, error) {
	f, err := os.OpenFile(config.Get().System.GetStatesPath(), os.O_RDONLY|os.O_CREATE, 0o644)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	defer f.Close()

	var states map[string]string
	if err := json.NewDecoder(f).Decode(&states); err != nil && err != io.EOF {
		return nil, errors.WithStack(err)
	}

	out := make(map[string]string, 0)
	// Only return states for servers that we're currently tracking in the system.
	for id, state := range states {
		if _, ok := m.Get(id); ok {
			out[id] = state
		}
	}

	return out, nil
}

// InitServer initializes a server using a data byte array. This will be
// marshaled into the given struct using a YAML marshaler. This will also
// configure the given environment for a server.
func (m *Manager) InitServer(data remote.ServerConfigurationResponse) (*Server, error) {
	return m.initServer("", data)
}

func (m *Manager) initServer(trustedID string, data remote.ServerConfigurationResponse) (*Server, error) {
	s, err := New(m.client)
	if err != nil {
		return nil, err
	}

	success := false
	defer func() {
		if !success {
			s.CtxCancel()
		}
	}()

	// Setup the base server configuration data which will be used for all of the
	// remaining functionality in this call.
	s.syncMu.Lock()
	staged, err := stageConfiguration(data.Settings, trustedID)
	if err == nil {
		s.commitConfiguration(staged, data.ProcessConfiguration)
	}
	s.syncMu.Unlock()
	if err != nil {
		var policyErr *networkPolicyConfigError
		force := errors.As(err, &policyErr)
		if force {
			staged = policyErr.configuration
		}
		failure := m.failInitialPolicy(s.Context(), trustedID, staged, err, force)
		var initialErr *initialNetworkPolicyError
		if errors.As(failure, &initialErr) {
			return nil, initialErr
		}

		return nil, errors.WithStackIf(err)
	}

	s.fs, err = filesystem.New(filepath.Join(config.Get().System.Data, s.ID()), s.DiskSpace(), s.Config().Egg.FileDenylist)
	if err != nil {
		return nil, m.failInitialPolicy(s.Context(), trustedID, staged, errors.WithStackIf(err), staged.NetworkPolicy != nil)
	}

	// Right now we only support a Docker based environment, so I'm going to hard code
	// this logic in. When we're ready to support other environment we'll need to make
	// some modifications here, obviously.
	settings := environment.Settings{
		Mounts:      s.Mounts(),
		Allocations: s.cfg.Allocations,
		Limits:      s.cfg.Build,
		Labels:      s.cfg.Labels,
	}

	envCfg := environment.NewConfiguration(settings, s.GetEnvironmentVariables())
	meta := docker.Metadata{
		Image: s.Config().Container.Image,
	}

	if env, err := docker.New(s.ID(), &meta, envCfg); err != nil {
		return nil, m.failInitialPolicy(s.Context(), trustedID, staged, err, staged.NetworkPolicy != nil)
	} else {
		s.Environment = env
		if err := env.SetNetworkSettings(s.Context(), settings, s.cfg.NetworkPolicy); err != nil {
			s.CtxCancel()
			return nil, &initialNetworkPolicyError{cause: err}
		}
		if trustedID == "" {
			env.MonitorNetwork(s.Context())
			s.StartEventListeners()
		}
	}

	// If the server's data directory exists, force disk usage calculation.
	if _, err := os.Stat(s.Filesystem().Path()); err == nil {
		s.Filesystem().HasSpaceAvailable(true)
	}

	success = true
	return s, nil
}

func (m *Manager) failInitialPolicy(
	ctx context.Context,
	id string,
	c *Configuration,
	cause error,
	force bool,
) error {
	parsed, err := uuid.Parse(id)
	if id == "" || err != nil || parsed.String() != id {
		return cause
	}

	durable, loadErr := networkpolicy.Load(config.Get().System.RootDirectory, id)
	if loadErr != nil {
		force = true
		cause = fmt.Errorf("%w; durable network policy cannot be loaded: %v", cause, loadErr)
	} else if durable.Revision > 0 {
		force = true
	}
	if !force {
		return cause
	}

	reject := m.rejectInitial
	if reject == nil {
		reject = rejectInitialNetworkPolicy
	}
	if err := reject(ctx, id, c, cause); err != nil {
		cause = err
	}
	return &initialNetworkPolicyError{cause: cause}
}

func rejectInitialNetworkPolicy(
	ctx context.Context,
	id string,
	c *Configuration,
	cause error,
) error {
	settings := environment.Settings{
		Allocations: c.Allocations,
		Limits:      c.Build,
		Labels:      c.Labels,
	}
	envCfg := environment.NewConfiguration(settings, nil)
	env, err := docker.New(id, &docker.Metadata{Image: c.Container.Image}, envCfg)
	if err != nil {
		return fmt.Errorf("%w; could not initialize fail-closed environment: %v", cause, err)
	}

	return env.RejectNetworkPolicy(ctx, cause)
}

// initializeFromRemoteSource iterates over a given directory and loads all
// the servers listed before returning them to the calling function.
func (m *Manager) init(ctx context.Context) error {
	log.Info("fetching list of servers from API")
	servers, err := m.client.GetServers(ctx, config.Get().RemoteQuery.BootServersPerPage)
	if err != nil {
		if !remote.IsRequestError(err) {
			return errors.WithStackIf(err)
		}

		return errors.WrapIf(err, "manager: failed to retrieve server configurations")
	}

	start := time.Now()
	log.WithField("total_configs", len(servers)).Info("processing servers returned by the API")

	pool := workerpool.New(runtime.NumCPU())
	fatal := make(chan error, len(servers))
	log.Debugf("using %d workerpools to instantiate server instances", runtime.NumCPU())
	for _, data := range servers {
		data := data
		pool.Submit(func() {
			// Parse the json.RawMessage into an expected struct value. We do this here so that a single broken
			// server does not cause the entire boot process to hang, and allows us to show more useful error
			// messaging in the output.
			d := remote.ServerConfigurationResponse{
				Settings: data.Settings,
			}
			log.WithField("server", data.Uuid).Info("creating new server object from API response")
			if err := json.Unmarshal(data.ProcessConfiguration, &d.ProcessConfiguration); err != nil {
				staged, stageErr := stageConfiguration(data.Settings, data.Uuid)
				force := staged.NetworkPolicy != nil
				var policyErr *networkPolicyConfigError
				if errors.As(stageErr, &policyErr) {
					staged = policyErr.configuration
					force = true
				}
				cause := err
				if stageErr != nil {
					cause = fmt.Errorf("%w; server settings are also invalid: %v", cause, stageErr)
				}
				failure := m.failInitialPolicy(ctx, data.Uuid, staged, cause, force)
				var initialErr *initialNetworkPolicyError
				if errors.As(failure, &initialErr) {
					fatal <- initialErr
				}
				log.WithField("server", data.Uuid).WithField("error", err).Error("failed to parse server configuration from API response, skipping...")
				return
			}

			s, err := m.initServer(data.Uuid, d)
			if err != nil {
				var policyErr *initialNetworkPolicyError
				if errors.As(err, &policyErr) {
					fatal <- policyErr
				}
				log.WithField("server", data.Uuid).WithField("error", err).Error("failed to load server, skipping...")
				return
			}

			m.Add(s)
		})
	}

	// Wait until we've processed all the configuration files in the directory
	// before continuing.
	pool.StopWait()
	select {
	case err := <-fatal:
		for _, server := range m.All() {
			server.CtxCancel()
		}
		return err
	default:
	}

	for _, server := range m.All() {
		if env, ok := server.Environment.(*docker.Environment); ok {
			env.MonitorNetwork(server.Context())
		}
		server.StartEventListeners()
	}

	diff := time.Now().Sub(start)
	log.WithField("duration", fmt.Sprintf("%s", diff)).Info("finished processing server configurations")

	return nil
}
