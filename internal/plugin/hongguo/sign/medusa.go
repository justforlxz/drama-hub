// SPDX-License-Identifier: GPL-3.0-only
// Signing algorithms adapted from woshishiq1/drpys
// commit 22261adfa31435e3b3ef5a730b8a2a75bcaf1715, spider/js/红果果[短].js,
// via N3urda/hongguoTV eea73e6a45569a2cbbb9440f7cd5290c459b0b70,
// ported to Go from jackotom/hongguo-drama src/native/app-source.js.

package sign

import (
	"encoding/base64"
	"encoding/binary"
)

// branchOf 是签名分支选择：分支 0 与分支 2 各有独立的散列路径，其余分支共用
// 一条退化路径。取流请求必须落在分支 0，所以 ticket 要重试到 branchOf == 0。
func branchOf(url string, body []byte, khronos uint32) int {
	bodyMd5 := zero16()
	if len(body) > 0 {
		bodyMd5 = md5Sum(body)
	}
	q := SM3([]byte(queryOf(url)))
	iv := getIv(getIv(getIv(0x20230928, q), bodyMd5), le32(khronos))
	low := iv & 15
	return int(low) - int((low*171)>>9)*3
}

func queryOf(url string) string {
	for i := 0; i < len(url); i++ {
		if url[i] == '?' {
			return url[i+1:]
		}
	}
	return ""
}

var md5V3Shifts = [4][16]uint32{
	{7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22},
	{5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20},
	{4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23},
	{6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21},
}

func md5V3Step(kind int, a, b, c, d, m, shift, constant uint32) uint32 {
	var f uint32
	switch kind {
	case 0:
		f = (b & c) | (^b & d)
	case 1:
		f = (b & d) | (c & ^d)
	case 2:
		f = b ^ c ^ d
	default:
		f = c ^ (b | ^d)
	}
	return b + rol32(a+f+m+constant, shift)
}

// md5SumV3 是参考实现的 32 字节变体散列：16 字节主体 + 4 字节 32 位校验和。
func md5SumV3(message []byte, countV2 uint32, orders []byte, countV1 uint32) []byte {
	sv := make([]uint32, len(branch2SV))
	for i, v := range branch2SV {
		sv[i] = ror32(v, countV1)
	}
	start := [4]uint32{
		ror32(0x79e0f2fb, countV2), ror32(0xc8b52570, countV2),
		ror32(0xebc2f8cd, countV2), ror32(0x7c104d93, countV2),
	}
	endCount := (countV2 + 6) & 255
	end := [4]uint32{
		ror32(0x19be4866, endCount), ror32(0xe85986b4, endCount),
		ror32(0xe19b326e, endCount), ror32(0x71d1d7d4, endCount),
	}
	var m [16]uint32
	for i := 0; i < 16; i++ {
		m[i] = le32At(message[i*4:])
	}
	a, b, c, d := start[0], start[1], start[2], start[3]
	run := func(kind, offset int) {
		for i := 0; i < 16; i++ {
			mi := m[orders[offset+i]]
			sh := md5V3Shifts[kind][i]
			cs := sv[offset+i]
			switch i & 3 {
			case 0:
				a = md5V3Step(kind, a, b, c, d, mi, sh, cs)
			case 1:
				d = md5V3Step(kind, d, a, b, c, mi, sh, cs)
			case 2:
				c = md5V3Step(kind, c, d, a, b, mi, sh, cs)
			case 3:
				b = md5V3Step(kind, b, c, d, a, mi, sh, cs)
			}
		}
	}
	run(0, 0)
	run(1, 16)
	run(2, 32)
	run(3, 48)

	ret := make([]byte, 16)
	binary.LittleEndian.PutUint32(ret[0:], (start[0]+a)^end[0])
	binary.LittleEndian.PutUint32(ret[4:], (start[1]+b)^end[1])
	binary.LittleEndian.PutUint32(ret[8:], (start[2]+c)^end[2])
	binary.LittleEndian.PutUint32(ret[12:], (start[3]+d)^end[3])
	return concat(ret, le32(sumMd5(ret)))
}

