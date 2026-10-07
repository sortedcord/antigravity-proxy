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
	image                         nativeImage
	code                          []byte
	base                          uint64
	id, secret                    string
	consumer, tail, failure, jump int
}

func arm64TestMOVZ(register uint32, value int) uint32 {
	return 0xd2800000 | uint32(value)<<5 | register
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

func arm64Fixture(t *testing.T, base, poolBase uint64, afterPage, afterPair []uint32) arm64OAuthFixture {
	t.Helper()
	id := "123456789012-generated_gcp_fixture.apps.googleusercontent.com"
	secret := "GOCSPX-" + strings.Repeat("A", 28)
	consumerID := "987654321012-generated_consumer_fixture.apps.googleusercontent.com"
	consumerSecret := "GOCSPX-" + strings.Repeat("B", 28)
	// Deliberately place consumer first and concatenate the GCP secret with an
	// unrelated string-pool neighbor. Selection must come from instructions.
	pool := []byte(consumerID + "\x00" + consumerSecret + "\x00" + id + "\x00" + secret + "pool_neighbor")
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
		arm64TestMOVZ(0, len(id)),
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
	appendPair(id, secret, afterPage)
	words = append(words, afterPair...)
	jump := len(words) * 4
	words = append(words, 0) // B shared return, skipping the entire consumer sibling.
	consumer := len(words) * 4
	words = append(words, 0xf100203f, 0, arm64TestMOVZ(0, len(consumerID))) // CMP X1,#8; B.NE failure.
	appendPair(consumerID, consumerSecret, nil)
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
	words[jump/4] = arm64TestBranch(jump, tail, false)
	words[consumer/4+1] = arm64TestBranch(consumer+4, failure, true)
	code := make([]byte, len(words)*4)
	for index, word := range words {
		binary.LittleEndian.PutUint32(code[index*4:], word)
	}
	text := nativeSection{address: base, data: code}
	return arm64OAuthFixture{
		image: nativeImage{arch: "arm64", text: text, sections: []nativeSection{text, {address: poolBase, data: pool}}},
		code:  code, base: base, id: id, secret: secret,
		consumer: consumer, tail: tail, failure: failure, jump: jump,
	}
}

func (fixture arm64OAuthFixture) set(pc int, word uint32) {
	binary.LittleEndian.PutUint32(fixture.code[pc:pc+4], word)
}

func TestARM64CloudCodeSelectsGCPNotConsumer(t *testing.T) {
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
			id, secret := fixture.image.arm64CloudCodePair(fixture.code, fixture.base)
			if id != fixture.id || secret != fixture.secret {
				t.Fatal("native GCP return pair was not selected independently of consumer literal order")
			}
		})
	}
}

func TestARM64SelectorRequiresConnectedGCPLoadsAndComparisons(t *testing.T) {
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
		{"obsolete_gcu_selector", func(f arm64OAuthFixture) { f.set(28, 0x7101d4df) }},
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
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := arm64Fixture(t, 0x1000, 0x8000, nil, nil)
			test.mutate(fixture)
			id, secret := fixture.image.arm64CloudCodePair(fixture.code, fixture.base)
			if id != "" || secret != "" {
				t.Fatal("disconnected or unsupported selector accepted an OAuth pair")
			}
		})
	}
}

func TestARM64SelectedPathFailsClosedBeforeConsumer(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(arm64OAuthFixture)
	}{
		{"fallthrough_into_consumer", func(f arm64OAuthFixture) { f.set(f.jump, 0xd503201f) }},
		{"jump_to_consumer_guard", func(f arm64OAuthFixture) { f.set(f.jump, arm64TestBranch(f.jump, f.consumer, false)) }},
		{"jump_to_consumer_literals", func(f arm64OAuthFixture) { f.set(f.jump, arm64TestBranch(f.jump, f.consumer+8, false)) }},
		{"conditional_path_after_literals", func(f arm64OAuthFixture) { f.set(f.jump, arm64TestBranch(f.jump, f.tail, true)) }},
		{"call_after_literals", func(f arm64OAuthFixture) { f.set(f.jump, arm64TestBranch(f.jump, f.tail, false)|0x80000000) }},
		{"backward_jump", func(f arm64OAuthFixture) { f.set(f.jump, arm64TestBranch(f.jump, 36, false)) }},
		{"jump_to_failure", func(f arm64OAuthFixture) { f.set(f.jump, arm64TestBranch(f.jump, f.failure, false)) }},
		{"missing_return", func(f arm64OAuthFixture) { f.set(f.failure-4, 0xd503201f) }},
		{"wrong_secret_return_length", func(f arm64OAuthFixture) { f.set(f.tail, arm64TestMOVZ(3, 36)) }},
		{"wrong_id_return_length", func(f arm64OAuthFixture) { f.set(36, arm64TestMOVZ(0, 1)) }},
		{"conditional_common_tail", func(f arm64OAuthFixture) { f.set(f.tail+4, arm64TestBranch(f.tail+4, f.failure, true)) }},
		{"tail_overwrites_id", func(f arm64OAuthFixture) { f.set(f.tail+4, arm64TestMOVZ(1, 0)) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := arm64Fixture(t, 0x1000, 0x8000, nil, nil)
			test.mutate(fixture)
			id, secret := fixture.image.arm64CloudCodePair(fixture.code, fixture.base)
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
			id, secret := fixture.image.arm64CloudCodePair(fixture.code, fixture.base)
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
		id, secret := fixture.image.arm64CloudCodePair(fixture.code[:size], fixture.base)
		if id != "" || secret != "" {
			t.Fatal("truncated selector or missing complete return was accepted")
		}
	}
}

func TestARM64RejectsConflictingNativeGCPSelectors(t *testing.T) {
	first := arm64Fixture(t, 0x1000, 0x8000, nil, nil)
	secondBase := first.base + uint64(len(first.code))
	second := arm64Fixture(t, secondBase, 0xa000, nil, nil)
	// Replace only generated literals with equally long generated alternatives.
	secondPool := second.image.sections[1].data
	copy(secondPool[bytes.Index(secondPool, []byte(second.id)):], strings.Replace(second.id, "generated_gcp", "generated_alt", 1))
	copy(secondPool[bytes.Index(secondPool, []byte(second.secret)):], "GOCSPX-"+strings.Repeat("C", 28))
	code := append(first.code, second.code...)
	image := nativeImage{arch: "arm64", sections: []nativeSection{first.image.sections[1], second.image.sections[1]}}
	id, secret := image.arm64CloudCodePair(code, first.base)
	if id != "" || secret != "" {
		t.Fatal("two distinct native GCP selector results were treated as unambiguous")
	}
}

func TestARM64ADDHonorsPageShift(t *testing.T) {
	fixture := arm64Fixture(t, 0x1000, 0x9000, []uint32{0x91400421}, nil) // ADD X1,X1,#1,LSL #12.
	// The initial ADRP supplies the preceding page; shifted ADD advances it.
	fixture.set(40, arm64TestADRP(1, fixture.base+40, 0x8000))
	id, secret := fixture.image.arm64CloudCodePair(fixture.code, fixture.base)
	if id != fixture.id || secret != fixture.secret {
		t.Fatal("shifted ADD did not resolve the selected credential address")
	}
}
