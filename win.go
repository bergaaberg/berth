//go:build windows

package main

// Windows calls, straight to the system DLLs (no packages to fetch): named pipes with this user's
// SID as their only reader and writer, a lock so one server runs, the process list, the
// certificate store, and telling Explorer the user's PATH changed.

import (
	"bufio"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procCreateNamedPipeW    = kernel32.NewProc("CreateNamedPipeW")
	procConnectNamedPipe    = kernel32.NewProc("ConnectNamedPipe")
	procCreateEventW        = kernel32.NewProc("CreateEventW")
	procGetOverlappedResult = kernel32.NewProc("GetOverlappedResult")
	procCancelIoEx          = kernel32.NewProc("CancelIoEx")
	procFlushFileBuffers    = kernel32.NewProc("FlushFileBuffers")
	procDisconnectNamedPipe = kernel32.NewProc("DisconnectNamedPipe")
	procWaitNamedPipeW      = kernel32.NewProc("WaitNamedPipeW")
	procCreateMutexW        = kernel32.NewProc("CreateMutexW")
	procOpenMutexW          = kernel32.NewProc("OpenMutexW")

	advapi32                = syscall.NewLazyDLL("advapi32.dll")
	procConvertStringSDToSD = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")

	user32                  = syscall.NewLazyDLL("user32.dll")
	procSendMessageTimeoutW = user32.NewProc("SendMessageTimeoutW")
)

const (
	pipeAccessDuplex        = 0x3
	fileFlagFirstPipeInst   = 0x00080000
	fileFlagOverlapped      = 0x40000000
	pipeRejectRemoteClients = 0x8
	pipeUnlimitedInstances  = 255
	errorPipeConnected      = 535
	errorIOPending          = 997
	errorFileNotFound       = 2
	errorAlreadyExists      = 183
	sddlRevision1           = 1
	synchronize             = 0x00100000

	createNoWindow        = 0x08000000
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200

	hwndBroadcast   = 0xffff
	wmSettingChange = 0x001A
	smtoAbortIfHung = 0x0002
)

const lockName = `Local\berth-serve`

// pipeServed: something serves this pipe now. Busy (every instance taken) still means served;
// only "no such pipe" doesn't.
func pipeServed(name string) bool {
	p, _ := syscall.UTF16PtrFromString(PipePath(name))
	r, _, err := procWaitNamedPipeW.Call(uintptr(unsafe.Pointer(p)), 1)
	return r != 0 || err != syscall.Errno(errorFileNotFound)
}

func userSID() (string, error) {
	t, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return "", err
	}
	defer t.Close()
	u, err := t.GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String()
}

func pipeSecurity() (*syscall.SecurityAttributes, error) {
	sid, err := userSID()
	if err != nil {
		return nil, err
	}
	s, _ := syscall.UTF16PtrFromString(PipeSDDL(sid))
	var sd uintptr
	if r, _, err := procConvertStringSDToSD.Call(uintptr(unsafe.Pointer(s)), sddlRevision1, uintptr(unsafe.Pointer(&sd)), 0); r == 0 {
		return nil, err
	}
	sa := &syscall.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	return sa, nil
}

func newInstance(name string, sa *syscall.SecurityAttributes, first bool) (syscall.Handle, error) {
	p, _ := syscall.UTF16PtrFromString(PipePath(name))
	mode := uint32(pipeAccessDuplex | fileFlagOverlapped)
	if first {
		// Fails if someone else made this pipe first: we never share a name with a squatter.
		mode |= fileFlagFirstPipeInst
	}
	h, _, err := procCreateNamedPipeW.Call(uintptr(unsafe.Pointer(p)), uintptr(mode), pipeRejectRemoteClients,
		pipeUnlimitedInstances, 64<<10, 64<<10, 0, uintptr(unsafe.Pointer(sa)))
	if syscall.Handle(h) == syscall.InvalidHandle {
		return syscall.InvalidHandle, err
	}
	return syscall.Handle(h), nil
}

// connect waits for a client on an overlapped instance; CancelIoEx on h ends the wait.
func connect(h syscall.Handle) error {
	ev, _, err := procCreateEventW.Call(0, 1, 0, 0)
	if ev == 0 {
		return err
	}
	defer syscall.CloseHandle(syscall.Handle(ev))
	var ov syscall.Overlapped
	ov.HEvent = syscall.Handle(ev)
	r, _, err := procConnectNamedPipe.Call(uintptr(h), uintptr(unsafe.Pointer(&ov)))
	if r != 0 || err == syscall.Errno(errorPipeConnected) {
		return nil
	}
	if err != syscall.Errno(errorIOPending) {
		return err
	}
	var n uint32
	if r, _, err := procGetOverlappedResult.Call(uintptr(h), uintptr(unsafe.Pointer(&ov)), uintptr(unsafe.Pointer(&n)), 1); r == 0 {
		return err
	}
	return nil
}

// Listener serves one pipe name until Close: each client gets its own instance, handed to relay.
type Listener struct {
	name   string
	mu     sync.Mutex
	h      syscall.Handle // the instance waiting for a client
	closed bool
	done   chan struct{}
}

