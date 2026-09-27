# 多节点 spec.addresses 支持 — 设计大纲（下一轮）

状态：大纲，待 Oracle 评审后实现。前置：v0.1.1（default server 移除 + 证书路径修复）落地。

## 1. 目标

多节点 DaemonSet 部署下支持 Gateway `spec.addresses`：地址即节点归属，
每个节点的控制器只编程本机持有的地址，天然实现按地址分片的多节点数据面。

## 2. 核心机制

- **节点地址指纹**：控制器启动 + 周期探测（60s）本机全部 IP（各网卡 v4/v6 + 127/8 环回池），
  缓存为节点地址集合；变化时触发全量 reconcile。
- **地址过滤**：listener 绑定地址（spec.addresses 优先，listen-addresses 注解次之）与节点集合取交集：
  - 交集非空 → 只渲染交集部分 listen；
  - 通配（无地址）→ 本节点绑定通配（所有节点都服务）；
  - 交集为空且非通配 → 本节点完全跳过该 listener（不渲染、不写状态、不报错——归属别的节点）。
- **spec.addresses 语义**：绑定意图（映射为 listen 指令），不再是 provision 请求；
  listen-addresses 注解降级为遗留兼容或移除。

## 3. 状态写回归属

- Gateway 的 `status.addresses`：由持有该 Gateway listener 的各节点按分片贡献（合并去重）。
- conditions：仅由"持有该 Gateway 至少一个 listener"的节点写；跨节点值必须收敛一致
  （Accepted 取 min，Programmed 各节点报告自身，需设计合并规则——候选：加 per-node
  Programmed 子状态或约定全节点一致才为 True）。
- 完全不属于本节点的 Gateway：不写任何状态（避免跨节点状态污染）。
- 单节点场景退化为现有行为（回归保障）。

## 4. 冲突判定调整（§3.2）

- distinctness 元组从"单机 socket"扩展为"地址分片后的有效 socket"：
  跨节点同端口同 hostname + 不同 addresses = 可区分（合法共存）。
- 同节点内判定不变。
- spec.addresses 与另一 Gateway 的注解地址混用时按解析后绑定集合比较。

## 5. 同轮捆绑项（协议正确性 + 注解框架）

1. **WebSocket 默认修复**：所有 proxy location 无条件注入
   `proxy_http_version 1.1` + `Upgrade`/`Connection` 头（$connection_upgrade map）。
2. **白名单注解框架**（延后）：`gateway.host-nginx/<name>` 命名空间，逐个实现映射到模板字段，
   拒绝任意配置片段注入（防 configuration-snippet 类 CVE）。首批候选：
   proxy 超时、proxy body size、buffering、backend protocol (HTTP/HTTPS/gRPC)。
   注解作用域：Gateway / HTTPRoute / 规则层级，优先级 route > gateway。

## 5a. 逃生舱：危险开关（先行实现，白名单之前的过渡方案）

环境假设：小规模、可信部署。启动开关默认全关，开启后解锁原始配置注解
（`hng.victri.dev/` 命名空间）；安全网 = 既有 `nginx -t` 验证 + 失败回滚。

**设计原则**：不提供 http-conf 级注入（`load_module`/`lua_shared_dict` 等）——
多个 Gateway 的 http 级指令互相冲突，属反模式；此类需求应由宿主管理员写进主配置。
仅提供 server / location 两级 snippet + 附加文件机制。

| 开关 | 注解（`hng.victri.dev/`） | 注入位置 | 用例 |
|---|---|---|---|
| `--dangerously-allow-nginx-snippets` | `server-snippet`（Gateway）、`location-snippet`（HTTPRoute 规则级） | server / location block 内 | per-route 调优、lua 内容指令 |
| `--dangerously-allow-extra-files` | `extra-files`（Gateway，`configmap:ns/name` / `secret:ns/name` 逗号列表） | 物化到 `<conf-dir>/files/<ns>_<name>/<key>`（原子写+孤儿清理，同 certs） | lua 脚本、证书链等附加文件 |

**文件引用机制**：snippet 中以 `@<key>@` 占位符引用 extra-files 的条目
（如 `content_by_lua_file @script.lua@;`），渲染时替换为物化后的绝对路径
`<conf-dir>/files/<ns>_<name>/<key>`；未匹配的占位符 → 渲染错误（fail-fast，不静默）。

- 开关关闭时注解被忽略并记 warning 日志（不进 status 条件）。
- 替换与注入均逐字进行，不做清洗；`nginx -t` 失败 → 既有回滚 + stderr 进 status。
- IR 承载：`Server.RawServerSnippet`、`Location.RawSnippet`、`Configuration.ExtraFiles`；
  多路由片段按 ns/name 确定序合并。
- 威胁模型声明：注解写入者 = 集群管理员级信任；文档明示。
- 白名单框架落地后逐步用受控字段替代常见 snippet 用法。
- **注解命名空间迁移**：全部自有注解统一为 `hng.victri.dev/<name>`。
- **注解退场计划（与 spec.addresses 同轮实施）**：`listen-addresses`（被 spec.addresses
  绑定意图取代）与 `publish-addresses`（status.addresses 改为从实际绑定 listen 集合
  自动推导）在此轮一并移除；conformance harness 的 loopback bind shim 同步迁移到
  spec.addresses。迁移版本内旧注解保留 deprecation 日志读取或直接切断（实现时定）。

## 6. 测试

- 单测：地址交集矩阵（属于/不属于/通配/部分交集）、状态归属、收敛合并。
- 多节点 E2E：k3s 双节点（虚机或 container 节点），Gateway 地址指向节点 A →
  断言仅 A 的 nginx 出现 server block，B 跳过且状态干净。
- conformance 37/37 不回归（单节点 + 通配路径不受影响）。

## 7. 风险

- 多节点状态竞争写：conditions 合并规则复杂度；MVP 可先限制"Gateway 只能归属一个节点"（addresses 全部落在一个节点），多节点归属延后。
- 节点 IP 抖动导致配置震荡：防抖（连续 N 次探测一致才更新指纹）。
- argocd 等真实应用依赖 helm 生成 Gateway 的场景需文档说明 addresses 用法。
