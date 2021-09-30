package main

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/cyverse/irodsfs-pool/commons"
	"github.com/cyverse/irodsfs-pool/service"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

const (
	// InterProcessCommunicationFinishSuccess is the message that parent process receives when child process is executed successfully
	InterProcessCommunicationFinishSuccess string = "<<COMMUNICATION_CLOSE_SUCCESS>>"
	// InterProcessCommunicationFinishError is the message that parent process receives when child process fails to run
	InterProcessCommunicationFinishError string = "<<COMMUNICATION_CLOSE_ERROR>>"
)

// NilWriter drains output
type NilWriter struct{}

// Write does nothing
func (w *NilWriter) Write(p []byte) (n int, err error) {
	return len(p), nil
}

func main() {
	// check if this is subprocess running in the background
	isChildProc := false

	childProcessArgument := fmt.Sprintf("-%s", ChildProcessArgument)
	for _, arg := range os.Args[1:] {
		if arg == childProcessArgument {
			// background
			isChildProc = true
			break
		}
	}

	if isChildProc {
		// child process
		childMain()
	} else {
		// parent process
		parentMain()
	}
}

// RunFSDaemon runs irodsfs-pool as a daemon
func RunFSDaemon(execPath string, config *commons.Config) error {
	return parentRun(execPath, config)
}

// parentRun executes irodsfs-pool with the config given
func parentRun(irodsfsPoolExec string, config *commons.Config) error {
	logger := log.WithFields(log.Fields{
		"package":  "main",
		"function": "parentRun",
	})

	err := config.Validate()
	if err != nil {
		logger.WithError(err).Error("invalid argument")
		return err
	}

	if !config.Foreground {
		// run child process in background and pass parameters via stdin PIPE
		// receives result from the child process
		logger.Info("Running the process in the background mode")
		childProcessArgument := fmt.Sprintf("-%s", ChildProcessArgument)
		cmd := exec.Command(irodsfsPoolExec, childProcessArgument)
		subStdin, err := cmd.StdinPipe()
		if err != nil {
			logger.WithError(err).Error("failed to communicate to background process")
			return err
		}

		subStdout, err := cmd.StdoutPipe()
		if err != nil {
			logger.WithError(err).Error("failed to communicate to background process")
			return err
		}

		cmd.Stderr = cmd.Stdout

		err = cmd.Start()
		if err != nil {
			logger.WithError(err).Errorf("failed to start a child process")
			return err
		}

		logger.Infof("Process id = %d", cmd.Process.Pid)

		logger.Info("Sending configuration data")
		configBytes, err := yaml.Marshal(config)
		if err != nil {
			logger.WithError(err).Error("failed to serialize configuration")
			return err
		}

		// send it to child
		_, err = io.WriteString(subStdin, string(configBytes))
		if err != nil {
			logger.WithError(err).Error("failed to communicate to background process")
			return err
		}
		subStdin.Close()
		logger.Info("Successfully sent configuration data to background process")

		childProcessFailed := false

		// receive output from child
		subOutputScanner := bufio.NewScanner(subStdout)
		for {
			if subOutputScanner.Scan() {
				errMsg := strings.TrimSpace(subOutputScanner.Text())
				if errMsg == InterProcessCommunicationFinishSuccess {
					logger.Info("Successfully started background process")
					break
				} else if errMsg == InterProcessCommunicationFinishError {
					logger.Error("failed to start background process")
					childProcessFailed = true
					break
				} else {
					fmt.Fprintln(os.Stderr, errMsg)
				}
			} else {
				// check err
				if subOutputScanner.Err() != nil {
					logger.Error(subOutputScanner.Err().Error())
					childProcessFailed = true
					break
				}
			}
		}

		subStdout.Close()

		if childProcessFailed {
			return fmt.Errorf("failed to start background process")
		}
	} else {
		// foreground
		err = run(config, false)
		if err != nil {
			logger.WithError(err).Error("failed to run iRODS FUSE Lite Pool Service")
			return err
		}
	}

	return nil
}

