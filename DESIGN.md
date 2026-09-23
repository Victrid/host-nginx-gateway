# HostNginxGateway 设计文档

一个运行在主机侧的 Kubernetes Gateway API 控制器，配置主机上的 nginx。
目标场景：k3s 等小型部署，与主机既有 nginx 配置和其它服务**共存**，不独占 80/443，
免除通过 nodePort 手动管理主机侧 nginx 的负担。

参考实现：ingress-nginx（模板与 reload 机制）、nginx-gateway-fabric（IR 管道与状态翻译）、Envoy Gateway（full-reconcile 模式）。
（v2：并入 Oracle 审核修订——B1~B7、S1~S8、N4~N6。）

## 1. 总体架构

单 Go 二进制，以 DaemonSet Pod 运行在节点上（in-cluster），通过 ServiceAccount 访问 k3s API。

```
k3s API ──watch──► [ Informers / controller-runtime cache ]
                          │
                          ▼
                 [ Full Reconciler ]        ── 状态写回 (Accepted / Programmed / ResolvedRefs / Conflicted)
                          │
                          ▼
                 [ graph (内部 IR) ]         ── 解析 parentRefs / EndpointSlices / Secrets
                          │
                          ▼
              [ dataplane.Configuration ]    ◄── 锁定的 IR 契约（见 §5）
                          │
                          ▼
           [ text/template 渲染 nginx 配置 ]
                          │
     临时文件 + 临时主配置 → nginx -t → rename(.prev 备份) → nginx -s reload → 回滚状态机
```

- **Full reconcile**（Envoy Gateway 风格）：watch 全部相关类型，任一事件入队一次全量 sync，从本地 cache 全量重建期望配置；MVP **不做** per-object predicate/index/MapFunc 过滤（S7），性能不足时再加。
- **No-op 判定**（S6）：对*实际渲染字节*维护 applied hash，与期望渲染比较；不一致即写文件。控制器**启动时无条件重建全部 owned 文件**，保证重启恢复所有权。用户手工篡改会被下次 sync 覆盖（记录事件日志）。
- `/healthz` + `/metrics` 端点（reload 延迟/失败计数）。

## 2. 共存模型（核心约束）

- 控制器**只拥有** `/etc/nginx/conf.d/k8s-gw/` 目录，永不修改用户 `/etc/nginx/nginx.conf`。
- 每个 Gateway 生成一个 `<namespace>-<name>.conf`；主配置需包含：
  `include /etc/nginx/conf.d/k8s-gw/*.conf;`
- **include 检测策略：仅检测不修改。** 通过 §5 的**临时主配置验证法**判定 include 是否生效
  （能覆盖通配 include `conf.d/*.conf` 等情形，S2）；不生效时 Gateway `Accepted=False (Invalid)`，
  message 说明需添加的 include 行。绝不自动注入。
- 生成文件头部含 ownership 注释；目录内非 owned 文件仅记录日志，不做哈希防篡改机制（N1，YAGNI）。
- 证书 Secret 写入 `k8s-gw/certs/`，见 §5 证书生命周期（B6）。

## 3. Gateway API 映射

### 3.1 端口与 listen（权威：spec.port）

- `spec.listeners[].port` 在 Gateway API **v1 中为 required**（S1）。省略即拒绝：Listener `Accepted=False (Accepted, 原因 UnsupportedProtocol/Invalid)`。不实现推断回退。
- `protocol` → ssl/http2 语义：HTTP → `listen <port>`；HTTPS → `listen <port> ssl http2`；TLS → `listen <port> ssl`，路由以 SNI 区分。
- 注解 `gateway.host-nginx/listen-addresses`（Gateway 级，逗号分隔）→ 每端口额外 listen 行：
  ```nginx
  listen 8080;
  listen 192.168.1.10:8080;   # 来自注解
  listen [::]:8080;           # 来自注解
  ```
- **动态增删**：reconcile diff listeners 集合，新增端口进模板，删除端口随文件删除；reload 生效。
- 权限：控制器不 bind 端口（nginx master 负责），**controller Pod 不需要 CAP_NET_BIND_SERVICE**（B5）；
  nginx master 需相应能力或 root。

### 3.2 冲突检测（B2 修订 + v5 跨 Gateway 地址分配）

