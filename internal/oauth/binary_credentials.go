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
			if function.Entry < image.text.address || function.End < function.Entry || function.End-image.text.address > uint64(len(image.text.data)) {
				return "", "", errors.New("agy consumer OAuth selector has invalid native text bounds")
			}
			code := image.text.data[function.Entry-image.text.address : function.End-image.text.address]
			var id, secret string
			switch image.arch {
			case "amd64":
				id, secret = image.amd64ConsumerPair(code, function.Entry)
			case "arm64":
				id, secret = image.arm64ConsumerPair(code, function.Entry)
			}
			if id == "" || secret == "" {
				return "", "", errors.New("cannot resolve agy consumer OAuth selector safely; set ANTIGRAVITY_OAUTH_CLIENT_ID and ANTIGRAVITY_OAUTH_CLIENT_SECRET explicitly")
			}
			pairs[id+"\x00"+secret] = [2]string{id, secret}
		}
	}
	if len(pairs) != 1 {
		return "", "", errors.New("cannot unambiguously resolve agy consumer OAuth credentials; set ANTIGRAVITY_OAUTH_CLIENT_ID and ANTIGRAVITY_OAUTH_CLIENT_SECRET explicitly")
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

// Follow the native string switch, including the literal consumer comparison.
// Decode the selected instructions to the Go return, never string-pool order.
func (image nativeImage) amd64ConsumerPair(code []byte, base uint64) (string, string) {
	var id, secret string
	for i := 0; i+19 <= len(code); i++ {
		if code[i] != 0x48 || code[i+1] != 0x83 || code[i+2]&0xf8 != 0xf8 || code[i+3] != 3 || code[i+4] != 0x75 ||
			code[i+6] != 0x66 || code[i+7] != 0x81 || code[i+8]&0xf8 != 0x38 || code[i+9] != 'g' || code[i+10] != 'c' || code[i+11] != 0x75 ||
			code[i+13] != 0x80 || code[i+14]&0xf8 != 0x78 || code[i+15] != 2 || code[i+16] != 'p' || code[i+17] != 0x75 {
			continue
		}
		pointer, length := code[i+8]&7, code[i+2]&7
		consumer := i + 6 + int(int8(code[i+5]))
		failure := i + 13 + int(int8(code[i+12]))
		if pointer == 4 || pointer == 5 || pointer == length || code[i+14]&7 != pointer ||
			failure != i+19+int(int8(code[i+18])) || consumer <= i+19 || consumer+21 > failure || failure >= len(code) {
			return "", ""
		}
		guard := code[consumer : consumer+21]
		if guard[0] != 0x48 || guard[1] != 0x83 || guard[2] != 0xf8|length || guard[3] != 8 || guard[4] != 0x75 ||
			consumer+6+int(int8(guard[5])) != failure || guard[6] != 0x48 || guard[7]&0xf8 != 0xb8 ||
			binary.LittleEndian.Uint64(guard[8:16]) != 0x72656d75736e6f63 || guard[16] != 0x48 || guard[17] != 0x39 ||
			guard[18] != (guard[7]&7)<<3|pointer || guard[19] != 0x75 || consumer+21+int(int8(guard[20])) != failure ||
			guard[7]&7 == pointer || guard[7]&7 == length {
			return "", ""
		}
		candidateID, candidateSecret := image.amd64ReturnedPair(code, base, consumer+21, failure)
		if candidateID == "" || candidateSecret == "" || (id != "" && (id != candidateID || secret != candidateSecret)) {
			return "", ""
		}
		id, secret = candidateID, candidateSecret
	}
	return id, secret
}

func (image nativeImage) amd64ReturnedPair(code []byte, base uint64, start, failure int) (string, string) {
	var registers [16]uint64
	var known [16]bool
	var referencedID, referencedSecret string
	for pc := start; pc < failure; {
		remaining := code[pc:failure]
		if remaining[0] == 0xc3 {
			if !known[0] || !known[1] || !known[3] || !known[7] {
				return "", ""
			}
			id := image.literalAt(registers[0], binaryClientID)
			secret := image.literalAt(registers[1], binaryClientSecret)
			if id == "" || secret == "" || registers[3] != uint64(len(id)) || registers[7] != uint64(len(secret)) {
				return "", ""
			}
			return id, secret
		}
		if remaining[0] == 0x90 || remaining[0] == 0x5d { // NOP; POP RBP epilogue.
			if remaining[0] == 0x5d {
				known[5] = false
			}
			pc++
			continue
		}
		rex, offset := byte(0), 0
		if remaining[0]&0xf0 == 0x40 {
			rex, offset = remaining[0], 1
		}
		if len(remaining) <= offset {
			return "", ""
		}
		op := remaining[offset]
		if op&0xf8 == 0xb8 && rex&8 == 0 && len(remaining) >= offset+5 {
			rd := int(op&7) + int(rex&1)*8
			registers[rd], known[rd] = uint64(binary.LittleEndian.Uint32(remaining[offset+1:offset+5])), true
			pc += offset + 5
			continue
		}
		if len(remaining) < offset+2 {
			return "", ""
		}
		modrm := remaining[offset+1]
		rd, rm := int((modrm>>3)&7)+int((rex>>2)&1)*8, int(modrm&7)+int(rex&1)*8
		switch {
		case op == 0x8d && rex&8 != 0 && rex&3 == 0 && modrm&0xc7 == 5 && len(remaining) >= offset+6:
			address := uint64(int64(base+uint64(pc+offset+6)) + int64(int32(binary.LittleEndian.Uint32(remaining[offset+2:offset+6]))))
			registers[rd], known[rd] = address, true
			if value := image.literalAt(address, binaryClientID); value != "" {
				if referencedID != "" && referencedID != value {
					return "", ""
				}
				referencedID = value
			}
			if value := image.literalAt(address, binaryClientSecret); value != "" {
				if referencedSecret != "" && referencedSecret != value {
					return "", ""
				}
				referencedSecret = value
			}
			pc += offset + 6
		case (op == 0x89 || op == 0x31) && modrm&0xc0 == 0xc0:
			if op == 0x31 {
				if rd != rm {
					return "", ""
				}
				registers[rm], known[rm] = 0, true
			} else {
				registers[rm], known[rm] = registers[rd], known[rd]
				if rex&8 == 0 {
					registers[rm] &= 0xffffffff
				}
			}
			pc += offset + 2
		case rex == 0x48 && op == 0x83 && modrm == 0xc4 && len(remaining) >= 4: // ADD RSP,#imm8.
			known[4] = false
			pc += 4
		default:
			return "", "" // Calls, branches and unknown instructions fail closed.
		}
	}
	return "", ""
}
