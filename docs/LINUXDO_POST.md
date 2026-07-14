# [开源] cpa-xai-quota-guard：CPA 原生插件，专治 xAI 额度冷却 / 死号清理

**仓库**：https://github.com/Mortal520/cpa-xai-quota-guard  
**协议**：MIT · **版本**：0.3.11 · **形态**：CLIProxyAPI **c-shared 原生插件**

---

## 一句话

CPA 多号跑 Grok 时，429 free 额度拖垮整池、402/401/403 混着误删——这个插件按 **xAI 真实错误码** 做：额度先冷却、死号才删、手动停用永不误开。

---

## 做什么

| 上游信号 | 动作 |
|----------|------|
| **429** free-usage（rolling 24h） | 自动禁用 → 到期恢复 |
| **402** spending-limit | 冷却不删，巡查可再测 |
| **401** / 真 **403** 权限 | 删除凭证 |
| 区域不可用、426、网络/5xx | **不删** |
| 用户手动停用 | **永不自动启用** |

另外：主动/定时巡查（只扫启用号；冷却号可单独复核）、弹性并发、CPA 内嵌管理页（额度/巡查/账号）。

**只认 xAI**，不照搬 Codex 冷却逻辑。

---

## 截图

![状态栏](https://raw.githubusercontent.com/Mortal520/cpa-xai-quota-guard/main/docs/screenshots/dashboard.png)

![巡查](https://raw.githubusercontent.com/Mortal520/cpa-xai-quota-guard/main/docs/screenshots/patrol.png)

![账号](https://raw.githubusercontent.com/Mortal520/cpa-xai-quota-guard/main/docs/screenshots/accounts.png)

（脱敏示意；裂图可看仓库 `docs/screenshots`）

---

## 安装（最短）

### 方式 A · 插件商店（推荐）

`config.yaml` 加商店源后重启，在管理中心 → 插件商店安装：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  store-sources:
    - "https://raw.githubusercontent.com/Mortal520/cpa-xai-quota-guard/main/registry.json"
  configs:
    cpa-xai-quota-guard:
      enabled: true
      quota_guard_enabled: true
      management_url: "http://127.0.0.1:8317"
      management_key: "<CPA_MANAGEMENT_KEY>"
      state_path: "data/cpa-xai-quota-guard-state.json"
      patrol_auth_dir: "/root/.cli-proxy-api"
```

### 方式 B · Release 手动

1. 下载 [Releases](https://github.com/Mortal520/cpa-xai-quota-guard/releases) 最新对应架构 zip  
2. 解压后把库文件放到 `plugins/<goos>/<goarch>/`  
3. 合并上方 `plugins.configs` 配置  
4. 重启 CPA  

细文档：[INSTALL](https://github.com/Mortal520/cpa-xai-quota-guard/blob/main/docs/INSTALL.md)

---

## 边界（诚实）

- 只服务 **xAI** 号池  
- 日额度池是启用数估算，不是官方账单 API  
- 巡查质量依赖探测模型 + 出口区域  
- 需要已有 CPA + management key  

---

## 链接

- GitHub：https://github.com/Mortal520/cpa-xai-quota-guard  
- Release：https://github.com/Mortal520/cpa-xai-quota-guard/releases  
- Issue 请贴脱敏后的 `HTTP 状态 + body.code + 期望动作`

Star / Issue / PR 都欢迎。有用转给同在 CPA 扫 Grok 的朋友。

*基于仓库 0.3.11；以 README / CHANGELOG 为准。*