- 同 Gateway 内 Listener `port + hostname` **等价**（hostname 相同、或均为空——两种都无法按请求区分）→ 双方 `Conflicted=True (HostnameConflict)`，不生成对应 server block（spec MUST）。精确与覆盖它的通配符**可区分**（GEP-722：请求按最具体 server_name 匹配），不冲突，nginx 原生支持。
- **跨 Gateway**：控制器把所有 Gateway 合并进同一个 nginx 数据面，Gateway API v1（GatewaySpec "Distinct Listeners"）明确该合并形态同样适用 Listener 区分度规则——区分度元组含**地址**分量。控制器作为地址供给方：跨 Gateway 的不可区分 Listener 组（同 port、hostname 等价、bind 集重叠）中的每个 Gateway 获得一个**确定性分配的 loopback 绑定地址**（`127.0.0.(8 + hash(ns/name) mod 232)`，冲突线性探测；hash 保证无关 Gateway 增删不改变既有分配——避免运行中 bind 迁移），并写入 status.addresses。两边**都完整编程**，无胜者裁决（B2 精神保留：不靠名字决胜，而靠地址分离）。
- 显式 `listen-addresses` 注解绑定的 Gateway 不参与自动分配；显式绑定仍相互重叠时回落到原 §3.2 行为：双方 `Conflicted=True`（双方，无胜者）。
- wildcard（无 hostname）listener 与同端口的具名 listener 不构成冲突：nginx 按 server_name 分发，具名优先；两个 wildcard（或相同/前缀重叠 hostname）在同一 Gateway 内仍冲突。
- 全无冲突时正常生成。

### 3.3 MVP 资源范围

| 资源 | 处理 |
|---|---|
| GatewayClass | 校验 controllerName；status Accepted。parametersRef 一律拒绝（不支持任何参数类型）：`Accepted=False (InvalidParameters)`，其下 Gateway 同拒。finalizer 不实现（N2，YAGNI） |
| Gateway | 生成 .conf；status: Accepted / Programmed + Listener 级条件（含 Conflicted）+ **status.addresses**（§3.4）。部分 listener 无效时 Gateway `Accepted=True (ListenersNotValid)`，全部无效才 `False` |
| HTTPRoute | hostname 与 listener 取交集才挂载（S4）；无交集 → `Accepted=False (NoMatchingListenerHostname 或 NotAllowedByListeners)`。**按路由 hostname 分发**（v3）：每个有效 hostname 组生成独立 nginx server block（`server_name` = 生效 hostname），请求按 Host 精确分发到对应路由的 location；无 hostnames 的路由继承 listener hostname；同路径去重仅在**同一 hostname 组内**生效。**Hostname 语义（v3.1 修订）**：通配符为**后缀匹配**——`*.example.com` 匹配 `test.example.com` 与 `foo.test.example.com`（任意层），不匹配裸域；精确 listener `test.example.com` 亦与路由 `*.example.com` 相交（以路由通配符为生效 hostname）；转发**保留 Host 头**（`proxy_set_header Host $http_host`，spec: "MUST forward this header unmodified"） |
| **GEP-722 优先级贯穿（v5）** | 位置（location）计算以 **socket** 为单位（同一 listen 集合的所有 listener 贡献到同一 socket 的 server block 集合）：精确 hostname 块吸收**覆盖它的**更宽通配块与 catch-all 块的 location 条目（先自身、更具体通配次之、更宽通配再次、catch-all 兜底；同路径更具体 hostname 方胜出）。请求落在更具体块中而规则不匹配（路径/方法等）时**穿透**到更宽路由。catch-all 组存在时它作为 socket 默认 server（spec：匹配一切 host）；否则生成**合成的空默认块**——未知 host 一律 404，**绝不泄漏进任何路由**。location 内部：可适配（路径 spec 覆盖整个 location）的规则按优先级顺序构成 map 分发树（方法/header/query，见 §3.3 过滤器映射表）；首个"无约束且 spec 等于 location 自身"的规则遮蔽其后所有规则（first-match-wins）。 |
| Secret (TLS) | 拉取证书到 certs/（生命周期见 §5.3）；跨 namespace 证书需目标 namespace 的 ReferenceGrant（§3.5） |
| Service | （v2 conformance 新增）backendRef 端口映射：service port → port name → EndpointSlice 端口；命名/多端口 Service 因此可解析 |
| Namespace | （v2 conformance 新增）allowedRoutes `from=Selector` 的标签评估 |
| ReferenceGrant | （v3 新增）见 §3.5：跨 namespace backendRef / certificateRefs 的准入判定 |
| EndpointSlices | **仅 EndpointSlice**（S3，k3s 默认启用），label selector `kubernetes.io/service-name` 过滤；解析 `Conditions.Ready` + IPv4/IPv6 |