// branch2F13 是分支 2 的散列路径（另一套置换表与初值）。
func branch2F13(iv uint32, querySm3, bodyMd5, tsBytes []byte, khronos uint32) []byte {
	v := (iv & 13) * 86
	ivV0 := ((v >> 15) & 255) + ((v >> 8) & 255) // 由 v 的取值域保证 ≤ 4
	n1List := [...]uint32{0x8980f29b, 0xeb549c7f, 0xb08726db, 0xd40cb5e6, 0xe8f559e4}
	n1 := n1List[ivV0]
	countV1 := (n1 + khronos + 1) & 255
	countV2 := n1 + khronos
	shift := (countV2 + 5) & 7
	seed := []byte{0x84, 0x96, 0x77, 0x9d, 0xd4, 0x15, 0x0b, 0xf8}
	pad := make([]byte, len(seed))
	for i, value := range seed {
		pad[i] = byte(((uint32(value) | uint32(value)<<8) >> shift) & 255)
	}
	input := concat(querySm3, bodyMd5, tsBytes, pad, []byte{0xa0, 0x01, 0x00, 0x00})
	off := int(ivV0) << 6
	return md5SumV3(input, countV2, branch2Orders[off:off+64], countV1)
}

// hashF13 是 medusa 里的「query+body+时间戳」联合散列，按 iv 的取值选三条路径。
func hashF13(querySm3, bodyMd5, tsBytes []byte, khronos uint32) []byte {
	iv := getIv(getIv(getIv(0x20230928, querySm3), bodyMd5), tsBytes)
	low := iv & 15
	ivV0 := int((low * 171) >> 9) // ≤ 5，由 low ∈ [0,15] 保证
	if branch := int(low) - ivV0*3; branch == 2 {
		return branch2F13(iv, querySm3, bodyMd5, tsBytes, khronos)
	} else if branch != 0 {
		return concat(querySm3[:16], bodyMd5, le32(sumMd5(concat(querySm3, bodyMd5))))
	}

	ivV1 := ivV1List[ivV0]
	countV1 := (ivV1 + khronos + 1) & 255
	countV2 := ivV1 + khronos
	tt := make([]uint32, len(tt02))
	for i, v := range tt02 {
		tt[i] = ror32(v, countV1)
	}
	n0 := (countV2 + 2) & 7
	seed := []byte{0xfa, 0x45, 0x61, 0xd7}
	pad := make([]byte, len(seed))
	for i, value := range seed {
		pad[i] = byte(((uint32(value) | uint32(value)<<8) >> n0) & 255)
	}
	data := concat(querySm3, bodyMd5, tsBytes, pad, []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0xa0})
	di := make([]uint32, 128)
	for i := 0; i < 16; i++ {
		di[i] = be32At(data[i*4:])
	}
	prev := di[0]
	for i := 0; i < 112; i++ {
		d1 := di[i+1]
		d14 := di[i+14]
		r1 := rol32(d1, 14) ^ rol32(d1, 25) ^ (d1 >> 3)
		r2 := rol32(d14, 13) ^ rol32(d14, 15) ^ (d14 >> 10)
		di[i+16] = prev + di[i+9] + r1 + r2
		prev = d1
	}

	init := make([]uint32, len(aesInitWords))
	for i, v := range aesInitWords {
		init[i] = ror32(v, countV2&31)
	}
	chosen := aesChosen[ivV0]
	d := make([]uint32, len(init))
	copy(d, init)
	for i := 0; i < chosen[0]; i++ {
		base := ivV1 + uint32(i)
		n0v := di[base&127]
		n1 := ((d[chosen[3]] ^ d[chosen[4]]) & d[chosen[1]]) ^ d[chosen[3]]
		n2 := rol32(d[chosen[1]], 26) ^ rol32(d[chosen[1]], 21) ^ rol32(d[chosen[1]], 7)
		n4 := n0v + n1 + n2 + tt[base&63] + d[chosen[5]]
		n5 := rol32(d[chosen[2]], 30) ^ rol32(d[chosen[2]], 19) ^ rol32(d[chosen[2]], 10)
		n6 := (d[chosen[2]] & d[chosen[6]]) | ((d[chosen[2]] | d[chosen[6]]) & d[chosen[7]])
		n7 := n5 + n6
		old := d[chosen[9]]
		last := d[len(d)-1]
		copy(d[1:], d[:len(d)-1])
		d[0] = last
		d[chosen[10]] = n7 + n4
		d[chosen[8]] = old + n4
	}

	ret := make([]byte, 32)
	for i := 0; i < 8; i++ {
		be32Put(ret[i*4:], d[i]+init[i])
	}
	for i := 0; i < 16; i++ {
		ret[i] ^= ret[i+16]
	}
	return concat(ret[:16], le32(sumMd5(ret[:16])))
}

