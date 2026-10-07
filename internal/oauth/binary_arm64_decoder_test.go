package oauth

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// Every literal and instruction is generated here. No real application or
// account credential data is included in the fixture or failure messages.
type arm64OAuthFixture struct {
	image                                nativeImage
	code                                 []byte
	base                                 uint64
	id, secret                           string
	consumer, start, tail, failure, jump int
}

func arm64TestMOVZ(register uint32, value int) uint32 {
	return 0xd2800000 | uint32(value)<<5 | register
}

func arm64TestMOVK(register uint32, value, shift int) uint32 {
	return 0xf2800000 | uint32(shift/16)<<21 | uint32(value)<<5 | register
}

func arm64TestBranch(pc, target int, conditional bool) uint32 {
	if conditional {
		return 0x54000001 | (uint32((target-pc)/4)&0x7ffff)<<5
	}
	return 0x14000000 | (uint32((target-pc)/4) & 0x3ffffff)
}

func arm64TestADRP(register uint32, pc, address uint64) uint32 {
	imm := uint32((int64(address&^4095)-int64(pc&^4095))>>12) & 0x1fffff
	return 0x90000000 | (imm&3)<<29 | (imm>>2)<<5 | register
}

func arm64TestADD(destination, source uint32, immediate uint64) uint32 {
	return 0x91000000 | uint32(immediate)<<10 | source<<5 | destination
}

func arm64Fixture(t *testing.T, base, poolBase uint64, afterPage, afterPair []uint32, consumerFirst ...bool) arm64OAuthFixture {
	t.Helper()
	id := "987654321012-generated_consumer_fixture.apps.googleusercontent.com"
	secret := "GOCSPX-" + strings.Repeat("B", 28)
	siblingID := "123456789012-generated_gcp_fixture.apps.googleusercontent.com"
	siblingSecret := "GOCSPX-" + strings.Repeat("A", 28)
	// GCP literals precede consumer literals, whose secret has a pool neighbor.
	// Selection must follow the guarded consumer path, never string-pool order.
	pool := []byte(siblingID + "\x00" + siblingSecret + "\x00" + id + "\x00" + secret + "pool_neighbor")
	if len(consumerFirst) != 0 && consumerFirst[0] {
		pool = []byte(id + "\x00" + secret + "\x00" + siblingID + "\x00" + siblingSecret + "pool_neighbor")
	}
	address := func(literal string) uint64 { return poolBase + uint64(bytes.Index(pool, []byte(literal))) }
	words := []uint32{
		0xf1000c3f, // CMP X1, #3.
		0,          // B.NE consumer.
		0x79400006, // LDRH W6, [X0].
		arm64TestMOVZ(7, 0x6367),
		0x6b0700df, // CMP W6, W7.
		0,          // B.NE failure.
		0x39400806, // LDRB W6, [X0, #2].
		0x7101c0df, // CMP W6, #'p'.
		0,          // B.NE failure.
		arm64TestMOVZ(0, len(siblingID)),
	}
	appendPair := func(pairID, pairSecret string, pageInstructions []uint32) {
		for index, literal := range []string{pairID, pairSecret} {
			register := uint32(index + 1)
			addr := address(literal)
			words = append(words, arm64TestADRP(register, base+uint64(len(words)*4), addr))
			if index == 0 {
				words = append(words, pageInstructions...)
			}
			words = append(words, arm64TestADD(register, register, addr&4095))
		}
	}
	appendPair(siblingID, siblingSecret, nil)
	siblingJump := len(words) * 4
	words = append(words, 0) // B shared return, skipping the consumer sibling.
	consumer := len(words) * 4
	words = append(words,
		0xf100203f, 0, // CMP X1,#8; B.NE failure.
		0xf9400006, // LDR X6,[X0].
		arm64TestMOVZ(7, 0x6f63),
		arm64TestMOVK(7, 0x736e, 16),
		arm64TestMOVK(7, 0x6d75, 32),
		arm64TestMOVK(7, 0x7265, 48),
		0xeb0700df, 0, // CMP X6,X7; B.NE failure.
	)
	start := len(words) * 4
	words = append(words, arm64TestMOVZ(0, len(id)))
	appendPair(id, secret, afterPage)
	words = append(words, afterPair...)
	jump := len(words) * 4
	words = append(words, 0xd503201f) // Consumer falls through to shared return.
	tail := len(words) * 4
	words = append(words,
		arm64TestMOVZ(3, len(secret)),
		0xaa1f03e4, // MOV X4, XZR.
		0xaa1f03e5, // MOV X5, XZR.
		0xaa0003e6, // MOV X6, X0.
		0xaa0103e0, // MOV X0, X1.
		0xaa0603e1, // MOV X1, X6.
		0xf85f83fd, // LDUR X29, [SP, #-8].
		0xf84707fe, // LDR X30, [SP], #0x70.
		0xd65f03c0, // RET.
	)
	failure := len(words) * 4
	words = append(words, 0xd65f03c0)
	words[1] = arm64TestBranch(4, consumer, true)
	words[5] = arm64TestBranch(20, failure, true)
	words[8] = arm64TestBranch(32, failure, true)
	words[siblingJump/4] = arm64TestBranch(siblingJump, tail, false)
	words[consumer/4+1] = arm64TestBranch(consumer+4, failure, true)
	words[consumer/4+8] = arm64TestBranch(consumer+32, failure, true)
	code := make([]byte, len(words)*4)
	for index, word := range words {
		binary.LittleEndian.PutUint32(code[index*4:], word)
	}
	text := nativeSection{address: base, data: code}
	return arm64OAuthFixture{
		image: nativeImage{arch: "arm64", text: text, sections: []nativeSection{text, {address: poolBase, data: pool}}},
		code:  code, base: base, id: id, secret: secret,
		consumer: consumer, start: start, tail: tail, failure: failure, jump: jump,
	}
}