不支持（后续版本）：GRPCRoute、TLSRoute/TCPRoute/UDPRoute、URLRewrite/RequestRedirect 的 ExtensionRef、CORS/ExternalAuth（experimental）、sessionPersistence。（RequestMirror 系过滤器与 backendRef 级 RequestHeaderModifier 自 v7 起支持，§5.1.1；跨 namespace 路由挂载自 v6 起支持，§3.5。）
### 3.4 状态条件（B3 修订：全部使用标准 reason；v5 增 status.addresses）

| 情形 | 条件 | Status / Reason |
|---|---|---|
| include 指令缺失/未生效 | Gateway Accepted | False / `Invalid` |
| nginx 未运行 | Gateway Programmed | False / `Pending` |
| Listener 端口或主机名冲突 | Listener Conflicted | True / `HostnameConflict` 等 |
| 配置生成且 reload 成功（含生效验证，§5.2） | Gateway Programmed | True / `Programmed` |
| nginx -t 或 reload 失败 | Gateway Programmed | False / `Invalid`（message 含截断的 stderr，N6） |
| reload 后新增 bind 失败（生效验证检出） | Gateway Programmed | False / `Invalid`（message 含 error log 的 bind 失败行） |
| 后端缺失（GEP-1364） | HTTPRoute Accepted | True / `Accepted` |
| 后端缺失（GEP-1364） | HTTPRoute ResolvedRefs | False / `BackendNotFound`；数据面该 rule 返回 500 |

**status.addresses（v5）**：控制器对其编程的每个 Gateway 写入 `status.addresses`
（Type=IPAddress）。来源优先级：

1. Gateway 注解 `gateway.host-nginx/publish-addresses`（逗号分隔 IP）；
2. 启动旗标 `--publish-addresses`（逗号分隔 IP；显式运营者声明）;
3. 自动分配的 loopback 绑定（§3.2 跨 Gateway 地址分离——报告的是真实绑定地址）;
4. 后备值：节点主 IP（`HNG_NODE_IP` downward-API env，DaemonSet 形态；
   非 Pod 环境退化为接口路由探测默认路由源地址）。

全空 → 不触碰已有 status.addresses（无可信来源时绝不写假地址）。地址列表经
read-modify-write 写入 status 子资源，与条件合并共用一次提交。

所有条件：read-modify-write，`observedGeneration = metadata.generation`，status 子资源提交。
### 3.5 跨 namespace 引用（v3：ReferenceGrant 已实现；v6：跨 ns 路由挂载按 allowedRoutes 放行）

跨 namespace 的 backendRef（HTTPRoute → Service）与 certificateRefs
（Gateway listener → Secret）**仅在目标 namespace 存在匹配的 ReferenceGrant
时放行**（ReferenceGrant spec 语义）：

- grant 必须位于**被引用对象的 namespace**；
- 同一 grant 内需同时存在匹配的 `spec.from` 条目（group/kind/namespace 匹配
  引用方：HTTPRoute 或 Gateway）**和**匹配的 `spec.to` 条目
  （group/kind 匹配目标，`to.name` 设置时限定精确名称）；from 条目之间、
  to 条目之间为 OR 关系，但 from 与 to 必须出自**同一个 grant**；
- 放行 → 正常解析（backend upstream / 证书材料化，证书文件名按 Secret
  实际 namespace：`<ns>_<name>.pem`）；
- 不放行 → 维持既有行为：backend `Accepted=True + ResolvedRefs=False
  (RefNotPermitted)` + 数据面静态 500（GEP-1364）；listener 证书
  `ResolvedRefs=False (RefNotPermitted)` 且不生成 server block；
- ReferenceGrant 被 watch（§7），增/删/改均触发全量 reconcile——删除即回收。

未变：ReferenceGrant **不适用于路由挂载**（spec 明确排除 Gateway-route
attachment——allowedRoutes 即授权，挂载从不要求也从不查询 grant）。

