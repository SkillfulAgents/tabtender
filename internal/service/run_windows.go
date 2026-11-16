package service

import (
	"context"
	"os"
	"os/signal"

	"golang.org/x/sys/windows/svc"
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for {
		select {
		case h.err = <-done:
			status <- svc.Status{State: svc.StopPending}
			if h.err != nil {
				return false, 1
			}
			return false, 0
		case req := <-requests:
			switch req.Cmd {
			case svc.Interrogate:
				status <- req.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending, WaitHint: 30000}
				cancel()
			}
		}
	}
}
