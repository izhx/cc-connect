# Linux 单文件程序：自动构建与下载

`main` 分支每次更新并通过 `CI` 工作流后，`Linux binaries` 工作流会自动构建并发布：

| 运行环境中的 `uname -m` | 架构 | 下载文件 |
| --- | --- | --- |
| `x86_64` | Linux x86_64 / amd64（64 位 Intel、AMD） | `cc-connect-linux-amd64` |
| `aarch64`、`arm64` | Linux ARM64 | `cc-connect-linux-arm64` |

这两个文件都是静态链接的可执行程序，内嵌 Web 管理界面。运行 cc-connect 本体无需
安装 Go、Node.js 或 pnpm。Claude Code、Codex 等 Agent 功能仍需要对应 CLI 及其运行环境。
32 位 x86 和 ARMv7 不在此工作流的构建范围内。

## 自动发布与手动构建

- 合并 PR 到 `main` 会触发已有的 `CI`。只有该次 `main` 的 push CI 成功，且其 commit
  仍是 `main` 最新提交，才自动构建发布；已被更新提交取代的 CI 和 PR 的 CI 不会触发发布。
- 在仓库 **Actions → Linux binaries → Run workflow** 中选择 `main`，也可以手动构建发布。
  手动构建是独立入口，不等待另一次 CI。
- 每个 commit 对应一个 `main-<12位commit>` 预发布版本，附带两个程序、`SHA256SUMS`
  和 `build-info.txt`。手动重跑同一 commit 会更新该版本的附件。
- 构建附件也会在该次 Actions 运行中保留 14 天；Releases 中的附件不受此保留期影响。
- 仓库需要启用 GitHub Actions，并允许工作流通过 `GITHUB_TOKEN` 写入 Releases。

## 在服务器上下载

打开运行此工作流的仓库的 **Releases** 页面（本仓库为
[izhx/cc-connect/releases](https://github.com/izhx/cc-connect/releases)），选择需要的
`main-...` 版本，复制版本名称。预发布版本不会通过 GitHub 的 `releases/latest` 路径提供。

在实际运行 cc-connect 的服务器或容器里执行，替换下方版本号：

```bash
set -euo pipefail

repo=izhx/cc-connect
version=main-替换为实际的12位commit
case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo '此下载示例仅支持 Linux x86_64 和 ARM64' >&2; exit 1 ;;
esac

asset="cc-connect-linux-$arch"
base="https://github.com/$repo/releases/download/$version"
curl -fL --retry 3 "$base/$asset" -o "$asset"
curl -fL --retry 3 "$base/SHA256SUMS" -o SHA256SUMS
awk -v asset="$asset" '$2 == asset { print }' SHA256SUMS | sha256sum -c -
chmod +x "$asset"
"./$asset" --version
```

校验成功后，先停止原 cc-connect 进程，再替换程序并使用原配置启动：

```bash
./cc-connect-linux-amd64 --config /path/to/config.toml
# ARM64 使用 ./cc-connect-linux-arm64
```

更新时保留原 `config.toml`、`data_dir` 和 Claude CLI 的 Session / Transcript 目录。
此发布流程不改变这些文件，也不要求 systemd。

## 本地复现构建

需要仓库要求的 Go 版本、Node.js 20+、pnpm 10，以及 `git`、`file` 和 `sha256sum`：

```bash
cd web
pnpm install --frozen-lockfile
pnpm build
cd ..
go mod download
go mod verify
bash .github/build-linux.sh
```

产物位于 `dist/linux/`，版本号取自 commit，构建时间字段使用提交时间以便复现。
脚本通过 `CGO_ENABLED=0` 和仓库已有的 `goolm` 构建标记，
为两种架构编译完整程序，并检查 ELF 架构、静态链接和 SHA-256 校验值；在构建机
架构匹配时还会执行 `--version`。交叉编译的 ARM64 程序仍需在 ARM64 服务器上验证运行。
