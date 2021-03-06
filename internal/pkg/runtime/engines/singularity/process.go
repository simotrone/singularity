// Copyright (c) 2018, Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package singularity

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"reflect"
	"strings"
	"syscall"
	"unsafe"

	"github.com/sylabs/singularity/internal/pkg/security"

	"github.com/sylabs/singularity/internal/pkg/util/mainthread"
	"github.com/sylabs/singularity/internal/pkg/util/user"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sylabs/singularity/internal/pkg/instance"
	"github.com/sylabs/singularity/internal/pkg/sylog"
	"golang.org/x/crypto/ssh/terminal"
)

func (engine *EngineOperations) checkExec() error {
	shell := engine.EngineConfig.GetShell()

	if shell == "" {
		shell = "/bin/sh"
	}

	args := engine.EngineConfig.OciConfig.Process.Args
	env := engine.EngineConfig.OciConfig.Process.Env

	// match old behavior of searching path
	oldpath := os.Getenv("PATH")
	defer func() {
		os.Setenv("PATH", oldpath)
		engine.EngineConfig.OciConfig.Process.Args = args
		engine.EngineConfig.OciConfig.Process.Env = env
	}()

	for _, keyval := range env {
		if strings.HasPrefix(keyval, "PATH=") {
			os.Setenv("PATH", keyval[5:])
			break
		}
	}

	// If args[0] is an absolute path, exec.LookPath() looks for
	// this file directly instead of within PATH
	if _, err := exec.LookPath(args[0]); err == nil {
		return nil
	}

	// If args[0] isn't executable (either via PATH or absolute path),
	// look for alternative approaches to handling it
	switch args[0] {
	case "/.singularity.d/actions/exec":
		if p, err := exec.LookPath("/.exec"); err == nil {
			args[0] = p
			return nil
		}
		if p, err := exec.LookPath(args[1]); err == nil {
			sylog.Warningf("container does not have %s, calling %s directly", args[0], args[1])
			args[1] = p
			args = args[1:]
			return nil
		}
		return fmt.Errorf("no executable %s found", args[1])
	case "/.singularity.d/actions/shell":
		if p, err := exec.LookPath("/.shell"); err == nil {
			args[0] = p
			return nil
		}
		if p, err := exec.LookPath(shell); err == nil {
			sylog.Warningf("container does not have %s, calling %s directly", args[0], shell)
			args[0] = p
			return nil
		}
		return fmt.Errorf("no %s found inside container", shell)
	case "/.singularity.d/actions/run":
		if p, err := exec.LookPath("/.run"); err == nil {
			args[0] = p
			return nil
		}
		if p, err := exec.LookPath("/singularity"); err == nil {
			args[0] = p
			return nil
		}
		return fmt.Errorf("no run driver found inside container")
	case "/.singularity.d/actions/start":
		if _, err := exec.LookPath(shell); err != nil {
			return fmt.Errorf("no %s found inside container, can't run instance", shell)
		}
		args = []string{shell, "-c", `echo "instance start script not found"`}
		return nil
	case "/.singularity.d/actions/test":
		if p, err := exec.LookPath("/.test"); err == nil {
			args[0] = p
			return nil
		}
		return fmt.Errorf("no test driver found inside container")
	}

	return fmt.Errorf("no %s found inside container", args[0])
}

