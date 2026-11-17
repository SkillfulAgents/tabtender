package service

import (
	"context"
	"os"
	"os/signal"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"

	"github.com/SkillfulAgents/tabtender/internal/config"
)

// Service-specific exit codes reported to the SCM. Any non-zero code makes
// the SCM run the configured recovery actions (given "sc failureflag 1").
const (
	exitDaemonFailed  = 1
	exitUnsafeDataDir = 2
)

const (
	stopProgressPeriod = 5 * time.Second
	// stopWaitHint must comfortably exceed stopProgressPeriod: the SCM
	// considers the service hung if no new checkpoint arrives within it.
	stopWaitHint = 20 * time.Second
)

// IsWindowsService reports whether the process runs under the Windows SCM.
func IsWindowsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// Run executes the daemon either under the Service Control Manager or, when
// started from a console, until Ctrl+C.
func Run(run RunFunc) error {
	if !IsWindowsService() {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		return run(ctx)
	}
	h := &handler{run: run}
	if err := svc.Run(Name, h); err != nil {
		return err
	}
	return h.err
}

type handler struct {
	run RunFunc
	err error
}

// Execute implements svc.Handler, translating SCM stop and shutdown requests
// into context cancellation.
func (h *handler) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}

	// The data directory holds the configuration, helper executables and
	// secrets the service acts on, so it must be out of reach of other
	// accounts before anything in it is read.
	if h.err = secureServiceDataDir(); h.err != nil {
		// The daemon log lives in the directory that was just refused.
		reportToEventLog(h.err)
		return true, exitUnsafeDataDir
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	// While stopping, the SCM is sent a fresh checkpoint periodically:
	// closing every browser can take longer than any single wait hint an
	// SCM client is willing to honor.
	stopping := false
	var checkpoint uint32
	progress := time.NewTicker(stopProgressPeriod)
	defer progress.Stop()
	stopPending := func() svc.Status {
		checkpoint++
		return svc.Status{State: svc.StopPending, CheckPoint: checkpoint, WaitHint: uint32(stopWaitHint.Milliseconds())}
	}

	for {
		select {
		case h.err = <-done:
			// An error while carrying out a requested stop is logged by
			// the daemon but reported as a clean exit: a non-zero code
			// would make the SCM restart a service that was just stopped.
			if h.err == nil || stopping {
				return false, 0
			}
			return true, exitDaemonFailed
		case <-progress.C:
			if stopping {
				status <- stopPending()
			}
		case req := <-requests:
			switch req.Cmd {
			case svc.Interrogate:
				status <- req.CurrentStatus
			case svc.Stop, svc.Shutdown:
				stopping = true
				status <- stopPending()
				cancel()
			}
		}
	}
}

func secureServiceDataDir() error {
	paths, err := config.ResolvePaths(nil)
	if err != nil {
		return err
	}
	return secureDataDir(paths.DataDir)
}

// reportToEventLog records a startup failure in the Application event log.
// It is best effort: the event source may not be registered, in which case
// Event Viewer still shows the message below a generic preamble.
func reportToEventLog(err error) {
	log, openErr := eventlog.Open(Name)
	if openErr != nil {
		return
	}
	defer log.Close() //nolint:errcheck // nothing to do on failure
	_ = log.Error(1, "tabtenderd could not start: "+err.Error())
}
