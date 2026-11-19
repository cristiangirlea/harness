//go:build windows

package prefs

import (
	"path/filepath"
	"syscall"
	"unsafe"
)

var (
	advapi32                 = syscall.NewLazyDLL("advapi32.dll")
	procGetNamedSecurityInfo = advapi32.NewProc("GetNamedSecurityInfoW")
	procEqualSid             = advapi32.NewProc("EqualSid")
)

func withOrgOwner(path string, check func(*syscall.SID) bool) (bool, error) {
	const seFileObject, ownerSecurityInformation = 1, 1
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	var owner *syscall.SID
	var descriptor syscall.Handle
	status, _, _ := procGetNamedSecurityInfo.Call(uintptr(unsafe.Pointer(name)), seFileObject, ownerSecurityInformation,
		uintptr(unsafe.Pointer(&owner)), 0, 0, 0, uintptr(unsafe.Pointer(&descriptor)))
	if status != 0 {
		return false, syscall.Errno(status)
	}
	defer syscall.LocalFree(descriptor)
	return owner != nil && check(owner), nil
}

func adminOrgOwner(owner *syscall.SID) bool {
	for _, sid := range []string{
		"S-1-5-32-544",
		"S-1-5-18",
		"S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464",
	} {
		trusted, err := syscall.StringToSid(sid)
		if err != nil {
			return false
		}
		equal, _, _ := procEqualSid.Call(uintptr(unsafe.Pointer(owner)), uintptr(unsafe.Pointer(trusted)))
		if equal != 0 {
			return true
		}
	}
	return false
}

func trustedOrgPath(path string) bool {
	for _, name := range []string{filepath.Dir(path), path} {
		trusted, err := withOrgOwner(name, adminOrgOwner)
		if err != nil || !trusted {
			return false
		}
	}
	return true
}