// StartProcess starts the process
func (engine *EngineOperations) StartProcess(masterConn net.Conn) error {
	isInstance := engine.EngineConfig.GetInstance()
	bootInstance := isInstance && engine.EngineConfig.GetBootInstance()
	shimProcess := false

	if err := os.Chdir(engine.EngineConfig.OciConfig.Process.Cwd); err != nil {
		if err := os.Chdir(engine.EngineConfig.GetHomeDest()); err != nil {
			os.Chdir("/")
		}
	}

	if err := engine.checkExec(); err != nil {
		return err
	}

	if engine.EngineConfig.File.MountDev == "minimal" || engine.EngineConfig.GetContain() {
		// If on a terminal, reopen /dev/console so /proc/self/fd/[0-2
		//   will point to /dev/console.  This is needed so that tty and
		//   ttyname() on el6 will return the correct answer.  Newer
		//   ttyname() functions might work because they will search
		//   /dev if the value of /proc/self/fd/X doesn't exist, but
		//   they won't work if another /dev/pts/X is allocated in its
		//   place.  Also, programs that don't use ttyname() and instead
		//   directly do readlink() on /proc/self/fd/X need this.
		for fd := 0; fd <= 2; fd++ {
			if !terminal.IsTerminal(fd) {
				continue
			}
			consfile, err := os.OpenFile("/dev/console", os.O_RDWR, 0600)
			if err != nil {
				sylog.Debugf("Could not open minimal /dev/console, skipping replacing tty descriptors")
				break
			}
			sylog.Debugf("Replacing tty descriptors with /dev/console")
			consfd := int(consfile.Fd())
			for ; fd <= 2; fd++ {
				if !terminal.IsTerminal(fd) {
					continue
				}
				syscall.Close(fd)
				syscall.Dup3(consfd, fd, 0)
			}
			consfile.Close()
			break
		}
	}

	args := engine.EngineConfig.OciConfig.Process.Args
	env := engine.EngineConfig.OciConfig.Process.Env

	if engine.EngineConfig.OciConfig.Linux != nil {
		namespaces := engine.EngineConfig.OciConfig.Linux.Namespaces
		for _, ns := range namespaces {
			if ns.Type == specs.PIDNamespace {
				if !engine.EngineConfig.GetNoInit() {
					shimProcess = true
				}
				break
			}
		}
	}

	for _, img := range engine.EngineConfig.GetImageList() {
		if err := syscall.Close(int(img.Fd)); err != nil {
			return fmt.Errorf("failed to close file descriptor for %s", img.Path)
		}
	}

	for _, fd := range engine.EngineConfig.GetOpenFd() {
		if err := syscall.Close(fd); err != nil {
			return fmt.Errorf("aborting failed to close file descriptor: %s", err)
		}
	}

	if err := security.Configure(&engine.EngineConfig.OciConfig.Spec); err != nil {
		return fmt.Errorf("failed to apply security configuration: %s", err)
	}

	if (!isInstance && !shimProcess) || bootInstance || engine.EngineConfig.GetInstanceJoin() {
		err := syscall.Exec(args[0], args, env)
		return fmt.Errorf("exec %s failed: %s", args[0], err)
	}

	// Spawn and wait container process, signal handler
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = env

	var status syscall.WaitStatus
	errChan := make(chan error, 1)
	signals := make(chan os.Signal, 1)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("exec %s failed: %s", args[0], err)
	}

	go func() {
		errChan <- cmd.Wait()
	}()

	// Modify argv argument and program name shown in /proc/self/comm
	name := "sinit"

	argv0str := (*reflect.StringHeader)(unsafe.Pointer(&os.Args[0]))
	argv0 := (*[1 << 30]byte)(unsafe.Pointer(argv0str.Data))[:argv0str.Len]
	progname := make([]byte, argv0str.Len)

	if len(name) > argv0str.Len {
		return fmt.Errorf("program name too short")
	}

	copy(progname, name)
	copy(argv0, progname)

	ptr := unsafe.Pointer(&progname[0])
	if _, _, err := syscall.Syscall(syscall.SYS_PRCTL, syscall.PR_SET_NAME, uintptr(ptr), 0); err != 0 {
		return syscall.Errno(err)
	}

	// Manage all signals
	signal.Notify(signals)

	masterConn.Close()

	var lastSignal syscall.Signal

	for {
		select {
		case s := <-signals:
			sylog.Debugf("Received signal %s", s.String())
			switch s {
			case syscall.SIGCHLD:
				for {
					wpid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
					if wpid <= 0 || err != nil {
						break
					}
				}
			default:
				lastSignal = s.(syscall.Signal)
				if isInstance {
					if err := syscall.Kill(-1, lastSignal); err == syscall.ESRCH {
						sylog.Debugf("No child process, exiting ...")
						os.Exit(128 + int(lastSignal))
					}
				} else {
					// kill ourself with SIGKILL whatever signal was received
					syscall.Kill(syscall.Gettid(), syscall.SIGKILL)
				}
			}
		case err := <-errChan:
			if e, ok := err.(*exec.ExitError); ok {
				if status, ok := e.Sys().(syscall.WaitStatus); ok {
					if status.Signaled() {
						syscall.Kill(syscall.Gettid(), syscall.SIGKILL)
					}
					os.Exit(status.ExitStatus())
				}
				return fmt.Errorf("command exit with error: %s", err)
			} else if e, ok := err.(*os.SyscallError); ok {
				// handle possible race with Wait4 call above because command
				// execution can return "no such processes" error which means
				// container process execution has been interrupted by signal
				if e.Err.(syscall.Errno) == syscall.ECHILD {
					sylog.Debugf("No child processes, exiting ...")
					os.Exit(128 + int(lastSignal))
				}
			}
			if !isInstance {
				os.Exit(0)
			}
		}
	}
}

// PostStartProcess will execute code in smaster context after execution of container
// process, typically to write instance state/config files or execute post start OCI hook
func (engine *EngineOperations) PostStartProcess(pid int) error {
	sylog.Debugf("Post start process")

	if engine.EngineConfig.GetInstance() {
		uid := os.Getuid()
		gid := os.Getgid()
		name := engine.CommonConfig.ContainerID
		privileged := true

		if err := os.Chdir("/"); err != nil {
			return fmt.Errorf("failed to change directory to /: %s", err)
		}

		if engine.EngineConfig.OciConfig.Linux != nil {
			for _, ns := range engine.EngineConfig.OciConfig.Linux.Namespaces {
				if ns.Type == specs.UserNamespace {
					privileged = false
					break
				}
			}
		}

		file, err := instance.Add(name, privileged)
		if err != nil {
			return err
		}

		file.Config, err = json.Marshal(engine.CommonConfig)
		if err != nil {
			return err
		}

		pw, err := user.GetPwUID(uint32(uid))
		if err != nil {
			return err
		}
		file.User = pw.Name
		file.Pid = pid
		file.PPid = os.Getpid()
		file.Image = engine.EngineConfig.GetImage()

		if privileged {
			var err error

			mainthread.Execute(func() {
				if err = syscall.Setresuid(0, 0, uid); err != nil {
					err = fmt.Errorf("failed to escalate uid privileges")
					return
				}
				if err = syscall.Setresgid(0, 0, gid); err != nil {
					err = fmt.Errorf("failed to escalate gid privileges")
					return
				}
				if err = file.Update(); err != nil {
					return
				}
				if err = syscall.Setresgid(gid, gid, 0); err != nil {
					err = fmt.Errorf("failed to escalate gid privileges")
					return
				}
				if err := syscall.Setresuid(uid, uid, 0); err != nil {
					err = fmt.Errorf("failed to escalate uid privileges")
					return
				}
			})

			return err
		}

		return file.Update()
	}
	return nil
}
