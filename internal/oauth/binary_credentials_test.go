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
	// Repeated references to one complete pair remain a safe single-pair fallback.
	gotID, gotSecret, err = extractBinaryCredentials([]byte(id + "\x00" + secret + "\x00" + id + "\x00" + secret))
	if err != nil || gotID != id || gotSecret != secret {
		t.Fatal("duplicate copies of one complete pair became ambiguous")
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

type amd64OAuthFixture struct {
	image                          nativeImage
	code, pool                     []byte
	base                           uint64
	id, secret                     string
	consumer, start, tail, failure int
}

// The instruction layout follows getOauthParams, with synthetic literals only.
func amd64Fixture(t *testing.T, base, poolBase uint64, consumerFirst bool, extra []byte) amd64OAuthFixture {
	t.Helper()
	id := "123456789012-generated_consumer_fixture.apps.googleusercontent.com"
	secret := "GOCSPX-" + strings.Repeat("B", 28)
	siblingID := "123456789012-generated_gcp_fixture.apps.googleusercontent.com"
	siblingSecret := "GOCSPX-" + strings.Repeat("A", 28)
	pool := []byte(siblingID + "\x00" + siblingSecret + "\x00" + id + "\x00" + secret + "pool_neighbor")
	if consumerFirst {
		pool = []byte(id + "\x00" + secret + "\x00" + siblingID + "\x00" + siblingSecret + "pool_neighbor")
	}
	code := []byte{0x48, 0x83, 0xfb, 3, 0x75, 0, 0x66, 0x81, 0x38, 'g', 'c', 0x75, 0, 0x80, 0x78, 2, 'p', 0x75, 0}
	appendPair := func(pairID, pairSecret string) {
		code = append(code, 0xb8, byte(len(pairID)), 0, 0, 0) // MOV EAX,len(ID).
		for index, literal := range []string{pairSecret, pairID} {
			instruction := []byte{0x48, 0x8d, byte(0x0d + index*8), 0, 0, 0, 0} // LEA RCX,secret; LEA RDX,ID.
			address := int64(poolBase) + int64(bytes.Index(pool, []byte(literal)))
			pc := int64(base) + int64(len(code)+7)
			binary.LittleEndian.PutUint32(instruction[3:], uint32(int32(address-pc)))
			code = append(code, instruction...)
		}
	}
	appendPair(siblingID, siblingSecret)
	siblingJump := len(code)
	code = append(code, 0xeb, 0)
	consumer := len(code)
	code = append(code, 0x48, 0x83, 0xfb, 8, 0x75, 0, 0x48, 0xba, 'c', 'o', 'n', 's', 'u', 'm', 'e', 'r', 0x48, 0x39, 0x10, 0x75, 0)
	start := len(code)
	appendPair(id, secret)
	code = append(code, extra...)
	tail := len(code)
	code = append(code, 0x89, 0xc3, 0xbf, 35, 0, 0, 0, 0x31, 0xf6, 0x45, 0x31, 0xc0,
		0x48, 0x89, 0xd0, 0x48, 0x83, 0xc4, 0x60, 0x5d, 0xc3)
	failure := len(code)
	code = append(code, 0xc3)
	for _, branch := range []struct{ pc, target int }{
		{4, consumer}, {11, failure}, {17, failure}, {siblingJump, tail}, {consumer + 4, failure}, {consumer + 19, failure},
	} {
		if branch.target-branch.pc-2 > 127 {
			t.Fatal("synthetic selector exceeds supported short-branch range")
		}
		code[branch.pc+1] = byte(branch.target - branch.pc - 2)
	}
	text := nativeSection{address: base, data: code}
	return amd64OAuthFixture{
		image: nativeImage{arch: "amd64", text: text, sections: []nativeSection{text, {address: poolBase, data: pool}}},
		code:  code, pool: pool, base: base, id: id, secret: secret,
		consumer: consumer, start: start, tail: tail, failure: failure,
	}
}

func TestNativeConsumerSelectionUsesCodeReferencesNotStringOrder(t *testing.T) {
	for _, consumerFirst := range []bool{false, true} {
		fixture := amd64Fixture(t, 0x1000, 0x2000, consumerFirst, nil)
		id, secret := fixture.image.amd64ConsumerPair(fixture.code, fixture.base)
		if id != fixture.id || secret != fixture.secret {
			t.Fatal("consumer native return did not select its pair independently of GCP sibling or literal order")
		}
		// Neither unsupported GCP-body instructions nor the sibling's references
		// are evidence of the selected consumer return.
		fixture.code[19] = 0xe8
		id, secret = fixture.image.amd64ConsumerPair(fixture.code, fixture.base)
		if id != fixture.id || secret != fixture.secret {
			t.Fatal("unselected GCP sibling changed the consumer pair")
		}
	}
}

func TestAMD64ConsumerSelectorsFailClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(amd64OAuthFixture)
	}{
		{"wrong_length", func(f amd64OAuthFixture) { f.code[3] = 4 }},
		{"length_compares_pointer", func(f amd64OAuthFixture) { f.code[2] = 0xf8 }},
		{"disconnected_gc_and_p_pointer", func(f amd64OAuthFixture) { f.code[14] = 0x79 }},
		{"wrong_p_literal", func(f amd64OAuthFixture) { f.code[16] = 'u' }},
		{"different_character_failures", func(f amd64OAuthFixture) { f.code[12]-- }},
		{"backward_consumer_branch", func(f amd64OAuthFixture) { f.code[5] = 0xff }},
		{"wrong_consumer_length", func(f amd64OAuthFixture) { f.code[f.consumer+3] = 7 }},
		{"different_consumer_length_register", func(f amd64OAuthFixture) { f.code[f.consumer+2] = 0xfa }},
		{"wrong_consumer_literal", func(f amd64OAuthFixture) { f.code[f.consumer+8] = 'C' }},
		{"consumer_literal_clobbers_pointer", func(f amd64OAuthFixture) { f.code[f.consumer+7] = 0xb8; f.code[f.consumer+18] = 0 }},
		{"consumer_compares_other_pointer", func(f amd64OAuthFixture) { f.code[f.consumer+18] = 0x11 }},
		{"consumer_comparison_is_a_load", func(f amd64OAuthFixture) { f.code[f.consumer+17] = 0x8b }},
		{"different_consumer_failure", func(f amd64OAuthFixture) { f.code[f.consumer+20]-- }},
		{"conditional_selected_path", func(f amd64OAuthFixture) { f.code[f.tail] = 0x75 }},
		{"call_selected_path", func(f amd64OAuthFixture) { f.code[f.tail] = 0xe8 }},
		{"jump_to_gcp_sibling", func(f amd64OAuthFixture) { f.code[f.tail] = 0xeb; f.code[f.tail+1] = byte(19 - f.tail - 2) }},
		{"unknown_instruction", func(f amd64OAuthFixture) { f.code[f.tail] = 0xf4 }},
		{"wrong_id_length", func(f amd64OAuthFixture) { f.code[f.start+1] = 1 }},
		{"wrong_secret_length", func(f amd64OAuthFixture) { f.code[f.tail+3] = 36 }},
		{"secret_pointer_clobbered", func(f amd64OAuthFixture) { f.code[f.tail+8] = 0xc9 }},
		{"missing_return", func(f amd64OAuthFixture) { f.code[f.failure-1] = 0x90 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := amd64Fixture(t, 0x1000, 0x2000, false, nil)
			test.mutate(fixture)
			if id, secret := fixture.image.amd64ConsumerPair(fixture.code, fixture.base); id != "" || secret != "" {
				t.Fatal("corrupt or unsupported consumer selector accepted a pair")
			}
		})
	}
}

