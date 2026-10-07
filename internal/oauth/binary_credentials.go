package oauth

import (
	"bytes"
	"debug/elf"
	"debug/macho"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
)

var (
	binaryClientID = regexp.MustCompile(`[0-9]+-[A-Za-z0-9_-]+\.apps\.googleusercontent\.com`)
	// Native Go string pools concatenate literals without delimiters. Google's
	// GOCSPX payload is 28 URL-safe characters; never greedily consume its neighbor.
	binaryClientSecret = regexp.MustCompile(`GOCSPX-[A-Za-z0-9_-]{28}`)
)

type nativeSection struct {
	address uint64
	data    []byte
}

type nativeImage struct {
	arch     string
	text     nativeSection
	sections []nativeSection
}

// extractBinaryCredentials reads data only: neither local nor downloaded CLI
// code is executed. Multiple identities must be associated by native OAuth code.
func extractBinaryCredentials(data []byte) (string, string, error) {
	ids := uniqueBinaryStrings(binaryClientID, data)
	secrets := uniqueBinaryStrings(binaryClientSecret, data)
	if len(ids) == 0 || len(secrets) == 0 {
		return "", "", errors.New("agy binary contains no complete OAuth client credentials")
	}
	if len(ids) == 1 && len(secrets) == 1 {
		return ids[0], secrets[0], nil
	}
	image, err := readNativeImage(data)
	if err != nil {
		return "", "", fmt.Errorf("ambiguous agy OAuth credentials: %w", err)
	}
	pairs := make(map[string][2]string)
	magic := []byte{0xf1, 0xff, 0xff, 0xff, 0, 0}
	for start := 0; start < len(data); {
		i := bytes.Index(data[start:], magic)
		if i < 0 {
			break
		}
		offset := start + i
		start = offset + len(magic)
		functions := nativeFunctions(data[offset:], image.text.address)
		for _, function := range functions {
			// Only the native CLI OAuth selector is decoded.
			code, ok := image.rangeAt(function.Entry, function.End-function.Entry)
			if !ok {
				continue
			}
			var id, secret string
			switch image.arch {
			case "amd64":
				id, secret = image.amd64CloudCodePair(code, function.Entry)
			case "arm64":
				id, secret = image.arm64CloudCodePair(code, function.Entry)
			}
			if id != "" && secret != "" {
				pairs[id+"\x00"+secret] = [2]string{id, secret}
			}
		}
	}
	if len(pairs) != 1 {
		return "", "", errors.New("cannot unambiguously resolve agy Cloud Code OAuth credentials; set ANTIGRAVITY_OAUTH_CLIENT_ID and ANTIGRAVITY_OAUTH_CLIENT_SECRET explicitly")
	}
	for _, pair := range pairs {
		return pair[0], pair[1], nil
	}
	panic("unreachable")
}

