#!/usr/bin/env bash
# 生成测试用视频 fixture：两个 5 秒、编码参数不同的小 MP4 + 一个 m3u8。
# 生成物进 .gitignore，脚本本身进版本库。见 docs/TEST-STANDARDS.md。
set -euo pipefail
cd "$(dirname "$0")/.."

out=internal/store/testdata/media
mkdir -p "$out"

# 集 A：h264 + aac，30fps，yuv420p
ffmpeg -y -hide_banner -loglevel error \
  -f lavfi -i "testsrc2=size=320x240:rate=30:duration=5" \
  -f lavfi -i "sine=frequency=440:duration=5" \
  -c:v libx264 -pix_fmt yuv420p -profile:v baseline -level 3.0 \
  -c:a aac -shortest "$out/ep-a.mp4"

# 集 B：hevc + aac（解码器签名与 A 不同，用于测试 codec_sig 分派）
ffmpeg -y -hide_banner -loglevel error \
  -f lavfi -i "testsrc2=size=320x240:rate=30:duration=5" \
  -f lavfi -i "sine=frequency=880:duration=5" \
  -c:v libx265 -pix_fmt yuv420p \
  -c:a aac -shortest "$out/ep-b.mp4"

# 单集 HLS 源（App 回退路径的一种上游形态）
ffmpeg -y -hide_banner -loglevel error \
  -f lavfi -i "testsrc2=size=320x240:rate=30:duration=5" \
  -f lavfi -i "sine=frequency=220:duration=5" \
  -c:v libx264 -pix_fmt yuv420p -c:a aac \
  -hls_time 3 -hls_list_size 0 -hls_playlist_type vod \
  "$out/hls/index.m3u8"

printf 'fixtures written to %s\n' "$out"