func TestAMD64RejectsAmbiguousReferencesAndConflictingSelectors(t *testing.T) {
	// A transient reference to a second secret remains ambiguous even if a later
	// move would leave the original secret in the actual return registers.
	fixture := amd64Fixture(t, 0x1000, 0x2000, false, []byte{0x48, 0x8d, 0x35, 0, 0, 0, 0})
	extra := fixture.tail - 7
	address := int64(0x2000 + bytes.Index(fixture.pool, []byte("GOCSPX-"+strings.Repeat("A", 28))))
	binary.LittleEndian.PutUint32(fixture.code[extra+3:extra+7], uint32(int32(address-int64(fixture.base)-int64(extra+7))))
	if id, secret := fixture.image.amd64ConsumerPair(fixture.code, fixture.base); id != "" || secret != "" {
		t.Fatal("two selected-path secret references were treated as unambiguous")
	}
	first := amd64Fixture(t, 0x1000, 0x2000, false, nil)
	second := amd64Fixture(t, first.base+uint64(len(first.code)), 0x3000, false, nil)
	copy(second.pool[bytes.Index(second.pool, []byte(second.secret)):], "GOCSPX-"+strings.Repeat("C", 28))
	image := nativeImage{arch: "amd64", sections: []nativeSection{first.image.sections[1], second.image.sections[1]}}
	code := append(first.code, second.code...)
	if id, secret := image.amd64ConsumerPair(code, first.base); id != "" || secret != "" {
		t.Fatal("conflicting consumer selectors were treated as unambiguous")
	}
	// A valid earlier selector must not hide a later corrupt selector.
	second.code[second.tail] = 0xe8
	code = append(first.code[:len(first.code):len(first.code)], second.code...)
	if id, secret := image.amd64ConsumerPair(code, first.base); id != "" || secret != "" {
		t.Fatal("a valid selector masked an unsupported selected path")
	}
}

