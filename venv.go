// Functions and types for interacting with virtual environments

package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
  "regexp"
  "strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

var (
	venvCommandTimeout = 2 * time.Hour // 2 hours timeout
)

// VenvConfig defines a Python Virtual Environment.
type VenvConfig struct {
	Path   string // path to the virtualenv root
	Python string // path to the desired Python installation
}

func getPythonVersion(interpreter string) (int, int, error) {
  // Return (major, minor, error)
  cmd := exec.Command(interpreter, "--version")
  output, err := cmd.Output()
  if err != nil {
    failedCommandLogger(cmd)
    return -1, -1, errors.Wrap(err, "Unable to determine Python version.")
  }
  r := regexp.MustCompile(`Python (\d).(\d+)(\.\d+)?`)
  matches := r.FindStringSubmatch(string(output))
  majorVersion, err := strconv.Atoi(matches[1])
  if err != nil {
    return -1, -1, errors.Wrap(err, "Unable to parse Python Major version.")
  }

  minorVersion, err := strconv.Atoi(matches[2])
  if err != nil {
    return -1, 1, errors.Wrap(err, "Unable to parse Python Minor version.")
  }

  return majorVersion, minorVersion, nil
}

// Takes a VenvConfig and will create a new virtual environment.
func makeVenv(cfg VenvConfig) error {
  // Let's check python version first, since python version 3.3 or greater should have venv module
  // https://docs.python.org/3/library/venv.html
  majorV, minorV, err := getPythonVersion(cfg.Python)
  if err != nil {
    return errors.Wrap(err, "Unable to determine python version for specified interpreter.")
  }

  if majorV >= 3 && minorV >= 3 {
    err = makeVenvViaModule(cfg)
  } else {
    err = makeVenvLegacy(cfg)
  }
  if err != nil {
    return errors.Wrap(err,  "unable to create virtual environment")
  }

	return nil
}

func makeVenvViaModule(cfg VenvConfig) error {
  logrus.Debugln("Creating virtualenv via python module venv.")
  cmd := exec.Command(cfg.Python, "-m", "venv", cfg.Path)
  err := cmd.Run()
	if err != nil {
		failedCommandLogger(cmd)
    return err
	}
  return nil
}

func makeVenvLegacy(cfg VenvConfig) error {
  // Create virtualenv using legancy `virtualenv` command
  venvExecutable, err := exec.LookPath("virtualenv")
	if err != nil {
		return errors.Wrap(err, "virtualenv not found in path")
	}

	cmd := exec.Command(venvExecutable, "--python", cfg.Python, cfg.Path)
	err = cmd.Run()
	if err != nil {
		failedCommandLogger(cmd)
    return err
	}

  return nil
}

// Ensure ensures that a virtual environment exists, if not, it attempts to create it
func (c VenvConfig) Ensure() error {
	_, err := os.Stat(c.Path)
	if os.IsNotExist(err) {
		err := makeVenv(c)
		if err != nil {
			return err
		}
	}

	return nil
}

// Update updates the virtualenv for the given config with the specified requirements file
func (c VenvConfig) Update(requirementsFile string) error {
	vCmd := VenvCommand{
		Config: c,
		Binary: "pip",
		Args:   []string{"install", "-r", requirementsFile},
	}
	venvCommandOutput := vCmd.Run()
	if venvCommandOutput.Error != nil {
		return errors.Wrap(venvCommandOutput.Error, "unable to update virtualenv")
	}

	return nil
}

// VenvCommand enables you to run a system command in a virtualenv.
type VenvCommand struct {
	Config       VenvConfig
	Binary       string   // path to the binary under $venv/bin
	Args         []string // args to pass to the command that is called
	Cwd          string   // Directory to change to, if needed
	Env          []string // Additions to the runtime environment
	StreamOutput bool     // Whether or not the application should stream output stdout/stderr
}

type VenvCommandRunOutput struct {
	Stdout   string
	Stderr   string
	Error    error
	Exitcode int
}

// Run will execute the command described in VenvCommand.
//
// The strings returned are Stdout/Stderr.
func (c VenvCommand) Run() VenvCommandRunOutput {
	ctx, cancel := context.WithTimeout(context.Background(), venvCommandTimeout)
	CommandOutput := VenvCommandRunOutput{
		Stdout:   "",
		Stderr:   "",
		Error:    nil,
		Exitcode: -1,
	}

	defer cancel() // The cancel should be deferred so resources are cleaned up

	path, ok := os.LookupEnv("PATH")
	if !ok {
		CommandOutput.Error = errors.New("Unable to lookup the $PATH env variable")
		return CommandOutput
	}

	// Updating $PATH variable to include the venv path
	venvPath := filepath.Join(c.Config.Path, "bin")
	if !strings.Contains(path, venvPath) {
		newVenvPath := fmt.Sprintf("%s:%s", filepath.Join(c.Config.Path, "bin"), path)
		logrus.Debugln("PATH: ", newVenvPath)
		os.Setenv("PATH", newVenvPath)
	}

	cmd := exec.CommandContext(
		ctx,
		filepath.Join(c.Config.Path, "bin", c.Binary),
		c.Args...,
	)

	if c.Cwd != "" {
		cmd.Dir = c.Cwd
	}

	cmd.Env = append(os.Environ(), c.Env...)

	if c.StreamOutput {
		stdout, _ := cmd.StdoutPipe()
		stderr, _ := cmd.StderrPipe()
		if err := cmd.Start(); err != nil {
			CommandOutput.Error = errors.Wrap(err, "unable to start command")
			return CommandOutput
		}

		for _, stream := range []io.ReadCloser{stdout, stderr} {
			go func(s io.ReadCloser) {
				scanner := bufio.NewScanner(s)
				scanner.Split(bufio.ScanLines)
				for scanner.Scan() {
					m := scanner.Text()
					fmt.Println(m)
				}
			}(stream)
		}

		if err := cmd.Wait(); err != nil {
			exitError, _ := err.(*exec.ExitError)
			CommandOutput.Error = errors.Wrap(err, "unable to complete command")
			CommandOutput.Exitcode = exitError.ExitCode()
			return CommandOutput
		}

		CommandOutput.Exitcode = 0

		return CommandOutput
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	logrus.Debugln("Running venv command: ", cmd.Args)
	err := cmd.Run()

	CommandOutput.Stderr = stderr.String()
	CommandOutput.Stdout = stdout.String()

	if ctx.Err() == context.DeadlineExceeded {
		CommandOutput.Error = errors.Wrap(err, "Execution timed out")
		return CommandOutput
	} else if err != nil {
		failedCommandLogger(cmd)
		if exitError, ok := err.(*exec.ExitError); ok {
			CommandOutput.Exitcode = exitError.ExitCode()
		}
		CommandOutput.Error = errors.Wrap(err, "unable to complete command")
		return CommandOutput
	}

	CommandOutput.Exitcode = 0

	return CommandOutput
}
