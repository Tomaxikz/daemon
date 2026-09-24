package docker

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net/netip"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"
	"github.com/google/uuid"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/internal/networkpolicy"
)

const networkOwner = "io.pterodactyl.networkstats.server"

var (
	ErrNetworkConflict    = errors.New("network policy revision conflict or restart required")
	ErrNetworkInvalid     = errors.New("invalid network policy")
	ErrNetworkUnsupported = errors.New("network policy backend unavailable")
)

func networkCeiling() networkpolicy.Bandwidth {
	c := config.Get().Docker.NetworkPolicy
	return networkpolicy.Bandwidth{
		Upload:   c.MaxUploadBPS,
		Download: c.MaxDownloadBPS,
	}
}

func (e *Environment) NetworkCapabilities(ctx context.Context) (networkpolicy.Capabilities, error) {
	if err := e.networkSupported(ctx, e.Configuration.Allocations()); err != nil {
		return networkpolicy.Capabilities{APIVersion: 1}, fmt.Errorf("%w: %v", ErrNetworkUnsupported, err)
	}

	return networkpolicy.Capabilities{
		APIVersion: 1,
		Firewall:   true,
		Bandwidth:  true,
	}, nil
}

func (e *Environment) networkSupported(ctx context.Context, allocations environment.Allocations) error {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	c := config.Get()
	if !c.Docker.NetworkPolicy.Enabled {
		return errors.New("administrator has not enabled docker.network_policy")
	}
	if c.System.User.Rootless.Enabled || c.Docker.UsernsMode != "" {
		return errors.New("rootless and user-namespace Docker modes are unsupported")
	}
	if c.Docker.Network.Driver != "bridge" || c.Docker.Network.ISPN || allocations.ForceOutgoingIP {
		return errors.New("requires ordinary Docker bridge networking; ISPN and force_outgoing_ip are unsupported")
	}

	mode := container.NetworkMode(c.Docker.Network.Mode)
	if mode.IsHost() || mode.IsNone() || mode.IsContainer() {
		return errors.New("host, none and shared-container network modes are unsupported")
	}
	if !strings.HasPrefix(e.client.DaemonHost(), "unix://") {
		return errors.New("requires a local Unix Docker socket")
	}
	if len(c.Docker.Network.Dns) == 0 {
		return errors.New("explicit non-loopback Docker DNS servers are required")
	}

	for _, value := range c.Docker.Network.Dns {
		ip, err := netip.ParseAddr(value)
		if err != nil || ip.IsLoopback() || ip.IsUnspecified() || ip.Zone() != "" {
			return errors.New("host-loopback DNS forwarding is unsupported by bridge policies")
		}
	}

	n, err := e.client.NetworkInspect(ctx, mode.NetworkName(), network.InspectOptions{})
	if err != nil {
		return err
	}
	if n.Driver != "bridge" || n.Scope != "local" {
		return errors.New("configured Docker network must be a local bridge")
	}

	info, err := e.client.Info(ctx)
	if err != nil {
		return err
	}

	for _, option := range info.SecurityOptions {
		if strings.Contains(option, "rootless") {
			return errors.New("rootless Docker is unsupported")
		}
	}

	if info.OSType != "linux" {
		return errors.New("requires Linux Docker")
	}
	if name := c.Docker.NetworkPolicy.Runtime; name != "" {
		registered, found := info.Runtimes[name]
		if !found {
			return fmt.Errorf("NSM runtime %q is not registered in Docker", name)
		}
		if err := networkpolicy.ValidateRuntimeRegistration(registered.Path, registered.Args, c.Path()); err != nil {
			return fmt.Errorf("invalid NSM runtime registration: %w", err)
		}
	}

	backend := networkpolicy.Linux()
	if err := backend.Probe(ctx); err != nil {
		return err
	}

	bridge := n.Options["com.docker.network.bridge.name"]
	if bridge == "" && n.Name == "bridge" {
		bridge = "docker0"
	}
	if bridge == "" {
		return errors.New("Docker network does not identify its host bridge interface")
	}

	_, err = backend.Link(ctx, nil, bridge)
	if err != nil {
		return fmt.Errorf("configured Docker bridge is not present in the Wings network namespace: %w", err)
	}

	return nil
}