// xmxor 是 medusa 的内层可逆混淆（逐字节 + 首尾互相关联）。
func xmxor(data, key []byte) []byte {
	encoded := make([]byte, len(data))
	for i := 0; i < len(data); i++ {
		at := (i * 4) & 28
		d0 := key[at]
		d1 := key[at+1]
		v := (uint32(data[i])<<4 | uint32(data[i])>>4) & 255
		d2 := int32(v) + int32(d0)
		d2 = ^d2 ^ int32(d1)
		low := uint32(d2) & 255
		d2 = int32(((low << 3) | (low >> 5)) & 255)
		d2 = (d2 + int32(d1)) & 255
		d2 = (d2 ^ int32(d0)) & 255
		encoded[len(data)-i-1] = byte(^d2 & 255)
	}
	last := encoded[len(encoded)-1] ^ encoded[len(encoded)-2]
	first := encoded[0]
	encoded[0] = byte((^int32(last) + int32(first)) & 255)
	encoded[1] = byte((int32(encoded[0]) ^ int32(encoded[len(encoded)-1]) ^ 254 + int32(encoded[1])) & 255)
	rot := byte(encoded[1]<<3 | encoded[1]>>5)
	encoded[2] = byte((int32(encoded[2]) + ((int32(last) - int32(first)) ^ int32(rot) ^ 2)) & 255)
	for i := 0; i < len(encoded)-4; i++ {
		rot2 := byte(encoded[i+2]<<3 | encoded[i+2]>>5)
		temp := int32(rot2) ^ int32(encoded[i+1]) ^ int32(i+3)
		encoded[i+3] = byte((^temp + int32(encoded[i+3])) & 255)
	}
	encoded[len(encoded)-1] ^= encoded[len(encoded)-2]
	sum := 0
	for i := 0; i < len(encoded)-1; i++ {
		sum += int(encoded[i+1])
	}
	encoded[0] = byte((int32(encoded[0]) ^ int32(encoded[1]) + int32(sum)) & 255)
	return encoded
}

// keyHash 派生 medusa 的混淆密钥与轮换种子。
func keyHash(signKey []byte, random uint32) ([]byte, []byte) {
	hash := SM3(concat(signKey, le32(random), signKey))
	d1 := (random >> 16) & 255
	d2 := ((d1 << 11) | (random >> 24)) ^ (d1 >> 5) ^ d1
	return hash, le32(^d2)
}

// aesV3 是参考实现里那套「把 32 字节密钥藏进请求体低比特」的 AES 变体。
type aesV3 struct {
	box   []byte
	con   [4]int
	con2  [4]int
	order [16]int
	keys  [][4]byte
}

var aesCon = [4][4]int{{1, 0, 2, 3}, {1, 3, 0, 2}, {0, 1, 3, 2}, {1, 0, 2, 3}}
var aesCon2 = [4][4]int{{1, 0, 2, 3}, {2, 0, 3, 1}, {0, 1, 3, 2}, {1, 0, 2, 3}}
var aesOrder = [4][16]int{
	{0, 9, 14, 11, 4, 13, 2, 7, 8, 1, 6, 15, 12, 5, 10, 3},
	{0, 9, 14, 15, 4, 13, 2, 7, 8, 1, 6, 3, 12, 5, 10, 11},
	{0, 9, 14, 7, 4, 13, 2, 11, 8, 1, 6, 3, 12, 5, 10, 15},
	{0, 9, 14, 11, 4, 13, 2, 7, 8, 1, 6, 15, 12, 5, 10, 3},
}
var aesInit = [4]uint32{0xca025ddc, 0x823dc546, 0xc9420583, 0xc298225f}

