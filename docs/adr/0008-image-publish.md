# ADR-0008：镜像构建与发布口径（ghcr / 双架构 / ci 全绿才发 / `latest` 跟 main）

状态：**已接受**（2026-10-31，MKACG-615）。口径由 lxz 2026-10-31 定稿，本 ADR 只记录
落地位置、被否决的写法，以及实测代价。

## 背景

2.0 之前镜像只是 `make image` 的本地产物：部署方要自己构建，不同人构建出的二进制里
`version`/`commit` 各不相同，排障时无法把「跑着的那个」对应回 commit。

发布所需的东西其实已经齐了：`Containerfile` 的 runtime 段是 `USER app` +
`VOLUME ["/data"]` + `EXPOSE 8080`，默认 env 指向容器内路径；`cmd/drama-hub` 支持
`-ldflags -X main.version / main.commit`。

一个实测坑：`env.lock` 里的 `GO_IMAGE` 原来固定到 `golang@sha256:bff16a94…`，那是
**单平台（amd64）manifest** digest。用 `--platform linux/arm64` 构建并不报错，BuildKit
只给一条 `InvalidBaseImagePlatform` 警告，然后**拿 amd64 工具链编出 amd64 二进制**塞进
arm64 镜像（实测镜像内 `/usr/local/bin/drama-hub` 是 `ELF 64-bit LSB executable, x86-64`）。
所以多架构发布的前置条件是：基础镜像 digest 换成多架构 **index** digest。

## 决定

1. **registry 固定** `ghcr.io/justforlxz/drama-hub`，认证只用 `GITHUB_TOKEN` +
    `packages: write`，不引入额外 secret。
2. **平台** `linux/amd64` + `linux/arm64`，同一个 manifest list。
3. **门禁**：发布在独立的 `.github/workflows/image.yml` 里，`image` job `needs: [lint, test]`
   ——任一步失败就是**没有任何 tag 被推**。这条不写成脚本里的 `if`，让「绿才发」成为结构
   约束。跨 workflow 没有 `needs`，所以 `image.yml` 里的 lint/test 只能自己定义；两者跑的
   是同一个入口 `scripts/ci.sh`，检查内容不会分叉（代价见「后果」）。
4. **tag 矩阵**（唯一真值 `scripts/image-tags.sh`，自带 `--self-test`，`ci.sh --lint` 会跑）：

    | 事件 | 推的 tag | `VERSION` build-arg |
    |---|---|---|
    | 分支 push | `sha-<7位>`；`main` 上另加 `latest` | 7 位 sha |
    | `v*` tag push | `vX.Y.Z` | tag 名 |

   `COMMIT` 一律是完整 sha。
5. **`latest` 只跟 `main`**：分支与 tag 构建都不动它。
6. **`sha-<7位>` 不可变，因此 tag push 不重出 sha tag**：tag 构建的 `VERSION` 是 tag 名，
   与分支构建的 `VERSION` 不同，重出会把同一个 sha tag 改写成另一份字节。
7. **缓存** `cache-from/to=type=gha`（`Containerfile` 已按 `go.mod` 分层）；`concurrency`
   按 ref 分组的 `group: image-<ref>` + `cancel-in-progress: true`（同一 ref 上新的 push
   取消旧的构建）。
8. **`testbin` 独立成段**：发布只依赖 `build` 段，另一个架构不必再为发布编一遍测试二进制。

## 被否决的方案

1. **把 `image` job 塞进现有的 `ci.yml`（触发面扩到任意分支 + `v*` tag）。**
   否决理由：那会把「PR 门禁」和「发布」压进同一个触发面——`ci.yml` 一旦扩到任意分支，
   每次分支 push 都多跑一遍 lint+test，且 `packages: write` 与 PR 检查同处一个文件更容易
   把权限写宽。文件分开后职责清楚：`ci.yml` 管 PR，`image.yml` 管发布。代价是 `main` push
   时 lint/test 各跑两遍（见「后果」）。
2. **tag 逻辑写在 workflow 的 shell 里。**
   否决理由：YAML 里的 shell 本地跑不了、也没法自测。抽到 `scripts/image-tags.sh` 后，
   口径可以在本机与 lint 容器里断言（`--self-test` 四个 fixture），YAML 只剩调用。
3. **用 `docker/metadata-action` 生成 tag。**
   否决理由：矩阵本身它也能表达，但口径会藏进第三方 action 的表达式里，本地无法验证，
   也没法在 ci 里回归；同样的矩阵 15 行 POSIX sh 就能自证。
4. **tag push 时把已有的 `sha-*` 清单重打成 `vX.Y.Z`（不重新构建）。**
   否决理由：省一次构建，但 `v0.1.0` 里的 `version` 会永远显示 sha，且要额外处理「tag
   指向的 commit 没有 sha 镜像」的兜底。发布构建本身已经只要几分钟。
5. **tag push 也推动 `latest`。**
   否决理由：与「`latest` 只跟 main」直接冲突（见「后果」）。

## 后果

- **每个分支的每次 push 都跑 lint+test+image**（`sha-<7>` 要对任意分支出）。本机实测两平台
  发布约 5–6 分钟（缓存命中），GH runner 上更慢；PR 事件不触发发布。
- **`main` push 时 lint/test 会跑两遍**（`ci.yml` 一遍、`image.yml` 的 `needs` 一遍）。这是
  「发布必须自带门禁、又不许跨 workflow 引用 job」的必然代价；两处跑的是同一个
  `scripts/ci.sh`，不会给出两个不同的结论，只是多花一次 runner 时间。
- arm64 走 QEMU，构建时长是本方案的主要成本。本机（8 核）冷构建实测：
  - 拆分前（发布也编测试二进制）：arm64 runtime 冷构建 **1045 s**；
  - 其中测试二进制那一段单独测：**824 s**；
  - 拆分后（`--no-cache`）：**418 s**。
  → 拆分是为了让「发布」不必付「为另一个架构编一遍 -race 测试二进制」的钱。e2e 镜像
  行为不变：测试二进制仍从 `testbin` 段拷进 e2e。
- `vX.Y.Z` 与 `latest` 是两次独立构建（`VERSION` 不同 → 二进制不同 → digest 不同）。
  **tag push 不动 `latest` 是口径本身，不是缺陷**（PL 2026-10-31 裁决）：`latest` 跟 main，
  `v*` 只是给某个 commit 一个语义化名字，同一个 commit 的两次构建 digest 不同是预期。
  若将来要「发版时 `latest` 也指向 `vX.Y.Z`」，那是一次口径变更（`image.yml` 给 tag 事件
  加一步），不属于修 bug。
- ghcr 包默认私有：镜像在，但外部拉取需要带 `read:packages` 的 PAT。
- 版本 tag 不再是「构建产物不可变」的额外保险：`v0.1.0` 是可以被重推的 tag（`sha-*` 才
  不可变）。发布改版请以 `sha-*` 为准。

## 需要同步修改的文档

| 文件 | 改了什么 |
|---|---|
| `README.md` | 新增「容器镜像」一节：tag 矩阵、门禁、`--userns=keep-id` 的坑、拉取认证 |
| `docs/adr/0008-image-publish.md` | 本文件 |
| `Containerfile` / `env.lock` | `GOIMG`/`GO_IMAGE` 换成多架构 index digest；测试二进制拆到 `testbin` 段 |
| `scripts/image-tags.sh` | tag 口径的唯一真值 + `--self-test`（`scripts/ci-lint.sh` 调用） |
| `.github/workflows/image.yml` | 新增：`push`（任意分支 + `v*` tag）触发，lint/test 门禁 + 双架构发布 |