func (fixture arm64OAuthFixture) set(pc int, word uint32) {
	binary.LittleEndian.PutUint32(fixture.code[pc:pc+4], word)
}

func TestARM64ConsumerSelectsConsumerNotGCPSibling(t *testing.T) {
	for _, test := range []struct {
		name       string
		base, pool uint64
	}{
		{"positive_page_displacement", 0x1000, 0x8000},
		{"negative_page_displacement", 0x18000, 0x7000},
		{"pc_page_crossing", 0x1fe0, 0x8000},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := arm64Fixture(t, test.base, test.pool, nil, nil)
			id, secret := fixture.image.arm64ConsumerPair(fixture.code, fixture.base)
			if id != fixture.id || secret != fixture.secret {
				t.Fatal("native consumer return pair was not selected independently of GCP literal order")
			}
		})
	}
}

func TestARM64ConsumerSelectorRequiresConnectedSiblingComparisons(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(arm64OAuthFixture)
	}{
		{"wrong_length", func(f arm64OAuthFixture) { f.set(0, 0xf100103f) }},
		{"unrelated_gc_register", func(f arm64OAuthFixture) { f.set(12, arm64TestMOVZ(8, 0x6367)) }},
		{"comparison_uses_other_register", func(f arm64OAuthFixture) { f.set(16, 0x6b0800df) }},
		{"comparison_uses_other_loaded_register", func(f arm64OAuthFixture) { f.set(16, 0x6b07011f) }},
		{"length_compares_pointer", func(f arm64OAuthFixture) { f.set(0, 0xf1000c1f) }},
		{"length_compares_stack_pointer", func(f arm64OAuthFixture) { f.set(0, 0xf1000fff) }},
		{"halfword_is_not_a_load", func(f arm64OAuthFixture) { f.set(8, 0x79000006) }},
		{"halfword_at_other_offset", func(f arm64OAuthFixture) { f.set(8, 0x79400406) }},
		{"byte_from_different_pointer", func(f arm64OAuthFixture) { f.set(24, 0x39400826) }},
		{"byte_at_other_offset", func(f arm64OAuthFixture) { f.set(24, 0x39400c06) }},
		{"wrong_sibling_last_character", func(f arm64OAuthFixture) { f.set(28, 0x7101d4df) }},
		{"unrelated_p_comparison", func(f arm64OAuthFixture) { f.set(28, 0x7101c0ff) }},
		{"different_character_failures", func(f arm64OAuthFixture) { f.set(20, arm64TestBranch(20, f.failure-4, true)) }},
		{"equal_branch_instead_of_not_equal", func(f arm64OAuthFixture) { f.set(32, arm64TestBranch(32, f.failure, true)&^1) }},
		{"missing_character_branch", func(f arm64OAuthFixture) { f.set(32, 0xd503201f) }},
		{"backward_character_branch", func(f arm64OAuthFixture) { f.set(32, arm64TestBranch(32, 0, true)) }},
		{"out_of_range_character_branches", func(f arm64OAuthFixture) {
			f.set(20, arm64TestBranch(20, len(f.code)+4, true))
			f.set(32, arm64TestBranch(32, len(f.code)+4, true))
		}},
		{"selector_load_clobbers_pointer", func(f arm64OAuthFixture) {
			f.set(8, 0x79400000)
			f.set(16, 0x6b07001f)
		}},
		{"wrong_consumer_length", func(f arm64OAuthFixture) { f.set(f.consumer, 0xf1001c3f) }},
		{"different_consumer_length_register", func(f arm64OAuthFixture) { f.set(f.consumer, 0xf100205f) }},
		{"different_consumer_failure", func(f arm64OAuthFixture) { f.set(f.consumer+4, arm64TestBranch(f.consumer+4, f.failure-4, true)) }},
		{"consumer_length_branch_is_conditional_equal", func(f arm64OAuthFixture) { f.set(f.consumer+4, arm64TestBranch(f.consumer+4, f.failure, true)&^1) }},
		{"consumer_not_loaded", func(f arm64OAuthFixture) { f.set(f.consumer+8, 0xf9000006) }},
		{"consumer_loaded_from_other_pointer", func(f arm64OAuthFixture) { f.set(f.consumer+8, 0xf9400026) }},
		{"consumer_load_clobbers_pointer", func(f arm64OAuthFixture) { f.set(f.consumer+8, 0xf9400000) }},
		{"wrong_consumer_low_characters", func(f arm64OAuthFixture) { f.set(f.consumer+12, arm64TestMOVZ(7, 0x6f64)) }},
		{"wrong_consumer_high_characters", func(f arm64OAuthFixture) { f.set(f.consumer+24, arm64TestMOVK(7, 0x7264, 48)) }},
		{"consumer_movk_uses_other_register", func(f arm64OAuthFixture) { f.set(f.consumer+16, arm64TestMOVK(8, 0x736e, 16)) }},
		{"consumer_movk_uses_other_shift", func(f arm64OAuthFixture) { f.set(f.consumer+20, arm64TestMOVK(7, 0x6d75, 16)) }},
		{"consumer_constant_clobbers_pointer", func(f arm64OAuthFixture) { f.set(f.consumer+12, arm64TestMOVZ(0, 0x6f63)) }},
		{"consumer_comparison_uses_other_loaded_register", func(f arm64OAuthFixture) { f.set(f.consumer+28, 0xeb07011f) }},
		{"consumer_comparison_uses_other_constant", func(f arm64OAuthFixture) { f.set(f.consumer+28, 0xeb0800df) }},
		{"consumer_comparison_uses_32_bits", func(f arm64OAuthFixture) { f.set(f.consumer+28, 0x6b0700df) }},
		{"different_consumer_character_failure", func(f arm64OAuthFixture) { f.set(f.consumer+32, arm64TestBranch(f.consumer+32, f.failure-4, true)) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := arm64Fixture(t, 0x1000, 0x8000, nil, nil)
			test.mutate(fixture)
			id, secret := fixture.image.arm64ConsumerPair(fixture.code, fixture.base)
			if id != "" || secret != "" {
				t.Fatal("disconnected or unsupported selector accepted an OAuth pair")
			}
		})
	}
}

