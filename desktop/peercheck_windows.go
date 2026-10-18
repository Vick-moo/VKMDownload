//go:build windows

package main

import (
	"fmt"
	"net"
	"strconv"
	"syscall"
	"unsafe"
)

const (
	tcpTableOwnerPidAll = 5
	afInet              = 2
)

const (
	// WinVerifyTrust state actions (wintrust.h).
	wtdStateActionVerify = 1
	wtdStateActionClose  = 2
	// Revocation check mode: none. A whole-chain check costs a network
	// round trip per new executable and breaks offline machines; the
	// signature itself (vendor key, intact file) is what impersonation
	// cannot forge. Revoked-but-still-signed is an accepted residual risk.
	wtdRevokeNone = 0
)

var (
	iphlpapi                = syscall.NewLazyDLL("iphlpapi.dll")
	procGetExtendedTcpTable = iphlpapi.NewProc("GetExtendedTcpTable")
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procOpenProcess         = kernel32.NewProc("OpenProcess")
	procQueryImageName      = kernel32.NewProc("QueryFullProcessImageNameW")
	procCloseHandle         = kernel32.NewProc("CloseHandle")
	wintrust                = syscall.NewLazyDLL("wintrust.dll")
	procWinVerifyTrust      = wintrust.NewProc("WinVerifyTrust")
)

// tcpOwnerPid returns the PID owning the client socket described by clientAddr
// (the server's view of the peer, "ip:port") connected to port on this machine.
//
// Matching only the client's local port was not enough, and the gap was a real
// bypass: a program that bound its source address to 127.0.0.2 (loopback is a
// whole /8 on Windows) produced a table row whose local address is not
// 127.0.0.1, so the lookup found nothing and the caller failed *open*. Matching
// the remote end as well — which is what makes a row the right socket — closes
// that without weakening anything else.
func tcpOwnerPid(clientAddr string, serverPort uint16) (uint32, error) {
	host, portStr, err := net.SplitHostPort(clientAddr)
	if err != nil {
		return 0, fmt.Errorf("bad peer address %q: %w", clientAddr, err)
	}
	clientIP := net.ParseIP(host)
	if clientIP == nil {
		return 0, fmt.Errorf("bad peer ip %q", host)
	}
	clientPort64, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("bad peer port %q: %w", portStr, err)
	}
	clientPort := uint16(clientPort64)

	var size uint32
	// Size-probe call; its return is intentionally ignored.
	procGetExtendedTcpTable.Call(0, uintptr(unsafe.Pointer(&size)),
		0, uintptr(afInet), uintptr(tcpTableOwnerPidAll), 0)
	if size < 4 {
		return 0, fmt.Errorf("GetExtendedTcpTable returned empty table")
	}
	buf := make([]byte, size)
	r1, _, _ := procGetExtendedTcpTable.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
		0, uintptr(afInet), uintptr(tcpTableOwnerPidAll), 0)
	if r1 != 0 {
		return 0, fmt.Errorf("GetExtendedTcpTable: %d", r1)
	}
	if len(buf) < 4 {
		return 0, fmt.Errorf("GetExtendedTcpTable returned short table")
	}
	n := *(*uint32)(unsafe.Pointer(&buf[0]))
	const rowSize = 24 // MIB_TCPROW_OWNER_PID: state + local + remote + pid
	for i := uint32(0); i < n; i++ {
		off := 4 + int(i)*rowSize
		if off+rowSize > len(buf) {
			break
		}
		localAddr := *(*uint32)(unsafe.Pointer(&buf[off+4]))
		localPort := ntohs(*(*uint32)(unsafe.Pointer(&buf[off+8])))
		remoteAddr := *(*uint32)(unsafe.Pointer(&buf[off+12]))
		remotePort := ntohs(*(*uint32)(unsafe.Pointer(&buf[off+16])))

		// The client socket is the row whose local end is the peer we are
		// talking to and whose remote end is this listener. Both ends have to
		// be loopback; the addresses are compared as 127.0.0.0/8 rather than
		// exactly 127.0.0.1 so a client that legitimately binds another
		// loopback address is still identified instead of silently skipped.
		if localPort != clientPort || remotePort != serverPort {
			continue
		}
		if !isLoopbackV4(localAddr) || !isLoopbackV4(remoteAddr) {
			continue
		}
		return *(*uint32)(unsafe.Pointer(&buf[off+20])), nil
	}
	return 0, fmt.Errorf("no owner for %s connected to port %d", clientAddr, serverPort)
}

