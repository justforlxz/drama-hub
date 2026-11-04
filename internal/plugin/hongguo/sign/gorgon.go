// SPDX-License-Identifier: GPL-3.0-only
// Signing algorithms adapted from woshishiq1/drpys
// commit 22261adfa31435e3b3ef5a730b8a2a75bcaf1715, spider/js/红果果[短].js,
// via N3urda/hongguoTV eea73e6a45569a2cbbb9440f7cd5290c459b0b70,
// ported to Go from jackotom/hongguo-drama src/native/app-source.js.

package sign

import "encoding/hex"

// gorgonSDK 是 x-gorgon 里写死的 SDK 版本号（上游协议事实，改了签名就废）。
const gorgonSDK = 67503104

func reverseBits8(v byte) byte {
	var out byte
	for i := 0; i < 8; i++ {
		out = out<<1 | (v>>uint(i))&1
	}
	return out
}

// rc4Stream 是参考实现的 RC4 变体（密钥调度只写 s[i] = s[j]，与教科书 RC4 不同，
// 必须照抄；「修正」成标准 RC4 会让上游直接判签名错）。
func rc4Stream(data, key []byte) []byte {
	s := make([]byte, 256)
	for i := range s {
		s[i] = byte(i)
	}
	j := 0
	for i := 0; i < 256; i++ {
		j = (j + int(s[i]) + int(key[i%len(key)])) & 255
		s[i] = s[j]
	}
	i, j := 0, 0
	out := make([]byte, len(data))
	for k, v := range data {
		i++
		x := s[i&255]
		j = (j + int(x)) & 255
		y := s[j]
		s[i&255] = y
		out[k] = v ^ s[(int(y)+int(y))&255]
	}
	return out
}

// xGorgon 生成 x-gorgon 头：4 字节魔数 + random + 固定尾，后接 RC4 变换后的摘要。
//
// query 是**已编码的完整查询串**，body 是**实际发出的字节**；两者与最终请求逐字节
// 一致，否则上游验签不过（这是本实现最容易踩的坑）。
func xGorgon(query string, body []byte, timestamp uint32, random uint16) string {
	qmd5 := md5Sum([]byte(query))
	bmd5 := zero16()
	if len(body) > 0 {
		bmd5 = md5Sum(body)
	}
	input := concat(
		qmd5[:4],
		bmd5[:4],
		make([]byte, 4),
		le32(gorgonSDK),
		be32(timestamp),
	)
	key := []byte{0x4a, 0x40, 0x16, byte(random >> 8), 0x47, 0x6c, 0x01, byte(random)}
	out := rc4Stream(input, key)
	for i := 0; i < len(out); i++ {
		value := (out[i]>>4 | out[i]<<4)
		next := out[0]
		if i+1 < len(out) {
			next = out[i+1]
		}
		out[i] = ^(reverseBits8((next^value)&255) ^ 20)
	}
	junk := []byte{byte(random), byte(random >> 8), 0x40, 0x01}
	return hex.EncodeToString(append([]byte{0x84, 0x04}, append(junk, out...)...))
}