func (e *Environment) NetworkPolicy(ctx context.Context) (networkpolicy.Status, error) {
	if err := ctx.Err(); err != nil {
		return e.networkStatusSnapshot(), err
	}
	if !e.networkMu.TryLock() {
		return e.networkStatusSnapshot(), nil
	}

	defer e.networkMu.Unlock()

	status := e.networkStatusSnapshot()
	p, err := networkpolicy.Load(config.Get().System.RootDirectory, e.Id)
	if err != nil {
		wasApplied := status.State == "applied"
		e.networkVerified.Store(false)
		status.APIVersion = 1
		status.State = "failed"
		status.Error = err.Error()
		if wasApplied {
			checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()

			c, _ := e.ContainerInspect(checkCtx)
			if closeErr := e.closeNetwork(checkCtx, status.Desired, c); closeErr != nil {
				status.Error += "; fail-closed protection failed: " + closeErr.Error()
			}
		}
		e.storeNetworkStatus(status)
		return status, nil
	}

	if status.State == "" ||
		status.Desired.Revision != p.Revision ||
		status.Desired.PolicyHash != p.PolicyHash {
		e.networkVerified.Store(false)
		status = e.pendingNetworkStatus(p)
		e.storeNetworkStatus(status)
	}
	if status.State != "applied" {
		return status, nil
	}

	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	c, inspectErr := e.ContainerInspect(checkCtx)
	if inspectErr == nil && c.State != nil && c.State.Running && c.ID == e.networkContainerID {
		if err := e.verifyNetwork(checkCtx, p, c); err == nil {
			return status, nil
		} else {
			if interruptedNetworkCheck(err) {
				return e.deferNetworkCheck(p, err), nil
			}
			e.networkVerified.Store(false)
			status.State = "failed"
			status.Error = err.Error()
			if closeErr := e.closeNetwork(checkCtx, p, c); closeErr != nil {
				status.Error += "; fail-closed protection failed: " + closeErr.Error()
			}
			e.storeNetworkStatus(status)
			e.scheduleNetworkReconcile()
			return status, nil
		}
	}

	status.State = "pending"
	e.networkVerified.Store(false)
	if inspectErr == nil && c.State != nil && c.State.Running {
		status.State = "failed"
		status.Error = "applied container identity changed"
		if closeErr := e.closeNetwork(checkCtx, p, c); closeErr != nil {
			status.Error += "; fail-closed protection failed: " + closeErr.Error()
		}
		e.scheduleNetworkReconcile()
	} else if inspectErr != nil && !errdefs.IsNotFound(inspectErr) {
		if interruptedNetworkCheck(inspectErr) {
			return e.deferNetworkCheck(p, inspectErr), nil
		}
		status.State = "failed"
		status.Error = inspectErr.Error()
		if closeErr := e.closeNetwork(checkCtx, p, c); closeErr != nil {
			status.Error += "; fail-closed protection failed: " + closeErr.Error()
		}
		e.scheduleNetworkReconcile()
	}
	e.storeNetworkStatus(status)
	return status, nil
}

