// SPDX-License-Identifier: GPL-3.0-only
// Signing algorithms adapted from woshishiq1/drpys
// commit 22261adfa31435e3b3ef5a730b8a2a75bcaf1715, spider/js/红果果[短].js,
// via N3urda/hongguoTV eea73e6a45569a2cbbb9440f7cd5290c459b0b70,
// ported to Go from jackotom/hongguo-drama src/native/app-source.js.

package sign

import (
	"encoding/base64"
	"encoding/binary"
	"strconv"
)

// helios 生成 x-helios 头：64 位字上的自定义轮函数，输入是
// 「khronos-1588093228-8662」文本，输出 base64（前 4 字节是随机数）。
func (s *Signer) helios(khronos uint32) string {
	random := s.rand32()
	digest := md5Sum(concat(le32(random), []byte("8662")))
	ascii := make([]byte, 32)
	for i, b := range digest {
		hexByte := func(v byte) byte {
			if v < 10 {
				return '0' + v
			}
			return 'a' + v - 10
		}
		ascii[i*2] = hexByte(b >> 4)
		ascii[i*2+1] = hexByte(b & 15)
	}

	words := []uint64{le64At(ascii[0:]), le64At(ascii[8:]), le64At(ascii[16:]), le64At(ascii[24:])}
	table := []uint64{words[0]}
	b0, b8 := words[0], words[1]
	words = words[2:]
	for i := uint64(0); i < 34; i++ {
		x8 := ror64(b8, 8) + b0
		x8 ^= i
		words = append(words, x8)
		x8 ^= ror64(b0, 61)
		table = append(table, x8)
		b0 = x8
		b8 = words[0]
		words = words[1:]
	}

	text := []byte(strconv.FormatUint(uint64(khronos), 10) + "-1588093228-8662")
	size := (len(text) + 1 + 15) / 16 * 16
	pad := make([]byte, size)
	for i := range pad {
		pad[i] = byte(16 - len(text)%16)
	}
	copy(pad, text)

	out := make([]byte, 0, 4+size)
	out = append(out, le32(random)...)
	block := make([]byte, 16)
	for at := 0; at < len(pad); at += 16 {
		a := le64At(pad[at:])
		b := le64At(pad[at+8:])
		for i := 0; i < 34; i++ {
			b = table[i] ^ (a + ror64(b, 8))
			a = b ^ ror64(a, 61)
		}
		binary.LittleEndian.PutUint64(block[0:], a)
		binary.LittleEndian.PutUint64(block[8:], b)
		out = append(out, block...)
	}
	return base64.StdEncoding.EncodeToString(out)
}