func TestARM64ConsumerSelectedPathFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(arm64OAuthFixture)
	}{
		{"unsupported_instruction_after_literals", func(f arm64OAuthFixture) { f.set(f.jump, 0xca040084) }},
		{"jump_to_consumer_guard", func(f arm64OAuthFixture) { f.set(f.jump, arm64TestBranch(f.jump, f.consumer, false)) }},
		{"jump_to_consumer_literals", func(f arm64OAuthFixture) { f.set(f.jump, arm64TestBranch(f.jump, f.start, false)) }},
		{"conditional_path_after_literals", func(f arm64OAuthFixture) { f.set(f.jump, arm64TestBranch(f.jump, f.tail, true)) }},
		{"call_after_literals", func(f arm64OAuthFixture) { f.set(f.jump, arm64TestBranch(f.jump, f.tail, false)|0x80000000) }},
		{"backward_jump_to_gcp_sibling", func(f arm64OAuthFixture) { f.set(f.jump, arm64TestBranch(f.jump, 36, false)) }},
		{"jump_to_failure", func(f arm64OAuthFixture) { f.set(f.jump, arm64TestBranch(f.jump, f.failure, false)) }},
		{"missing_return", func(f arm64OAuthFixture) { f.set(f.failure-4, 0xd503201f) }},
		{"wrong_secret_return_length", func(f arm64OAuthFixture) { f.set(f.tail, arm64TestMOVZ(3, 36)) }},
		{"wrong_id_return_length", func(f arm64OAuthFixture) { f.set(f.start, arm64TestMOVZ(0, 1)) }},
		{"conditional_common_tail", func(f arm64OAuthFixture) { f.set(f.tail+4, arm64TestBranch(f.tail+4, f.failure, true)) }},
		{"tail_overwrites_id", func(f arm64OAuthFixture) { f.set(f.tail+4, arm64TestMOVZ(1, 0)) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := arm64Fixture(t, 0x1000, 0x8000, nil, nil)
			test.mutate(fixture)
			id, secret := fixture.image.arm64ConsumerPair(fixture.code, fixture.base)
			if id != "" || secret != "" {
				t.Fatal("unsupported selected control flow or return accepted an OAuth pair")
			}
		})
	}
}