跨 namespace **路由挂载**（HTTPRoute parentRef 指向其它 namespace 的
Gateway）自 v6 起按 Gateway API v1 语义放行，由**目标 Gateway 的
listener 级 allowedRoutes.namespaces**（per-listener 策略，逐个命中
listener 评估）决定：
- `from=Same`（未设置时同此——CRD 默认 `{namespaces:{from:Same}}`）→
  仅同 ns 路由；跨 ns 路由 `Accepted=False (NotAllowedByListeners)`；
- `from=All` → 任意 namespace 放行；
- `from=Selector` → 路由所在 namespace 的标签须匹配 selector；
- `from=None` → 一律拒绝。
`sectionName`/`port` 匹配与 §3.3 hostname 交集规则与 namespace 无关，
照常适用。挂载后的 backendRef 仍以**路由自身 namespace** 为默认（spec），
与 Gateway 所在 namespace 无关；显式跨 ns backendRef 仍按本节 grant
规则判定。

## 4. 后端解析

- 仅 EndpointSlice：ready endpoints（`Conditions.Ready=true`）入 upstream；notReady 标记 down 保留感知。
- upstream `zone` + 共享内存；确定性命名 `ns_service_port`。
- MVP 端点变化走全量 reload；动态 upstream API 后续版本再评估。

## 5. dataplane（B1/B6/B7 修订）

### 5.1 验证策略（nginx -t 正确用法）

`nginx -t -c` 接受单文件而非目录，直接测试生成的 .conf 缺少 http/events 上下文不可靠。采用：

1. 复制用户 `/etc/nginx/nginx.conf` 到 `/tmp/hng-nginx-test.conf`；
2. 在 http block 中注入 `include /etc/nginx/conf.d/k8s-gw/*.conf;`（**仅临时副本**，原文件不动）；
3. `nginx -t -c /tmp/hng-nginx-test.conf -p /etc/nginx` 验证；
4. 通过后原子写入正式文件，再 `nginx -s reload`。

此法同时解决 include 生效检测（§2）：若用户主配置本身已 include 本目录，临时副本会双重 include——
检测逻辑在注入前先判断副本中是否已有覆盖本目录的 include，已有则不注入。

### 5.1.1 HTTPRoute 过滤器与匹配 → nginx 映射表（v5；v7 增镜像与 backendRef 级头修改）