func uniqueBinaryStrings(pattern *regexp.Regexp, data []byte) []string {
	seen := make(map[string]struct{})
	var result []string
	for _, match := range pattern.FindAll(data, -1) {
		value := string(match)
		if _, exists := seen[value]; !exists {
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	return result
}

type nativeFunction struct {
	Entry uint64
	End   uint64
}

// Decode only the named selector, not every name in a potentially malformed
// table. Offsets and name lengths are bounded before reading or allocating.
func nativeFunctions(data []byte, text uint64) []nativeFunction {
	if len(data) < 72 || data[7] != 8 || (data[6] != 1 && data[6] != 4) {
		return nil
	}
	nfunc := binary.LittleEndian.Uint64(data[8:16])
	names := binary.LittleEndian.Uint64(data[32:40])
	namesEnd := binary.LittleEndian.Uint64(data[40:48])
	pcln := binary.LittleEndian.Uint64(data[64:72])
	size := uint64(len(data))
	if nfunc == 0 || pcln > size || size-pcln < 8 || nfunc > (size-pcln-8)/8 || names >= namesEnd || namesEnd > size {
		return nil
	}
	const target = "google3/third_party/jetski/cli/backend/auth/auth.getOauthParams"
	var result []nativeFunction
	for i := uint64(0); i < nfunc; i++ {
		at := pcln + i*8
		entry := uint64(binary.LittleEndian.Uint32(data[at : at+4]))
		next := uint64(binary.LittleEndian.Uint32(data[at+8 : at+12]))
		offset := uint64(binary.LittleEndian.Uint32(data[at+4 : at+8]))
		if offset > size-pcln || size-pcln-offset < 8 {
			return nil
		}
		nameOffset := uint64(binary.LittleEndian.Uint32(data[pcln+offset+4 : pcln+offset+8]))
		if nameOffset >= namesEnd-names || uint64(len(target)+1) > namesEnd-names-nameOffset {
			continue
		}
		start := names + nameOffset
		if data[start+uint64(len(target))] != 0 || !bytes.Equal(data[start:start+uint64(len(target))], []byte(target)) {
			continue
		}
		if next <= entry || text > ^uint64(0)-next {
			return nil
		}
		result = append(result, nativeFunction{Entry: text + entry, End: text + next})
	}
	return result
}

func readNativeImage(data []byte) (image nativeImage, err error) {
	defer func() {
		if recover() != nil {
			image = nativeImage{}
			err = errors.New("invalid native executable metadata")
		}
	}()
	if bytes.HasPrefix(data, []byte{0x7f, 'E', 'L', 'F'}) {
		file, err := elf.NewFile(bytes.NewReader(data))
		if err != nil {
			return image, errors.New("invalid ELF executable")
		}
		if file.Class != elf.ELFCLASS64 || file.ByteOrder != binary.LittleEndian {
			return image, errors.New("unsupported ELF executable format")
		}
		switch file.Machine {
		case elf.EM_X86_64:
			image.arch = "amd64"
		case elf.EM_AARCH64:
			image.arch = "arm64"
		default:
			return image, errors.New("unsupported ELF executable architecture")
		}
		for _, section := range file.Sections {
			if section.Type == elf.SHT_NOBITS || section.Size == 0 || section.Offset > uint64(len(data)) || section.Size > uint64(len(data))-section.Offset {
				continue
			}
			part := nativeSection{section.Addr, data[section.Offset : section.Offset+section.Size]}
			image.sections = append(image.sections, part)
			if section.Name == ".text" {
				image.text = part
			}
		}
	} else {
		file, err := macho.NewFile(bytes.NewReader(data))
		if err != nil {
			return image, errors.New("unsupported native executable format")
		}
		switch file.Cpu {
		case macho.CpuAmd64:
			image.arch = "amd64"
		case macho.CpuArm64:
			image.arch = "arm64"
		default:
			return image, errors.New("unsupported Mach-O executable architecture")
		}
		for _, section := range file.Sections {
			if uint64(section.Offset) > uint64(len(data)) || section.Size > uint64(len(data))-uint64(section.Offset) {
				continue
			}
			part := nativeSection{section.Addr, data[uint64(section.Offset) : uint64(section.Offset)+section.Size]}
			image.sections = append(image.sections, part)
			if section.Name == "__text" {
				image.text = part
			}
		}
	}
	if len(image.text.data) == 0 {
		return image, errors.New("native executable has no text section")
	}
	return image, nil
}

func (image nativeImage) rangeAt(address, size uint64) ([]byte, bool) {
	for _, section := range image.sections {
		if address < section.address {
			continue
		}
		offset := address - section.address
		if offset <= uint64(len(section.data)) && size <= uint64(len(section.data))-offset {
			return section.data[offset : offset+size], true
		}
	}
	return nil, false
}

func (image nativeImage) literalAt(address uint64, pattern *regexp.Regexp) string {
	for _, section := range image.sections {
		if address < section.address || address-section.address >= uint64(len(section.data)) {
			continue
		}
		data := section.data[address-section.address:]
		if len(data) > 256 {
			data = data[:256]
		}
		match := pattern.FindIndex(data)
		if match != nil && match[0] == 0 {
			return string(data[:match[1]])
		}
	}
	return ""
}

// The CLI's getOauthParams distinguishes GCP (Cloud Code) from consumer auth.
// Read only the GCP branch: selecting the first literal in a string pool is unsafe.
func (image nativeImage) amd64CloudCodePair(code []byte, base uint64) (string, string) {
	for i := 0; i+16 < len(code); i++ {
		// CMP word ptr [reg], 'gc', JNE; CMP byte ptr [reg+2], 'p', JNE.
		if code[i] != 0x66 || code[i+1] != 0x81 || code[i+2]&0xf8 != 0x38 || code[i+3] != 'g' || code[i+4] != 'c' || code[i+5] != 0x75 || code[i+7] != 0x80 || code[i+8]&0xf8 != 0x78 || code[i+9] != 2 || code[i+10] != 'p' || code[i+11] != 0x75 {
			continue
		}
		end := i + 13 + int(int8(code[i+12]))
		if end <= i+13 || end > len(code) {
			continue
		}
		return image.amd64ReferencedPair(code[i+13:end], base+uint64(i+13))
	}
	return "", ""
}

func (image nativeImage) amd64ReferencedPair(code []byte, base uint64) (string, string) {
	var id, secret string
	for i := 0; i+7 <= len(code); i++ {
		if (code[i] == 0xeb || code[i] == 0xe9 || code[i] == 0xc3) && id != "" && secret != "" {
			return id, secret // GCP exits before the adjacent consumer branch.
		}
		if (code[i] != 0x48 && code[i] != 0x4c) || code[i+1] != 0x8d || code[i+2]&0xc7 != 0x05 {
			continue
		}
		address := uint64(int64(base+uint64(i)+7) + int64(int32(binary.LittleEndian.Uint32(code[i+3:i+7]))))
		if value := image.literalAt(address, binaryClientID); value != "" {
			if id != "" && id != value {
				return "", ""
			}
			id = value
		}
		if value := image.literalAt(address, binaryClientSecret); value != "" {
			if secret != "" && secret != value {
				return "", ""
			}
			secret = value
		}
		i += 6 // Do not interpret a LEA displacement as another instruction.
	}
	return id, secret
}