// ntohs converts a port stored in the table's network byte order.
func ntohs(raw uint32) uint16 {
	return uint16(((raw & 0xFF) << 8) | ((raw >> 8) & 0xFF))
}

// isLoopbackV4 reports whether an address in the table is 127.0.0.0/8. The
// value is a DWORD in network byte order, so only its low byte varies.
func isLoopbackV4(addr uint32) bool {
	return addr&0x000000FF == 0x0000007F
}

// processImagePath returns the executable path of pid.
func processImagePath(pid uint32) (string, error) {
	const queryLimited = 0x1000
	h, _, errno := procOpenProcess.Call(
		uintptr(queryLimited), 0, uintptr(pid))
	if h == 0 {
		return "", fmt.Errorf("OpenProcess(%d): %v", pid, errno)
	}
	defer procCloseHandle.Call(h)
	// 32k units: MAX_PATH truncates long install paths.
	buf := make([]uint16, 32768)
	size := uint32(len(buf))
	r1, _, errno := procQueryImageName.Call(
		h, 0, uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)))
	if r1 == 0 {
		return "", fmt.Errorf("QueryFullProcessImageName(%d): %v", pid, errno)
	}
	return syscall.UTF16ToString(buf[:size]), nil
}

// winTrustActionGenericVerifyV2 is WINTRUST_ACTION_GENERIC_VERIFY_V2.
var winTrustActionGenericVerifyV2 = syscall.GUID{
	Data1: 0x00aac56b, Data2: 0xcd44, Data3: 0x11d0,
	Data4: [8]byte{0x8c, 0xc2, 0x00, 0xc0, 0x4f, 0xc2, 0x95, 0xee},
}

// verifyFileTrust reports whether path carries a valid Authenticode
// signature chaining to a trusted root.
func verifyFileTrust(path string) bool {
	path16, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	type fileInfo struct {
		cbStruct     uint32
		filePath     *uint16
		handle       syscall.Handle
		knownSubject *syscall.GUID
	}
	type wintrustData struct {
		cbStruct         uint32
		_                uint32 // pad: pointers below need 8-alignment
		policyCallback   unsafe.Pointer
		sipClientData    unsafe.Pointer
		uiChoice         uint32
		revocationChecks uint32
		unionChoice      uint32
		_2               uint32 // pad: union holds a pointer
		fileOrCatOrBlob  unsafe.Pointer
		stateAction      uint32
		_3               uint32 // pad: HANDLE below
		stateData        syscall.Handle
		urlReference     *uint16
		provFlags        uint32
		uiContext        uint32
	}
	var fiSizeCheck fileInfo
	var wdSizeCheck wintrustData
	if unsafe.Sizeof(fiSizeCheck) != 32 || unsafe.Sizeof(wdSizeCheck) != 80 {
		return false
	}
	fi := fileInfo{filePath: path16}
	fi.cbStruct = uint32(unsafe.Sizeof(fi))
	const choiceFile = 1
	wd := wintrustData{uiChoice: 2, revocationChecks: wtdRevokeNone,
		unionChoice:     choiceFile,
		fileOrCatOrBlob: unsafe.Pointer(&fi),
		stateAction:     wtdStateActionVerify}
	wd.cbStruct = uint32(unsafe.Sizeof(wd))
	r1, _, _ := procWinVerifyTrust.Call(
		0,
		uintptr(unsafe.Pointer(&winTrustActionGenericVerifyV2)),
		uintptr(unsafe.Pointer(&wd)))
	// Always close the state to avoid a per-call handle leak.
	wd.stateAction = wtdStateActionClose
	procWinVerifyTrust.Call(0,
		uintptr(unsafe.Pointer(&winTrustActionGenericVerifyV2)),
		uintptr(unsafe.Pointer(&wd)))
	return r1 == 0
}
