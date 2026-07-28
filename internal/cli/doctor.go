package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/nkoteb/nimbus/internal/audit"
	"github.com/nkoteb/nimbus/internal/device"
	"github.com/nkoteb/nimbus/internal/state"
)

// identity resolves this device's stable identity.
func (e *Env) identity() (*device.Identity, error) {
	return device.LoadOrCreateIdentity(e.Paths.IdentityFile())
}

// auditLog opens this device's audit trail inside the state repo, so the
// record travels with the fleet rather than staying on one machine.
func (e *Env) auditLog(nodeID string) (*audit.Log, error) {
	path := e.Paths.RepoAuditFile(nodeID)
	return audit.Open(path, nodeID)
}

func runDoctor(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	asJSON := fs.Bool("json", false, "emit the profile as JSON")
	publish := fs.Bool("publish", false, "write the profile into the state repo")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if err := env.Paths.EnsureDirs(); err != nil {
		return err
	}

	id, err := env.identity()
	if err != nil {
		return err
	}

	profile := device.Detect(ctx, id, nil)

	if *asJSON {
		data, err := json.MarshalIndent(profile, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(env.Out, string(data))
	} else {
		printProfile(env, profile)
	}

	if !*publish {
		return nil
	}

	repo, err := state.Open(env.Paths.Repo, env.repoAuth())
	if err != nil {
		return fmt.Errorf("cannot publish: no state repo at %s (run `nimbus init`)", env.Paths.Repo)
	}

	path, err := profile.Save(env.Paths.Repo)
	if err != nil {
		return err
	}
	if log, lerr := env.auditLog(profile.ID); lerr == nil {
		log.Record("local", "doctor.publish", profile.ID, "device profile refreshed", nil)
	}
	fmt.Fprintf(env.Out, "\npublished -> %s\n", filepath.Base(path))
	autoSync(ctx, env, repo, profile.ID, "nimbus: publish profile for "+profile.ID)
	return nil
}

func printProfile(env *Env, p *device.Profile) {
	fmt.Fprintf(env.Out, "device   %s\n", p.Label())
	fmt.Fprintf(env.Out, "host     %s (%s)\n", p.Hostname, device.ShortID(p.ID))

	osLine := p.OS.Platform + "/" + p.OS.Arch
	if p.OS.Distro != "" {
		osLine += "  " + p.OS.Distro
		if p.OS.Release != "" {
			osLine += " " + p.OS.Release
		}
	}
	if p.OS.Container {
		osLine += "  [container]"
	}
	fmt.Fprintf(env.Out, "os       %s\n", osLine)
	if p.OS.Kernel != "" {
		fmt.Fprintf(env.Out, "kernel   %s\n", p.OS.Kernel)
	}

	hw := fmt.Sprintf("%d cpu", p.Hardware.CPUs)
	if p.Hardware.MemoryGB > 0 {
		hw += fmt.Sprintf(", %.1f GB ram", p.Hardware.MemoryGB)
	}
	if len(p.Hardware.GPUs) > 0 {
		hw += ", gpu: " + strings.Join(p.Hardware.GPUs, "+")
	}
	if p.Hardware.HasScreen {
		hw += ", display"
	} else {
		hw += ", headless"
	}
	fmt.Fprintf(env.Out, "hardware %s\n", hw)

	if p.PkgManager != "" {
		fmt.Fprintf(env.Out, "packages %s\n", p.PkgManager)
	}

	fmt.Fprintln(env.Out, "\ntools")
	for _, t := range p.Tools {
		mark := "-"
		detail := "not installed"
		if t.Present {
			mark = "+"
			detail = t.Version
			if detail == "" {
				detail = t.Path
			}
		}
		fmt.Fprintf(env.Out, "  %s %-10s %s\n", mark, t.Name, detail)
	}

	if len(p.Missing) > 0 {
		fmt.Fprintf(env.Out, "\n%d missing: %s\n", len(p.Missing), strings.Join(p.Missing, ", "))
		fmt.Fprintln(env.Out, "run `nimbus install` to add what nimbus can install itself")
	}
}

func runFleet(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("fleet", flag.ContinueOnError)
	fs.SetOutput(env.Err)
	asJSON := fs.Bool("json", false, "emit the fleet as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Pull first, or this lists a snapshot from whenever the repo last synced.
	autoRefresh(ctx, env)

	fleet, err := device.LoadFleet(env.Paths.Repo)
	if err != nil {
		return err
	}
	if len(fleet) == 0 {
		return fmt.Errorf("no devices published yet (run `nimbus doctor --publish` on each device)")
	}

	if *asJSON {
		data, err := json.MarshalIndent(fleet, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(env.Out, string(data))
		return nil
	}

	// Mark the device we are on, so an operator reading a fleet list on a
	// borrowed machine is never confused about where they are.
	var selfID string
	if id, err := env.identity(); err == nil {
		selfID = id.ID
	}

	for _, p := range fleet {
		marker := " "
		if p.ID == selfID {
			marker = "*"
		}
		fmt.Fprintf(env.Out, "%s %-18s %-20s %s\n", marker, p.Label(), p.Hostname, p.Summary())
		if len(p.Missing) > 0 {
			fmt.Fprintf(env.Out, "  %-18s missing: %s\n", "", strings.Join(p.Missing, ", "))
		}
	}
	return nil
}
