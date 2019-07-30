package again

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"reflect"
	"strings"
	"sync"
	"syscall"
)

var OnForkHook func()

// Don't make the caller import syscall.
const (
	SIGINT  = syscall.SIGINT
	SIGQUIT = syscall.SIGQUIT
	SIGTERM = syscall.SIGTERM
	SIGUSR2 = syscall.SIGUSR2
)

// Service is a single service listening on a single net.Listener.
type Service struct {
	Name       string
	FdName     string
	Descriptor uintptr
	Listener   net.Listener
	Hooks      Hooks
}

// Hooks callbacks invoked when specific signal is received.
type Hooks struct {
	// OnSIGHUP is the function called when the server receives a SIGHUP
	// signal. The normal use case for SIGHUP is to reload the
	// configuration.
	OnSIGHUP func(*Service) error
	// OnSIGUSR1 is the function called when the server receives a
	// SIGUSR1 signal. The normal use case for SIGUSR1 is to repon the
	// log files.
	OnSIGUSR1 func(l *Service) error
	// OnSIGQUIT use this for graceful shutdown
	OnSIGQUIT func(*Service) error
}

// Again manages services that need graceful restarts
type Again struct {
	services *sync.Map
}

func New() *Again {
	return &Again{
		services: &sync.Map{},
	}
}

func (a *Again) Env() (m map[string]string, err error) {
	var fds []string
	var names []string
	var fdNames []string
	a.services.Range(func(k, value interface{}) bool {
		s := value.(*Service)
		names = append(names, s.Name)
		_, _, e1 := syscall.Syscall(syscall.SYS_FCNTL, s.Descriptor, syscall.F_SETFD, 0)
		if 0 != e1 {
			err = e1
			return false
		}
		fds = append(fds, fmt.Sprint(s.Descriptor))
		fdNames = append(fdNames, s.FdName)
		return true
	})
	if err != nil {
		return
	}
	return map[string]string{
		"GOAGAIN_FD":           strings.Join(fds, ","),
		"GOAGAIN_SERVICE_NAME": strings.Join(names, ","),
		"GOAGAIN_NAME":         strings.Join(fdNames, ","),
	}, nil
}

func ListerName(l net.Listener) string {
	addr := l.Addr()
	return fmt.Sprintf("%s:%s->", addr.Network(), addr.String())
}

func (a *Again) Range(fn func(*Service)) {
	a.services.Range(func(k, v interface{}) bool {
		s := v.(*Service)
		fn(s)
		return true
	})
}

// Close tries to close all service listeners
func (a *Again) Close() error {
	var e bytes.Buffer
	a.Range(func(s *Service) {
		if err := s.Listener.Close(); err != nil {
			e.WriteString(err.Error())
			e.WriteByte('\n')
		}
	})
	if e.Len() > 0 {
		return errors.New(e.String())
	}
	return nil
}

// Listen creates a new service with the given listener.
func (a *Again) Listen(name string, ls net.Listener) error {
	v := reflect.ValueOf(ls).Elem().FieldByName("fd").Elem()
	fdField := v.FieldByName("sysfd")

	if !fdField.IsValid() {
		fdField = v.FieldByName("pfd").FieldByName("Sysfd")
	}

	if !fdField.IsValid() {
		return fmt.Errorf("Not supported by current Go version")
	}
	fd := uintptr(fdField.Int())
	a.services.Store(name, &Service{
		Name:       name,
		FdName:     ListerName(ls),
		Listener:   ls,
		Descriptor: fd,
	})
	return nil
}

// Re-exec this same image without dropping the net.Listener.
func Exec(a *Again) error {
	var pid int
	fmt.Sscan(os.Getenv("GOAGAIN_PID"), &pid)
	if syscall.Getppid() == pid {
		return fmt.Errorf("goagain.Exec called by a child process")
	}
	argv0, err := lookPath()
	if nil != err {
		return err
	}
	if err := setEnvs(a); nil != err {
		return err
	}
	if err := os.Setenv(
		"GOAGAIN_SIGNAL",
		fmt.Sprintf("%d", syscall.SIGQUIT),
	); nil != err {
		return err
	}
	log.Println("re-executing", argv0)
	return syscall.Exec(argv0, os.Args, os.Environ())
}

// Fork and exec this same image without dropping the net.Listener.
func ForkExec(a *Again) error {
	argv0, err := lookPath()
	if nil != err {
		return err
	}
	wd, err := os.Getwd()
	if nil != err {
		return err
	}
	err = setEnvs(a)
	if nil != err {
		return err
	}
	if err := os.Setenv("GOAGAIN_PID", ""); nil != err {
		return err
	}
	if err := os.Setenv(
		"GOAGAIN_PPID",
		fmt.Sprint(syscall.Getpid()),
	); nil != err {
		return err
	}

	sig := syscall.SIGQUIT
	if err := os.Setenv("GOAGAIN_SIGNAL", fmt.Sprintf("%d", sig)); nil != err {
		return err
	}

	files := []*os.File{
		os.Stdin, os.Stdout, os.Stderr,
	}
	a.Range(func(s *Service) {
		files = append(files, os.NewFile(
			s.Descriptor,
			ListerName(s.Listener),
		))
	})
	p, err := os.StartProcess(argv0, os.Args, &os.ProcAttr{
		Dir:   wd,
		Env:   os.Environ(),
		Files: files,
		Sys:   &syscall.SysProcAttr{},
	})
	if nil != err {
		return err
	}
	log.Println("spawned child", p.Pid)
	if err = os.Setenv("GOAGAIN_PID", fmt.Sprint(p.Pid)); nil != err {
		return err
	}
	return nil
}

