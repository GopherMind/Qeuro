package clientcfg

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func loadStoredToken() (string, error) {
	p, err := tokenPath()
	if err != nil {
		return "", err
	}
	// #nosec G304 -- p is this process's own token path under the user's home
	// directory, derived by tokenPath(); it is not caller-supplied.
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	in := blob(data)
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, 0, &out); err != nil {
		return "", fmt.Errorf("clientcfg: decrypt token: %w", err)
	}
	// #nosec G103 -- LocalFree takes the DPAPI-allocated buffer as a handle;
	// unsafe.Pointer is the required conversion for the Win32 ABI.
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	plain := blobBytes(out)
	return string(plain), nil
}

func saveStoredToken(token string) error {
	p, err := tokenPath()
	if err != nil {
		return err
	}
	if token == "" {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	in := blob([]byte(token))
	name, err := windows.UTF16PtrFromString("qeuro cli token")
	if err != nil {
		return err
	}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, name, nil, 0, nil, 0, &out); err != nil {
		return fmt.Errorf("clientcfg: encrypt token: %w", err)
	}
	// #nosec G103 -- LocalFree takes the DPAPI-allocated buffer as a handle;
	// unsafe.Pointer is the required conversion for the Win32 ABI.
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return os.WriteFile(p, blobBytes(out), 0o600)
}

func omitTokenFromConfig() bool {
	return true
}

// storedTokenPresent reports whether a token is stored, without decrypting it.
//
// On Windows this is a stat: DPAPI decryption is the expensive half, and whether
// the user is signed in is answerable from the file existing. That is what lets
// the startup path draw the right status line without touching the secret
// (roadmap §8 "Startup").
func storedTokenPresent() bool {
	p, err := tokenPath()
	if err != nil {
		return false
	}
	info, err := os.Stat(p)
	// A zero-length token.dat is not a token: `qeuro logout` removes the file, so
	// an empty one means an interrupted write, and reporting "signed in" for it
	// would send unauthenticated requests and show 401s instead of the offline
	// notice.
	return err == nil && info.Size() > 0
}

func tokenStorageWarning() string {
	return ""
}

func blob(data []byte) windows.DataBlob {
	if len(data) == 0 {
		return windows.DataBlob{}
	}
	// #nosec G115 -- a token blob is a few hundred bytes; len cannot approach
	// uint32 overflow, and DATA_BLOB.cbData is uint32 by ABI.
	return windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
}

func blobBytes(b windows.DataBlob) []byte {
	if b.Size == 0 || b.Data == nil {
		return nil
	}
	// #nosec G103 -- unsafe.Slice over the DPAPI-returned buffer is the only way
	// to read a DATA_BLOB; the contents are copied out immediately below.
	src := unsafe.Slice(b.Data, int(b.Size))
	out := make([]byte, len(src))
	copy(out, src)
	return out
}