func newAesV3(key []byte, khronos uint32) *aesV3 {
	ws := int(khronos & 3)
	a := &aesV3{
		box:   sbox[ws*256 : (ws+1)*256],
		con:   aesCon[ws],
		con2:  aesCon2[ws],
		order: aesOrder[ws],
	}
	init := aesInit[ws]
	initial := make([]byte, 16)
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint32(initial[i*4:], init)
	}
	mk := make([]byte, 16)
	for i := 0; i < 16; i++ {
		mk[i] = initial[i] ^ key[i]
	}
	expanded := concat(mk, make([]byte, 32))
	rounds := 8
	for i := 4; i < 12; i++ {
		at := 4 * (i - 1)
		a0, b0, c0, d0 := expanded[at], expanded[at+1], expanded[at+2], expanded[at+3]
		if i&3 == 0 {
			t := byte((init >> uint(rounds&24)) ^ uint32(a.box[b0]))
			b0 = a.box[c0]
			c0 = a.box[d0]
			d0 = a.box[a0]
			a0 = t
		}
		rounds += 2
		expanded[at+4] = a0 ^ expanded[at-12]
		expanded[at+5] = b0 ^ expanded[at-11]
		expanded[at+6] = c0 ^ expanded[at-10]
		expanded[at+7] = d0 ^ expanded[at-9]
	}
	a.keys = make([][4]byte, 12)
	for i := 0; i < 12; i++ {
		copy(a.keys[i][:], expanded[i*4:i*4+4])
	}
	return a
}

func (a *aesV3) add(rows *[4][4]byte, key [][4]byte) {
	for i := 0; i < 4; i++ {
		for j := 0; j < 4; j++ {
			rows[i][j] ^= key[i][j]
		}
	}
}

func (a *aesV3) addCon(rows *[4][4]byte, keys [][4]byte) {
	for i := 0; i < 4; i++ {
		for j := 0; j < 4; j++ {
			rows[i][j] ^= keys[i][a.con2[j]]
		}
	}
}

func (a *aesV3) sub(rows *[4][4]byte) {
	for i := 0; i < 4; i++ {
		for j := 0; j < 4; j++ {
			rows[i][j] = a.box[rows[i][j]]
		}
	}
	old := *rows
	for i := 0; i < 4; i++ {
		rows[i] = old[a.con2[i]]
	}
}

func (a *aesV3) shift(rows *[4][4]byte) {
	var flat [16]byte
	for i := 0; i < 4; i++ {
		copy(flat[i*4:], rows[i][:])
	}
	for i := 0; i < 16; i++ {
		rows[i/4][i%4] = flat[a.order[i]]
	}
}

func (a *aesV3) shiftCon(rows *[4][4]byte) {
	old := *rows
	for i := 0; i < 4; i++ {
		for j := 0; j < 4; j++ {
			rows[i][j] = old[i][a.con2[j]]
		}
	}
}

func (a *aesV3) mix(rows *[4][4]byte) {
	xt := func(x byte) byte {
		if x&128 != 0 {
			return (x << 1) ^ 0x1b
		}
		return x << 1
	}
	for i := 0; i < 4; i++ {
		t := rows[0][i] ^ rows[1][i] ^ rows[2][i] ^ rows[3][i]
		u := rows[0][i]
		rows[0][i] ^= t ^ xt(rows[0][i]^rows[1][i])
		rows[1][i] ^= t ^ xt(rows[1][i]^rows[2][i])
		rows[2][i] ^= t ^ xt(rows[2][i]^rows[3][i])
		rows[3][i] ^= t ^ xt(rows[3][i]^u)
	}
}

func (a *aesV3) block(value []byte) []byte {
	var rows [4][4]byte
	for i := 0; i < 4; i++ {
		copy(rows[i][:], value[i*4:i*4+4])
	}
	a.addCon(&rows, a.keys[0:4])
	for i := 1; i < 3; i++ {
		a.sub(&rows)
		a.shift(&rows)
		if i == 1 {
			a.shiftCon(&rows)
			a.mix(&rows)
		}
		a.addCon(&rows, a.keys[i*4:i*4+4])
	}
	a.add(&rows, a.keys[4:8])
	out := make([]byte, 16)
	for i := 0; i < 4; i++ {
		copy(out[i*4:], rows[i][:])
	}
	return out
}

