//go:build windows

package prefs

import (
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

const seFileObject, ownerSecurityInformation = 1, 1

var (
	advapi32                 = syscall.NewLazyDLL("advapi32.dll")
	procGetNamedSecurityInfo = advapi32.NewProc("GetNamedSecurityInfoW")
	procGetSecurityInfo      = advapi32.NewProc("GetSecurityInfo")
	procEqualSid             = advapi32.NewProc("EqualSid")
)

func withOrgOwner(path string, check func(*syscall.SID) bool) (bool, error) {
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

func openTrustedOrg(path string) (*os.File, error) {
	skipOwner := path == orgPathOverride && orgPathTrusted
	dir := filepath.Dir(path)
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, orgOpenError(err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errOrgOwner
	}
	attrs, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return nil, errOrgRead
	}
	if attrs.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return nil, errOrgOwner
	}
	if !skipOwner {
		trusted, err := withOrgOwner(dir, adminOrgOwner)
		if err != nil {
			return nil, orgOpenError(err)
		}
		if !trusted {
			return nil, errOrgOwner
		}
	}
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, errOrgRead
	}
	// Backup semantics lets the handle check reject a directory in place of the file.
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ, nil, syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_OPEN_REPARSE_POINT|syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, orgOpenError(err)
	}
	f := os.NewFile(uintptr(h), path)
	if err := checkOrgFile(h, skipOwner); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func checkOrgFile(h syscall.Handle, skipOwner bool) error {
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		return errOrgRead
	}
	if info.FileAttributes&(syscall.FILE_ATTRIBUTE_DIRECTORY|syscall.FILE_ATTRIBUTE_REPARSE_POINT) != 0 || info.NumberOfLinks != 1 {
		return errOrgOwner
	}
	if skipOwner {
		return nil
	}
	var owner *syscall.SID
	var descriptor syscall.Handle
	status, _, _ := procGetSecurityInfo.Call(uintptr(h), seFileObject, ownerSecurityInformation,
		uintptr(unsafe.Pointer(&owner)), 0, 0, 0, uintptr(unsafe.Pointer(&descriptor)))
	if status != 0 {
		return errOrgRead
	}
	defer syscall.LocalFree(descriptor)
	if owner == nil || !adminOrgOwner(owner) {
		return errOrgOwner
	}
	return nil
}