func TestARM64RegisterWritesLoadsAndStores(t *testing.T) {
	for _, test := range []struct {
		name                 string
		afterPage, afterPair []uint32
		accepted             bool
	}{
		{"store_preserves_page_source", []uint32{0xf90007e1}, nil, true},       // STR X1,[SP,#8].
		{"pair_store_preserves_both_sources", nil, []uint32{0xa9000be1}, true}, // STP X1,X2,[SP].
		{"load_clobbers_page", []uint32{0xf94007e1}, nil, false},
		{"pair_load_clobbers_first_destination", []uint32{0xa9400fe1}, nil, false},  // LDP X1,X3,[SP].
		{"pair_load_clobbers_second_destination", []uint32{0xa94007e3}, nil, false}, // LDP X3,X1,[SP].
		{"pair_load_clobbers_return_pointers", nil, []uint32{0xa9400be1}, false},
		{"indexed_store_clobbers_base", []uint32{0xf8008c22}, nil, false},        // STR X2,[X1,#8]!.
		{"pair_store_writeback_clobbers_base", []uint32{0xa9800c22}, nil, false}, // STP X2,X3,[X1,#0]!.
		{"immediate_write_clobbers_page", []uint32{arm64TestMOVZ(1, 0)}, nil, false},
		{"unknown_write_fails_closed", []uint32{0xca010021}, nil, false}, // EOR X1,X1,X1.
		{"unknown_write_to_unrelated_register_fails_closed", nil, []uint32{0xca040084}, false},
		{"unsupported_atomic_encoding_fails_closed", nil, []uint32{0xf8200028}, false}, // LDADD X0,X8,[X1].
		{"load_to_unrelated_register_is_safe", nil, []uint32{0xf94007e8}, true},
		{"nop_is_safe", nil, []uint32{0xd503201f}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := arm64Fixture(t, 0x1000, 0x8000, test.afterPage, test.afterPair)
			id, secret := fixture.image.arm64ConsumerPair(fixture.code, fixture.base)
			if test.accepted {
				if id != fixture.id || secret != fixture.secret {
					t.Fatal("non-writing load/store operation incorrectly discarded selected credential references")
				}
			} else if id != "" || secret != "" {
				t.Fatal("clobbered credential register or unsupported instruction was accepted")
			}
		})
	}
}

func TestARM64RejectsTruncatedSelectorsAndReturns(t *testing.T) {
	fixture := arm64Fixture(t, 0x1000, 0x8000, nil, nil)
	for size := 0; size <= fixture.failure-4; size++ {
		id, secret := fixture.image.arm64ConsumerPair(fixture.code[:size], fixture.base)
		if id != "" || secret != "" {
			t.Fatal("truncated selector or missing complete return was accepted")
		}
	}
}

