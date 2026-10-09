// Package supervisor implements graceful self-restart: on SIGUSR1 (or the
// admin endpoint) the process spawns a copy of itself sharing the listening
// socket, waits for the child's READY byte over a handoff pipe, drains
// in-flight requests, then exits 0 — near-zero-downtime handoff.
package supervisor

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/nerdneilsfield/go-crawl4ai-mcp/internal/appconfig"
)

// ErrRestart tells main to exit 0 after a successful handoff.
var ErrRestart = errors.New("restart handed off")

// notifyRestart is replaced by Serve; Restart triggers a graceful self-restart.
var notifyRestart = func() {}

// Restart requests a graceful self-restart (used by the admin endpoint).
func Restart() { notifyRestart() }

// Serve runs serveFn until it exits, a restart is requested (SIGUSR1 or
// Restart()), or the process is signalled. On restart it spawns the child
// (RESTART_FD=3 listener, READY_FD=4 pipe), waits for READY, calls drainFn,
// and returns ErrRestart. serveFn should return nil on http.ErrServerClosed.
func Serve(ln net.Listener, serveFn func() error, drainFn func()) error {
	log := appconfig.Logger.Named("supervisor")

	restartCh := make(chan struct{}, 1)
	notifyRestart = func() {
		select {
		case restartCh <- struct{}{}:
		default:
		}
	}
	defer func() { notifyRestart = func() {} }()

	sigusr1 := make(chan os.Signal, 1)
	signal.Notify(sigusr1, syscall.SIGUSR1)
	defer signal.Stop(sigusr1)

	errCh := make(chan error, 1)
	go func() { errCh <- serveFn() }()

	select {
	case err := <-errCh:
		return err
	case <-sigusr1:
		log.Info("restart requested (SIGUSR1)")
	case <-restartCh:
		log.Info("restart requested (admin)")
	}

	// File() dups the fd (blocking mode copy) without disturbing this
	// process's non-blocking listener.
	dupl, err := listenerFile(ln)
	if err != nil {
		return fmt.Errorf("restart: listener fd: %w", err)
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("restart: executable: %w", err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("restart: pipe: %w", err)
	}
	defer r.Close()

	gen := 1
	if g, err := strconv.Atoi(os.Getenv("RESTART_GEN")); err == nil {
		gen = g + 1
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(),
		"RESTART_FD=3", // ExtraFiles[0]
		"READY_FD=4",   // ExtraFiles[1]
		fmt.Sprintf("RESTART_GEN=%d", gen),
	)
	cmd.ExtraFiles = []*os.File{dupl, w}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		dupl.Close()
		w.Close()
		return fmt.Errorf("restart: spawn: %w", err)
	}
	dupl.Close()
	w.Close() // our write end; child writes READY, EOFs on exit

	ready := make(chan error, 1)
	go func() {
		buf := make([]byte, 5)
		n, err := r.Read(buf)
		switch {
		case err != nil:
			ready <- err
		case string(buf[:n]) == "READY":
			ready <- nil
		default:
			ready <- fmt.Errorf("unexpected handoff payload %q", buf[:n])
		}
	}()

	select {
	case err := <-ready:
		if err != nil {
			_ = cmd.Process.Kill()
			return fmt.Errorf("restart: child not ready: %w", err)
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		return errors.New("restart: child readiness timeout")
	}

	log.Info("child ready; draining old process",
		zap.Int("pid", os.Getpid()), zap.Int("child_pid", cmd.Process.Pid),
		zap.Int("generation", gen))
	if drainFn != nil {
		drainFn() // http.Server.Shutdown: closes idle conns, waits for in-flight
	}
	// cmd.Wait blocks until the child exits; the child inherits the listener
	// and its own supervisor loop keeps it alive, so wait for it only briefly
	// (it is a fully independent process — waiting forever would deadlock).
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	select {
	case <-waitCh:
	case <-time.After(5 * time.Second):
		// Child is running independently (normal); stop reaping it and exit.
		log.Info("child continues independently; old process exiting")
	}
	return ErrRestart
}

// listenerFile extracts a dup'd *os.File from the listener without flipping
// the original into blocking mode.
func listenerFile(ln net.Listener) (*os.File, error) {
	type fder interface{ File() (*os.File, error) }
	f, ok := ln.(fder)
	if !ok {
		return nil, fmt.Errorf("listener type %T does not support fd passing", ln)
	}
	return f.File()
}

// Inherit returns the listener and readiness pipe inherited from a
// restarting parent (RESTART_FD / READY_FD env). Both are nil when this
// process was started normally. The caller owns both files.
func Inherit() (listener *os.File, ready *os.File, err error) {
	fdStr := os.Getenv("RESTART_FD")
	if fdStr == "" {
		return nil, nil, nil
	}
	lfd, err := strconv.Atoi(fdStr)
	if err != nil {
		return nil, nil, fmt.Errorf("bad RESTART_FD %q: %w", fdStr, err)
	}
	rfd, err := strconv.Atoi(os.Getenv("READY_FD"))
	if err != nil {
		return nil, nil, fmt.Errorf("bad READY_FD: %w", err)
	}
	listener = os.NewFile(uintptr(lfd), "inherited-listener")
	ready = os.NewFile(uintptr(rfd), "ready-pipe")
	if listener == nil || ready == nil {
		return nil, nil, errors.New("inherited fds are invalid")
	}
	return listener, ready, nil
}
