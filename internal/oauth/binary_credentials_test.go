package oauth

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// The fixture is a valid ELF container with synthetic OAuth literals, not a copy
// of the installed application or any real application/account credentials.
func syntheticNativeCLI(t *testing.T) ([]byte, string, string) {
	t.Helper()
	id := "123456789012-synthetic_cli_registration.apps.googleusercontent.com"
	secret := "GOCSPX-" + strings.Repeat("A", 28)
	return syntheticELF([]byte{0xc3}, []byte(id+"\x00"+secret+"\x00")), id, secret
}

func syntheticAmbiguousNativeCLI(t *testing.T) []byte {
	t.Helper()
	return syntheticELF([]byte{0xc3}, []byte("123456789012-first.apps.googleusercontent.com\x00"+
		"123456789012-second.apps.googleusercontent.com\x00GOCSPX-"+strings.Repeat("A", 28)+"GOCSPX-"+strings.Repeat("B", 28)))
}

func syntheticELF(text, literals []byte) []byte {
	const textOffset = 128
	literalOffset := textOffset + len(text)
	sectionNames := []byte("\x00.text\x00.rodata\x00.shstrtab\x00")
	namesOffset := literalOffset + len(literals)
	sectionsOffset := (namesOffset + len(sectionNames) + 7) &^ 7
	data := make([]byte, sectionsOffset+4*64)
	copy(data, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(data[16:18], 2)
	binary.LittleEndian.PutUint16(data[18:20], 62)
	binary.LittleEndian.PutUint32(data[20:24], 1)
	binary.LittleEndian.PutUint64(data[40:48], uint64(sectionsOffset))
	binary.LittleEndian.PutUint16(data[52:54], 64)
	binary.LittleEndian.PutUint16(data[58:60], 64)
	binary.LittleEndian.PutUint16(data[60:62], 4)
	binary.LittleEndian.PutUint16(data[62:64], 3)
	copy(data[textOffset:], text)
	copy(data[literalOffset:], literals)
	copy(data[namesOffset:], sectionNames)
	for index, section := range []struct {
		name                         uint32
		flags, address, offset, size uint64
	}{
		{1, 6, 0x1000, textOffset, uint64(len(text))},
		{7, 2, 0x2000, uint64(literalOffset), uint64(len(literals))},
		{15, 0, 0, uint64(namesOffset), uint64(len(sectionNames))},
	} {
		at := sectionsOffset + (index+1)*64
		binary.LittleEndian.PutUint32(data[at:at+4], section.name)
		binary.LittleEndian.PutUint32(data[at+4:at+8], 1)
		if index == 2 {
			binary.LittleEndian.PutUint32(data[at+4:at+8], 3) // SHT_STRTAB
		}
		binary.LittleEndian.PutUint64(data[at+8:at+16], section.flags)
		binary.LittleEndian.PutUint64(data[at+16:at+24], section.address)
		binary.LittleEndian.PutUint64(data[at+24:at+32], section.offset)
		binary.LittleEndian.PutUint64(data[at+32:at+40], section.size)
		binary.LittleEndian.PutUint64(data[at+48:at+56], 1)
	}
	return data
}

func TestBinaryCredentialsRequireCompleteUnambiguousPair(t *testing.T) {
	data, id, secret := syntheticNativeCLI(t)
	gotID, gotSecret, err := extractBinaryCredentials(data)
	if err != nil || gotID != id || gotSecret != secret {
		t.Fatalf("unique native pair extraction failed: %v", err)
	}
	for _, invalid := range [][]byte{
		nil,
		[]byte(id),
		[]byte(secret),
		syntheticAmbiguousNativeCLI(t),
	} {
		gotID, gotSecret, err := extractBinaryCredentials(invalid)
		if err == nil || gotID != "" || gotSecret != "" {
			t.Fatal("incomplete or ambiguous native credentials were accepted")
		}
		if bytes.Contains([]byte(err.Error()), []byte(secret)) {
			t.Fatal("discovery error leaked a secret")
		}
	}
}

func TestNativeCloudCodeSelectionUsesCodeReferencesNotStringOrder(t *testing.T) {
	gcuID := "123456789012-gcu_fixture.apps.googleusercontent.com"
	consumerID := "123456789012-consumer_fixture.apps.googleusercontent.com"
	gcuSecret := "GOCSPX-" + strings.Repeat("A", 28)
	consumerSecret := "GOCSPX-" + strings.Repeat("B", 28)
	// Consumer literals deliberately precede GCU literals in the string pool.
	pool := []byte(consumerID + "\x00" + consumerSecret + "\x00" + gcuID + "\x00" + gcuSecret + "\x00")
	code := []byte{0x66, 0x81, 0x38, 'g', 'c', 0x75, 0x20, 0x80, 0x78, 2, 'p', 0x75, 14}
	for _, literal := range []string{gcuID, gcuSecret} {
		address := int64(0x2000 + bytes.Index(pool, []byte(literal)))
		pc := int64(0x1000 + len(code) + 7)
		instruction := []byte{0x48, 0x8d, 0x0d, 0, 0, 0, 0}
		binary.LittleEndian.PutUint32(instruction[3:], uint32(int32(address-pc)))
		code = append(code, instruction...)
	}
	code = append(code, 0xc3)
	image, err := readNativeImage(syntheticELF(code, pool))
	if err != nil {
		t.Fatal(err)
	}
	id, secret := image.amd64CloudCodePair(code, 0x1000)
	if id != gcuID || secret != gcuSecret {
		t.Fatal("GCU code references were not paired independently of literal order")
	}
	// A branch whose first reference is followed by two distinct secrets is
	// ambiguous even when one secret happens to be closest to the client ID.
	second := []byte{0x48, 0x8d, 0x0d, 0, 0, 0, 0}
	address := int64(0x2000 + bytes.Index(pool, []byte(consumerSecret)))
	pc := int64(0x1000 + len(code) - 1 + 7)
	binary.LittleEndian.PutUint32(second[3:], uint32(int32(address-pc)))
	code = append(code[:len(code)-1], second...)
	code[12] = 21
	id, secret = image.amd64CloudCodePair(code, 0x1000)
	if id != "" || secret != "" {
		t.Fatal("ambiguous GCU native branch was accepted")
	}
}

func TestBinaryCredentialsResolveMultiplePairsThroughNativeMetadata(t *testing.T) {
	id := "123456789012-cloud_fixture.apps.googleusercontent.com"
	secret := "GOCSPX-" + strings.Repeat("C", 28)
	pool := []byte("123456789012-consumer_fixture.apps.googleusercontent.com\x00GOCSPX-" + strings.Repeat("D", 28) + "\x00" + id + "\x00" + secret + "\x00")
	code := []byte{0x66, 0x81, 0x38, 'g', 'c', 0x75, 0x20, 0x80, 0x78, 2, 'p', 0x75, 14}
	for _, literal := range []string{id, secret} {
		instruction := []byte{0x48, 0x8d, 0x0d, 0, 0, 0, 0}
		address := int64(0x2000 + bytes.Index(pool, []byte(literal)))
		pc := int64(0x1000 + len(code) + 7)
		binary.LittleEndian.PutUint32(instruction[3:], uint32(int32(address-pc)))
		code = append(code, instruction...)
	}
	code = append(code, 0xc3)
	name := []byte("google3/third_party/jetski/cli/backend/auth/auth.getOauthParams\x00")
	pcln := (72 + len(name) + 7) &^ 7
	metadata := make([]byte, pcln+24)
	copy(metadata, []byte{0xf1, 0xff, 0xff, 0xff, 0, 0, 1, 8})
	binary.LittleEndian.PutUint64(metadata[8:16], 1)
	binary.LittleEndian.PutUint64(metadata[32:40], 72)
	binary.LittleEndian.PutUint64(metadata[40:48], uint64(72+len(name)))
	binary.LittleEndian.PutUint64(metadata[64:72], uint64(pcln))
	copy(metadata[72:], name)
	binary.LittleEndian.PutUint32(metadata[pcln+4:pcln+8], 16)
	binary.LittleEndian.PutUint32(metadata[pcln+8:pcln+12], uint32(len(code)))
	data := syntheticELF(code, append(pool, metadata...))
	gotID, gotSecret, err := extractBinaryCredentials(data)
	if err != nil || gotID != id || gotSecret != secret {
		t.Fatalf("native metadata did not select the issuing Cloud Code pair: %v", err)
	}
	// Corrupted metadata must not accidentally fall back to string order.
	binary.LittleEndian.PutUint64(metadata[32:40], ^uint64(0))
	data = syntheticELF(code, append(pool, metadata...))
	if _, _, err := extractBinaryCredentials(data); err == nil {
		t.Fatal("invalid native name offsets selected a credential pair")
	}
}
