package secrets

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

const defaultBackend = BackendDPAPI

func newPlatformStore(dir string) (Store, error) {
	return newSealedStore(dir, sealer{backend: BackendDPAPI, ext: ".dpapi", seal: dpapiProtect, unseal: dpapiUnprotect})
}

func blob(data []byte) *windows.DataBlob {
	if len(data) == 0 {
		return &windows.DataBlob{}
	}
	return &windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
}

func takeBlob(b *windows.DataBlob) []byte {
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(b.Data))) //nolint:errcheck // nothing to do on failure
	return append([]byte(nil), unsafe.Slice(b.Data, b.Size)...)
}

// dpapiProtect encrypts data so that only the current account (the service
// account when running as a Windows Service) can decrypt it. The DPAPI keys
// live in that account's profile, which the Service Control Manager loads for
// every service account, including virtual accounts such as
// NT SERVICE\tabtenderd. The secret is therefore tied to the account: it
// cannot be read after the service is switched to another one.
func dpapiProtect(plain []byte) ([]byte, error) {
	var out windows.DataBlob
	if err := windows.CryptProtectData(blob(plain), nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	return takeBlob(&out), nil
}

func dpapiUnprotect(sealed []byte) ([]byte, error) {
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(blob(sealed), nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	return takeBlob(&out), nil
}

// restrictDir replaces the directory ACL with one granting access only to
// SYSTEM, the Administrators group, and the account running the service.
func restrictDir(dir string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sddl := "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + user.User.Sid.String() + ")"
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil)
}
