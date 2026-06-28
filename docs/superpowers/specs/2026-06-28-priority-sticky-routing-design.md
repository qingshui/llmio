# 主备模式 + 按 IP 亲和性调度 设计

- 日期: 2026-06-28
- 范围: 在现有 Lottery/Rotor 负载均衡之上，新增「主备」策略（按优先级分层降级）与「IP 亲和性 sticky」调度。

## 目标

1. 支持主备模式：模型关联多个 provider 时，按 `Priority` 字段分层，永远先用最高优先级层，该层全部失败/熔断后才降到下一层；下一请求重新从最高层开始，被熔断的 provider 由现有熔断器自动恢复。
2. 支持按 IP 亲和性调度：同一客户端 IP 在 TTL 内尽量粘性到同一 provider，利于上游缓存命中；粘性 provider 失败后失效并重选，重试落到备用后粘性更新到备用。

## 非目标 (YAGNI)

- 不引入 CIDR 白名单或独立 IP 路由规则表（采用 sticky 语义）。
- 不做 sticky 后台清理 goroutine，采用 lazy 过期。
- priority 桶内固定用 Lottery（加权随机），不暴露桶内策略选择。
- 不修改 breaker 任何逻辑。

## 设计决策

- 主备实现：模型 `Strategy` 新增 `priority` 选项，与 `lottery`/`rotor` 并列；选中后按关联的 `Priority` 字段分层降级。**不**把 priority 做成正交修饰，避免策略与优先级耦合。
- IP 调度语义：IP 亲和性 sticky（非 CIDR 白名单、非独立规则表）。
- 主备恢复：依赖熔断器自动恢复；降级后下一请求即重新从最高层开始，无需请求级锁定。
- `Priority` 数字越大越优先（与现有 `DisplayOrder`/`Weight` 语义一致）。
- sticky 缓存用包级内存 `sync.Map`，重启丢失，可接受。
- 客户端 IP 复用 `c.ClientIP()`，`main.go` 已 `SetTrustedProxies`。

## 数据模型变更 (`models/model.go`)

### `ModelWithProvider` 增加

```go
Priority int // 优先级，数字越大越优先；0 为默认，主备策略下分层降级
```

### `Model` 增加

```go
Sticky    *bool // 是否开启 IP 亲和性调度
StickyTTL int    // 粘性缓存秒数，0 表示用默认常量
```

GORM auto-migrate 自动加列。旧数据 `Priority=0`、`Sticky=nil` 行为等同于不启用，向后兼容。

## 常量 (`consts/consts.go`)

```go
// 按优先级分层，永远先用最高优先级层，全部失败后降到下一层。
BalancerPriority = "priority"
```

`GetModels` 的策略校验 switch (`handler/api.go:259-267`) 增加 `case consts.BalancerPriority`。

## 新增「主备」策略 (`balancers/priority.go`)

`PriorityBalancer` 实现 `Balancer` 接口：

- 字段：
  - `buckets map[int]*Lottery` —— 按 priority 分桶，每桶是一个 Lottery（同层按 Weight 加权随机）。
  - `keyPriority map[uint]int` —— key 到所属 priority 的反查。
  - `orderedPriorities []int` —— 降序排列的 priority 列表，缓存避免每次排序。
- `NewPriority(items map[uint]int, priorities map[uint]int) *PriorityBalancer`：按 priorities 分桶，每桶传 items 中对应 key 的 weight。
- `Pop()`：遍历 `orderedPriorities`，取第一个非空桶，调用其 `Pop()`。所有桶空返回 `fmt.Errorf("no provide items or all items are disabled")`（与 Lottery 一致）。
- `Delete(key)`：从所属桶 `Delete(key)`，删除 `keyPriority[key]`（硬失败）。
- `Reduce(key)`：委托所属桶 `Reduce(key)`（429 降权，仍在原层）。
- `Success(key)`：no-op（Lottery 的 Success 仅记录，priority 层不依赖）。
- `Has(key) bool`：key 是否仍在某桶中（供 sticky 校验命中有效性）。

语义：永远先用最高优先级层；该层 provider 全部失败/熔断移除后，自然落到下一层。降级后**下一请求重新从最高层开始**；被熔断的 provider 由现有熔断器 60s 冷却后 HalfOpen 恢复。无需新增恢复逻辑。

注：breakers `BalancerWrapperBreaker` 在包装时会把 Open 节点 `Delete` 出底层 balancer（`breaker.go:48-50`），对 `PriorityBalancer` 同样生效——Open 的 provider 会从其所属桶移除，从而触发降级。

## 新增 IP 亲和性 (`balancers/sticky.go`)

`StickyBalancer` 包装内层 balancer（最外层，在 breaker 之外）：

