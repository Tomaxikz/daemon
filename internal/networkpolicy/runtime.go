package networkpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const (
	RuntimeServerKey     = "io.pterodactyl.networkstats.server"
	RuntimeGenerationKey = "io.pterodactyl.networkstats.generation"
)

var runtimeID = regexp.MustCompile(`^[0-9a-f]{64}$`)

type RuntimeInvocation struct {
	Global  []string
	Command string
	Args    []string
}

func ParseRuntimeInvocation(args []string) (RuntimeInvocation, error) {
	var call RuntimeInvocation
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			call.Global = append([]string(nil), args[:i]...)
			call.Command = arg
			call.Args = args[i+1:]
			break
		}
		name, _, inline := strings.Cut(arg, "=")
		switch name {
		case "--root", "--log", "--log-format", "--criu", "--rootless":
			if !inline {
				i++
				if i >= len(args) {
					return call, errors.New("missing runc option value")
				}
			}
		case "--debug", "--systemd-cgroup":
		case "--version", "-v", "--help", "-h":
			if len(args) != 1 {
				return call, errors.New("ambiguous runtime information request")
			}
			call.Command = "info"
			return call, nil
		default:
			return call, fmt.Errorf("unsupported runc global option %q", name)
		}
	}
	switch call.Command {
	case "start":
		if len(call.Args) != 1 || !runtimeID.MatchString(call.Args[0]) {
			return call, errors.New("runtime start requires one full Docker container ID")
		}
	case "exec":
		if _, err := call.ExecID(); err != nil {
			return call, err
		}
	case "create", "delete", "kill", "state", "list", "ps", "events", "pause", "resume", "update", "features", "checkpoint", "spec":
	case "run", "restore":
		return call, errors.New("this runtime supports managed containers through create/start only; run and restore are refused")
	default:
		return call, errors.New("unsupported or missing runc operation")
	}
	return call, nil
}

func (c RuntimeInvocation) ExecID() (string, error) {
	for i := 0; i < len(c.Args); i++ {
		arg := c.Args[i]
		if !strings.HasPrefix(arg, "-") {
			if !runtimeID.MatchString(arg) {
				return "", errors.New("runtime exec requires a full Docker container ID")
			}
			return arg, nil
		}
		name, _, inline := strings.Cut(arg, "=")
		switch name {
		case "--console-socket", "--cwd", "--env", "-e", "--user", "-u", "--additional-gids", "--process", "-p", "--pid-file", "--process-label", "--apparmor", "--cap", "--cgroup":
			if !inline {
				i++
				if i >= len(c.Args) {
					return "", errors.New("missing runc exec option value")
				}
			}
		case "--tty", "-t", "--detach", "-d", "--no-new-privs":
		default:
			return "", fmt.Errorf("unsupported runc exec option %q", name)
		}
	}
	return "", errors.New("missing runtime exec container ID")
}

func ValidateRuntimeExecutable(path string) error {
	if _, err := TrustedRuntimePath(path); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Mode().Perm()&0111 == 0 || owner.Uid != uint32(os.Geteuid()) {
		return errors.New("runtime executable must be owner-controlled and not group/world writable")
	}
	return nil
}

// TrustedRuntimePath resolves links one at a time, checking every directory entry.
// Sticky shared directories are safe only when their children are owner-controlled.
func TrustedRuntimePath(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("runtime paths must be clean and absolute")
	}
	if _, err := runtimePathInfo("/"); err != nil {
		return "", err
	}
	resolved := "/"
	pending := strings.Split(strings.TrimPrefix(path, "/"), "/")
	links := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			resolved = filepath.Dir(resolved)
			continue
		}
		candidate := filepath.Join(resolved, part)
		info, err := runtimePathInfo(candidate)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			links++
			if links > 40 {
				return "", errors.New("too many runtime path symlinks")
			}
			target, err := os.Readlink(candidate)
			if err != nil {
				return "", err
			}
			if filepath.IsAbs(target) {
				resolved = "/"
			}
			pending = append(strings.Split(target, "/"), pending...)
			continue
		}
		if len(pending) > 0 && !info.IsDir() {
			return "", fmt.Errorf("runtime ancestor %q is not a directory", candidate)
		}
		resolved = candidate
	}
	return resolved, nil
}

func runtimePathInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (owner.Uid != 0 && owner.Uid != uint32(os.Geteuid())) ||
		(info.IsDir() && info.Mode().Perm()&0022 != 0 && info.Mode()&os.ModeSticky == 0) {
		return nil, fmt.Errorf("runtime ancestor %q is not owner-controlled", path)
	}
	return info, nil
}

func ValidateRuntimeRegistration(path string, args []string, configPath string) error {
	if err := ValidateRuntimeExecutable(path); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	current, err := os.Stat(executable)
	if err != nil {
		return err
	}
	registered, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(current, registered) {
		return errors.New("registered runtime must use the same Wings binary as the daemon")
	}
	if len(args) != 6 || args[0] != "nsm-runtime" || args[1] != "--config" || args[2] != configPath || args[3] != "--runc" || args[5] != "--" {
		return errors.New("runtimeArgs must be: nsm-runtime --config <active Wings config> --runc <absolute runc path> --")
	}
	if err := ValidateRuntimeExecutable(args[4]); err != nil {
		return err
	}
	delegate, err := os.Stat(args[4])
	if err != nil {
		return err
	}
	if os.SameFile(delegate, current) {
		return errors.New("runtime cannot delegate to itself")
	}
	_, err = ReadRuntimeFile(configPath)
	return err
}

// RuntimeBinding ties durable desired state to one Docker container generation.
// Allocations are the published bindings at creation, not a mutable Panel claim.
type RuntimeBinding struct {
	Version     int              `json:"version"`
	Server      string           `json:"server"`
	Container   string           `json:"container"`
	Generation  string           `json:"generation"`
	Runtime     string           `json:"runtime"`
	Allocations map[string][]int `json:"allocations"`
	Revision    uint64           `json:"revision"`
	PolicyHash  string           `json:"policy_hash"`
}

type RuntimeState struct {
	ID          string            `json:"id"`
	PID         int               `json:"pid"`
	Status      string            `json:"status"`
	Bundle      string            `json:"bundle"`
	Annotations map[string]string `json:"annotations"`
}

func runtimePath(root, id string) (string, error) {
	path, err := policyPath(root, id)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), "runtime", id+".json"), nil
}

func (b RuntimeBinding) validate() error {
	if _, _, _, err := Names(b.Server); err != nil {
		return err
	}
	generation, err := uuid.Parse(b.Generation)
	if err != nil || generation.String() != b.Generation || generation == uuid.Nil {
		return errors.New("invalid runtime generation")
	}
	if b.Version != 1 || !runtimeID.MatchString(b.Container) || b.Runtime == "" || b.Allocations == nil {
		return errors.New("incomplete runtime binding")
	}
	if (b.Revision == 0 && b.PolicyHash != "") || (b.Revision > 0 && !hashPattern.MatchString(b.PolicyHash)) {
		return errors.New("invalid runtime policy identity")
	}
	_, err = assignedAllocations(b.Allocations)
	return err
}

func SaveRuntimeBinding(root string, binding RuntimeBinding) error {
	root, err := TrustedRuntimePath(root)
	if err != nil {
		return err
	}
	if err := binding.validate(); err != nil {
		return err
	}
	path, err := runtimePath(root, binding.Server)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := privatePolicyDirectory(filepath.Dir(filepath.Dir(path))); err != nil {
		return err
	}
	if err := privatePolicyDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	data, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	if len(data) > MaxBody {
		return errors.New("runtime binding exceeds 1 MiB")
	}
	return writeState(path, data)
}

