// SPDX-License-Identifier: GPL-3.0-only
// Signing algorithms adapted from woshishiq1/drpys
// commit 22261adfa31435e3b3ef5a730b8a2a75bcaf1715, spider/js/红果果[短].js,
// via N3urda/hongguoTV eea73e6a45569a2cbbb9440f7cd5290c459b0b70,
// ported to Go from jackotom/hongguo-drama src/native/sm3.js.

package sign

// SM3 是 GB/T 32905 杂凑函数，签名里用它摘要查询串与设备字段。
//
// 只用标准库实现（不引依赖）：上游把 SM3 结果直接当签名材料，实现错一位
// 上游就静默拒绝，所以这里照参考实现逐轮移植，不换更「优雅」的写法。
func SM3(input []byte) []byte {
	// 填充：0x80 起，补零到 mod 64 == 56，再写 8 字节大端比特长度。
	n := len(input)
	total := (n + 9 + 63) / 64 * 64
	data := make([]byte, total)
	copy(data, input)
	data[n] = 0x80
	bitLen := uint64(n) * 8
	for i := 0; i < 8; i++ {
		data[total-1-i] = byte(bitLen >> (8 * uint(i)))
	}

	state := [8]uint32{
		0x7380166f, 0x4914b2b9, 0x172442d7, 0xda8a0600,
		0xa96f30bc, 0x163138aa, 0xe38dee4d, 0xb0fb0e4e,
	}
	var w [68]uint32
	for off := 0; off < total; off += 64 {
		for i := 0; i < 16; i++ {
			w[i] = be32At(data[off+i*4:])
		}
		for i := 16; i < 68; i++ {
			x := w[i-16] ^ w[i-9] ^ rol32(w[i-3], 15)
			w[i] = x ^ rol32(x, 15) ^ rol32(x, 23) ^ rol32(w[i-13], 7) ^ w[i-6]
		}
		a, b, c, d := state[0], state[1], state[2], state[3]
		e, f, g, h := state[4], state[5], state[6], state[7]
		for i := 0; i < 64; i++ {
			a12 := rol32(a, 12)
			t := uint32(0x7a879d8a)
			if i < 16 {
				t = 0x79cc4519
			}
			s1 := rol32(a12+e+rol32(t, uint32(i)), 7)
			s2 := s1 ^ a12
			var ff, gg uint32
			if i < 16 {
				ff = a ^ b ^ c
				gg = e ^ f ^ g
			} else {
				ff = (a & b) | (a & c) | (b & c)
				gg = (e & f) | (^e & g)
			}
			t1 := ff + d + s2 + (w[i] ^ w[i+4])
			t2 := gg + h + s1 + w[i]
			d = c
			c = rol32(b, 9)
			b = a
			a = t1
			h = g
			g = rol32(f, 19)
			f = e
			e = t2 ^ rol32(t2, 9) ^ rol32(t2, 17)
		}
		next := [8]uint32{a, b, c, d, e, f, g, h}
		for i := 0; i < 8; i++ {
			state[i] ^= next[i]
		}
	}

	out := make([]byte, 32)
	for i := 0; i < 8; i++ {
		be32Put(out[i*4:], state[i])
	}
	return out
}