```go
type stickyEntry struct {
    key    uint
    expiry time.Time
}

var stickyCache sync.Map // key = modelName + "|" + clientIP -> stickyEntry

type StickyBalancer struct {
    Balancer       // 内层（可能已包 breaker）
    modelKey  string
    clientIP  string
    ttl       time.Duration
}

func NewSticky(inner Balancer, modelKey, clientIP string, ttl time.Duration) *StickyBalancer
```

- 包级默认 TTL 常量：`DefaultStickyTTL = 10 * time.Minute`。`ttl <= 0` 时用默认。
- `Pop()`：
  1. 计算缓存 key `modelKey + "|" + clientIP`。
  2. 读缓存：若 `entry` 存在且 `expiry.After(now)` 且 `inner.Has(entry.key)` 为真 → 返回 `entry.key`（粘性命中，不调内层 Pop）。
  3. 否则 `inner.Pop()` 取一个 key，写入缓存（设 expiry = now + ttl），返回。
- `Delete(key)`：委托 `inner.Delete(key)` **并失效缓存项**（粘性 provider 失败后，本请求重试换下一个，下一请求重新选择）。
- `Reduce(key)`：委托 `inner.Reduce(key)`，**不**失效缓存（429 仅降权，下次仍可能命中，由内层决定是否仍选中）。
- `Success(key)`：把缓存更新为最终成功的 provider 并续期 TTL（重试落到备用后，后续粘性到备用，直到它也失败）；委托 `inner.Success(key)`。

`Has(key)` 透传给内层。sticky 是最外层，`Balancer` 接口不含 `Has`；通过最小接口判断内层是否支持：

```go
type hasChecker interface { Has(uint) bool }
```

sticky `Pop` 中 `if h, ok := inner.(hasChecker); ok { valid := h.Has(entry.key) } else { valid = true }`；若内层不实现 `Has`，视为缓存命中即有效。

**要求 Lottery、Rotor、PriorityBalancer、Breaker 均实现 `Has(key) bool`**：
- Lottery: key 在 store 且未 Delete。
- Rotor: key 在 list。
- PriorityBalancer: key 在某桶。
- Breaker: 委托内层 `b.Balancer.(hasChecker)`（若内层不实现则返回 true）。

由于 sticky 包装顺序为 `inner = breaker(若开启)`，sticky 的 `inner.(hasChecker)` 命中的是 `*Breaker`，`Breaker.Has` 再委托到 Lottery/Rotor/Priority，链路打通。

## 包装顺序 (`service/chat.go:37-49`)

```
NewLottery / NewRotor / NewPriority  (按 strategy)
  -> 若 Breaker: BalancerWrapperBreaker(...)
  -> 若 Sticky:  NewSticky(..., before.Model, reqMeta.RemoteIP, stickyTTL)
```

sticky 在最外层。sticky 调用 breaker（已包装的）`Pop`/`Delete` 等，从而熔断器逻辑不变；sticky 的 `Has` 校验通过 `hasChecker` 接口查询内层。

## `ProvidersWithMeta` 扩展 (`service/chat.go`)

```go
type ProvidersWithMeta struct {
    ModelWithProviderMap map[uint]models.ModelWithProvider
    WeightItems          map[uint]int
    PriorityItems        map[uint]int   // 新增：key -> priority
    ProviderMap          map[uint]models.Provider
    MaxRetry             int
    TimeOut              int
    Strategy             string
    Breaker              bool
    Sticky               bool          // 新增
    StickyTTL            int           // 新增：秒
}
```

`ProvidersWithMetaBymodelsName` (`chat.go:296-381`) 在构建 `weightItems` 的循环中同步构建 `priorityItems`（`priorityItems[mp.ID] = mp.Priority`）；从 `model` 读 `Sticky`/`StickyTTL`（`StickyTTL` 为 0 时 service 层不修正，由 sticky 层用默认）。

`BalanceChat` (`chat.go:37-49`) switch 增加：

```go
case consts.BalancerPriority:
    balancer = balancers.NewPriority(providersWithMeta.WeightItems, providersWithMeta.PriorityItems)
```

sticky 包装（breaker 之后）：

```go
if providersWithMeta.Sticky && reqMeta.RemoteIP != "" {
    ttl := time.Duration(providersWithMeta.StickyTTL) * time.Second
    balancer = balancers.NewSticky(balancer, before.Model, reqMeta.RemoteIP, ttl)
}
```

`reqMeta.RemoteIP` 已在 `BalanceChat` 入参可用（`chat.go:24`）。`before.Model` 同样可用。

## handler / API (`handler/api.go`)

### `ModelRequest` 增加

```go
Sticky    bool `json:"sticky"`
StickyTTL int  `json:"sticky_ttl"`
```

`CreateModel`/`UpdateModel` 赋值 `Sticky: &req.Sticky`、`StickyTTL: req.StickyTTL`。

### `ModelWithProviderRequest` 增加

```go
Priority int `json:"priority"`
```