func LoadRuntimeBinding(root, id string) (RuntimeBinding, error) {
	var binding RuntimeBinding
	root, err := TrustedRuntimePath(root)
	if err != nil {
		return binding, err
	}
	path, err := runtimePath(root, id)
	if err != nil {
		return binding, err
	}
	if err := privatePolicyDirectory(filepath.Dir(filepath.Dir(path))); err != nil {
		return binding, err
	}
	if err := privatePolicyDirectory(filepath.Dir(path)); err != nil {
		return binding, err
	}
	data, err := ReadRuntimeFile(path)
	if err != nil {
		return binding, err
	}
	if err := Decode(data, &binding); err != nil {
		return binding, err
	}
	if binding.Server != id {
		return binding, errors.New("runtime binding belongs to another server")
	}
	return binding, binding.validate()
}

// SyncRuntimeBinding must run under LockRuntime after persisting the policy.
// A crash between those writes leaves an identity mismatch, which blocks start.
func SyncRuntimeBinding(root, id string, policy Policy) error {
	binding, err := LoadRuntimeBinding(root, id)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	binding.Revision = policy.Revision
	binding.PolicyHash = policy.PolicyHash
	return SaveRuntimeBinding(root, binding)
}

func RemoveRuntimeBinding(root, id string) error {
	path, err := runtimePath(root, id)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	root, err = TrustedRuntimePath(root)
	if err != nil {
		return err
	}
	path, err = runtimePath(root, id)
	if err != nil {
		return err
	}
	if err := privatePolicyDirectory(filepath.Dir(filepath.Dir(path))); err != nil {
		return err
	}
	if err := privatePolicyDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ReadRuntimeFile refuses symlinks, special files and writable/non-owner state.
func ReadRuntimeFile(path string) ([]byte, error) {
	resolved, err := TrustedRuntimePath(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("runtime state must not be a symlink")
	}
	file, err := os.OpenFile(resolved, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return nil, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || owner.Uid != uint32(os.Geteuid()) || owner.Nlink != 1 {
		return nil, errors.New("runtime state must be an owner-controlled regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxBody {
		return nil, errors.New("runtime state exceeds 1 MiB")
	}
	return data, nil
}

// LockRuntime serializes runtime starts with policy writes and reconciliation.
// Never hold this lock during Docker API calls: Docker may be waiting for its
// wrapper to acquire the same lock, including during container inspection.
func LockRuntime(ctx context.Context, root, id string) (*os.File, error) {
	root, err := TrustedRuntimePath(root)
	if err != nil {
		return nil, err
	}
	path, err := policyPath(root, id)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if err := privatePolicyDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(strings.TrimSuffix(path, ".json")+".lock", os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || owner.Uid != uint32(os.Geteuid()) || owner.Nlink != 1 {
		file.Close()
		return nil, errors.New("runtime lock must be private and owner-controlled")
	}
	for {
		if err := ctx.Err(); err != nil {
			file.Close()
			return nil, err
		}
		if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
			return file, nil
		} else if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			file.Close()
			return nil, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func runtimeNamespace(state RuntimeState, binding RuntimeBinding) (string, error) {
	if state.Status != "created" || state.PID <= 1 || state.ID != binding.Container ||
		state.Annotations[RuntimeServerKey] != binding.Server ||
		state.Annotations[RuntimeGenerationKey] != binding.Generation {
		return "", errors.New("runtime state does not match the created server container")
	}
	data, err := ReadRuntimeFile(filepath.Join(state.Bundle, "config.json"))
	if err != nil {
		return "", err
	}
	var spec struct {
		Annotations map[string]string `json:"annotations"`
		Linux       struct {
			Namespaces []struct {
				Type string `json:"type"`
				Path string `json:"path"`
			} `json:"namespaces"`
		} `json:"linux"`
	}
	if err := validateUniqueJSON(data); err != nil {
		return "", err
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		return "", err
	}
	if spec.Annotations[RuntimeServerKey] != binding.Server || spec.Annotations[RuntimeGenerationKey] != binding.Generation {
		return "", errors.New("OCI bundle does not match the runtime binding")
	}
	var namespace string
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == "user" {
			return "", errors.New("runtime policies do not support user namespaces")
		}
		if ns.Type == "network" {
			if namespace != "" || !strings.HasPrefix(ns.Path, "/var/run/docker/netns/") || strings.Contains(ns.Path, "..") {
				return "", errors.New("runtime requires a Docker-owned network namespace")
			}
			namespace = ns.Path
		}
	}
	if namespace == "" {
		return "", errors.New("runtime network namespace is missing")
	}
	return namespace, nil
}

// RuntimeStart enforces saved state before start may execute the game process.
// It uses no Docker/Wings API calls and holds the lease until runc start returns.
func (b Backend) RuntimeStart(
	ctx context.Context,
	root, runtime string,
	ceiling Bandwidth,
	loadState func() (RuntimeState, error),
	start func() error,
) error {
	root, err := TrustedRuntimePath(root)
	if err != nil {
		return err
	}
	state, err := loadState()
	if err != nil {
		return err
	}

	id := state.Annotations[RuntimeServerKey]
	lease, err := LockRuntime(ctx, root, id)
	if err != nil {
		return err
	}
	defer lease.Close()

	state, err = loadState()
	if err != nil {
		return err
	}
	binding, err := LoadRuntimeBinding(root, id)
	if err != nil {
		return fmt.Errorf("load runtime binding: %w", err)
	}
	if runtime == "" || binding.Runtime != runtime {
		return errors.New("runtime selection changed; recreate this server through Wings")
	}

	policy, err := Load(root, id)
	if err != nil {
		return err
	}
	if policy.Revision != binding.Revision || policy.PolicyHash != binding.PolicyHash {
		return errors.New("saved policy and runtime binding differ; synchronize through Wings before starting")
	}
	if err := ValidateEnforcement(policy, binding.Allocations, ceiling); err != nil {
		return err
	}

	namespace, err := runtimeNamespace(state, binding)
	if err != nil {
		return err
	}
	ns, err := OpenNamespace(state.PID, namespace)
	if err != nil {
		return err
	}
	defer ns.Close()

	_, bridge, _, _ := Names(id)
	host, err := b.Links(ctx, nil, bridge)
	if err != nil {
		return err
	}
	inside, err := b.Links(ctx, ns, "")
	if err != nil {
		return err
	}

	var device Link
	for _, link := range inside {
		if link.Name == "lo" {
			continue
		}
		if device.Name != "" {
			return errors.New("multiple runtime network interfaces are unsupported")
		}
		device = link
	}
	if len(host) != 1 || device.Name == "" || device.Peer != host[0].Index || host[0].Peer != device.Index {
		return errors.New("runtime interface is not paired with this server's dedicated bridge")
	}

	if policy.Body.Firewall.Enabled {
		if err := b.Firewall(ctx, id, policy, nil, true); err != nil {
			return err
		}
		if err := b.VerifyFirewall(ctx, id, policy, nil, true); err != nil {
			return err
		}
	}

	rate := policy.Effective(ceiling)
	if err := b.Shape(ctx, ns, device.Name, rate.Upload, device.MTU); err != nil {
		return err
	}
	if err := b.VerifyShape(ctx, ns, device.Name, rate.Upload, device.MTU); err != nil {
		return err
	}

	if err := b.Shape(ctx, nil, host[0].Name, rate.Download, host[0].MTU); err != nil {
		return err
	}
	if err := b.VerifyShape(ctx, nil, host[0].Name, rate.Download, host[0].MTU); err != nil {
		return err
	}

	if err := b.Firewall(ctx, id, policy, binding.Allocations, false); err != nil {
		return err
	}
	if err := b.VerifyFirewall(ctx, id, policy, binding.Allocations, false); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	return start()
}