func TestAMD64RejectsTruncatedSelectorsAndReturns(t *testing.T) {
	fixture := amd64Fixture(t, 0x1000, 0x2000, false, nil)
	for size := 0; size <= fixture.failure; size++ {
		if id, secret := fixture.image.amd64ConsumerPair(fixture.code[:size], fixture.base); id != "" || secret != "" {
			t.Fatal("truncated native selector or failure bound accepted a pair")
		}
	}
}

func syntheticSelectorMetadata(codeSize int) []byte {
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
	binary.LittleEndian.PutUint32(metadata[pcln+8:pcln+12], uint32(codeSize))
	return metadata
}

func TestBinaryCredentialsResolveConsumerPairThroughNativeMetadata(t *testing.T) {
	fixture := amd64Fixture(t, 0x1000, 0x2000, false, nil)
	metadata := syntheticSelectorMetadata(len(fixture.code))
	data := syntheticELF(fixture.code, append(fixture.pool, metadata...))
	gotID, gotSecret, err := extractBinaryCredentials(data)
	if err != nil || gotID != fixture.id || gotSecret != fixture.secret {
		t.Fatalf("native metadata did not select the issuing consumer pair: %v", err)
	}
	// Corrupted metadata must never fall back to string-pool order.
	binary.LittleEndian.PutUint64(metadata[32:40], ^uint64(0))
	data = syntheticELF(fixture.code, append(fixture.pool, metadata...))
	if _, _, err := extractBinaryCredentials(data); err == nil {
		t.Fatal("invalid native name offsets selected a credential pair")
	}
	metadata = syntheticSelectorMetadata(len(fixture.code))
	pcln := int(binary.LittleEndian.Uint64(metadata[64:72]))
	binary.LittleEndian.PutUint32(metadata[pcln+8:pcln+12], uint32(len(fixture.code)+1))
	data = syntheticELF(fixture.code, append(fixture.pool, metadata...))
	if _, _, err := extractBinaryCredentials(data); err == nil {
		t.Fatal("selector extending outside the native text section selected a pair")
	}
}

func TestBinaryCredentialsRejectConflictingAndUnsupportedConsumerSelectors(t *testing.T) {
	first := amd64Fixture(t, 0x1000, 0x2000, false, nil)
	second := amd64Fixture(t, first.base+uint64(len(first.code)), 0x3000, false, nil)
	copy(second.pool[bytes.Index(second.pool, []byte(second.secret)):], "GOCSPX-"+strings.Repeat("C", 28))
	pool := make([]byte, 4096)
	copy(pool, first.pool)
	pool = append(pool, second.pool...)
	for _, corruptSecond := range []bool{false, true} {
		if corruptSecond {
			second.code[second.tail] = 0xe8
		}
		code := append(first.code[:len(first.code):len(first.code)], second.code...)
		data := syntheticELF(code, append(pool[:len(pool):len(pool)], syntheticSelectorMetadata(len(code))...))
		id, secret, err := extractBinaryCredentials(data)
		if err == nil || id != "" || secret != "" {
			t.Fatal("conflicting or corrupt native consumer selectors selected a pair")
		}
		if strings.Contains(err.Error(), first.secret) || strings.Contains(err.Error(), second.secret) {
			t.Fatal("failed native extraction disclosed credential literals")
		}
	}
}
