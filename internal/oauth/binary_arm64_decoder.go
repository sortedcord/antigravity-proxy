package oauth

import "encoding/binary"

// arm64ConsumerPair follows getOauthParams' length switch to consumer auth.
// Both cases require connected loads and exact string comparisons; character
// failures and the length guards must share the native failure block.
func (image nativeImage) arm64ConsumerPair(code []byte, base uint64) (string, string) {
	if base%4 != 0 || len(code)%4 != 0 {
		return "", ""
	}
	var id, secret string
	for i := 0; i+40 <= len(code); i += 4 {
		word := func(offset int) uint32 { return binary.LittleEndian.Uint32(code[i+offset : i+offset+4]) }
		length := word(0)
		if length&0xfffffc1f != 0xf1000c1f { // CMP Xn, #3.
			continue
		}
		consumer, ok := arm64NEBranch(word(4), i+4, len(code))
		if !ok {
			return "", ""
		}
		half, constant, compare := word(8), word(12), word(16)
		if half&0xfffffc00 != 0x79400000 || // LDRH Wn, [Xm].
			constant&0x7fe00000 != 0x52800000 || (constant>>5)&0xffff != 0x6367 || // MOVZ Wn/Xn, #'gc'.
			compare&0xffe0fc1f != 0x6b00001f { // CMP Wn, Wm, without shift.
			return "", ""
		}
		pointer, loaded, literal := (half>>5)&31, half&31, constant&31
		if pointer == 31 || (length>>5)&31 == 31 || (length>>5)&31 == pointer ||
			loaded == 31 || literal == 31 || loaded == literal || pointer == loaded || pointer == literal ||
			(compare>>5)&31 != loaded || (compare>>16)&31 != literal {
			return "", ""
		}
		failure, ok := arm64NEBranch(word(20), i+20, len(code))
		if !ok {
			return "", ""
		}
		last, pCompare := word(24), word(28)
		if last&0xfffffc00 != 0x39400800 || (last>>5)&31 != pointer || last&31 == 31 || last&31 == pointer ||
			pCompare&0xfffffc1f != 0x7101c01f || (pCompare>>5)&31 != last&31 { // LDRB [pointer+2]; CMP Wn, #'p'.
			return "", ""
		}
		lastFailure, ok := arm64NEBranch(word(32), i+32, len(code))
		start := i + 36
		if !ok || failure != lastFailure || consumer <= start || consumer+36 >= failure {
			return "", ""
		}
		guard := binary.LittleEndian.Uint32(code[consumer : consumer+4])
		consumerFailure, ok := arm64NEBranch(binary.LittleEndian.Uint32(code[consumer+4:consumer+8]), consumer+4, len(code))
		if guard&0xfffffc1f != 0xf100201f || (guard>>5)&31 != (length>>5)&31 || !ok || consumerFailure != failure {
			return "", ""
		}
		consumerWord := func(offset int) uint32 { return binary.LittleEndian.Uint32(code[consumer+offset : consumer+offset+4]) }
		load, low, consumerCompare := consumerWord(8), consumerWord(12), consumerWord(28)
		consumerLoaded, consumerLiteral := load&31, low&31
		if load&0xfffffc00 != 0xf9400000 || (load>>5)&31 != pointer || consumerLoaded == 31 || consumerLoaded == pointer ||
			low&0xffe00000 != 0xd2800000 || (low>>5)&0xffff != 0x6f63 || consumerLiteral == 31 ||
			consumerLiteral == pointer || consumerLiteral == consumerLoaded || consumerLiteral == (length>>5)&31 ||
			consumerCompare&0xffe0fc1f != 0xeb00001f || (consumerCompare>>5)&31 != consumerLoaded || (consumerCompare>>16)&31 != consumerLiteral {
			return "", ""
		}
		// MOVZ 'co'; MOVK 'ns',LSL16; MOVK 'um',LSL32; MOVK 'er',LSL48.
		for index, halfword := range [3]uint32{0x736e, 0x6d75, 0x7265} {
			part := consumerWord(16 + index*4)
			if part&0xffe00000 != 0xf2800000|uint32(index+1)<<21 || part&31 != consumerLiteral || (part>>5)&0xffff != halfword {
				return "", ""
			}
		}
		consumerFailure, ok = arm64NEBranch(consumerWord(32), consumer+32, len(code))
		if !ok || consumerFailure != failure {
			return "", ""
		}
		candidateID, candidateSecret := image.arm64SelectedPair(code, base, consumer+36, failure)
		if candidateID == "" || candidateSecret == "" {
			return "", ""
		}
		if id != "" && (id != candidateID || secret != candidateSecret) {
			return "", ""
		}
		id, secret = candidateID, candidateSecret
	}
	return id, secret
}

func arm64NEBranch(word uint32, pc, size int) (int, bool) {
	if word&0xff00001f != 0x54000001 { // B.NE.
		return 0, false
	}
	imm := int32((word>>5)&0x7ffff) << 13 >> 11
	target := pc + int(imm)
	return target, target > pc && target < size && target%4 == 0
}