// Listen makes the pipe (first instance: fails if anyone else has the name) and serves it.
func Listen(name string, relay func(syscall.Handle)) (*Listener, error) {
	sa, err := pipeSecurity()
	if err != nil {
		return nil, err
	}
	h, err := newInstance(name, sa, true)
	if err != nil {
		return nil, err
	}
	l := &Listener{name: name, h: h, done: make(chan struct{})}
	go func() {
		defer close(l.done)
		for {
			err := connect(h)
			l.mu.Lock()
			closed := l.closed
			l.mu.Unlock()
			if closed {
				_ = syscall.CloseHandle(h)
				return
			}
			if err != nil {
				_ = syscall.CloseHandle(h)
			} else {
				go relay(h)
			}
			for {
				l.mu.Lock()
				if l.closed {
					l.mu.Unlock()
					return
				}
				h, err = newInstance(name, sa, false)
				if err == nil {
					l.h = h
					l.mu.Unlock()
					break
				}
				l.mu.Unlock()
				time.Sleep(time.Second)
			}
		}
	}()
	return l, nil
}

// Close stops taking clients; the name is free once the clients it has go (their relays end
// when the distro does).
func (l *Listener) Close() {
	l.mu.Lock()
	l.closed = true
	procCancelIoEx.Call(uintptr(l.h), 0)
	l.mu.Unlock()
	select {
	case <-l.done:
	case <-time.After(3 * time.Second):
	}
}

// endClient lets go of one client's instance.
func endClient(h syscall.Handle) {
	procFlushFileBuffers.Call(uintptr(h))
	procDisconnectNamedPipe.Call(uintptr(h))
}

// TakeLock: true when this is the one server; the lock lasts as long as the process.
func TakeLock() bool {
	name, _ := syscall.UTF16PtrFromString(lockName)
	h, _, err := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		return false
	}
	if err == syscall.Errno(errorAlreadyExists) {
		_ = syscall.CloseHandle(syscall.Handle(h))
		return false
	}
	return true
}

// Serving: whether a server holds the lock.
func Serving() bool {
	name, _ := syscall.UTF16PtrFromString(lockName)
	h, _, _ := procOpenMutexW.Call(synchronize, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		return false
	}
	_ = syscall.CloseHandle(syscall.Handle(h))
	return true
}

// DesktopRunning: Docker Desktop (or its backend) is running for anyone on this machine.
func DesktopRunning() bool {
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(snap)
	var e syscall.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = syscall.Process32First(snap, &e); err == nil; err = syscall.Process32Next(snap, &e) {
		if IsDesktopProcess(syscall.UTF16ToString(e.ExeFile[:])) {
			return true
		}
	}
	return false
}

// hide: a child without a console window of its own.
func hide(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

// Detached starts a program that outlives this one, without a console window.
func Detached(bin string, args ...string) error {
	cmd := exec.Command(bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: detachedProcess | createNewProcessGroup}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// RootCerts: every certificate Windows trusts as a root for this user (its own, the machine's and
// policy's), as DER.
func RootCerts() ([][]byte, error) {
	name, _ := syscall.UTF16PtrFromString("ROOT")
	store, err := syscall.CertOpenSystemStore(0, name)
	if err != nil {
		return nil, err
	}
	defer syscall.CertCloseStore(store, 0)
	var out [][]byte
	var ctx *syscall.CertContext
	for {
		ctx, err = syscall.CertEnumCertificatesInStore(store, ctx)
		if err != nil || ctx == nil {
			break
		}
		der := unsafe.Slice(ctx.EncodedCert, ctx.Length)
		out = append(out, append([]byte(nil), der...))
	}
	return out, nil
}

// SettingChanged tells Explorer (and so new terminals) that the user's environment changed.
func SettingChanged() {
	env, _ := syscall.UTF16PtrFromString("Environment")
	var res uintptr
	procSendMessageTimeoutW.Call(hwndBroadcast, wmSettingChange, 0, uintptr(unsafe.Pointer(env)), smtoAbortIfHung, 2000, uintptr(unsafe.Pointer(&res)))
}

// EngineVersion asks the engine on a pipe for its version (GET /version), or says why not.
func EngineVersion(pipe string, within time.Duration) (string, error) {
	p, _ := syscall.UTF16PtrFromString(PipePath(pipe))
	r, _, _ := procWaitNamedPipeW.Call(uintptr(unsafe.Pointer(p)), uintptr(within/time.Millisecond))
	if r == 0 {
		return "", errors.New("nothing serves " + PipePath(pipe))
	}
	f, err := os.OpenFile(PipePath(pipe), os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-done:
		case <-time.After(within):
			_ = f.Close()
		}
	}()
	if _, err := f.WriteString("GET /version HTTP/1.0\r\nHost: docker\r\n\r\n"); err != nil {
		return "", err
	}
	res, err := http.ReadResponse(bufio.NewReader(f), nil)
	if err != nil {
		return "", errors.New("the engine didn't answer")
	}
	defer res.Body.Close()
	var buf strings.Builder
	_, _ = bufio.NewReader(res.Body).WriteTo(&buf)
	if res.StatusCode != 200 {
		return "", errors.New("the engine answered " + res.Status)
	}
	return jsonString(buf.String(), "Version"), nil
}