func TestARM64RejectsConflictingNativeConsumerSelectors(t *testing.T) {
	first := arm64Fixture(t, 0x1000, 0x8000, nil, nil)
	secondBase := first.base + uint64(len(first.code))
	second := arm64Fixture(t, secondBase, 0xa000, nil, nil)
	// Replace only generated literals with equally long generated alternatives.
	secondPool := second.image.sections[1].data
	copy(secondPool[bytes.Index(secondPool, []byte(second.id)):], strings.Replace(second.id, "generated_consumer", "generated_variant", 1))
	copy(secondPool[bytes.Index(secondPool, []byte(second.secret)):], "GOCSPX-"+strings.Repeat("C", 28))
	code := append(first.code, second.code...)
	image := nativeImage{arch: "arm64", sections: []nativeSection{first.image.sections[1], second.image.sections[1]}}
	id, secret := image.arm64ConsumerPair(code, first.base)
	if id != "" || secret != "" {
		t.Fatal("two distinct native consumer selector results were treated as unambiguous")
	}
	// A valid earlier result cannot hide a later unsupported consumer path.
	second.set(second.jump, 0x94000000)
	code = append(first.code[:len(first.code):len(first.code)], second.code...)
	id, secret = image.arm64ConsumerPair(code, first.base)
	if id != "" || secret != "" {
		t.Fatal("valid selector masked an unsupported consumer path")
	}
}

func TestARM64ADDHonorsPageShift(t *testing.T) {
	fixture := arm64Fixture(t, 0x1000, 0x9000, []uint32{0x91400421}, nil) // ADD X1,X1,#1,LSL #12.
	// The initial ADRP supplies the preceding page; shifted ADD advances it.
	page := fixture.start + 4
	fixture.set(page, arm64TestADRP(1, fixture.base+uint64(page), 0x8000))
	id, secret := fixture.image.arm64ConsumerPair(fixture.code, fixture.base)
	if id != fixture.id || secret != fixture.secret {
		t.Fatal("shifted ADD did not resolve the selected credential address")
	}
}

func TestARM64ConsumerSelectionIgnoresStringOrderAndUnselectedSibling(t *testing.T) {
	for _, consumerFirst := range []bool{false, true} {
		fixture := arm64Fixture(t, 0x1000, 0x8000, nil, nil, consumerFirst)
		fixture.set(36, 0x94000000) // Unsupported call in the unselected GCP body.
		id, secret := fixture.image.arm64ConsumerPair(fixture.code, fixture.base)
		if id != fixture.id || secret != fixture.secret {
			t.Fatal("GCP body or literal order selected the consumer identity")
		}
	}
}

func TestBinaryCredentialsResolveARM64ConsumerThroughNativeMetadata(t *testing.T) {
	fixture := arm64Fixture(t, 0x1000, 0x2000, nil, nil)
	metadata := syntheticSelectorMetadata(len(fixture.code))
	metadata[6] = 4 // ARM64 minimum instruction size.
	data := syntheticELF(fixture.code, append(fixture.image.sections[1].data, metadata...))
	binary.LittleEndian.PutUint16(data[18:20], 183) // EM_AARCH64.
	id, secret, err := extractBinaryCredentials(data)
	if err != nil || id != fixture.id || secret != fixture.secret {
		t.Fatalf("ARM64 native metadata did not select the consumer pair: %v", err)
	}
}

func TestARM64RejectsAmbiguousSelectedReferences(t *testing.T) {
	fixture := arm64Fixture(t, 0x1000, 0x8000, nil, []uint32{0, 0})
	pool := fixture.image.sections[1]
	address := pool.address + uint64(bytes.Index(pool.data, []byte("GOCSPX-"+strings.Repeat("A", 28))))
	pc := fixture.jump - 8
	fixture.set(pc, arm64TestADRP(8, fixture.base+uint64(pc), address))
	fixture.set(pc+4, arm64TestADD(8, 8, address&4095))
	id, secret := fixture.image.arm64ConsumerPair(fixture.code, fixture.base)
	if id != "" || secret != "" {
		t.Fatal("transient second secret reference was treated as an unambiguous return")
	}
}

func TestARM64ConsumerCommonReturnAndAlignment(t *testing.T) {
	fixture := arm64Fixture(t, 0x1000, 0x8000, nil, nil)
	fixture.set(fixture.jump, arm64TestBranch(fixture.jump, fixture.tail, false))
	id, secret := fixture.image.arm64ConsumerPair(fixture.code, fixture.base)
	if id != fixture.id || secret != fixture.secret {
		t.Fatal("forward branch to the consumer common return lost its selected pair")
	}
	if id, secret := fixture.image.arm64ConsumerPair(fixture.code, fixture.base+1); id != "" || secret != "" {
		t.Fatal("misaligned ARM64 function address accepted a pair")
	}
	if id, secret := fixture.image.arm64ConsumerPair(append(fixture.code, 0), fixture.base); id != "" || secret != "" {
		t.Fatal("partial trailing ARM64 instruction accepted a pair")
	}
}
