// SPDX-License-Identifier: GPL-3.0-only
// Signing algorithms adapted from woshishiq1/drpys
// commit 22261adfa31435e3b3ef5a730b8a2a75bcaf1715, spider/js/红果果[短].js,
// via N3urda/hongguoTV eea73e6a45569a2cbbb9440f7cd5290c459b0b70,
// ported to Go from jackotom/hongguo-drama src/native/app-source.js.

package sign

import (
	"crypto/md5"
	"encoding/binary"
)

// 32/64 位环绕与字节序工具：签名算法全程按 32 位无符号环绕，用 uint32
// 天然环绕，别引入 uint64 中间量（会改变溢出时机）。

func rol32(v uint32, n uint32) uint32 {
	n &= 31
	if n == 0 {
		return v
	}
	return v<<n | v>>(32-n)
}

func ror32(v uint32, n uint32) uint32 {
	n &= 31
	if n == 0 {
		return v
	}
	return v>>n | v<<(32-n)
}

func ror64(v uint64, n uint) uint64 {
	n &= 63
	if n == 0 {
		return v
	}
	return v>>n | v<<(64-n)
}

func le32(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}

func be32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func be32Put(dst []byte, v uint32) { binary.BigEndian.PutUint32(dst, v) }

func be32At(b []byte) uint32 { return binary.BigEndian.Uint32(b) }

func le32At(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }

func le64At(b []byte) uint64 { return binary.LittleEndian.Uint64(b) }

func md5Sum(data []byte) []byte {
	sum := md5.Sum(data)
	out := make([]byte, 16)
	copy(out, sum[:])
	return out
}

func zero16() []byte { return make([]byte, 16) }

func concat(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// getIv 是参考实现的查询串散列初值折叠：奇偶字节走两条不同的混合式。
func getIv(iv uint32, data []byte) uint32 {
	value := iv
	for i := 0; i < len(data); i++ {
		if i&1 == 0 {
			value = (value >> 4) ^ value ^ (value << 6) ^ uint32(data[i])
		} else {
			value = ^((value >> 7) ^ value ^ (uint32(data[i]) | (value << 12)))
		}
	}
	return value
}

// sumMd5 是参考实现的 32 位校验和，签名字段里当尾部校验用。
func sumMd5(data []byte) uint32 {
	check := uint32(0x20220420)
	for i := 0; i < 12; i++ {
		var temp uint32
		if i&1 == 0 {
			temp = (check >> 3) ^ check
			check = uint32(data[i]) ^ (check << 7)
		} else {
			temp = (check >> 5) ^ check
			check = uint32(data[i]) | (check << 11)
			check = ^check
		}
		check ^= temp
	}
	return (check | 4) ^ 0x1000000
}