| Gateway API 特性 | nginx 映射 | 备注 |
|---|---|---|
| `backendRef.weight`（单后端） | 服务级 upstream（`ns_svc_port`），weight=1 不渲染 | |
| `backendRef.weight`（多后端>1） | 规则私有组合 upstream `hng_wr_<hash>`，server 行携带 `weight=N`；任一 backendRef 解析失败 → 整条 rule 静态 500（GEP-1364 一致化） | weight=0 的 backendRef 不产生 server 行 |
| RequestRedirect `scheme/hostname/port/statusCode` | `return <code> <scheme>://<host>[:port]$request_uri;` | scheme/host 未设 → `$scheme`/`$host`（保留）；port 为 scheme 默认值时省略；statusCode 缺省 301 |
| RequestRedirect `replaceFullPath` | `return <code> <target><新路径>$is_args$args;` | 查询串保留（`$is_args$args`） |
| RequestRedirect `replacePrefixMatch` | 精确孪生 location：同上（目标=新前缀）；前缀 location：`if ($uri ~ "^<前缀>/(?<hng_r>.*)$") { return <code> <目标>/$hng_r$is_args$args; }` | 303/307/308 无法用 rewrite 标志，统一用守卫式 return |
| URLRewrite `hostname` | `proxy_set_header Host "<hostname>";` | 仅改发往后端的 Host，不是重定向 |
| URLRewrite `replaceFullPath` | `rewrite ^ <新路径> break;` | nginx 自动保留查询串 |
| URLRewrite `replacePrefixMatch` | 精确孪生：`rewrite ^ <新前缀> break;`；前缀 location：`rewrite ^<前缀>/(?<hng_r>.*)$ <新前缀>/$hng_r break;` | |
| RequestHeaderModifier `set/add` | `proxy_set_header <N> <V>;` | nginx 语义：替换入站头（add 与 set 在 nginx 的表达等价；入站原值不保留——已记录偏差） |
| RequestHeaderModifier `remove` | `proxy_set_header <N> "";` | nginx：空值即不向下游转发该头 |
| backendRef 级 RequestHeaderModifier（BackendRequestHeaderModification，v7） | 同上，但渲染在**该 backendRef 的代理位置**上（仅单后端规则支持）；规则级先应用、backendRef 级后应用（同名头后者胜出，GEP-1310） | 多后端规则带 backendRef 级过滤器 → 整条 rule 丢弃（PartiallyInvalid；加权组合 upstream 无法按 backend 区分头——已记录偏差） |
| RequestMirror（percent/fraction 未设或 100，v7） | 业务 location 内 `mirror /hng_mirror_<hash>;` + 同 server 内内部镜像 location：`location = /hng_mirror_<hash> { internal; proxy_pass http://<mirror_upstream>$request_uri; }` | 无 split_clients、无门控；`$request_uri` 恢复客户端原始 path+query（子请求 URI 是镜像路径）。规则级 RequestHeaderModifier 随镜像副本生效（NGF 行为） |
| RequestMirror 0 < percent/fraction < 100（v7） | http 级 `split_clients $request_id $hng_sc_<hash> { <pct>% "/hng_mirror_<hash>"; * ""; }` + 内部镜像 location 门控 `if ($hng_sc_<hash> = "") { return 204; }`（key=$request_id，NGF 同机制；百分比两位小数） | percent=0 → 直接丢弃镜像（行为等价、免死配置）；同一 location 内同一目标取**最大百分比**（NGF 行为）；多目标 = 多条 `mirror` 指令 |
| RequestMirror 校验 | percent 与 fraction 互斥；0≤percent≤100；denominator>0（缺省 100）、0≤numerator≤denominator；违规 → 整条 rule 丢弃（PartiallyInvalid） | fraction 换算 `num*100/den`（1/3 → 33.33） |
| RequestMirror backendRef 不可解析 | **丢弃镜像、路由照常服务**（不静态 500）；`ResolvedRefs=False (BackendNotFound/RefNotPermitted)` 按 spec 上报 | spec："dropped from the Gateway ... not configure this backend" |
| ResponseHeaderModifier `add` | `add_header <N> <V> always;` | |
| ResponseHeaderModifier `set` | `proxy_hide_header <N>;` + `add_header <N> <V> always;` | 先压后端原值再加 |
| ResponseHeaderModifier `remove` | `proxy_hide_header <N>;` | |
| 匹配 `method` | map `$request_method` | 精确匹配（大写枚举） |
| 匹配 `headers[].Exact` | map `$http_<name>` 精确键 | nginx map 字符串匹配大小写不敏感 == spec 的 header Exact 语义 |
| 匹配 `headers[].RegularExpression` | map `~<regex>` 键 | 声明序求值，大小写敏感 |
| 匹配 `queryParams[].Exact/RegularExpression` | map `$arg_<name>` | |
| 匹配 AND（单个 match 内多约束）/ OR（多 match） | map 级联决策树：每个输入一层，键分支=匹配该键的规则 ∪ 不约束该输入的规则（保持优先级顺序）；叶=upstream 名；`""` → location 内 `if ($var = "") { return 404; }` | 规则优先级 = 附加顺序（GEP-722 合并序） |
| 有约束规则 + 失败后端混合 | 静态 500 条目从分发用例中剔除（已记录偏差）：标记条目单独存在时才渲染 `return 500` | |

已知偏差（均在 Suites 可观测范围内如实记录）：
- `add` 请求头时若入站请求已携带同名头，nginx 只发出新值（不追加第二份）；
- redirect 条目与代理条目共享同一 location 时，高优先级一方完全占有该 location；
- map 对 Exact 值的大小写不敏感匹配会放大 query param Exact 的匹配面（query 的 spec
  语义未明确定义大小写敏感性）；
- `mirror` 指令是 **location 级**的（nginx 无按 match case 的镜像）：一个 location
  内所有**可达** case 的镜像目标都会对命中该 location 的请求生效（目标的并集；
  被 first-match 遮蔽的 case 的镜像不生效）。同一目标在多个 case 中出现时取最大
  百分比（NGF 行为）；
- 镜像副本收到的是**原始**请求 URI（`$request_uri`）：URLRewrite 不作用于镜像副本
  （NGF 作用于副本——两实现在此可观测地不同；spec 未明确要求）；