`CreateModelProvider`/`UpdateModelProvider` 赋值 `Priority: req.Priority`。

### `GetModels` 策略校验

```go
switch strategy {
case consts.BalancerLottery, consts.BalancerRotor, consts.BalancerPriority:
    query = query.Where("strategy = ?", strategy)
...
```

## 前端 (`webui`)

### `src/lib/api.ts`

- `ModelWithProvider` 增加 `Priority: number`。
- `Model` 接口增加 `Sticky: boolean | null`、`StickyTTL: number`。
- `createModel`/`updateModel` 请求体增加 `sticky`、`sticky_ttl`。
- `createModelProvider`/`updateModelProvider` 请求体增加 `priority`。

### `src/routes/model-providers.tsx`

- 关联表表格新增「优先级」列，展示 `association.Priority`。
- 关联表单（创建/编辑）新增 Priority 数字输入，默认 0，提示「数字越大越优先；仅主备策略生效」。
- 关联排序选项可考虑增加按 priority 排序（可选，非必须）。

### models 表单页面 (`src/routes/models.tsx` 或对应文件)

- 策略 `<Select>` 新增「主备（priority）」选项。
- 新增 Sticky 开关 `<Switch>` 与 StickyTTL 数字输入（仅 sticky 开启时显示/可编辑）。
- i18n 文案补充（中英）。

## 测试

### `balancers/priority_test.go`

- `TestPriorityPopHighestFirst`：三桶 P2/P1/P0，连续 Pop 全部从 P2 取。
- `TestPriorityFallbackOnDelete`：P2 全部 Delete 后，Pop 落到 P1。
- `TestPriorityReduceStaysInBucket`：Reduce 某 key 后仍在原桶（Pop 仍可能命中）。
- `TestPrioritySameLayerWeighted`：同层多 key 权重不同，统计命中分布符合权重比（放宽断言）。
- `TestPriorityEmpty`：所有桶空 Pop 返回 error。

### `balancers/sticky_test.go`

- `TestStickyCacheHit`：同 IP 连续 Pop 返回相同 key。
- `TestStickyInvalidateOnDelete`：Delete(key) 后下一 Pop 重新选择。
- `TestStickyRebindOnSuccess`：Pop 返回 A，Success(B) 后下一 Pop 返回 B。
- `TestStickyTTLExpiry`：写入过期 entry 后 Pop 重新选择。
- `TestStickyHasCheckInvalid`：缓存命中但内层 `Has` 为 false（已被 Delete）时重新选择。
- `TestStickyDifferentIP`：不同 IP 缓存独立。

复用现有 `balancers_test.go`/`breaker_test.go` 的表风格与 `math/rand/v2`。

### 现有测试回归

- `go test ./balancers/...` 全绿。
- `go test ./...` 全绿。

## 涉及文件清单

- `consts/consts.go` —— 新增 `BalancerPriority` 常量
- `models/model.go` —— `ModelWithProvider.Priority`、`Model.Sticky`/`StickyTTL`
- `balancers/balancers.go` —— Lottery/Rotor 实现 `Has(key) bool`
- `balancers/breaker.go` —— `Breaker` 实现 `Has(key) bool`（委托内层）
- `balancers/priority.go` —— 新增 `PriorityBalancer`
- `balancers/sticky.go` —— 新增 `StickyBalancer` 与 `stickyCache`
- `service/chat.go` —— `ProvidersWithMeta` 字段、`ProvidersWithMetaBymodelsName` 构建、`BalanceChat` switch 与包装
- `handler/api.go` —— `ModelRequest`/`ModelWithProviderRequest` 字段、Create/Update 赋值、`GetModels` 策略校验
- `webui/src/lib/api.ts` —— 类型与请求体
- `webui/src/routes/model-providers.tsx` —— 关联表 priority 列与表单
- `webui/src/routes/models.tsx`（或对应）—— 策略选项、sticky 开关与 TTL
- `balancers/priority_test.go`、`balancers/sticky_test.go` —— 新增测试
- i18n 文件 —— 中英文案

## 边界与风险

- sticky 缓存为包级 `sync.Map`，进程重启丢失；多副本部署时各副本独立缓存（可接受，当前为单实例）。
- `StickyTTL` 为 0 时用 `DefaultStickyTTL`，避免用户未填导致永久粘性。
- priority 桶内固定 Lottery；若用户对同层期望顺序轮转，文档说明用 Rotor 而非 priority。
- `Has` 接口需 Lottery/Rotor/PriorityBalancer/Breaker 四者均实现，sticky 包装非这三者（如未来新策略）时按"命中即有效"兜底。
- breaker 包装 PriorityBalancer 时，`BalancerWrapperBreaker` 遍历全局 `nodes` 调 `balancer.Delete` 移除 Open 节点——`PriorityBalancer.Delete` 正确从所属桶移除，行为一致。