// parentMain handles command-line parameters and run parent process
func parentMain() {
	logger := log.WithFields(log.Fields{
		"package":  "main",
		"function": "parentMain",
	})

	// parse argument
	config, logFile, err, exit := processArguments()
	if err != nil {
		logger.WithError(err).Error("failed to process arguments")
		if exit {
			logger.Fatal(err)
		}
	}
	if exit {
		os.Exit(0)
	}

	// run
	err = parentRun(os.Args[0], config)
	if err != nil {
		logger.WithError(err).Error("failed to run the foreground process")
		logger.Fatal(err)
	}

	// clean up
	if logFile != nil {
		logFile.Close()
		// do not remove the log file
		//os.Remove(config.LogPath)
	}

	os.Exit(0)
}

// childMain runs child process
func childMain() {
	logger := log.WithFields(log.Fields{
		"package":  "main",
		"function": "childMain",
	})

	logger.Info("Start background process")

	// read from stdin
	_, err := os.Stdin.Stat()
	if err != nil {
		logger.WithError(err).Error("failed to communicate to foreground process")
		fmt.Fprintln(os.Stderr, InterProcessCommunicationFinishError)
		os.Exit(1)
	}

	configBytes, err := ioutil.ReadAll(os.Stdin)
	if err != nil {
		logger.WithError(err).Error("failed to read configuration")
		fmt.Fprintln(os.Stderr, InterProcessCommunicationFinishError)
		os.Exit(1)
	}

	config, err := commons.NewConfigFromYAML(configBytes)
	if err != nil {
		logger.WithError(err).Error("failed to read configuration")
		fmt.Fprintln(os.Stderr, InterProcessCommunicationFinishError)
		os.Exit(1)
	}

	// output to log file
	var logFile *os.File
	logFile = nil
	if len(config.LogPath) > 0 {
		logFileHandle, err := os.OpenFile(config.LogPath, os.O_WRONLY|os.O_CREATE, 0755)
		if err != nil {
			logger.WithError(err).Error("failed to create log file")
			fmt.Fprintln(os.Stderr, InterProcessCommunicationFinishError)
			os.Exit(1)
		} else {
			log.SetOutput(logFileHandle)
			logFile = logFileHandle
		}
	}

	err = config.Validate()
	if err != nil {
		logger.WithError(err).Error("invalid configuration")
		fmt.Fprintln(os.Stderr, InterProcessCommunicationFinishError)
		os.Exit(1)
	}

	// background
	err = run(config, true)
	if err != nil {
		logger.WithError(err).Error("failed to run iRODS FUSE Lite Pool Service")
		os.Exit(1)
	}

	if logFile != nil {
		logFile.Close()
		// do not remove the log file
		//os.Remove(config.LogPath)
	}
}

// run runs irodsfs-pool
func run(config *commons.Config, isChildProcess bool) error {
	logger := log.WithFields(log.Fields{
		"package":  "main",
		"function": "run",
	})

	// run a service
	svc, err := service.NewPoolService(config)
	if err != nil {
		logger.WithError(err).Error("failed to create the service")
		if isChildProcess {
			fmt.Fprintln(os.Stderr, InterProcessCommunicationFinishError)
		}
		return err
	}

	err = svc.Init()
	if err != nil {
		logger.WithError(err).Error("failed to init the service")
		if isChildProcess {
			fmt.Fprintln(os.Stderr, InterProcessCommunicationFinishError)
		}
		return err
	}

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL, syscall.SIGQUIT)

	go func() {
		receivedSignal := <-signalChan

		logger.Infof("received signal (%s), terminating iRODS FUSE Lite Pool Service", receivedSignal.String())
		if isChildProcess {
			fmt.Fprintln(os.Stderr, InterProcessCommunicationFinishError)
		}

		svc.Destroy()
		os.Exit(0)
	}()

	if isChildProcess {
		fmt.Fprintln(os.Stdout, InterProcessCommunicationFinishSuccess)
		if len(config.LogPath) == 0 {
			// stderr is not a local file, so is closed by parent
			var nilWriter NilWriter
			log.SetOutput(&nilWriter)
		}
	}

	err = svc.Start()
	if err != nil {
		logger.WithError(err).Error("failed to start the service, terminating iRODS FUSE Lite Pool Service")
		svc.Destroy()
		return err
	}

	// returns if fails, or stopped.
	logger.Info("Service stopped, terminating iRODS FUSE Lite Pool Service")
	svc.Destroy()
	return nil
}