- 多后端（加权）规则上的 backendRef 级 RequestHeaderModifier 不支持（整条 rule
  丢弃）：规则私有加权 upstream 无法按 backend 区分 proxy 头。


### 5.2 reload 失败回滚状态机 + 生效验证（v5）

1. 渲染 → `<gw>.conf.tmp`；临时主配置 `nginx -t` 验证；
2. `rename(.tmp → .conf)`，同时保留上一份 `.conf.prev`；
3. `nginx -s reload`；
4. **生效验证（v5）**：本次渲染相对上次成功配置**新增** listen socket 时，以 reload
   前的 error log 字节偏移为界，有界窗口（默认 1s）内轮询 nginx error log 的追加内容；
   检出 `bind() to … failed` 行即判定 reload 被 master 拒绝（graceful reload 的
   "信号致盲"：`nginx -s reload` 退出 0 但新 worker 绑定失败、master 继续用旧配置），
   转入步骤 5 回滚。无新增 listen（多数同步）或日志不可读时跳过（尽力而为）；
5. reload 失败或生效验证检出失败 → 从 `.prev` 恢复 `.conf` → 再次 reload →
   `Programmed=False (Invalid)` + 失败原因（stderr 或 bind 失败行）进 message。
   在步骤 4 通过前**不得**置 Programmed=True。

### 5.3 证书生命周期

- 确定性文件名：`<namespace>_<name>.pem`；
- 每次 reconcile：渲染期望证书集合 → 变更证书写临时文件后 `rename(2)` 原子替换 → **删除 certs/ 中不在期望集合内的孤儿文件**；
- 权限：`0640`，属主供 nginx master master 进程读取（root 启动后 workers 无需直接访问）。

### 5.4 模板机制

- `text/template`，主模板 go:embed 内嵌；**不做**外部覆盖机制（N3，YAGNI，后续再加）。
- funcMap：`buildServerName`、`buildListen`、`buildLocation`、`buildProxyPass`、`buildTLS`。
- IR 契约（在并行开发前**锁定**，S8）：

```go
type Configuration struct {
    Servers   []*Server
    Upstreams []*Upstream
}
type Server struct {
    Hostname  string
    Listens   []Listen      // {Port, Address, SSL, HTTP2}
    TLSCert   string
    Locations []*Location   // {Path, Upstream, Rewrite, Timeouts}
}
type Upstream struct {
    Name      string
    Endpoints []Endpoint    // {IP, Port, Ready}
}
```

- `--nginx-binary=/usr/sbin/nginx` 可配置（N5）。

## 6. nginx 进程边界

- 控制器不启动/监管 nginx master；检测方式（S5）：读 `/run/nginx.pid`（pid 路径可配）→ `kill(pid, 0)` 探活；失败则 `Programmed=False (Pending)` 并跳过 reload。不依赖 `nginx -s reload` 退出码做探活（语义含糊）。
- `kill(pid, 0)` 返回 EPERM 视为**存活**（进程存在但不可发送信号——如非 root 探测 root master）；真正的 reload 失败会另行报错。
- **nginx 可执行路径的调用方式由 exec seam（`dataplane.Commander`）抽象**：所有
  `nginx -t` / `nginx -s reload` 一律经 `nsenter -t 1 -m -- <binary>` 进入**主机
  mount namespace** 执行主机的 nginx（唯一路径，见 §8.1 DaemonSet 部署形态）。
  `-c` 临时主配置与 `-p` 前缀本就是主机路径（`/etc/nginx` 经 hostPath 挂载）。

## 7. Reconciler 实现

- controller-runtime manager；单一 reconciler 忽略 request，全量处理本 controllerName 之下的资源。
- Watches：GatewayClass、Gateway、HTTPRoute、Secret（TLS）、EndpointSlice、Service（端口映射）、Namespace（Selector 评估）、ReferenceGrant（§3.5）；无 predicate/index（MVP）。
- 限速：全量 sync 最小间隔 250ms + workqueue 指数退避（applied-hash no-op 使冗余
  sync 成本仅为缓存重建 + 渲染哈希比较；间隔主要防止 reload 风暴）。
- Leader election：不实现（单主机单实例）。

## 8. 部署形态与安全（B5 修订）

