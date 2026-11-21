package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"q8s/internal/server"
	"q8s/internal/store"
	"q8s/internal/systemd"
)

// migrateServeActiveUnit reports the first q8s unit that is active. It uses
// the same systemctl is-active check as q8s status, but treats inactive units
// as a normal answer rather than an error.
var migrateServeActiveUnit = func(flags []string) (bool, string, error) {
	for _, unit := range []string{"q8s-api.service", "q8s.socket"} {
		args := append(append([]string{}, flags...), "is-active", unit)
		out, _ := exec.Command("systemctl", args...).Output()
		if strings.TrimSpace(string(out)) == "active" {
			return true, unit, nil
		}
	}
	return false, "", nil
}

type migrateNamesOptions struct {
	dryRun   bool
	toLegacy bool
}

func printMigrateNamesUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: q8s migrate-names [--dry-run] [--to-legacy]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Migrates the host artifacts of stored objects to the new generated names.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  --dry-run    Print what the startup migration would stop, remove and start")
	fmt.Fprintln(w, "  --to-legacy  Remove the new-format artifacts so the previous release can run")
	fmt.Fprintln(w, "  -h, --help")
}

func cmdMigrateNames() {
	var opts migrateNamesOptions
	for _, arg := range os.Args[2:] {
		switch arg {
		case "-h", "--help":
			printMigrateNamesUsage(os.Stdout)
			return
		case "--dry-run":
			opts.dryRun = true
		case "--to-legacy":
			opts.toLegacy = true
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", arg)
			printMigrateNamesUsage(os.Stderr)
			os.Exit(2)
		}
	}

	rootful := os.Getuid() == 0
	mode := systemd.ModeRootless
	if rootful {
		mode = systemd.ModeRootful
	}
	d := resolveDirs(rootful)
	st, err := store.Load(filepath.Join(d.dataDir, "store.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load store: %v\n", err)
		os.Exit(1)
	}
	mgr, err := systemd.NewManager(mode)
	if err != nil {
		// The dry run reads files only. A real run without systemd cannot
		// stop the old units, but it still removes files and the marker.
		fmt.Fprintf(os.Stderr, "warning: systemd manager unavailable: %v\n", err)
		mgr = nil
	}
	if mgr != nil {
		defer mgr.Close()
	}
	srv := server.NewNamesMigrationServer(server.Config{
		Store:      st,
		QuadletDir: d.quadletDir,
		SystemdDir: d.systemdDir,
		ConfigDir:  d.configDir,
		TraefikDir: filepath.Join(d.dataDir, "traefik"),
		DataDir:    d.dataDir,
		Mode:       mode,
		Manager:    mgr,
	})
	serveActive := func() (bool, string, error) {
		return migrateServeActiveUnit(systemctlFlags(rootful))
	}
	if err := runMigrateNames(opts, srv, serveActive, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

// runMigrateNames is the command body without process exits, so tests can
// drive it with a store, temp directories and a fake service-state check.
func runMigrateNames(opts migrateNamesOptions, srv *server.Server, serveActive func() (bool, string, error), out io.Writer) error {
	if !opts.dryRun {
		active, unit, err := serveActive()
		if err != nil {
			return fmt.Errorf("cannot check whether q8s serve is active: %w", err)
		}
		if active {
			return fmt.Errorf("refusing to change generated names while %s is active; run 'q8s stop' first", unit)
		}
	}

	if opts.toLegacy {
		claims := srv.NewFormatPVCClaims()
		if len(claims) > 0 {
			fmt.Fprintln(out, "Refusing to roll back to the previous generated names.")
			fmt.Fprintln(out, "These PVCs are bound to a new-format volume:")
			for _, claim := range claims {
				fmt.Fprintf(out, "  %s\n", claim)
			}
			fmt.Fprintln(out, "The previous release would bind each one to a new, empty old-name volume.")
			fmt.Fprintln(out, "Copy their data to an old-format volume before rolling back.")
			return fmt.Errorf("%d PVC(s) use a new-format volume", len(claims))
		}
		plan := srv.PlanNewNamesRollback()
		if opts.dryRun {
			fmt.Fprintln(out, "Dry run: q8s migrate-names --to-legacy would remove:")
			plan.Print(out)
			fmt.Fprintln(out, "Nothing was changed.")
			return nil
		}
		if err := srv.ApplyNameMigrationPlan(plan); err != nil {
			return err
		}
		if err := srv.RemoveNamesMigrationMarker(); err != nil {
			return err
		}
		fmt.Fprintf(out, "Removed the new-format artifacts of %d object(s).\n", len(plan.Objects))
		fmt.Fprintln(out, "Next: install the previous q8s binary, then start it with 'q8s start' (or 'q8s enable').")
		return nil
	}

	if opts.dryRun {
		plan := srv.PlanLegacyMigration()
		fmt.Fprintln(out, "Dry run: q8s serve would migrate these host artifacts at startup:")
		plan.Print(out)
		fmt.Fprintln(out, "Nothing was changed.")
		return nil
	}
	if !srv.NamesMigrationPending() {
		fmt.Fprintln(out, "The name migration already finished on this host (names-v2 marker exists).")
		return nil
	}
	srv.MigrateLegacyNames()
	// Write the new files and start the workloads now, instead of leaving
	// them stopped until the next q8s serve.
	srv.ReconcileQuadlets()
	srv.StartPodsWaitingForClaims()
	srv.EnsureWorkloadsStarted()
	if err := srv.WriteNamesMigrationMarker(); err != nil {
		return err
	}
	fmt.Fprintln(out, "Name migration finished.")
	return nil
}
