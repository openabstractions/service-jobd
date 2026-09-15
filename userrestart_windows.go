package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// A per-user upgrade that fails after StopPreviousUserSupervisor stopped the
// predecessor rolls the old product back, and nothing starts its supervisor
// until the next sign-in. The installer's rollback action runs this: for each
// related product, the activation its own Startup shortcut runs, from the
// folder the product recorded.

const userRestartBudget = 30 * time.Second

type activationRunner func(image string, args []string) error

// activationArguments is the Startup shortcut command a released per-user
// product registered. 0.1.5 authored LogonStartFile Arguments="start"; 0.1.6
// and later author "start --runtime".
func activationArguments(version string) ([]string, error) {
	parts := strings.Split(version, ".")
	if len(parts) < 3 {
		return nil, fmt.Errorf("related product version %q is not MAJOR.MINOR.PATCH", version)
	}
	var numbers [3]int
	for i := range numbers {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return nil, fmt.Errorf("related product version %q is not MAJOR.MINOR.PATCH", version)
		}
		numbers[i] = n
	}
	if numbers[0] == 0 && (numbers[1] == 0 || (numbers[1] == 1 && numbers[2] <= 5)) {
		return []string{"start"}, nil
	}
	return []string{"start", "--runtime"}, nil
}

// restartUserPredecessors starts every related product whose folder held a
// process the upgrade stopped, and reports each product it does not start. A
// failure to start one product does not skip the others.
func restartUserPredecessors(list string, info productInfoFunc, exists func(string) bool, run activationRunner, stopped func(folder string) bool) (int, []string, error) {
	products, err := relatedProducts(list, info)
	if err != nil {
		return 0, nil, err
	}
	var notes []string
	var failures []error
	started := 0
	for _, p := range products {
		if p.location == "" {
			notes = append(notes, fmt.Sprintf("related product %s %s records no install location; its supervisor was not started", p.code, p.version))
			continue
		}
		root, err := userInstallFolder(p.location)
		if err != nil {
			failures = append(failures, fmt.Errorf("related product %s: %w", p.code, err))
			continue
		}
		if !stopped(root) {
			notes = append(notes, fmt.Sprintf("related product %s %s had no process stopped by this upgrade; it was not started", p.code, p.version))
			continue
		}
		args, err := activationArguments(p.version)
		if err != nil {
			failures = append(failures, fmt.Errorf("related product %s: %w", p.code, err))
			continue
		}
		image := filepath.Join(root, "tools", "jobdw.exe")
		if !exists(image) {
			notes = append(notes, fmt.Sprintf("related product %s %s has no %s; its supervisor was not started", p.code, p.version, image))
			continue
		}
		if err := run(image, args); err != nil {
			failures = append(failures, fmt.Errorf("start related product %s %s with %s %s: %w", p.code, p.version, image, strings.Join(args, " "), err))
			continue
		}
		started++
	}
	return started, notes, errors.Join(failures...)
}

// serviceStartUser releases the user upgrade exclusion, then restarts the
// related products whose processes that exclusion recorded as stopped. A note
// that cannot be written is joined into the result and the restart still runs.
func serviceStartUser(related string) error {
	record, released := systemExclusionEnv().release("user")
	var output error
	for _, note := range released {
		output = errors.Join(output, printed("%s\n", note))
	}
	started, notes, err := restartUserPredecessors(related, msiProductInfo, regularFile, runActivation, record.recordedIn)
	for _, note := range notes {
		output = errors.Join(output, printed("%s\n", note))
	}
	output = errors.Join(output, printed("started the supervisor of %d related per-user product(s)\n", started))
	return errors.Join(err, output)
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// runActivation waits for the activation command, which starts a detached
// supervisor and exits. No output pipes are inherited, so the wait ends with
// the command rather than with the supervisor it started.
func runActivation(image string, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), userRestartBudget)
	defer cancel()
	command := exec.CommandContext(ctx, image, args...)
	command.Dir = filepath.Dir(image)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	return command.Run()
}