// encrypt 把 packed 的前 248 字节塞成 32 字节密钥流再写回低位比特，
// 返回 33 字节（末字节 + 改写后的 packed）——上游按这个形态解包。
func (a *aesV3) encrypt(packed, iv []byte) []byte {
	at := func(i int) byte {
		if i < len(packed) {
			return packed[i]
		}
		return 0
	}
	plaintext := make([]byte, 32)
	for i := 0; i < 31; i++ {
		off := i * 8
		n0 := (at(off) >> 4) & 2
		n1 := n0 | (at(off+1) & 64)
		n2 := n1 | ((at(off+2) >> 2) & 1)
		n3 := n2 | ((at(off+3) << 3) & 128)
		n4 := n3 | ((at(off+4) >> 1) & 4)
		n5 := n4 | ((at(off+5) << 3) & 16)
		n6 := n5 | ((at(off+6) << 5) & 32)
		plaintext[i] = n6 | ((at(off+7) >> 4) & 8)
	}
	plaintext[31] = 1

	var blocks []byte
	previous := make([]byte, 16)
	copy(previous, iv)
	for i := 0; i < len(plaintext); i += 16 {
		block := make([]byte, 16)
		for j := 0; j < 16; j++ {
			block[j] = plaintext[i+j] ^ previous[j]
		}
		value := a.block(block)
		blocks = append(blocks, value...)
		previous = value
	}

	out := make([]byte, len(packed))
	copy(out, packed)
	// 逐字节写入：参考实现对越界下标是静默忽略，所以这里也逐字节判界
	// （按组跳过会漏掉「组跨越末尾」的那一个字节）。
	set := func(idx int, clear, bits byte) {
		if idx < len(out) {
			out[idx] = out[idx]&clear | bits
		}
	}
	for i := 0; i < 31; i++ {
		off := i * 8
		k := blocks[i]
		set(off, 0xdf, (k<<4)&32)
		set(off+1, 0xbf, k&64)
		set(off+2, 0xfb, (k<<2)&4)
		set(off+3, 0xef, (k>>3)&16)
		set(off+4, 0xf7, (k+k)&8)
		set(off+5, 0xfd, (k>>3)&2)
		set(off+6, 0xfe, (k>>5)&1)
		set(off+7, 0x7f, (k<<4)&128)
	}
	return concat(blocks[31:32], out)
}

// deviceProto 是 medusa 里嵌的设备指纹消息（字段表照参考实现，是协议事实）。
func deviceProto(deviceID, nonce string, started int64) []byte {
	return concat(
		pSint(1, 1),
		pSint(2, 2),
		pString(3, "8662"),
		pString(4, deviceID),
		pString(5, nonce),
		pString(6, "!noperm!"),
		pSint(7, -888888),
		pSint(8, -888888),
		pSint(9, 3),
		pSint(10, -888888),
		pString(11, "!notset!"),
		pString(12, "Asia/Shanghai,8"),
		pString(13, "zh_CN"),
		pSint(14, 4),
		pFloat(16, 0),
		pFloat(17, 0),
		pFloat(18, 0),
		pFloat(19, 0),
		pFloat(20, 0),
		pFloat(21, 0),
		pString(22, "16"),
		pSint(23, 41),
		pSint(24, 36),
		pSint(25, started),
		pSint(26, started),
		pSint(27, started),
		pSint(28, started),
		pSint(29, -1),
		pString(30, "25053RT47C"),
		pString(31, "Redmi"),
		pString(32, "25053RT47C"),
		pString(33, "25053RT47C"),
		pString(34, "Xiaomi"),
		pString(35, "Redmi"),
		pString(36, "Redmi"),
		pSint(38, 31),
	)
}

// medusaSignKey 是 medusa 内层混淆的固定密钥（协议常量，不是凭据）。
var medusaSignKey = []byte{
	0x8e, 0xbd, 0xfa, 0x38, 0x06, 0xec, 0xc5, 0xce,
	0xe7, 0x94, 0x23, 0xe6, 0x02, 0x9e, 0xd8, 0x25,
	0x40, 0xbc, 0x22, 0x18, 0xbb, 0x7e, 0xae, 0xf7,
	0x1c, 0xb6, 0x91, 0xf7, 0xaa, 0x8a, 0xa2, 0xf5,
}

var medusaAesKey = []byte{
	0xf1, 0x59, 0x33, 0x76, 0x76, 0x6e, 0xa9, 0x8d,
	0x34, 0xf3, 0x1b, 0x05, 0x7a, 0x9d, 0x5b, 0xe4,
}

