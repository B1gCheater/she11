//go:build windows

package identity

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func rawSMBIOSTestData(records ...[]byte) []byte {
	raw := make([]byte, 8)
	for _, record := range records {
		raw = append(raw, record...)
	}
	binary.LittleEndian.PutUint32(raw[4:8], uint32(len(raw)-8))
	return raw
}

func testSystemRecord(uuid []byte) []byte {
	record := make([]byte, 26)
	record[0], record[1] = 1, 24
	copy(record[8:24], uuid)
	return record
}

func testBoardRecord(serial string) []byte {
	record := []byte{2, 8, 0, 0, 0, 0, 0, 1}
	return append(record, append([]byte(serial), 0, 0)...)
}

func TestClonedWindowsImagesGetDistinctIDs(t *testing.T) {
	legacy := []byte("win:identical-cloned-machine-guid")
	uuidA := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	uuidB := append([]byte(nil), uuidA...)
	uuidB[15]++
	secretA := windowsIdentitySecret(legacy, smbiosIdentity(rawSMBIOSTestData(testSystemRecord(uuidA))))
	secretB := windowsIdentitySecret(legacy, smbiosIdentity(rawSMBIOSTestData(testSystemRecord(uuidB))))
	if ClientID(secretA) == ClientID(secretB) || FingerprintHex(secretA) == FingerprintHex(secretB) {
		t.Fatal("different hardware with cloned MachineGuid produced the same identity")
	}
	if len(ClientID(secretA)) != 8 {
		t.Fatal("client ID wire format changed")
	}
	// The same firmware information and OS identity reproduce the same ID.
	again := windowsIdentitySecret(legacy, smbiosIdentity(rawSMBIOSTestData(testSystemRecord(uuidA))))
	if !bytes.Equal(secretA, again) {
		t.Fatal("identity is not deterministic")
	}
}

func TestSMBIOSIdentityFallbackAndBounds(t *testing.T) {
	uuid := bytes.Repeat([]byte{0x42}, 16)
	for _, tc := range []struct {
		name string
		raw  []byte
		want string
	}{
		{"UUID preferred over board", rawSMBIOSTestData(testBoardRecord("BOARD-A12"), testSystemRecord(uuid)), "uuid:" + strings.Repeat("42", 16)},
		{"all zero UUID", rawSMBIOSTestData(testSystemRecord(make([]byte, 16)), testBoardRecord(" board-a12 ")), "board:BOARD-A12"},
		{"all FF UUID", rawSMBIOSTestData(testSystemRecord(bytes.Repeat([]byte{0xff}, 16)), testBoardRecord("BOARD-A12")), "board:BOARD-A12"},
		{"manufacturer placeholder", rawSMBIOSTestData(testBoardRecord("To Be Filled By O.E.M.")), ""},
		{"zero serial", rawSMBIOSTestData(testBoardRecord("0000-0000")), ""},
		{"empty data", nil, ""},
		{"short raw header", []byte{1, 2, 3}, ""},
		{"invalid record size", rawSMBIOSTestData([]byte{1, 2, 0, 0, 0, 0}), ""},
		{"truncated record", rawSMBIOSTestData([]byte{1, 24, 0, 0}), ""},
		{"missing string terminator", rawSMBIOSTestData([]byte{2, 8, 0, 0, 0, 0, 0, 1, 'A'}), ""},
		{"invalid string index", rawSMBIOSTestData([]byte{2, 8, 0, 0, 0, 0, 0, 2, 'A', 0, 0}), ""},
		{"table end", rawSMBIOSTestData([]byte{127, 4, 0, 0, 0, 0}, testBoardRecord("BOARD-A12")), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := smbiosIdentity(tc.raw); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if tc.want == "" {
				legacy := []byte("win:legacy-guid")
				if !bytes.Equal(windowsIdentitySecret(legacy, smbiosIdentity(tc.raw)), legacy) {
					t.Fatal("missing hardware changed the legacy identity")
				}
			}
		})
	}
	raw := rawSMBIOSTestData(testBoardRecord("BOARD-A12"))
	binary.LittleEndian.PutUint32(raw[4:8], 0xffffffff)
	if got := smbiosIdentity(raw); got != "" {
		t.Fatal("accepted raw length beyond buffer")
	}
}

func TestLiveWindowsFirmwareIdentity(t *testing.T) {
	if os.Getenv("SHE11_IDENTITY_EXPECT_UNELEVATED") == "1" && windows.GetCurrentProcessToken().IsElevated() {
		t.Fatal("ordinary-user probe unexpectedly elevated")
	}
	raw, err := readSMBIOS()
	if err != nil {
		t.Skipf("firmware API unavailable: %v", err)
	}
	hardware := smbiosIdentity(raw)
	if hardware == "" {
		t.Skip("firmware contains no usable identity")
	}
	first, err := MachineSecret()
	if err != nil {
		t.Fatal(err)
	}
	second, err := MachineSecret()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || !bytes.Equal(first, windowsIdentitySecret(legacyWindowsSecret(), hardware)) {
		t.Fatal("live identity does not consistently include firmware")
	}
	kind, _, _ := strings.Cut(hardware, ":")
	t.Logf("firmware source=%s, elevated=%t, repeated identity stable", kind, windows.GetCurrentProcessToken().IsElevated())
}

func TestUnelevatedWindowsFirmwareIdentity(t *testing.T) {
	current := windows.GetCurrentProcessToken()
	if !current.IsElevated() {
		t.Skip("live firmware test already runs without elevation")
	}
	linked, err := current.GetLinkedToken()
	if err != nil {
		t.Skipf("no linked ordinary-user token: %v", err)
	}
	defer linked.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestLiveWindowsFirmwareIdentity$", "-test.v")
	cmd.Env = append(os.Environ(), "SHE11_IDENTITY_EXPECT_UNELEVATED=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Token: syscall.Token(linked), HideWindow: true}
	output, err := cmd.CombinedOutput()
	if err != nil {
		if cmd.ProcessState != nil {
			t.Fatalf("ordinary-user firmware probe failed: %v: %s", err, output)
		}
		t.Skipf("cannot launch linked-token firmware probe: %v", err)
	}
	if !strings.Contains(string(output), "firmware source=") {
		t.Skipf("firmware unavailable to linked token: %s", output)
	}
	t.Logf("ordinary-user probe: %s", strings.TrimSpace(string(output)))
}
