// SPDX-License-Identifier: GPL-3.0-only
// Signing algorithms adapted from woshishiq1/drpys
// commit 22261adfa31435e3b3ef5a730b8a2a75bcaf1715, spider/js/红果果[短].js,
// via N3urda/hongguoTV eea73e6a45569a2cbbb9440f7cd5290c459b0b70,
// ported to Go from jackotom/hongguo-drama src/native/app-source.js.

package sign

import (
	"encoding/binary"
	"math"
)

// protobuf 线格式的最小子集：签名里要把设备与请求信息按 proto 打包再加密，
// 字段编号与类型是上游协议事实，不能自己改（改了上游解不开，签名就作废）。

func putVarint(dst []byte, v uint64) []byte {
	for v > 0x7f {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

func zigzag(v int64) uint64 {
	if v < 0 {
		return uint64(-v)*2 - 1
	}
	return uint64(v) * 2
}

func pString(tag int, s string) []byte {
	if s == "" {
		return nil
	}
	return pBytes(tag, []byte(s))
}

func pBytes(tag int, b []byte) []byte {
	if b == nil {
		return nil
	}
	out := putVarint(nil, uint64(tag)<<3|2)
	out = putVarint(out, uint64(len(b)))
	return append(out, b...)
}

// pMessage 与 pBytes 线格式相同（嵌套消息也是一段长度前缀字节），分开命名只为可读。
func pMessage(tag int, b []byte) []byte { return pBytes(tag, b) }

func pSint(tag int, v int64) []byte {
	out := putVarint(nil, uint64(tag)<<3)
	return putVarint(out, zigzag(v))
}

func pFloat(tag int, v float32) []byte {
	out := putVarint(nil, uint64(tag)<<3|5)
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], math.Float32bits(v))
	return append(out, buf[:]...)
}
