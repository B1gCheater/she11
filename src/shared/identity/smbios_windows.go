//go:build windows

package identity

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var getSystemFirmwareTable = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetSystemFirmwareTable")

func readSMBIOS() ([]byte, error) {
	if err := getSystemFirmwareTable.Find(); err != nil {
		return nil, err
	}
	const provider = 0x52534d42 // 'RSMB', raw SMBIOS provider
	size, _, err := getSystemFirmwareTable.Call(provider, 0, 0, 0)
	if size < 8 || size > 16<<20 {
		return nil, fmt.Errorf("SMBIOS size %d: %v", size, err)
	}
	data := make([]byte, int(size))
	written, _, err := getSystemFirmwareTable.Call(provider, 0, uintptr(unsafe.Pointer(&data[0])), size)
	if written < 8 || written > size {
		return nil, fmt.Errorf("SMBIOS read size %d: %v", written, err)
	}
	return data[:int(written)], nil
}

// smbiosIdentity reads the RawSMBIOSData header followed by SMBIOS structures.
// Prefer the system UUID (Type 1). The baseboard serial (Type 2) is a fallback.
// Ignore unrelated firmware fields so changing RAM or a BIOS version does not
// by itself change the identity. UUID bytes are hashed, never displayed as a GUID.
func smbiosIdentity(raw []byte) string {
	if len(raw) < 8 {
		return ""
	}
	length := uint64(binary.LittleEndian.Uint32(raw[4:8]))
	if length > uint64(len(raw)-8) {
		return ""
	}
	data := raw[8 : 8+int(length)]
	var boardSerial string
	for len(data) > 0 {
		if len(data) < 4 {
			return ""
		}
		kind, size := data[0], int(data[1])
		if size < 4 || size > len(data) {
			return ""
		}
		end := bytes.Index(data[size:], []byte{0, 0})
		if end < 0 {
			return ""
		}
		switch kind {
		case 1:
			if size >= 24 {
				uuid := data[8:24]
				if !bytes.Equal(uuid, bytes.Repeat([]byte{0}, 16)) && !bytes.Equal(uuid, bytes.Repeat([]byte{0xff}, 16)) {
					return "uuid:" + hex.EncodeToString(uuid)
				}
			}
		case 2:
			if size >= 8 && boardSerial == "" {
				index := int(data[7])
				values := bytes.Split(data[size:size+end], []byte{0})
				if index > 0 && index <= len(values) {
					boardSerial = validBoardSerial(string(values[index-1]))
				}
			}
		case 127:
			data = nil
			continue
		}
		data = data[size+end+2:]
	}
	if boardSerial != "" {
		return "board:" + boardSerial
	}
	return ""
}

func validBoardSerial(serial string) string {
	serial = strings.ToUpper(strings.TrimSpace(serial))
	compact := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '-' || r == '_' || r == '.' {
			return -1
		}
		return r
	}, serial)
	switch compact {
	case "", "TOBEFILLEDBYOEM", "DEFAULTSTRING", "NONE", "UNKNOWN", "INVALID", "NOTSPECIFIED", "NOTAPPLICABLE", "UNSPECIFIED", "SYSTEMSERIALNUMBER", "BASEBOARDSERIALNUMBER", "123456789", "0123456789":
		return ""
	}
	if strings.Trim(compact, "0") == "" || strings.Trim(compact, "F") == "" {
		return ""
	}
	return serial
}
