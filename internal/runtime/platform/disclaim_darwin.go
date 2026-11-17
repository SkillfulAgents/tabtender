package platform

import (
	"errors"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
)

// posixSpawnSetExec makes posix_spawn replace the calling process, like exec.
const posixSpawnSetExec = 0x0040

// DisclaimSupported reports whether DisclaimExec is available.
const DisclaimSupported = true

// DisclaimExec replaces the current process with the given program, started
// as its own "responsible process".
//
// macOS attributes what a process does to the application responsible for
// it, which by default is whoever launched it. A browser started by the
// daemon would therefore act in Tabtender's name: when Chrome maintains its
// own app bundle, the system reports that "Tabtender was prevented from
// modifying apps". Disclaiming responsibility makes the browser answer for
// itself, exactly as when it is opened from the Finder.
//
// On success DisclaimExec does not return. If the disclaim facility is not
// available, the program is executed without it.
func DisclaimExec(path string, argv, env []string) error {
	lib, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return syscall.Exec(path, argv, env)
	}
	symbols := map[string]uintptr{}
	for _, name := range []string{
		"posix_spawnattr_init", "posix_spawnattr_setflags",
		"responsibility_spawnattrs_setdisclaim", "posix_spawn",
	} {
		if symbols[name], err = purego.Dlsym(lib, name); err != nil {
			return syscall.Exec(path, argv, env)
		}
	}
	var (
		attrInit    func(attr *uintptr) int32
		setFlags    func(attr *uintptr, flags int16) int32
		setDisclaim func(attr *uintptr, disclaim int32) int32
		spawn       func(pid *int32, path *byte, fileActions unsafe.Pointer, attr *uintptr, argv, envp **byte) int32
	)
	purego.RegisterFunc(&attrInit, symbols["posix_spawnattr_init"])
	purego.RegisterFunc(&setFlags, symbols["posix_spawnattr_setflags"])
	purego.RegisterFunc(&setDisclaim, symbols["responsibility_spawnattrs_setdisclaim"])
	purego.RegisterFunc(&spawn, symbols["posix_spawn"])

	cPath, err := syscall.BytePtrFromString(path)
	if err != nil {
		return err
	}
	cArgv, err := syscall.SlicePtrFromStrings(argv)
	if err != nil {
		return err
	}
	cEnv, err := syscall.SlicePtrFromStrings(env)
	if err != nil {
		return err
	}

	var attr uintptr
	if rc := attrInit(&attr); rc != 0 {
		return syscall.Errno(rc)
	}
	if rc := setFlags(&attr, posixSpawnSetExec); rc != 0 {
		return syscall.Errno(rc)
	}
	if rc := setDisclaim(&attr, 1); rc != 0 {
		return syscall.Exec(path, argv, env)
	}
	// With POSIX_SPAWN_SETEXEC this only returns on failure.
	rc := spawn(nil, cPath, nil, &attr, &cArgv[0], &cEnv[0])
	runtime.KeepAlive(cPath)
	runtime.KeepAlive(cArgv)
	runtime.KeepAlive(cEnv)
	if rc == 0 {
		return errors.New("posix_spawn returned without replacing the process")
	}
	return syscall.Errno(rc)
}

// ResponsiblePID returns the process macOS holds responsible for pid.
func ResponsiblePID(pid int) (int, error) {
	lib, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return 0, err
	}
	symbol, err := purego.Dlsym(lib, "responsibility_get_pid_responsible_for_pid")
	if err != nil {
		return 0, err
	}
	var responsible func(pid int32) int32
	purego.RegisterFunc(&responsible, symbol)
	return int(responsible(int32(pid))), nil
}