var medusaAesIV = []byte{
	0x1f, 0xe1, 0x09, 0xa4, 0x12, 0x52, 0x83, 0xf4,
	0x18, 0xde, 0x9e, 0x05, 0x1a, 0x96, 0x9e, 0x12,
}

// medusa 生成 x-medusa 头：query/body 摘要 → proto 打包 → 混淆 → AES 变体封装。
func (s *Signer) medusa(url string, body []byte, khronos uint32, ident Identity) string {
	bodyMd5 := zero16()
	if len(body) > 0 {
		bodyMd5 = md5Sum(body)
	}
	query := queryOf(url)
	querySm3 := SM3([]byte(query))
	ts := le32(khronos)
	queryBodyTs := hashF13(querySm3, bodyMd5, ts, khronos)

	nested := concat(pSint(1, 111), pSint(2, 10), pSint(3, 694367), pSint(5, 586952199))
	messageRand := s.rand32()
	envLaunch := s.randInt(21) + 100
	envPid := s.randInt(2000) + 10001
	env := concat(
		pSint(1, envLaunch),
		pSint(2, 146331399),
		pSint(3, 146331396),
		pSint(5, 7),
		pString(6, "v04.06.04.03-bugfix"),
		pSint(7, envPid),
		pMessage(12, deviceProto(ident.DeviceID, s.Nonce, s.Started)),
		pMessage(13, concat(
			pSint(1, s.now().Unix()),
			pSint(2, -2),
			pSint(4, 200),
		)),
		pString(14, ident.VersionName),
	)
	queryHash := SM3(concat([]byte(query), bodyMd5, []byte("none")))
	message := concat(
		pBytes(1, []byte{0xf7, 0xe8, 0x5f, 0xfa, 0xd7, 0xd7, 0xdc, 0x3b, 0xd6, 0x2a, 0xc8, 0x70, 0x57, 0xcf, 0x61, 0x18}),
		pSint(2, 3),
		pSint(3, int64(messageRand)),
		pString(4, "8662"),
		pString(5, ident.DeviceID),
		pString(6, "1588093228"),
		pString(7, ident.VersionName),
		pString(8, "v04.06.04-ml-android"),
		pSint(9, gorgonSDK),
		pBytes(10, []byte{0x40, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}),
		pSint(12, int64(khronos)),
		pBytes(13, queryBodyTs),
		pBytes(14, querySm3[:6]),
		pMessage(15, nested),
		pString(16, s.Nonce),
		pSint(17, int64(khronos)),
		pBytes(19, queryHash),
		pString(20, "none"),
		pSint(21, 312),
		pMessage(23, env),
		pString(24, `{"cmr":16777216,"cmr2":16777216,"un_h":1879194040,"vpn":0,"kd":0,"fkd":3672518972,"pd":-1872573247,"dyn":"","do":0,"tk":true}`),
	)

	random := s.rand32()
	hash, seed := keyHash(medusaSignKey, random)
	transformed := xmxor(message, hash)
	transformed = concat([]byte{0x40, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, transformed)
	for i, j := 0, len(transformed)-1; i < j; i, j = i+1, j-1 {
		transformed[i], transformed[j] = transformed[j], transformed[i]
	}
	for i := 0; i < len(transformed); i++ {
		transformed[i] ^= seed[(^uint32(i))&3]
	}

	check := uint32(querySm3[0]&63)<<14 | 0x18000001 | (uint32(queryBodyTs[0]&63) << 8)
	xmRand := s.rand32()
	packed := concat(
		[]byte{0x35},
		le32(xmRand),
		le32(check),
		transformed,
		[]byte{byte(random >> 16), byte(random >> 24)},
	)
	encrypted := newAesV3(medusaAesKey, khronos).encrypt(packed, medusaAesIV)

	version := []byte{
		0x03, 0x00, 0x00, 0x00,
		0xf7, 0xe8, 0x5f, 0xfa, 0xd7, 0xd7, 0xdc, 0x3b, 0xd6, 0x2a, 0xc8, 0x70,
		0x57, 0xcf, 0x61, 0x18,
	}
	prefix := make([]byte, 20)
	for i := 0; i < 20; i += 4 {
		binary.LittleEndian.PutUint32(prefix[i:], le32At(version[i:])^khronos)
	}
	return base64.StdEncoding.EncodeToString(concat(prefix, []byte{byte(random), byte(random >> 8), 0x00, 0x01}, encrypted))
}
