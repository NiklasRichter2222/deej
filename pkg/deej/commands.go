package deej

import (
	"os/exec"
	"strings"

	"github.com/omriharel/deej/pkg/deej/util"
)

// RunConfiguredCommand executes the command configured for the given index, if any.
func (d *Deej) RunConfiguredCommand(index int) {
	logger := d.logger.Named("commands")

	spec, ok := d.config.Commands[index]
	if !ok || len(spec.Args) == 0 {
		if d.Verbose() {
			logger.Debugw("No command configured for index", "index", index)
		}
		return
	}

	args := append([]string(nil), spec.Args...)

	if spec.Shell {
		commandLine := strings.Join(args, " ")
		if util.Windows() {
			args = []string{"cmd.exe", "/C", commandLine}
		} else {
			args = []string{"/bin/bash", "-c", commandLine}
		}
	}

	if len(args) == 0 {
		if d.Verbose() {
			logger.Debugw("Command payload empty after processing", "index", index)
		}
		return
	}

	command := args[0]
	commandArgs := append([]string(nil), args[1:]...)

	go func(cmdName string, cmdArgs []string) {
		cmd := exec.Command(cmdName, cmdArgs...)

		if err := cmd.Start(); err != nil {
			logger.Warnw("Failed to execute configured command", "index", index, "command", cmdName, "args", cmdArgs, "error", err)
			return
		}

		if d.Verbose() {
			logger.Debugw("Started configured command", "index", index, "command", cmdName, "args", cmdArgs)
		}

		if err := cmd.Wait(); err != nil {
			logger.Warnw("Configured command exited with error", "index", index, "command", cmdName, "args", cmdArgs, "error", err)
			return
		}

		if d.Verbose() {
			logger.Debugw("Configured command finished successfully", "index", index, "command", cmdName)
		}
	}(command, commandArgs)
}
