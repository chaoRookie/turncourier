#!/usr/bin/env bash
# 扫描全部 Git 历史、暂存区与即将公开的工作树快照，不复制 Git 忽略的本地运行数据。
set -euo pipefail

# 外部注入的配置会优先于内置默认规则，清除后 gitleaks 只能使用默认规则。
unset GITLEAKS_CONFIG GITLEAKS_CONFIG_TOML

scanner="${GITLEAKS_BIN:-gitleaks}"
repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

# gitleaks 会自动读取仓库根目录的 .gitleaks.toml 与 .gitleaksignore。前者未声明 [extend] useDefault = true 时
# 会静默替换全部默认规则，后者会按指纹放行发现，扫描仍报告通过，因此只要受跟踪或存在于工作树就拒绝继续。
for custom_file in .gitleaks.toml .gitleaksignore; do
  if [ -e "$custom_file" ] || [ -L "$custom_file" ] || [ -n "$(git ls-files --cached -- "$custom_file")" ]; then
    printf '拒绝扫描：仓库根目录存在 %s。自定义规则或忽略清单可能静默替换 gitleaks 默认规则或放行真实密钥，请移除后再扫描。\n' "$custom_file" >&2
    exit 1
  fi
done

"$scanner" git --redact --no-banner --log-opts='--all' .

# 暂存区即下一次提交的内容，可能与工作树不同，需单独扫描。
"$scanner" git --staged --redact --no-banner .

scan_dir="$(mktemp -d "${TMPDIR:-/tmp}/turncourier-secret-scan.XXXXXX")"

# cleanup 仅移除本脚本创建的专用快照目录，不接触仓库或用户目录。
cleanup() {
  case "$scan_dir" in
    */turncourier-secret-scan.*) rm -rf -- "$scan_dir" ;;
    *) printf '拒绝清理不匹配的快照路径\n' >&2 ;;
  esac
}
trap cleanup EXIT

# Git 的空字符分隔避免空格或中文文件名被拆分，归档只包含受跟踪及未忽略文件。
# 已删除但未暂存或 sparse-checkout 未检出的路径在工作树中不存在，打包前跳过，否则 tar 会失败；
# 符号链接即使指向不存在的目标也保留。被跳过路径的暂存内容和已提交内容由上面的暂存区扫描与历史扫描覆盖。
git ls-files -z --cached --others --exclude-standard |
  while IFS= read -r -d '' path; do
    if [ -e "$path" ] || [ -L "$path" ]; then
      printf '%s\0' "$path"
    fi
  done |
  tar --null -T - -cf - | tar -xf - -C "$scan_dir"
"$scanner" dir --redact --no-banner "$scan_dir"