- **不使用 admin 凭据**：专用 ServiceAccount（in-cluster），RBAC 限定
  get/list/watch：`gatewayclasses, gateways, httproutes, referencegrants, endpointslices, services, secrets（TLS）, namespaces`（v2：services/namespaces 为端口映射与 Selector 评估新增；v3：referencegrants 为 §3.5 新增）。

### 8.1 DaemonSet 部署形态（v2 新增；现为唯一形态）

控制器以 DaemonSet Pod 运行在节点上（Helm chart：
`charts/host-nginx-gateway`）：

- **Pod 规格**：`hostPID: true`（探活需要主机 PID）；`privileged: true`
  （或 capabilities `[SYS_ADMIN, SYS_PTRACE, CAP_KILL]`：nsenter 进入主机
  mount namespace 需要 SYS_ADMIN + SYS_PTRACE，`nginx -s reload` 向 root
  master 发信号需要 root 或 CAP_KILL）。
- **挂载**：`/etc/nginx`（ReadWrite，hostPath——生成配置、certs、临时主配置
  均落在主机文件系统）；`/host/run`（ReadOnly，hostPath `/run`——读 `nginx.pid`；不直接挂 `/run`，
  pod 的 `/run` 归容器运行时所有——projected SA token 需要在其下创建挂载点）；
- **节点地址**：`HNG_NODE_IP`（downward API `status.hostIP`）——pod 内接口路由看到的是
  pod 网络，节点主 IP 必须经 downward API 注入（§3.4 status.addresses 后备源）。
- **nginx 调用**：所有 `nginx -t` / `nginx -s reload` 一律经
  `nsenter -t 1 -m --` 进入主机 mount namespace（唯一 exec 路径，无
  exec-mode 旗标）。镜像需带 `nsenter`（util-linux）。验证与 reload
  在主机 mount namespace 内执行主机 nginx，所见即主机实际生效状态。
- **集群连接**：in-cluster ServiceAccount（无 kubeconfig 挂载）；chart
  内置 ClusterRole/Binding（§8 RBAC）。
- **互斥警告**：每台主机**只能运行一个控制器实例**（多写者会互相
  覆盖 `/etc/nginx/conf.d/k8s-gw/00-global.conf` 并触发 reload 抖动）。

## 9. 非目标（v5 调整）

- 不管理主机 nginx 的安装/启动/主配置。
- 不做多租户隔离。
- 不做 Gateway 基础设施 provision。
- ~~不支持高级流量处理（限 MVP 子集）~~ → v5 已实现 B 级核心集：
  backendRef 权重、RequestRedirect、URLRewrite、Request/ResponseHeaderModifier、
  method/header/query 匹配。~~仍不支持：RequestMirror（含 multiple/percentage）、
  backendRef 级 HeaderModifier~~ → v7 已实现（§5.1.1）：RequestMirror
  （single/multiple/percentage，`split_clients $request_id` 门控 +
  internal 镜像 location）、backendRef 级 RequestHeaderModifier（单后端规则；
  多后端规则仍丢弃，见已知偏差）。仍不支持：URLRewrite/RequestRedirect 的
  ExtensionRef、CORS/ExternalAuth（experimental）。
- 不支持 GRPCRoute / TLSRoute / TCPRoute / UDPRoute profile（GATEWAY-HTTP 之外）。
- ~~跨 namespace 路由挂载维持拒绝~~ → v6 已实现：由目标 Gateway 的
  listener 级 allowedRoutes.namespaces 决定（Same/All/Selector/None），
  allowedRoutes 即授权，不涉及 ReferenceGrant（§3.5）。

## 10. 实现拆分（Oracle 修订排序：先锁 IR 契约）

1. **骨架 + 契约**：go.mod、flag、`dataplane.Configuration` 类型锁定、status 条件契约、错误分类（validation / reload / nginx-not-running → 标准 reason 映射）。
2. **Dataplane**：模板渲染、临时主配置 `nginx -t`、`.prev` 回滚状态机、applied-hash no-op、certs 生命周期。可独立于 k8s 用固定 IR 单测。
3. **K8s provider**：watches → graph/IR → 状态写回（Accepted/Programmed/Conflicted/ResolvedRefs、GEP-1364、同 namespace 限制）。
4. **E2E 验证**：k3s + 主机 nginx 冒烟：共存、端口动态增删、跨 Gateway 冲突双方 Conflicted、证书轮换与孤儿清理、后端缺失 500、reload 失败回滚、非 root 运行。