// Only the selected straight-line block and its common return tail are decoded.
// Unknown instructions, conditional branches, calls, loops, missing returns and
// jumps into a different credential-producing block all fail closed. Addresses
// must survive in the actual Go string return registers; a transient reference
// to an OAuth literal is not itself evidence of the returned credential pair.
func (image nativeImage) arm64SelectedPair(code []byte, base uint64, start, failure int) (string, string) {
	var registers [32]uint64
	var known [32]bool
	var selectedID, selectedSecret string
	var referencedID, referencedSecret string
	inTail := false
	for pc := start; pc+4 <= len(code) && pc < failure; pc += 4 {
		word := binary.LittleEndian.Uint32(code[pc : pc+4])
		rd, rn := word&31, (word>>5)&31
		if word&0xfffffc1f == 0xd65f0000 { // RET Xn.
			if rn != 30 {
				return "", ""
			}
			id, secret := image.arm64ReturnPair(&registers, &known)
			if inTail && (id != selectedID || secret != selectedSecret) {
				return "", ""
			}
			return id, secret
		}
		if word&0xfc000000 == 0x14000000 { // B, never BL.
			if inTail || !known[0] || !known[1] || !known[2] {
				return "", ""
			}
			selectedID = image.literalAt(registers[1], binaryClientID)
			selectedSecret = image.literalAt(registers[2], binaryClientSecret)
			if selectedID == "" || selectedSecret == "" || registers[0] != uint64(len(selectedID)) {
				return "", ""
			}
			imm := int32(word&0x3ffffff) << 6 >> 4
			target := pc + int(imm)
			if target <= pc || target >= failure || target%4 != 0 {
				return "", ""
			}
			inTail = true
			pc = target - 4
			continue
		}
		switch {
		case word == 0xd503201f: // NOP.
		case word&0x9f000000 == 0x90000000: // ADRP.
			imm := int64(((word>>5)&0x7ffff)<<2 | (word>>29)&3)
			imm = imm << 43 >> 31
			registers[rd] = uint64(int64((base+uint64(pc))&^4095) + imm)
			known[rd] = rd != 31
		case word&0xff800000 == 0x91000000: // ADD Xd, Xn, #imm{, LSL #12}.
			shift := uint((word>>22)&1) * 12
			registers[rd] = registers[rn] + (uint64((word>>10)&0xfff) << shift)
			known[rd] = rd != 31 && rn != 31 && known[rn]
			// A shifted ADD adjusts a page; only an unshifted ADD completes a literal address.
			if known[rd] && shift == 0 {
				if value := image.literalAt(registers[rd], binaryClientID); value != "" {
					if referencedID != "" && referencedID != value {
						return "", ""
					}
					referencedID = value
				}
				if value := image.literalAt(registers[rd], binaryClientSecret); value != "" {
					if referencedSecret != "" && referencedSecret != value {
						return "", ""
					}
					referencedSecret = value
				}
			}
		case word&0x7f800000 == 0x52800000: // MOVZ Wd/Xd, #imm, LSL #shift.
			shift := uint((word>>21)&3) * 16
			if word>>31 == 0 && shift >= 32 {
				return "", ""
			}
			registers[rd] = uint64((word>>5)&0xffff) << shift
			known[rd] = rd != 31
		case word&0x7fe0ffe0 == 0x2a0003e0: // MOV Wd/Xd, Wm/Xm (ORR with ZR).
			rm := (word >> 16) & 31
			if rm == 31 {
				registers[rd], known[rd] = 0, rd != 31
			} else {
				registers[rd], known[rd] = registers[rm], rd != 31 && known[rm]
				if word>>31 == 0 {
					registers[rd] &= 0xffffffff
				}
			}
		case word&0x3e000000 == 0x28000000 && word>>30 != 3: // Integer LDP/STP, including writeback.
			if word&(1<<22) != 0 {
				known[rd], known[(word>>10)&31] = false, false
			}
			mode := (word >> 23) & 3
			if mode == 1 || mode == 3 {
				known[rn] = false
			}
		case word&0x3f000000 == 0x39000000: // Integer unsigned-offset LDR/STR.
			if (word>>22)&3 != 0 {
				known[rd] = false
			}
		case word&0x3f200000 == 0x38000000: // Integer unscaled/indexed LDR/STR (not atomics).
			if (word>>22)&3 != 0 {
				known[rd] = false
			}
			mode := (word >> 10) & 3
			if mode == 1 || mode == 3 {
				known[rn] = false
			}
		default:
			return "", ""
		}
	}
	return "", ""
}

func (image nativeImage) arm64ReturnPair(registers *[32]uint64, known *[32]bool) (string, string) {
	if !known[0] || !known[1] || !known[2] || !known[3] {
		return "", ""
	}
	id := image.literalAt(registers[0], binaryClientID)
	secret := image.literalAt(registers[2], binaryClientSecret)
	if id == "" || secret == "" || registers[1] != uint64(len(id)) || registers[3] != uint64(len(secret)) {
		return "", ""
	}
	return id, secret
}
