//go:build windows

package identity

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows/registry"
)

// MachineSecret combines the existing OS identity with a firmware identity so
// cloned Windows images on different physical machines can be distinguished.
// Firmware access is best-effort and requires no elevation or runtime files.
func MachineSecret() ([]byte, error) {
	legacy := legacyWindowsSecret()
	data, err := readSMBIOS()
	if err != nil {
		return legacy, nil
	}
	hardware := smbiosIdentity(data)
	return windowsIdentitySecret(legacy, hardware), nil
}

func legacyWindowsSecret() []byte {
	if guid, err := registryMachineGuid(); err == nil && guid != "" {
		return []byte("win:" + guid)
	}
	hn, _ := os.Hostname()
	cn := os.Getenv("COMPUTERNAME")
	return fmt.Appendf(nil, "win:%s|%s|%s", cn, hn, os.Getenv("SystemDrive"))
}

func windowsIdentitySecret(legacy []byte, hardware string) []byte {
	if hardware == "" {
		return legacy
	}
	return fmt.Appendf(nil, "win:v2:%d:%s:%s", len(legacy), legacy, hardware)
}

func registryMachineGuid() (string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer k.Close()
	s, _, err := k.GetStringValue("MachineGuid")
	return s, err
}
