package service

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// secureDataDir makes sure the data directory of a Windows Service is
// accessible only to SYSTEM, Administrators and the service account, and
// that nothing in it was put there by anybody else.
//
// The default location, %ProgramData%, lets every local user create files
// and directories. A user who got there first could plant a configuration
// file, a helper executable or a provisioned API key for the service to act
// on. So:
//
//   - a missing directory is created with the restricted ACL in one step;
//   - an existing directory must be owned by a trusted account;
//   - an existing directory that is not restricted yet is restricted, and
//     then refused if anything inside it belongs to an untrusted account.
//
// install.ps1 applies the same rules before the service first starts; this
// covers services registered by other means.
func secureDataDir(dir string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	self := user.User.Sid
	trusted := func(sid *windows.SID) bool {
		return sid != nil && (sid.Equals(self) ||
			sid.IsWellKnown(windows.WinLocalSystemSid) ||
			sid.IsWellKnown(windows.WinBuiltinAdministratorsSid))
	}

	restricted, err := windows.SecurityDescriptorFromString(
		"D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + self.String() + ")")
	if err != nil {
		return err
	}
	dacl, _, err := restricted.DACL()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	path, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	sa := windows.SecurityAttributes{SecurityDescriptor: restricted}
	sa.Length = uint32(unsafe.Sizeof(sa))
	err = windows.CreateDirectory(path, &sa)
	if err == nil {
		return nil
	}
	if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return fmt.Errorf("create data directory %s: %w", dir, err)
	}

	attrs, err := windows.GetFileAttributes(path)
	if err != nil {
		return fmt.Errorf("inspect data directory %s: %w", dir, err)
	}
	if attrs&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("data directory %s is not a plain directory", dir)
	}
	current, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read the security descriptor of %s: %w", dir, err)
	}
	owner, _, err := current.Owner()
	if err != nil {
		return err
	}
	if !trusted(owner) {
		return fmt.Errorf("data directory %s is owned by %s, which is neither an administrator nor the service account; "+
			"remove it or reinstall with install.ps1", dir, owner)
	}
	if isRestricted(current, trusted) {
		return nil
	}

	setDACL := func(protection windows.SECURITY_INFORMATION) error {
		return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|protection, nil, nil, dacl, nil)
	}
	// Restrict first, then inspect: once the ACL is in place only owners of
	// existing entries can still change the contents, and those are exactly
	// what the inspection finds.
	if err := setDACL(windows.PROTECTED_DACL_SECURITY_INFORMATION); err != nil {
		return fmt.Errorf("restrict data directory %s (run install.ps1 to prepare it for this service account): %w", dir, err)
	}
	if err := checkContentsTrusted(dir, trusted); err != nil {
		// Leave the directory recognizably unrestricted so that the
		// inspection runs again on the next start instead of being skipped.
		_ = setDACL(windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
		return err
	}
	return nil
}

// isRestricted reports whether the DACL is cut off from the parent and
// grants access to trusted accounts only.
func isRestricted(sd *windows.SECURITY_DESCRIPTOR, trusted func(*windows.SID) bool) bool {
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return false
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return false
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return false
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return false
		}
		if !trusted((*windows.SID)(unsafe.Pointer(&ace.SidStart))) {
			return false
		}
	}
	return true
}

// checkContentsTrusted fails if any entry below root is a link or is owned
// by an untrusted account. The owner of a file can always rewrite it and its
// ACL, so such an entry stays under that account's control even inside a
// restricted directory.
func checkContentsTrusted(root string, trusted func(*windows.SID) bool) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		// Junctions are reported as irregular files; neither kind of link
		// is followed by WalkDir.
		if entry.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			return fmt.Errorf("data directory entry %s is a link; remove it", path)
		}
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
		if err != nil {
			return fmt.Errorf("read the owner of %s: %w", path, err)
		}
		owner, _, err := sd.Owner()
		if err != nil {
			return err
		}
		if !trusted(owner) {
			return fmt.Errorf("data directory entry %s is owned by %s, which is neither an administrator nor the service account; "+
				"it may have been planted, remove it", path, owner)
		}
		return nil
	})
}