func interruptedNetworkCheck(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// A read request cannot decide enforcement from its own canceled deadline.
// Leave the last rules installed and have a bounded independent worker verify them.
func (e *Environment) deferNetworkCheck(p networkpolicy.Policy, err error) networkpolicy.Status {
	e.networkVerified.Store(false)
	status := e.pendingNetworkStatus(p)
	status.Error = "network verification interrupted; retrying: " + err.Error()
	e.storeNetworkStatus(status)
	e.scheduleNetworkReconcile()
	return status
}

func (e *Environment) networkStatusSnapshot() networkpolicy.Status {
	if status := e.networkStatus.Load(); status != nil {
		return *status
	}

	return networkpolicy.Status{APIVersion: 1, State: "pending"}
}

func (e *Environment) pendingNetworkStatus(p networkpolicy.Policy) networkpolicy.Status {
	status := e.networkStatusSnapshot()
	status.APIVersion = 1
	status.State = "pending"
	status.Desired = p
	status.EffectiveBandwidth = p.Effective(networkCeiling())
	status.Error = ""
	return status
}

func (e *Environment) storeNetworkStatus(status networkpolicy.Status) {
	copy := status
	e.networkStatus.Store(&copy)
}

func (e *Environment) beginNetworkChange() error {
	p, err := networkpolicy.Load(config.Get().System.RootDirectory, e.Id)
	if err != nil {
		e.failNetworkStatus(p, err)
		return err
	}

	e.networkVerified.Store(false)
	e.storeNetworkStatus(e.pendingNetworkStatus(p))
	return nil
}

func (e *Environment) UpdateNetworkPolicy(ctx context.Context, p networkpolicy.Policy) (networkpolicy.Status, error) {
	if err := e.lockNetwork(ctx); err != nil {
		return e.networkStatusSnapshot(), err
	}

	defer e.networkMu.Unlock()

	if err := ctx.Err(); err != nil {
		return e.networkStatusSnapshot(), err
	}
	if e.networkDeleted {
		return e.networkStatusSnapshot(), ErrNetworkConflict
	}

	old, err := networkpolicy.Load(config.Get().System.RootDirectory, e.Id)
	if err != nil {
		return e.failNetworkStatus(old, err), err
	}
	if err := e.validatePolicyUpdate(old, p, e.Configuration.Allocations().Mappings); err != nil {
		return e.networkStatusSnapshot(), err
	}
	if err := e.checkPolicySupport(ctx, old, p, e.Configuration.Allocations()); err != nil {
		return e.networkStatusSnapshot(), err
	}
	if err := ctx.Err(); err != nil {
		return e.networkStatusSnapshot(), err
	}
	if old.Revision == p.Revision && old.PolicyHash == p.PolicyHash && e.networkVerified.Load() {
		c, inspectErr := e.ContainerInspect(ctx)
		if inspectErr == nil && c.State != nil && c.State.Running && c.ID == e.networkContainerID {
			if err := e.verifyNetwork(ctx, p, c); err == nil {
				return e.networkStatusSnapshot(), nil
			}
		}
		e.networkVerified.Store(false)
	}

	e.networkVerified.Store(false)
	e.storeNetworkStatus(e.pendingNetworkStatus(p))
	if err := e.saveNetworkPolicy(ctx, p, old.Revision != p.Revision || old.PolicyHash != p.PolicyHash); err != nil {
		// A rename can succeed before directory fsync fails. Reconcile the
		// visible policy without claiming that the write was durable.
		if applyErr := e.reconcileNetwork(ctx); applyErr != nil {
			err = fmt.Errorf("%w; reconciliation also failed: %w", err, applyErr)
		}
		return e.failNetworkStatus(p, err), err
	}

	e.scheduleNetworkReconcile()
	return e.networkStatusSnapshot(), nil
}

func (e *Environment) saveNetworkPolicy(ctx context.Context, p networkpolicy.Policy, changed bool) error {
	cfg := config.Get()
	if cfg.Docker.NetworkPolicy.Runtime != "" {
		lease, err := networkpolicy.LockRuntime(ctx, cfg.System.RootDirectory, e.Id)
		if err != nil {
			return err
		}
		defer lease.Close()
	}
	if changed {
		if err := networkpolicy.Save(cfg.System.RootDirectory, e.Id, p); err != nil {
			return err
		}
	}
	if cfg.Docker.NetworkPolicy.Runtime != "" {
		return networkpolicy.SyncRuntimeBinding(cfg.System.RootDirectory, e.Id, p)
	}
	return nil
}

func (e *Environment) lockNetwork(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.networkMu.TryLock() {
			if err := ctx.Err(); err != nil {
				e.networkMu.Unlock()
				return err
			}
			return nil
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// scheduleNetworkReconcile is called while networkMu is held. Successful owners
// clear their marker under that lock; the monitor retries timed-out lock waits.
func (e *Environment) scheduleNetworkReconcile() {
	if !e.networkReconcile.CompareAndSwap(false, true) {
		return
	}

	go func() {
		waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		e.reconcilePendingNetwork(waitCtx)
	}()
}

func (e *Environment) reconcilePendingNetwork(waitCtx context.Context) {
	if err := e.lockNetwork(waitCtx); err != nil {
		// Desired state is durable; the monitor retries if a long power action
		// outlasted this wait. Never run enforcement with an expired context.
		e.networkReconcile.Store(false)
		return
	}
	defer e.networkMu.Unlock()
	defer e.networkReconcile.Store(false)
	if e.networkDeleted {
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(waitCtx), 30*time.Second)
	defer cancel()
	checkCtx, checkCancel := context.WithTimeout(ctx, 10*time.Second)
	err := e.verifyRecordedNetwork(checkCtx)
	checkCancel()
	if err == nil {
		return
	}
	_ = e.reconcileNetwork(ctx)
}

func (e *Environment) closeNetwork(
	ctx context.Context,
	p networkpolicy.Policy,
	c container.InspectResponse,
) error {
	if !e.trustedServerContainer(c) {
		return errors.New("refusing to quarantine a container without the Wings server identity")
	}
	gateCtx, gateCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer gateCancel()
	if e.networkManaged(c) {
		if err := e.gateNetwork(gateCtx, p); err == nil {
			return nil
		}
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := e.client.ContainerKill(stopCtx, e.Id, "SIGKILL"); err != nil && !errdefs.IsNotFound(err) {
		return err
	}

	e.SetState(environment.ProcessStoppingState)
	return nil
}

func (e *Environment) trustedServerContainer(c container.InspectResponse) bool {
	return c.Config != nil &&
		c.Config.Labels["Service"] == "Pterodactyl" &&
		c.Config.Labels["ContainerType"] == "server_process" &&
		strings.TrimPrefix(c.Name, "/") == e.Id
}

func (e *Environment) gateNetwork(ctx context.Context, p networkpolicy.Policy) error {
	lease, err := e.runtimeLease(ctx)
	if err != nil {
		return err
	}
	if lease != nil {
		defer lease.Close()
	}
	backend := networkpolicy.Linux()
	if err := backend.Firewall(ctx, e.Id, p, nil, true); err != nil {
		return err
	}
	return backend.VerifyFirewall(ctx, e.Id, p, nil, true)
}

func (e *Environment) validatePolicyUpdate(old, next networkpolicy.Policy, owned map[string][]int) error {
	if err := networkpolicy.ValidateEnforcement(next, owned, networkCeiling()); err != nil {
		return fmt.Errorf("%w: %w", ErrNetworkInvalid, err)
	}
	if next.Revision < old.Revision {
		return ErrNetworkConflict
	}
	if next.Revision == old.Revision && next.PolicyHash != old.PolicyHash && !next.SameRules(old) {
		return fmt.Errorf("%w: policy rules changed without a newer revision", ErrNetworkConflict)
	}

	return nil
}

func (e *Environment) checkPolicySupport(
	ctx context.Context,
	old, next networkpolicy.Policy,
	allocations environment.Allocations,
) error {
	if !config.Get().Docker.NetworkPolicy.Enabled {
		return fmt.Errorf("%w: docker.network_policy.enabled is false", ErrNetworkUnsupported)
	}

	c, err := e.ContainerInspect(ctx)
	if err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	if err == nil &&
		c.State != nil &&
		c.State.Running &&
		next.Required(networkCeiling()) &&
		!e.networkManaged(c) {
		return fmt.Errorf("%w: stop the server before enabling a policy", ErrNetworkConflict)
	}
	if next.Required(networkCeiling()) ||
		old.Required(networkCeiling()) ||
		(err == nil && e.networkManaged(c)) {
		if err := e.networkSupported(ctx, allocations); err != nil {
			return fmt.Errorf("%w: %w", ErrNetworkUnsupported, err)
		}
		if err := networkpolicy.Linux().CheckPolicy(ctx, e.Id, next, allocations.Mappings, networkCeiling()); err != nil {
			return fmt.Errorf("%w: %w", ErrNetworkUnsupported, err)
		}
	}
	return nil
}

func (e *Environment) failNetworkStatus(p networkpolicy.Policy, err error) networkpolicy.Status {
	e.networkVerified.Store(false)
	status := e.pendingNetworkStatus(p)
	status.State = "failed"
	status.Error = err.Error()
	e.storeNetworkStatus(status)
	return status
}

// RejectNetworkPolicy fails closed after configuration staging rejects an NSM
// policy. It retains both the current settings and the durable desired policy.
func (e *Environment) RejectNetworkPolicy(ctx context.Context, cause error) error {
	if cause == nil {
		cause = ErrNetworkInvalid
	}
	if err := e.lockNetwork(ctx); err != nil {
		return fmt.Errorf("%w: could not serialize fail-closed handling: %v", cause, err)
	}

	defer e.networkMu.Unlock()

	if e.networkDeleted {
		return fmt.Errorf("%w: %w", cause, ErrNetworkConflict)
	}

	p, err := networkpolicy.Load(config.Get().System.RootDirectory, e.Id)
	if err != nil {
		cause = fmt.Errorf("%w; could not load durable network policy: %v", cause, err)
	}
	status := e.failNetworkStatus(p, cause)
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	c, inspectErr := e.ContainerInspect(checkCtx)
	if errdefs.IsNotFound(inspectErr) || (inspectErr == nil && (c.State == nil || !c.State.Running)) {
		return cause
	}
	if inspectErr != nil {
		status.Error += "; could not inspect container for fail-closed handling: " + inspectErr.Error()
		e.storeNetworkStatus(status)
		c = container.InspectResponse{}
	}
	if err := e.closeNetwork(checkCtx, p, c); err != nil {
		status.Error += "; fail-closed protection failed: " + err.Error()
		e.storeNetworkStatus(status)
		return fmt.Errorf("%w; fail-closed protection failed: %v", cause, err)
	}

	return cause
}

// MonitorNetwork starts one verifier tied to the server's lifetime.
func (e *Environment) MonitorNetwork(ctx context.Context) {
	e.monitorNetwork(ctx, 30*time.Second)
}

func (e *Environment) monitorNetwork(ctx context.Context, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	e.networkMu.Lock()
	if e.networkDeleted || !e.networkMonitor.CompareAndSwap(false, true) {
		e.networkMu.Unlock()
		close(done)
		return done
	}

	monitorCtx, cancel := context.WithCancel(ctx)
	e.networkMonitorStop = cancel
	e.networkMu.Unlock()

	go func() {
		defer close(done)
		defer e.networkMonitor.Store(false)

		// Spread initial checks across servers after a daemon restart. Reset
		// after each check so slow commands cannot create a catch-up loop.
		timer := time.NewTimer(networkCheckDelay(e.Id, interval))
		defer timer.Stop()

		for {
			select {
			case <-monitorCtx.Done():
				return
			case <-timer.C:
				status := e.networkStatusSnapshot()
				if status.Desired.Revision == 0 && !status.Desired.Required(networkCeiling()) {
					timer.Reset(interval)
					continue
				}
				checkCtx, checkCancel := context.WithTimeout(monitorCtx, 10*time.Second)
				if status.State == "applied" {
					_, _ = e.NetworkPolicy(checkCtx)
				} else if e.lockNetwork(checkCtx) == nil {
					if !e.networkDeleted {
						e.scheduleNetworkReconcile()
					}
					e.networkMu.Unlock()
				}
				checkCancel()
				timer.Reset(interval)
			}
		}
	}()
	return done
}

func networkCheckDelay(id string, interval time.Duration) time.Duration {
	if interval < 2 {
		return interval
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(id))
	half := interval / 2
	return half + time.Duration(hash.Sum64()%uint64(interval-half))
}

func (e *Environment) networkManaged(c container.InspectResponse) bool {
	return c.Config != nil && c.Config.Labels[networkOwner] == e.Id
}

// Called under networkMu by creation/start. A dedicated bridge lets us install
// a drop gate before Docker creates a veth or publishes the existing allocations.
func (e *Environment) prepareNetwork(
	ctx context.Context,
	conf *container.Config,
	host *container.HostConfig,
) error {
	if e.networkDeleted {
		return ErrNetworkConflict
	}

	p, err := networkpolicy.Load(config.Get().System.RootDirectory, e.Id)
	if err != nil {
		return err
	}

	delete(conf.Labels, networkOwner)
	defaults := networkpolicy.Empty()
	if err := networkpolicy.ValidateEnforcement(defaults, nil, networkCeiling()); err != nil {
		return err
	}
	if !p.Required(networkCeiling()) {
		return nil
	}
	if p.Revision > 0 {
		if err := networkpolicy.ValidateEnforcement(p, e.Configuration.Allocations().Mappings, networkCeiling()); err != nil {
			return err
		}
	}
	if err := e.networkSupported(ctx, e.Configuration.Allocations()); err != nil {
		return err
	}

	name, bridge, _, err := networkpolicy.Names(e.Id)
	if err != nil {
		return err
	}

	n, err := e.client.NetworkInspect(ctx, name, network.InspectOptions{})
	if errdefs.IsNotFound(err) {
		// Never adopt an administrator's bridge, even if Docker doesn't know it.
		_, linkErr := networkpolicy.Linux().Link(ctx, nil, bridge)
		if linkErr == nil {
			return errors.New("NSM bridge name is already in use")
		}
		if !errors.Is(linkErr, syscall.ENODEV) && !errors.Is(linkErr, syscall.ENOENT) {
			return linkErr
		}

		ipv6 := config.Get().Docker.NetworkPolicy.IPv6
		_, err = e.client.NetworkCreate(ctx, name, network.CreateOptions{
			Driver:     "bridge",
			EnableIPv6: &ipv6,
			Labels:     map[string]string{networkOwner: e.Id},
			Options: map[string]string{
				"com.docker.network.bridge.name":       bridge,
				"com.docker.network.bridge.enable_icc": "false",
			},
		})
		if err == nil {
			n, err = e.client.NetworkInspect(ctx, name, network.InspectOptions{})
		}
	}
	if err != nil {
		return err
	}
	if err := e.checkNetwork(n, ""); err != nil {
		return err
	}

	if err := e.gateNetwork(ctx, p); err != nil {
		return err
	}

	conf.Labels[networkOwner] = e.Id
	host.NetworkMode = container.NetworkMode(name)
	if runtime := config.Get().Docker.NetworkPolicy.Runtime; runtime != "" {
		host.Runtime = runtime
		if host.Annotations == nil {
			host.Annotations = make(map[string]string)
		}
		host.Annotations[networkpolicy.RuntimeServerKey] = e.Id
		host.Annotations[networkpolicy.RuntimeGenerationKey] = uuid.NewString()
	}
	// Keep these off even if future Wings defaults grant them: a tenant must
	// not be able to alter its qdisc or forge bridge-layer control packets.
	host.CapDrop = append(host.CapDrop, "NET_ADMIN", "SYS_ADMIN", "NET_RAW")
	return nil
}

func (e *Environment) bindNetworkRuntime(ctx context.Context, containerID string, host *container.HostConfig) error {
	cfg := config.Get()
	if cfg.Docker.NetworkPolicy.Runtime == "" {
		return networkpolicy.RemoveRuntimeBinding(cfg.System.RootDirectory, e.Id)
	}
	lease, err := networkpolicy.LockRuntime(ctx, cfg.System.RootDirectory, e.Id)
	if err != nil {
		return err
	}
	defer lease.Close()
	if host.Annotations[networkpolicy.RuntimeServerKey] != e.Id {
		return networkpolicy.RemoveRuntimeBinding(cfg.System.RootDirectory, e.Id)
	}
	p, err := networkpolicy.Load(cfg.System.RootDirectory, e.Id)
	if err != nil {
		return err
	}
	owned := e.Configuration.Allocations().Mappings
	if owned == nil {
		owned = make(map[string][]int)
	}
	return networkpolicy.SaveRuntimeBinding(cfg.System.RootDirectory, networkpolicy.RuntimeBinding{
		Version:     1,
		Server:      e.Id,
		Container:   containerID,
		Generation:  host.Annotations[networkpolicy.RuntimeGenerationKey],
		Runtime:     host.Runtime,
		Allocations: owned,
		Revision:    p.Revision,
		PolicyHash:  p.PolicyHash,
	})
}

func (e *Environment) runtimeLease(ctx context.Context) (*os.File, error) {
	cfg := config.Get()
	if cfg.Docker.NetworkPolicy.Runtime == "" {
		return nil, nil
	}
	return networkpolicy.LockRuntime(ctx, cfg.System.RootDirectory, e.Id)
}

func (e *Environment) verifyRuntimeContainer(p networkpolicy.Policy, c container.InspectResponse) error {
	cfg := config.Get()
	if cfg.Docker.NetworkPolicy.Runtime == "" {
		return nil
	}
	if c.HostConfig == nil || c.HostConfig.Runtime != cfg.Docker.NetworkPolicy.Runtime || c.HostConfig.Annotations[networkpolicy.RuntimeServerKey] != e.Id {
		return errors.New("runtime mode requires recreating the stopped server through Wings")
	}
	binding, err := networkpolicy.LoadRuntimeBinding(cfg.System.RootDirectory, e.Id)
	if err != nil {
		return err
	}
	if binding.Container != c.ID || binding.Generation != c.HostConfig.Annotations[networkpolicy.RuntimeGenerationKey] || binding.Runtime != c.HostConfig.Runtime || binding.Revision != p.Revision || binding.PolicyHash != p.PolicyHash {
		return errors.New("container generation or saved runtime policy identity changed")
	}
	return p.Validate(binding.Allocations, networkCeiling())
}

func (e *Environment) finishNetworkStart(ctx context.Context) error {
	if config.Get().Docker.NetworkPolicy.Runtime == "" {
		return e.reconcileNetwork(ctx)
	}
	p, err := networkpolicy.Load(config.Get().System.RootDirectory, e.Id)
	if err != nil {
		return e.failSyncedPolicy(ctx, p, err)
	}
	c, err := e.ContainerInspect(ctx)
	if err != nil {
		return e.failSyncedPolicy(ctx, p, err)
	}
	if !p.Required(networkCeiling()) && !e.networkManaged(c) {
		return nil
	}
	if err := e.verifyNetwork(ctx, p, c); err != nil {
		return e.failSyncedPolicy(ctx, p, err)
	}
	e.storeNetworkStatus(e.appliedNetworkStatus(p, c.ID))
	return nil
}

func (e *Environment) checkNetwork(n network.Inspect, containerID string) error {
	name, bridge, _, err := networkpolicy.Names(e.Id)
	if err != nil {
		return err
	}
	if n.Name != name ||
		n.Driver != "bridge" ||
		n.Scope != "local" ||
		n.Internal ||
		n.Ingress ||
		n.Labels[networkOwner] != e.Id ||
		n.Options["com.docker.network.bridge.name"] != bridge {
		return errors.New("network is not this server's NSM-owned local Docker bridge")
	}
	if n.EnableIPv6 != config.Get().Docker.NetworkPolicy.IPv6 {
		return errors.New("network IPv6 setting changed; remove the stopped server's old NSM network before restarting")
	}

	for id := range n.Containers {
		if containerID == "" || id != containerID {
			return errors.New("NSM network contains another container")
		}
	}

	return nil
}

func (e *Environment) ReconcileNetwork(ctx context.Context) error {
	if err := e.lockNetwork(ctx); err != nil {
		return err
	}
	defer e.networkMu.Unlock()
	workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return e.reconcileNetwork(workCtx)
}

func (e *Environment) reconcileNetwork(ctx context.Context) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.networkDeleted {
		return ErrNetworkConflict
	}

	p, err := networkpolicy.Load(config.Get().System.RootDirectory, e.Id)
	protect := p.Required(networkCeiling()) || err != nil
	e.networkVerified.Store(false)
	e.storeNetworkStatus(e.pendingNetworkStatus(p))
	defer func() {
		if result == nil {
			return
		}

		status := e.failNetworkStatus(p, result)
		if !protect {
			return
		}

		// Keep a successfully installed gate closed. If even the backend is
		// unavailable, quarantine only the verified Wings-owned container.
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		c, inspectErr := e.ContainerInspect(stopCtx)
		if errdefs.IsNotFound(inspectErr) || (inspectErr == nil && (c.State == nil || !c.State.Running)) {
			return
		}
		if inspectErr != nil {
			status.Error += "; could not inspect container for quarantine: " + inspectErr.Error()
			e.storeNetworkStatus(status)
			return
		}
		if closeErr := e.closeNetwork(stopCtx, p, c); closeErr != nil {
			status.Error += "; could not quarantine container: " + closeErr.Error()
			e.storeNetworkStatus(status)
		}
	}()

	if err != nil {
		return err
	}

	defaults := networkpolicy.Empty()
	if err := networkpolicy.ValidateEnforcement(defaults, nil, networkCeiling()); err != nil {
		return err
	}

	c, err := e.ContainerInspect(ctx)
	if errdefs.IsNotFound(err) || (err == nil && (c.State == nil || !c.State.Running)) {
		return nil
	}
	if err != nil {
		return err
	}

	protect = protect || e.networkManaged(c)
	if !p.Required(networkCeiling()) && !e.networkManaged(c) {
		if p.Revision > 0 {
			e.storeNetworkStatus(e.appliedNetworkStatus(p, c.ID))
		}
		return nil
	}
	if err := e.networkSupported(ctx, e.Configuration.Allocations()); err != nil {
		return err
	}
	if !e.networkManaged(c) {
		return errors.New("policy requires a restart onto an isolated NSM bridge")
	}

	name, bridge, _, err := networkpolicy.Names(e.Id)
	if err != nil {
		return err
	}

	n, err := e.client.NetworkInspect(ctx, name, network.InspectOptions{})
	if err != nil {
		return err
	}
	if err := e.checkNetwork(n, c.ID); err != nil {
		return err
	}

	b := networkpolicy.Linux()
	lease, err := e.runtimeLease(ctx)
	if err != nil {
		return err
	}
	if lease != nil {
		defer lease.Close()
	}
	if err := e.verifyRuntimeContainer(p, c); err != nil {
		return err
	}
	if err := b.Firewall(ctx, e.Id, p, nil, true); err != nil {
		return err
	}
	if c.HostConfig == nil ||
		c.NetworkSettings == nil ||
		string(c.HostConfig.NetworkMode) != name ||
		c.HostConfig.Privileged ||
		len(c.HostConfig.CapAdd) != 0 ||
		len(c.NetworkSettings.Networks) != 1 ||
		(c.HostConfig.RestartPolicy.Name != "" && c.HostConfig.RestartPolicy.Name != "no") {
		return errors.New("unsupported container privileges or network attachments")
	}

	allocations := e.Configuration.Allocations()
	if !samePortBindings(c.HostConfig.PortBindings, allocations.DockerBindings()) {
		return errors.New("allocations changed; restart the server before applying its policy")
	}
	if p.Revision > 0 {
		if err := networkpolicy.ValidateEnforcement(p, allocations.Mappings, networkCeiling()); err != nil {
			return err
		}
	}
	endpoint := c.NetworkSettings.Networks[name]
	if endpoint == nil {
		return errors.New("NSM network endpoint is missing")
	}

	ns, err := networkpolicy.OpenNamespace(c.State.Pid, c.NetworkSettings.SandboxKey)
	if err != nil {
		return err
	}

	defer func() {
		_ = ns.Close()
	}()

	hostLinks, err := b.Links(ctx, nil, bridge)
	if err != nil {
		return err
	}

	insideLinks, err := b.Links(ctx, ns, "")
	if err != nil {
		return err
	}
	if len(hostLinks) != 1 {
		return errors.New("expected exactly one veth on the server bridge")
	}

	var inside networkpolicy.Link
	for _, link := range insideLinks {
		if link.Name == "lo" {
			continue
		}
		if inside.Name != "" {
			return errors.New("multiple container network interfaces are unsupported")
		}

		inside = link
	}

	if inside.Name == "" ||
		inside.Peer != hostLinks[0].Index ||
		hostLinks[0].Peer != inside.Index ||
		!strings.EqualFold(inside.Address, endpoint.MacAddress) {
		return errors.New("container interface does not match the Docker-owned host veth")
	}

	rate := p.Effective(networkCeiling())
	if err := b.Shape(ctx, ns, inside.Name, rate.Upload, inside.MTU); err != nil {
		return err
	}
	if err := b.VerifyShape(ctx, ns, inside.Name, rate.Upload, inside.MTU); err != nil {
		return err
	}
	if err := b.Shape(ctx, nil, hostLinks[0].Name, rate.Download, hostLinks[0].MTU); err != nil {
		return err
	}
	if err := b.VerifyShape(ctx, nil, hostLinks[0].Name, rate.Download, hostLinks[0].MTU); err != nil {
		return err
	}
	if err := b.Firewall(ctx, e.Id, p, allocations.Mappings, false); err != nil {
		return err
	}
	if err := b.VerifyFirewall(ctx, e.Id, p, allocations.Mappings, false); err != nil {
		return err
	}

	e.storeNetworkStatus(e.appliedNetworkStatus(p, c.ID))
	return nil
}

func (e *Environment) appliedNetworkStatus(p networkpolicy.Policy, containerID string) networkpolicy.Status {
	e.networkContainerID = containerID
	e.networkVerified.Store(true)
	if p.Revision == 0 || p.PolicyHash == "" {
		return e.pendingNetworkStatus(p)
	}

	hash := p.PolicyHash
	status := networkpolicy.Status{
		APIVersion:         1,
		State:              "applied",
		AppliedRevision:    p.Revision,
		AppliedHash:        &hash,
		Desired:            p,
		EffectiveBandwidth: p.Effective(networkCeiling()),
	}
	return status
}

func (e *Environment) verifyRecordedNetwork(ctx context.Context) error {
	p, err := networkpolicy.Load(config.Get().System.RootDirectory, e.Id)
	if err != nil {
		return err
	}

	c, err := e.ContainerInspect(ctx)
	if err != nil {
		return err
	}
	if c.State == nil || !c.State.Running || c.ID != e.networkContainerID {
		return errors.New("verified container is no longer running")
	}

	if err := e.verifyNetwork(ctx, p, c); err != nil {
		return err
	}
	e.storeNetworkStatus(e.appliedNetworkStatus(p, c.ID))
	return nil
}

func (e *Environment) verifyNetwork(
	ctx context.Context,
	p networkpolicy.Policy,
	c container.InspectResponse,
) error {
	allocations := e.Configuration.Allocations()
	ceiling := networkCeiling()
	// Firewall readback below handles compilation and backend validation.
	if p.Revision > 0 {
		if err := p.Validate(allocations.Mappings, ceiling); err != nil {
			return err
		}
	} else if err := p.Validate(nil, ceiling); err != nil {
		return err
	}
	if !p.Required(ceiling) && !e.networkManaged(c) {
		return nil
	}
	if !e.networkManaged(c) {
		return errors.New("policy requires an isolated NSM bridge")
	}

	name, bridge, _, err := networkpolicy.Names(e.Id)
	if err != nil {
		return err
	}

	n, err := e.client.NetworkInspect(ctx, name, network.InspectOptions{})
	if err != nil {
		return err
	}
	if err := e.checkNetwork(n, c.ID); err != nil {
		return err
	}
	if c.State == nil || !c.State.Running ||
		c.HostConfig == nil ||
		c.NetworkSettings == nil ||
		string(c.HostConfig.NetworkMode) != name ||
		c.HostConfig.Privileged ||
		len(c.HostConfig.CapAdd) != 0 ||
		len(c.NetworkSettings.Networks) != 1 ||
		(c.HostConfig.RestartPolicy.Name != "" && c.HostConfig.RestartPolicy.Name != "no") ||
		!samePortBindings(c.HostConfig.PortBindings, allocations.DockerBindings()) {
		return errors.New("container network state no longer matches its policy")
	}

	endpoint := c.NetworkSettings.Networks[name]
	if endpoint == nil {
		return errors.New("NSM network endpoint is missing")
	}
	lease, err := e.runtimeLease(ctx)
	if err != nil {
		return err
	}
	if lease != nil {
		defer lease.Close()
	}
	if err := e.verifyRuntimeContainer(p, c); err != nil {
		return err
	}

	ns, err := networkpolicy.OpenNamespace(c.State.Pid, c.NetworkSettings.SandboxKey)
	if err != nil {
		return err
	}

	defer func() {
		_ = ns.Close()
	}()

	b := networkpolicy.Linux()
	hostLinks, err := b.Links(ctx, nil, bridge)
	if err != nil {
		return err
	}

	insideLinks, err := b.Links(ctx, ns, "")
	if err != nil {
		return err
	}
	if len(hostLinks) != 1 {
		return errors.New("expected exactly one veth on the server bridge")
	}

	var inside networkpolicy.Link
	for _, link := range insideLinks {
		if link.Name == "lo" {
			continue
		}
		if inside.Name != "" {
			return errors.New("multiple container network interfaces are unsupported")
		}

		inside = link
	}

	if inside.Name == "" ||
		inside.Peer != hostLinks[0].Index ||
		hostLinks[0].Peer != inside.Index ||
		!strings.EqualFold(inside.Address, endpoint.MacAddress) {
		return errors.New("container interface does not match the Docker-owned host veth")
	}

	rate := p.Effective(ceiling)
	if err := b.VerifyShape(ctx, ns, inside.Name, rate.Upload, inside.MTU); err != nil {
		return err
	}
	if err := b.VerifyShape(ctx, nil, hostLinks[0].Name, rate.Download, hostLinks[0].MTU); err != nil {
		return err
	}

	return b.VerifyFirewall(ctx, e.Id, p, allocations.Mappings, false)
}

func samePortBindings(a, b nat.PortMap) bool {
	if len(a) != len(b) {
		return false
	}

	for port, bindings := range a {
		other, found := b[port]
		if !found || len(bindings) != len(other) {
			return false
		}

		left, right := make([]string, len(bindings)), make([]string, len(other))
		for i, binding := range bindings {
			left[i] = binding.HostIP + "/" + binding.HostPort
		}

		for i, binding := range other {
			right[i] = binding.HostIP + "/" + binding.HostPort
		}

		sort.Strings(left)
		sort.Strings(right)
		for i := range left {
			if left[i] != right[i] {
				return false
			}
		}
	}

	return true
}

// SetNetworkSettings serializes allocation replacement with policy application.
// A revoked allocation must not return in a ruleset compiled from an old snapshot.
// A nil policy retains the durable policy accepted by an earlier PUT.
func (e *Environment) SetNetworkSettings(
	ctx context.Context,
	settings environment.Settings,
	incoming *networkpolicy.Policy,
) error {
	e.networkMu.Lock()
	defer e.networkMu.Unlock()

	if e.networkDeleted {
		return ErrNetworkConflict
	}

	old, err := networkpolicy.Load(config.Get().System.RootDirectory, e.Id)
	if err != nil {
		return e.failSyncedPolicy(ctx, old, err)
	}
	if incoming == nil && old.Revision == 0 && !old.Required(networkCeiling()) {
		e.Configuration.SetSettings(settings)
		e.storeNetworkStatus(e.pendingNetworkStatus(old))
		return nil
	}
	if incoming != nil && incoming.Revision < old.Revision {
		return ErrNetworkConflict
	}

	next := old
	if incoming != nil {
		if err := e.validatePolicyUpdate(old, *incoming, settings.Allocations.Mappings); err != nil {
			return e.failSyncedPolicy(ctx, old, err)
		}

		next = *incoming
	}

	if err := e.checkPolicySupport(ctx, old, next, settings.Allocations); err != nil {
		return e.failSyncedPolicy(ctx, old, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	wasVerified := e.networkVerified.Load()
	if err := e.saveNetworkPolicy(ctx, next, next.Revision != old.Revision || next.PolicyHash != old.PolicyHash); err != nil {
		visible, loadErr := networkpolicy.Load(config.Get().System.RootDirectory, e.Id)
		if loadErr != nil {
			err = fmt.Errorf("%w; could not reload visible policy: %v", err, loadErr)
			visible = old
		}
		return e.failSyncedPolicy(ctx, visible, err)
	}
	e.Configuration.SetSettings(settings)
	if old.Revision == next.Revision && old.PolicyHash == next.PolicyHash && wasVerified {
		c, inspectErr := e.ContainerInspect(ctx)
		if inspectErr == nil && c.State != nil && c.State.Running && c.ID == e.networkContainerID {
			if err := e.verifyNetwork(ctx, next, c); err == nil {
				return nil
			}
		}
	}
	e.networkVerified.Store(false)
	e.storeNetworkStatus(e.pendingNetworkStatus(next))
	return e.reconcileNetwork(ctx)
}

func (e *Environment) failSyncedPolicy(
	ctx context.Context,
	p networkpolicy.Policy,
	cause error,
) error {
	status := e.failNetworkStatus(p, cause)
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	c, err := e.ContainerInspect(stopCtx)
	if errdefs.IsNotFound(err) || (err == nil && (c.State == nil || !c.State.Running)) {
		return cause
	}
	if err != nil {
		status.Error += "; could not inspect container for fail-closed stop: " + err.Error()
		e.storeNetworkStatus(status)
		return fmt.Errorf("%w; could not inspect container for fail-closed stop: %v", cause, err)
	}
	if err := e.closeNetwork(stopCtx, p, c); err != nil {
		status.Error += "; could not quarantine container: " + err.Error()
		e.storeNetworkStatus(status)
		return fmt.Errorf("%w; could not quarantine container: %v", cause, err)
	}

	return cause
}

// Only deletion removes desired state. Ordinary stops/recreation retain it.
func (e *Environment) removeNetworkPolicy(ctx context.Context) (result error) {
	defer func() {
		if result == nil {
			e.networkDeleted = true
		}
	}()

	name, _, _, err := networkpolicy.Names(e.Id)
	if err != nil {
		return err
	}

	n, err := e.client.NetworkInspect(ctx, name, network.InspectOptions{})
	if errdefs.IsNotFound(err) {
		p, loadErr := networkpolicy.Load(config.Get().System.RootDirectory, e.Id)
		if loadErr != nil {
			return loadErr
		}
		if p.Revision > 0 {
			if err := networkpolicy.Linux().RemoveFirewall(ctx, e.Id); err != nil {
				return err
			}
		}
		return e.removeNetworkState(ctx)
	}
	if err != nil {
		return err
	}
	if err := e.checkNetwork(n, ""); err != nil {
		return err
	}
	if err := networkpolicy.Linux().RemoveFirewall(ctx, e.Id); err != nil {
		return err
	}
	if err := e.client.NetworkRemove(ctx, n.ID); err != nil {
		return err
	}

	return e.removeNetworkState(ctx)
}

func (e *Environment) removeNetworkState(ctx context.Context) error {
	root := config.Get().System.RootDirectory
	lease, err := e.runtimeLease(ctx)
	if err != nil {
		return err
	}
	if lease != nil {
		defer lease.Close()
	}
	if err := networkpolicy.RemoveRuntimeBinding(root, e.Id); err != nil {
		return err
	}
	return networkpolicy.Remove(root, e.Id)
}
