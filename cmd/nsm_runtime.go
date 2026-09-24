package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/internal/networkpolicy"
)

func newNetworkRuntimeCommand() *cobra.Command {
	return &cobra.Command{
		Use:                "nsm-runtime",
		Short:              "Docker runc wrapper for verified NSM startup (manual registration required)",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(_ *cobra.Command, args []string) error {
			if err := runNetworkRuntime(args); err != nil {
				var exit *exec.ExitError
				if errors.As(err, &exit) && exit.ExitCode() > 0 {
					os.Exit(exit.ExitCode())
				}
				return err
			}
			return nil
		},
	}
}

func runNetworkRuntime(args []string) error {
	if os.Geteuid() != 0 {
		return errors.New("NSM runtime requires root on the Docker host")
	}

	flags := flag.NewFlagSet("nsm-runtime", flag.ContinueOnError)
	configFile := flags.String("config", config.DefaultLocation, "active Wings configuration")
	runc := flags.String("runc", "/usr/bin/runc", "underlying runc executable")
	if err := flags.Parse(args); err != nil {
		return err
	}

	forward := flags.Args()
	call, err := networkpolicy.ParseRuntimeInvocation(forward)
	if err != nil {
		return err
	}
	if err := networkpolicy.ValidateRuntimeExecutable(*runc); err != nil {
		return err
	}
	native, err := networkpolicy.TrustedRuntimePath(*runc)
	if err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	current, err := os.Stat(self)
	if err != nil {
		return err
	}
	delegate, err := os.Stat(native)
	if err != nil {
		return err
	}
	if os.SameFile(current, delegate) {
		return errors.New("runtime cannot delegate to itself")
	}
	if call.Command != "start" && call.Command != "exec" {
		return syscall.Exec(native, append([]string{native}, forward...), os.Environ())
	}

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(signalCtx, 30*time.Second)
	defer cancel()

	id := ""
	if call.Command == "start" {
		id = call.Args[0]
	} else {
		id, err = call.ExecID()
		if err != nil {
			return err
		}
	}

	loadState := func() (networkpolicy.RuntimeState, error) {
		stateArgs := append(append([]string(nil), call.Global...), "state", id)
		output, err := networkpolicy.Linux().Run(ctx, nil, "", native, stateArgs...)
		var state networkpolicy.RuntimeState
		if err != nil {
			return state, fmt.Errorf("read runtime state: %w", err)
		}
		if err := json.NewDecoder(bytes.NewReader(output)).Decode(&state); err != nil {
			return state, err
		}
		if state.ID != id {
			return state, errors.New("runc state returned a different container ID")
		}
		return state, nil
	}

	if call.Command == "exec" {
		state, err := loadState()
		if err != nil {
			return err
		}
		if state.Status != "running" {
			return errors.New("runtime exec is allowed only after the managed container is running")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return syscall.Exec(native, append([]string{native}, forward...), os.Environ())
	}

	data, err := networkpolicy.ReadRuntimeFile(*configFile)
	if err != nil {
		return fmt.Errorf("runtime configuration: %w", err)
	}
	cfg, err := config.NewAtPath(*configFile)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return err
	}
	if !cfg.Docker.NetworkPolicy.Enabled || cfg.Docker.NetworkPolicy.Runtime == "" {
		return errors.New("NSM runtime mode is disabled; recreate the server through Wings after changing modes")
	}
	if cfg.System.User.Rootless.Enabled || cfg.Docker.UsernsMode != "" {
		return errors.New("NSM runtime requires rootful Docker without user namespace remapping")
	}

	ceiling := networkpolicy.Bandwidth{
		Upload:   cfg.Docker.NetworkPolicy.MaxUploadBPS,
		Download: cfg.Docker.NetworkPolicy.MaxDownloadBPS,
	}

	return networkpolicy.Linux().RuntimeStart(ctx, cfg.System.RootDirectory, cfg.Docker.NetworkPolicy.Runtime, ceiling, loadState, func() error {
		command := exec.CommandContext(ctx, native, forward...)
		command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
		command.WaitDelay = time.Second
		return command.Run()
	})
}
