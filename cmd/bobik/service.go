package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tichopad/bobik/internal/notes"
	"golang.org/x/sys/unix"
)

const serviceName = "bobik.service"

const serviceHelp = `Usage: bobik service install --vault <directory> --prompt <path> [options]

Install and enable a systemd user service. Accepts Bobik's usual watch options.
  --now        Start the service now, or restart it if replacing its configuration
  --replace    Replace a different existing bobik.service
  --no-linger  Refuse installation if systemd lingering is enabled for this user
`

func (a app) service(args []string, override string) int {
	if len(args) == 0 || len(args) == 1 && args[0] == "--help" {
		fmt.Fprint(a.stdout, serviceHelp)
		return exitOK
	}
	if args[0] != "install" {
		return a.fail(exitUsage, fmt.Errorf("unknown service command %q", args[0]))
	}
	var replace, start, noLinger bool
	var watchArgs []string
	for _, arg := range args[1:] {
		switch arg {
		case "--help", "-h":
			fmt.Fprint(a.stdout, serviceHelp)
			return exitOK
		case "--replace":
			replace = true
		case "--now":
			start = true
		case "--no-linger":
			noLinger = true
		default:
			watchArgs = append(watchArgs, arg)
		}
	}
	o, err := parseOptions(watchArgs)
	if err != nil {
		return a.fail(exitUsage, err)
	}
	if noLinger {
		if err := requireNonLingering(runServiceCommand); err != nil {
			return a.fail(exitRunError, err)
		}
	}
	cwd, err := a.cwd()
	if err != nil {
		return a.fail(exitRunError, err)
	}
	o.vault = fromDirectory(cwd, o.vault)
	o.prompt = fromDirectory(cwd, o.prompt)
	store, err := notes.NewVault(o.vault, o.rules)
	if err != nil {
		return a.fail(exitUsage, err)
	}
	defer store.Close()
	o.vault = store.Base()
	if _, err := readPrompt(store, o.prompt); err != nil {
		return a.fail(exitUsage, err)
	}
	if override != "" {
		override = fromDirectory(cwd, override)
		if err := a.checkPrivateSettings(o.vault, override, true); err != nil {
			return a.fail(exitUsage, err)
		}
		if _, err := loadConfig[settings](override, false); err != nil {
			return a.fail(exitUsage, err)
		}
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return a.fail(exitRunError, err)
	}
	if !filepath.IsAbs(configDir) {
		return a.fail(exitUsage, errors.New("the user configuration directory must be absolute"))
	}
	binary, err := os.Executable()
	if err != nil {
		return a.fail(exitRunError, err)
	}
	path := filepath.Join(configDir, "systemd", "user", serviceName)
	if err := installService(path, serviceUnit(binary, o, override), replace, start, runServiceCommand); err != nil {
		return a.fail(exitRunError, err)
	}
	fmt.Fprintf(a.stdout, "%s is enabled (%s).\n", serviceName, path)
	return exitOK
}

func serviceUnit(binary string, o options, override string) []byte {
	delay := fmt.Sprintf("%d.%09d", o.quietPeriod/time.Second, o.quietPeriod%time.Second)
	args := []string{":" + binary, "--watch", "--vault=" + o.vault, "--prompt=" + o.prompt,
		"--trigger=" + o.trigger, "--watch-delay=" + delay, "--model=" + o.model,
		"--vision-model=" + o.visionModel, "--max-steps=" + strconv.Itoa(o.steps)}
	if override != "" {
		args = append(args, "--config="+override)
	}
	for _, rule := range o.rules {
		args = append(args, "--"+string(rule.Mode)+"="+rule.Path)
	}
	// systemd expands specifiers even inside quotes. The ':' prefix disables
	// environment expansion, so '$' in a path or trigger stays literal.
	for i, arg := range args {
		args[i] = strconv.QuoteToASCII(strings.ReplaceAll(arg, "%", "%%"))
	}
	return []byte(`[Unit]
Description=Bobik vault watcher
StartLimitIntervalSec=60s
StartLimitBurst=5

[Service]
Type=exec
Environment=` + serviceModeEnv + `=1
ExecStart=` + strings.Join(args, " ") + `
Restart=on-failure
RestartSec=5s
RestartPreventExitStatus=2
KillMode=mixed
TimeoutStopSec=60s
StandardOutput=null
StandardError=journal

[Install]
WantedBy=default.target
`)
}

type serviceCommand func(name string, args ...string) (string, error)

func runServiceCommand(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if message := strings.TrimSpace(stderr.String()); message != "" {
			return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, message)
		}
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func requireNonLingering(run serviceCommand) error {
	linger, err := run("loginctl", "show-user", strconv.Itoa(os.Getuid()), "--property=Linger", "--value")
	if err != nil {
		return err
	}
	switch linger {
	case "yes":
		return errors.New("cannot install with --no-linger: systemd lingering is enabled for this user; omit --no-linger to allow it")
	case "no":
		return nil
	default:
		return fmt.Errorf("cannot determine whether systemd lingering is enabled: %q", linger)
	}
}

func installService(path string, unit []byte, replace, start bool, run serviceCommand) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	lock, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock systemd user unit directory: %w", err)
	}

	status, err := installedService(run)
	if err != nil {
		return err
	}
	fragment := status["FragmentPath"]
	if fragment != "" && fragment != path {
		return fmt.Errorf("%s is already provided by %s", serviceName, fragment)
	}
	var existing []byte
	info, err := os.Lstat(path)
	exists := err == nil
	switch {
	case exists:
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing to replace %s: it is not a regular file", path)
		}
		existing, err = os.ReadFile(path)
		if err != nil {
			return err
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	changed := !bytes.Equal(existing, unit)
	if changed {
		if exists && !replace {
			return fmt.Errorf("%s already exists with different settings; use --replace to update it", path)
		}
		if err := writeServiceUnit(path, unit); err != nil {
			return err
		}
	}

	// Also finish an earlier install that was interrupted before reload or enablement.
	if changed || fragment == "" || status["NeedDaemonReload"] == "yes" {
		if _, err := run("systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		status, err = installedService(run)
		if err != nil {
			return err
		}
	}
	if status["FragmentPath"] != path {
		return fmt.Errorf("systemd did not load %s; check the user manager's configuration directory", path)
	}
	if status["UnitFileState"] != "enabled" {
		if _, err := run("systemctl", "--user", "enable", serviceName); err != nil {
			return err
		}
	}
	if start {
		action := "start"
		if exists && changed {
			action = "restart"
		}
		_, err = run("systemctl", "--user", action, serviceName)
		return err
	}
	return nil
}

func installedService(run serviceCommand) (map[string]string, error) {
	out, err := run("systemctl", "--user", "show", "--property=FragmentPath",
		"--property=NeedDaemonReload", "--property=UnitFileState", serviceName)
	if err != nil {
		return nil, err
	}
	status := make(map[string]string)
	for line := range strings.SplitSeq(out, "\n") {
		key, value, _ := strings.Cut(line, "=")
		status[key] = value
	}
	return status, nil
}

func writeServiceUnit(path string, unit []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".bobik-unit-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(unit); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
