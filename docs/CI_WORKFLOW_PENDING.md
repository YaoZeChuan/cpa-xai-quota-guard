# CI workflow 待推送说明

## 当前状态

- 本地 `.github/workflows/build.yml` 已改为 **CPA 商店兼容布局**（与 `docs/ci-build.yml.store-compatible` 内容一致）
- 已发布的 **v0.3.10** Release 资产为正确布局：
  - `cpa-xai-quota-guard_0.3.10_{goos}_{goarch}.zip`（库文件在 zip **根目录**）
  - `checksums.txt`
- 推送 workflow 文件需要 GitHub OAuth/PAT 具备 **`workflow` scope**

## 为何重要

远端若仍是旧 CI，下一次 `v*` tag 可能重新打出**嵌套路径 zip**，导致 CPA 商店 **HTTP 502 / plugin_install_failed**。

## 推送步骤

```bash
# 1) 提权（交互）
gh auth refresh -s repo,workflow

# 2) 提交并推送（仅 workflow + 文档，不要改 v0.3.10 已有正确资产）
git add .github/workflows/build.yml CHANGELOG.md docs/
git commit -m "ci: package store-compatible release assets"
git push origin main
```

PowerShell 临时 PAT（**禁止写入 remote URL / 禁止提交**）：

```powershell
$env:GH_TOKEN = "<PAT_WITH_repo_AND_workflow>"
$env:ALL_PROXY = "socks5h://Default:***@10.10.10.5:2260"  # 按需
git push origin main
Remove-Item Env:GH_TOKEN -ErrorAction SilentlyContinue
```

## 商店 502 防再发清单

发版前自检：

1. zip 名：`{id}_{version}_{goos}_{goarch}.zip`（version **无** `v`）
2. `unzip -l` 根目录只有 `cpa-xai-quota-guard.so|.dll|.dylib`，**无** `linux/amd64/` 嵌套
3. 同 Release 有 `checksums.txt`（`sha256sum` 双空格格式）
4. **不要**再上传无版本号的 `cpa-xai-quota-guard_linux_amd64.zip` 旧命名

## 在 workflow 未推送前

继续使用已发布的 v0.3.10 规范资产安装；不要用旧布局 zip 覆盖 Release。