// IsErrClosing tests whether an error is equivalent to net.errClosing as returned by
// Accept during a graceful exit.
func IsErrClosing(err error) bool {
	if opErr, ok := err.(*net.OpError); ok {
		err = opErr.Err
	}
	return "use of closed network connection" == err.Error()
}

// Kill process specified in the environment with the signal specified in the
// environment; default to SIGQUIT.
func Kill() error {
	var (
		pid int
		sig syscall.Signal
	)
	_, err := fmt.Sscan(os.Getenv("GOAGAIN_PID"), &pid)
	if io.EOF == err {
		_, err = fmt.Sscan(os.Getenv("GOAGAIN_PPID"), &pid)
	}
	if nil != err {
		return err
	}
	if _, err := fmt.Sscan(os.Getenv("GOAGAIN_SIGNAL"), &sig); nil != err {
		sig = syscall.SIGQUIT
	}
	log.Println("sending signal", sig, "to process", pid)
	return syscall.Kill(pid, sig)
}

// Listen checks env and constructs a Again instance if this is a child process
// that was froked by again parent.
//
// forkHook if provided will be called before forking.
func Listen(forkHook func()) (*Again, error) {
	OnForkHook = forkHook
	a := New()
	fds := strings.Split(os.Getenv("GOAGAIN_FD"), ",")
	names := strings.Split(os.Getenv("GOAGAIN_SERVICE_NAME"), ",")
	fdNames := strings.Split(os.Getenv("GOAGAIN_NAME"), ",")
	if !((len(fds) == len(names)) && (len(fds) == len(fdNames))) {
		return nil, errors.New(("again: names/fds mismatch"))
	}
	for k, f := range fds {
		var s Service
		_, err := fmt.Sscan(f, &s.Descriptor)
		if err != nil {
			return nil, err
		}
		s.Name = names[k]
		s.FdName = fdNames[k]
		l, err := net.FileListener(os.NewFile(s.Descriptor, s.FdName))
		if err != nil {
			return nil, err
		}
		s.Listener = l
		switch l.(type) {
		case *net.TCPListener, *net.UnixListener:
		default:
			return nil, fmt.Errorf(
				"file descriptor is %T not *net.TCPListener or *net.UnixListener",
				l,
			)
		}
		if err = syscall.Close(int(s.Descriptor)); nil != err {
			return nil, err
		}
		fmt.Println("=> ", s.Name, s.FdName)
		a.services.Store(s.Name, &s)
	}
	return a, nil
}

// Wait waits for signals
func Wait(a *Again) (syscall.Signal, error) {
	ch := make(chan os.Signal, 2)
	signal.Notify(
		ch,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGQUIT,
		syscall.SIGTERM,
		syscall.SIGUSR1,
		syscall.SIGUSR2,
	)
	forked := false
	for {
		sig := <-ch
		log.Println(sig.String())
		switch sig {

		// SIGHUP should reload configuration.
		case syscall.SIGHUP:
			a.Range(func(s *Service) {
				if s.Hooks.OnSIGHUP != nil {
					if err := s.Hooks.OnSIGHUP(s); err != nil {
						log.Println("OnSIGHUP:", err)
					}
				}
			})

		// SIGINT should exit.
		case syscall.SIGINT:
			return syscall.SIGINT, nil

		// SIGQUIT should exit gracefully.
		case syscall.SIGQUIT:
			a.Range(func(s *Service) {
				if s.Hooks.OnSIGQUIT != nil {
					if err := s.Hooks.OnSIGQUIT(s); err != nil {
						log.Println("OnSIGQUIT:", err)
					}
				}
			})
			return syscall.SIGQUIT, nil

		// SIGTERM should exit.
		case syscall.SIGTERM:
			return syscall.SIGTERM, nil

		// SIGUSR1 should reopen logs.
		case syscall.SIGUSR1:
			a.Range(func(s *Service) {
				if s.Hooks.OnSIGHUP != nil {
					if err := s.Hooks.OnSIGUSR1(s); err != nil {
						log.Println("OnSIGUSR1:", err)
					}
				}
			})

		// SIGUSR2 forks and re-execs the first time it is received and execs
		// without forking from then on.
		case syscall.SIGUSR2:
			if OnForkHook != nil {
				OnForkHook()
			}
			if forked {
				return syscall.SIGUSR2, nil
			}
			forked = true
			if err := ForkExec(a); nil != err {
				return syscall.SIGUSR2, err
			}

		}
	}
}

func lookPath() (argv0 string, err error) {
	argv0, err = exec.LookPath(os.Args[0])
	if nil != err {
		return
	}
	if _, err = os.Stat(argv0); nil != err {
		return
	}
	return
}

func setEnvs(a *Again) error {
	e, err := a.Env()
	if err != nil {
		return err
	}
	for k, v := range e {
		os.Setenv(k, v)
	}
	return nil
}
