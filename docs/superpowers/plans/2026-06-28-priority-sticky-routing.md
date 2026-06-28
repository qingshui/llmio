# 主备模式 + IP 亲和性调度 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在现有 Lottery/Rotor 负载均衡之上，新增「主备」策略（按 `Priority` 分层降级）与「IP 亲和性 sticky」调度，并打通后端字段、API、前端表单与 i18n。

**Architecture:** `ModelWithProvider` 增加 `Priority` 字段；新增 `PriorityBalancer`（按 priority 降序分桶，每桶内复用 Lottery，永远先取最高非空桶）；新增 `StickyBalancer`（包级 `sync.Map` 缓存 `model|ip → key`，TTL 过期，Pop 命中校验 `Has`，Delete 失效，Success 重绑）。`Lottery`/`Rotor`/`PriorityBalancer`/`Breaker` 均实现 `Has(key) bool`。包装顺序：基础 balancer → breaker → sticky。`service/chat.go` 构建 priority map 并在 switch 增加 case；handler 与前端表单增字段。

**Tech Stack:** Go 1.24+（`math/rand/v2`、`container/list`、`sync`、`net`）、GORM（auto-migrate）、Gin、React 19 + TypeScript + react-hook-form + Zod、i18next（en/zh-CN/zh-TW）。

## Global Constraints

- 优先级语义：`Priority` 数字越大越优先（与现有 `DisplayOrder`/`Weight` 一致），默认 0。
- `Priority` 字段对 lottery/rotor 策略无影响（仅在 `priority` 策略下生效）。
- sticky 缓存为包级内存 `sync.Map`，进程重启丢失，单实例可接受。
- sticky 在最外层（breaker 之外）；`StickyTTL` 为 0 时用默认常量 `DefaultStickyTTL = 10 * time.Minute`。
- 客户端 IP 复用 `c.ClientIP()`；`reqMeta.RemoteIP == ""` 时不启用 sticky。
- 不修改 breaker 任何逻辑（仅新增 `Has` 方法）；不引入 CIDR 白名单或独立 IP 路由规则表。
- 每个任务结束前运行 `go build ./...` 与相关 `go test`，绿后提交。

---

## File Structure

- `consts/consts.go` — 新增 `BalancerPriority` 常量。
- `models/model.go` — `ModelWithProvider.Priority`；`Model.Sticky`/`Model.StickyTTL`。
- `balancers/balancers.go` — `Lottery`/`Rotor` 新增 `Has(key) bool` 方法。
- `balancers/breaker.go` — `Breaker` 新增 `Has(key) bool`（委托内层，内层不实现则 true）。
- `balancers/priority.go` — 新建：`PriorityBalancer` 实现 `Balancer` + `Has`。
- `balancers/sticky.go` — 新建：`StickyBalancer` + 包级 `stickyCache` + `hasChecker` 接口。
- `service/chat.go` — `ProvidersWithMeta` 增 `PriorityItems`/`Sticky`/`StickyTTL`；构建逻辑；`BalanceChat` switch 与包装。
- `handler/api.go` — `ModelRequest` 增 `Sticky`/`StickyTTL`；`ModelWithProviderRequest` 增 `Priority`；Create/Update 赋值；`GetModels` 策略校验。
- `webui/src/lib/api.ts` — `ModelWithProvider.Priority`；`Model.Sticky`/`StickyTTL`；请求体。
- `webui/src/routes/models.tsx` — 策略选项增 `priority`；增 sticky 开关 + TTL 输入；Zod schema。
- `webui/src/routes/model-providers.tsx` — 关联表增 Priority 列；表单增 Priority 输入。
- `webui/src/i18n/locales/{en,zh-CN,zh-TW}/models.json` — 策略/sticky 文案。
- `webui/src/i18n/locales/{en,zh-CN,zh-TW}/providers.json` — association_table.priority 等。
- `balancers/priority_test.go`、`balancers/sticky_test.go` — 新增测试。

---

### Task 1: 常量与数据模型字段

**Files:**
- Modify: `consts/consts.go:12-19`
- Modify: `models/model.go:27-55`

**Interfaces:**
- Produces: `consts.BalancerPriority = "priority"`；`models.ModelWithProvider.Priority int`；`models.Model.Sticky *bool`、`models.Model.StickyTTL int`。后续任务引用这些名字。

- [ ] **Step 1: 编辑 `consts/consts.go`，新增 priority 常量**

在 `BalancerRotor` 之后、`BalancerDefault` 之前插入：

```go
const (
	// 按权重概率抽取，类似抽签。
	BalancerLottery = "lottery"
	// 按顺序循环轮转，每次降低权重后移到队尾
	BalancerRotor = "rotor"
	// 按优先级分层，永远先用最高优先级层，全部失败后降到下一层。
	// 同层内按 Weight 加权随机。
	BalancerPriority = "priority"
	// 默认策略
	BalancerDefault = BalancerLottery
)
```

- [ ] **Step 2: 编辑 `models/model.go`，给 `Model` 增字段**

在 `Model` 结构体（`DisplayOrder` 行之后）增加：

```go
type Model struct {
	gorm.Model
	Name         string
	Remark       string
	MaxRetry     int    // 重试次数限制
	TimeOut      int    // 超时时间 单位秒
	Strategy     string // 负载均衡策略 默认 lottery
	Breaker      *bool  // 是否开启熔断
	DisplayOrder int    // 模型展示顺序，值越大越靠前
	Sticky       *bool  // 是否开启 IP 亲和性调度
	StickyTTL    int    // 粘性缓存秒数，0 表示用默认常量
}
```

- [ ] **Step 3: 编辑 `models/model.go`，给 `ModelWithProvider` 增字段**

在 `Weight` 字段行之后增加 `Priority`：

```go
	Weight           int
	Priority         int // 优先级，数字越大越优先；仅 priority 策略下分层降级
	InputPrice       *float64
```

- [ ] **Step 4: 构建确认 GORM 能识别新字段**

Run: `cd /root/work/llmio && go build ./...`
Expected: 编译通过，无错误。

- [ ] **Step 5: 提交**

```bash
cd /root/work/llmio
git add consts/consts.go models/model.go
git commit -m "feat: 新增 BalancerPriority 常量与 Priority/Sticky 数据模型字段"
```

---

### Task 2: Lottery / Rotor 实现 Has

**Files:**
- Modify: `balancers/balancers.go:20-124`
- Test: `balancers/balancers_test.go`

**Interfaces:**
- Produces: `(*Lottery).Has(key uint) bool`、`(*Rotor).Has(key uint) bool`。被 sticky 的 `hasChecker` 断言使用。

- [ ] **Step 1: 写失败测试（追加到 `balancers_test.go` 末尾，Benchmark 之前或文件末尾均可）**

在文件末尾追加：

```go
func TestLotteryHas(t *testing.T) {
	w := NewLottery(map[uint]int{1: 1, 2: 2})
	if !w.Has(1) {
		t.Errorf("expected Has(1)=true")
	}
	if w.Has(999) {
		t.Errorf("expected Has(999)=false")
	}
	w.Delete(1)
	if w.Has(1) {
		t.Errorf("expected Has(1)=false after Delete")
	}
}

func TestRotorHas(t *testing.T) {
	wl := NewRotor(map[uint]int{1: 10, 2: 20})
	if !wl.Has(1) {
		t.Errorf("expected Has(1)=true")
	}
	if wl.Has(999) {
		t.Errorf("expected Has(999)=false")
	}
	wl.Delete(1)
	if wl.Has(1) {
		t.Errorf("expected Has(1)=false after Delete")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd /root/work/llmio && go test ./balancers/ -run 'TestLotteryHas|TestRotorHas' -v`
Expected: FAIL，提示 `w.Has undefined` / `wl.Has undefined`。

- [ ] **Step 3: 给 Lottery 加 Has 方法**

在 `balancers.go` 的 `func (w *Lottery) Success` 之后追加：

```go
func (w *Lottery) Has(key uint) bool {
	_, ok := w.store[key]
	return ok
}
```

- [ ] **Step 4: 给 Rotor 加 Has 方法**

在 `func (w *Rotor) Success` 之后追加：

```go
func (w *Rotor) Has(key uint) bool {
	for e := w.Front(); e != nil; e = e.Next() {
		if e.Value.(uint) == key {
			return true
		}
	}
	return false
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `cd /root/work/llmio && go test ./balancers/ -run 'TestLotteryHas|TestRotorHas' -v`
Expected: PASS。

- [ ] **Step 6: 提交**

```bash
cd /root/work/llmio
git add balancers/balancers.go balancers/balancers_test.go
git commit -m "feat: Lottery/Rotor 实现 Has 方法"
```

---

### Task 3: Breaker 实现 Has

**Files:**
- Modify: `balancers/breaker.go:37-107`

**Interfaces:**
- Consumes: 内层 `Balancer` 可能实现 `hasChecker`（`Has(uint) bool`）。
- Produces: `(*Breaker).Has(key uint) bool`，委托内层。

- [ ] **Step 1: 写失败测试（追加到 `balancers/breaker_test.go` 末尾）**

```go
func TestBreakerHasDelegates(t *testing.T) {
	inner := NewLottery(map[uint]int{1: 1, 2: 2})
	b := BalancerWrapperBreaker(inner)
	if !b.Has(1) {
		t.Errorf("expected Has(1)=true via delegation")
	}
	if b.Has(999) {
		t.Errorf("expected Has(999)=false via delegation")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd /root/work/llmio && go test ./balancers/ -run TestBreakerHasDelegates -v`
Expected: FAIL，`b.Has undefined`。

- [ ] **Step 3: 给 Breaker 加 Has 方法**

在 `breaker.go` 末尾追加（`hasChecker` 接口定义将放在 `sticky.go`，但为避免循环引用问题，把接口定义放在 `breaker.go` 或共享处——这里放在 `breaker.go` 末尾，因为 breaker 是更基础层）：

实际上 `hasChecker` 接口供 sticky 使用，且 Breaker.Has 也需要它。把接口定义放在 `balancers.go`（包根，最基础文件）末尾更合适。**调整：在 `balancers.go` 末尾追加接口定义**：

```go
// hasChecker 用于校验某个 key 是否仍在 balancer 候选池中。
// sticky 在 Pop 命中缓存时用它校验缓存项是否仍有效。
type hasChecker interface {
	Has(uint) bool
}
```

在 `breaker.go` 末尾追加 Breaker.Has：

```go
func (b *Breaker) Has(key uint) bool {
	if h, ok := b.Balancer.(hasChecker); ok {
		return h.Has(key)
	}
	return true
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `cd /root/work/llmio && go test ./balancers/ -run TestBreakerHasDelegates -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
cd /root/work/llmio
git add balancers/balancers.go balancers/breaker.go balancers/breaker_test.go
git commit -m "feat: Breaker 实现 Has 委托内层，新增 hasChecker 接口"
```

---

### Task 4: PriorityBalancer 实现

**Files:**
- Create: `balancers/priority.go`
- Test: `balancers/priority_test.go`

**Interfaces:**
- Consumes: `NewLottery(items map[uint]int) *Lottery`（同桶内加权随机）；`(*Lottery).Pop/Delete/Reduce/Has`。
- Produces: `NewPriority(items map[uint]int, priorities map[uint]int) *PriorityBalancer`；`(*PriorityBalancer)` 实现 `Balancer` + `Has(uint) bool`。

- [ ] **Step 1: 写失败测试（新建 `balancers/priority_test.go`）**

```go
package balancers

import (
	"testing"
)

// 命中分布断言：在 P2 桶未空时，所有 Pop 必来自 P2。
func TestPriorityPopHighestFirst(t *testing.T) {
	items := map[uint]int{1: 10, 2: 10, 3: 10}
	priorities := map[uint]int{1: 2, 2: 2, 3: 0}
	p := NewPriority(items, priorities)
	for range 20 {
		id, err := p.Pop()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id == 3 {
			t.Errorf("P0 的 key 3 不应在 P2 还有候选时被选中")
		}
	}
}

// P2 全部 Delete 后，Pop 落到下一非空桶（P0）。
func TestPriorityFallbackOnDelete(t *testing.T) {
	items := map[uint]int{1: 10, 3: 10}
	priorities := map[uint]int{1: 2, 3: 0}
	p := NewPriority(items, priorities)
	p.Delete(1)
	id, err := p.Pop()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 3 {
		t.Errorf("expected fallback to P0 key 3, got %d", id)
	}
}

// Reduce 后 key 仍在原桶（仍可能被选中）。
func TestPriorityReduceStaysInBucket(t *testing.T) {
	items := map[uint]int{1: 9}
	priorities := map[uint]int{1: 2}
	p := NewPriority(items, priorities)
	p.Reduce(1)
	if !p.Has(1) {
		t.Errorf("expected Has(1)=true after Reduce")
	}
	id, err := p.Pop()
	if err != nil || id != 1 {
		t.Errorf("expected still pop 1, got %v %v", id, err)
	}
}

// 所有桶空 Pop 返回 error。
func TestPriorityEmpty(t *testing.T) {
	items := map[uint]int{1: 10}
	priorities := map[uint]int{1: 2}
	p := NewPriority(items, priorities)
	p.Delete(1)
	if _, err := p.Pop(); err == nil {
		t.Fatalf("expected error when all buckets empty")
	}
}

// Has 正确反映存在性。
func TestPriorityHas(t *testing.T) {
	items := map[uint]int{1: 10, 2: 10}
	priorities := map[uint]int{1: 2, 2: 0}
	p := NewPriority(items, priorities)
	if !p.Has(1) || !p.Has(2) {
		t.Errorf("expected both present")
	}
	p.Delete(1)
	if p.Has(1) {
		t.Errorf("expected Has(1)=false after Delete")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd /root/work/llmio && go test ./balancers/ -run TestPriority -v`
Expected: FAIL，`undefined: NewPriority`。

- [ ] **Step 3: 实现 `balancers/priority.go`**

```go
package balancers

import (
	"fmt"
	"slices"
)

// PriorityBalancer 按优先级分层降级：永远先用最高优先级层（桶），
// 该层全部失败/熔断移除后自然落到下一层。同层内按 Weight 加权随机（复用 Lottery）。
// 优先级数字越大越优先。
type PriorityBalancer struct {
	buckets           map[int]*Lottery
	keyPriority       map[uint]int
	orderedPriorities []int // 降序
}

// NewPriority 构造分层 balancer。
// items: key -> weight；priorities: key -> priority（数字越大越优先）。
func NewPriority(items map[uint]int, priorities map[uint]int) *PriorityBalancer {
	p := &PriorityBalancer{
		buckets:     map[int]*Lottery{},
		keyPriority: map[uint]int{},
	}
	for key, weight := range items {
		pr := priorities[key]
		if _, ok := p.buckets[pr]; !ok {
			p.buckets[pr] = NewLottery(map[uint]int{})
			p.orderedPriorities = append(p.orderedPriorities, pr)
		}
		p.buckets[pr].store[key] = weight
		p.keyPriority[key] = pr
	}
	slices.Sort(p.orderedPriorities)
	slices.Reverse(p.orderedPriorities) // 降序
	return p
}

func (p *PriorityBalancer) Pop() (uint, error) {
	for _, pr := range p.orderedPriorities {
		bucket, ok := p.buckets[pr]
		if !ok {
			continue
		}
		if len(bucket.store) == 0 {
			continue
		}
		return bucket.Pop()
	}
	return 0, fmt.Errorf("no provide items or all items are disabled")
}

func (p *PriorityBalancer) Delete(key uint) {
	pr, ok := p.keyPriority[key]
	if !ok {
		return
	}
	if bucket, ok := p.buckets[pr]; ok {
		bucket.Delete(key)
	}
	delete(p.keyPriority, key)
}

func (p *PriorityBalancer) Reduce(key uint) {
	pr, ok := p.keyPriority[key]
	if !ok {
		return
	}
	if bucket, ok := p.buckets[pr]; ok {
		bucket.Reduce(key)
	}
}

func (p *PriorityBalancer) Success(key uint) {
	// no-op：priority 层不依赖 success 记录
}

func (p *PriorityBalancer) Has(key uint) bool {
	_, ok := p.keyPriority[key]
	return ok
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `cd /root/work/llmio && go test ./balancers/ -run TestPriority -v`
Expected: PASS。

- [ ] **Step 5: 全量 balancers 测试回归**

Run: `cd /root/work/llmio && go test ./balancers/... -v`
Expected: 全绿。

- [ ] **Step 6: 提交**

```bash
cd /root/work/llmio
git add balancers/priority.go balancers/priority_test.go
git commit -m "feat: 新增 PriorityBalancer 主备分层策略"
```

---

### Task 5: StickyBalancer 实现

**Files:**
- Create: `balancers/sticky.go`
- Test: `balancers/sticky_test.go`

**Interfaces:**
- Consumes: `hasChecker`（来自 Task 3，定义在 `balancers.go`）；内层 `Balancer`。
- Produces: `NewSticky(inner Balancer, modelKey, clientIP string, ttl time.Duration) *StickyBalancer`；`DefaultStickyTTL`；`ResetStickyCache()`（测试用，清空包级缓存）。

- [ ] **Step 1: 写失败测试（新建 `balancers/sticky_test.go`）**

```go
package balancers

import (
	"testing"
	"time"
)

func TestStickyCacheHit(t *testing.T) {
	ResetStickyCache()
	inner := NewLottery(map[uint]int{1: 1, 2: 1})
	s := NewSticky(inner, "m", "1.2.3.4", time.Minute)
	first, err := s.Pop()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for range 10 {
		id, err := s.Pop()
		if err != nil || id != first {
			t.Errorf("expected sticky to %d, got %v %v", first, id, err)
		}
	}
}

func TestStickyInvalidateOnDelete(t *testing.T) {
	ResetStickyCache()
	inner := NewLottery(map[uint]int{1: 1, 2: 1})
	s := NewSticky(inner, "m", "1.2.3.4", time.Minute)
	first, _ := s.Pop()
	s.Delete(first)
	// Delete 后缓存失效，下一 Pop 重新选择
	_, err := s.Pop()
	if err != nil {
		t.Errorf("unexpected error after delete: %v", err)
	}
}

func TestStickyRebindOnSuccess(t *testing.T) {
	ResetStickyCache()
	inner := NewLottery(map[uint]int{1: 1, 2: 1})
	s := NewSticky(inner, "m", "1.2.3.4", time.Minute)
	s.Pop() // 粘到某 key（记为 A）
	s.Success(2) // 实际成功的是 2
	id, err := s.Pop()
	if err != nil || id != 2 {
		t.Errorf("expected rebound to 2, got %v %v", id, err)
	}
}

func TestStickyTTLExpiry(t *testing.T) {
	ResetStickyCache()
	inner := NewLottery(map[uint]int{1: 1, 2: 1})
	s := NewSticky(inner, "m", "1.2.3.4", 1*time.Nanosecond)
	s.Pop()
	time.Sleep(2 * time.Millisecond)
	// 过期后重新选择
	_, err := s.Pop()
	if err != nil {
		t.Errorf("unexpected error after expiry: %v", err)
	}
}

func TestStickyHasCheckInvalid(t *testing.T) {
	ResetStickyCache()
	inner := NewLottery(map[uint]int{1: 1, 2: 1})
	s := NewSticky(inner, "m", "1.2.3.4", time.Minute)
	first, _ := s.Pop()
	// 模拟内层把 first 删除（粘性目标已失效），sticky Pop 应重新选择
	inner.Delete(first)
	id, err := s.Pop()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id == first {
		t.Errorf("expected re-select since cached key invalid")
	}
}

func TestStickyDifferentIP(t *testing.T) {
	ResetStickyCache()
	inner := NewLottery(map[uint]int{1: 1})
	s1 := NewSticky(inner, "m", "1.1.1.1", time.Minute)
	s2 := NewSticky(inner, "m", "2.2.2.2", time.Minute)
	a, _ := s1.Pop()
	b, _ := s2.Pop()
	// 不同 IP 独立缓存，此处只有 key 1，都返回 1，但缓存键不同（不抛错即可）
	if a != 1 || b != 1 {
		t.Errorf("expected both 1, got %d %d", a, b)
	}
}

func TestStickyDefaultTTLWhenZero(t *testing.T) {
	ResetStickyCache()
	inner := NewLottery(map[uint]int{1: 1})
	s := NewSticky(inner, "m", "1.2.3.4", 0)
	if _, err := s.Pop(); err != nil {
		t.Errorf("unexpected error with zero ttl: %v", err)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd /root/work/llmio && go test ./balancers/ -run TestSticky -v`
Expected: FAIL，`undefined: NewSticky`。

- [ ] **Step 3: 实现 `balancers/sticky.go`**

```go
package balancers

import (
	"sync"
	"time"
)

// DefaultStickyTTL 是 sticky 缓存的默认过期时间。
const DefaultStickyTTL = 10 * time.Minute

type stickyEntry struct {
	key    uint
	expiry time.Time
}

var stickyCache sync.Map // key = modelKey + "|" + clientIP -> stickyEntry

// ResetStickyCache 清空包级 sticky 缓存，仅供测试使用。
func ResetStickyCache() {
	stickyCache = sync.Map{}
}

// StickyBalancer 在最外层（breaker 之外）包装内层 balancer，
// 使同一 model+clientIP 在 TTL 内尽量粘性到同一 provider。
type StickyBalancer struct {
	Balancer
	modelKey string
	clientIP string
	ttl      time.Duration
}

// NewSticky 构造 sticky balancer。ttl <= 0 时使用 DefaultStickyTTL。
func NewSticky(inner Balancer, modelKey, clientIP string, ttl time.Duration) *StickyBalancer {
	if ttl <= 0 {
		ttl = DefaultStickyTTL
	}
	return &StickyBalancer{Balancer: inner, modelKey: modelKey, clientIP: clientIP, ttl: ttl}
}

func (s *StickyBalancer) cacheKey() string {
	return s.modelKey + "|" + s.clientIP
}

func (s *StickyBalancer) loadValid() (uint, bool) {
	v, ok := stickyCache.Load(s.cacheKey())
	if !ok {
		return 0, false
	}
	entry := v.(stickyEntry)
	if !entry.expiry.After(time.Now()) {
		stickyCache.Delete(s.cacheKey())
		return 0, false
	}
	// 校验粘性 key 是否仍在内层候选池
	if h, ok := s.Balancer.(hasChecker); ok {
		if !h.Has(entry.key) {
			stickyCache.Delete(s.cacheKey())
			return 0, false
		}
	}
	return entry.key, true
}

func (s *StickyBalancer) store(key uint) {
	stickyCache.Store(s.cacheKey(), stickyEntry{key: key, expiry: time.Now().Add(s.ttl)})
}

func (s *StickyBalancer) Pop() (uint, error) {
	if key, ok := s.loadValid(); ok {
		return key, nil
	}
	id, err := s.Balancer.Pop()
	if err != nil {
		return 0, err
	}
	s.store(id)
	return id, nil
}

func (s *StickyBalancer) Delete(key uint) {
	s.Balancer.Delete(key)
	// 粘性目标失败，失效缓存，本请求后续重试重新选择
	if v, ok := stickyCache.Load(s.cacheKey()); ok {
		if v.(stickyEntry).key == key {
			stickyCache.Delete(s.cacheKey())
		}
	}
}

func (s *StickyBalancer) Reduce(key uint) {
	s.Balancer.Reduce(key)
	// 429 仅降权，不失效缓存：下次仍可能命中（由内层决定是否选中）
}

func (s *StickyBalancer) Success(key uint) {
	// 重试落到备用后，把粘性更新为最终成功的 provider 并续期
	s.store(key)
	s.Balancer.Success(key)
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `cd /root/work/llmio && go test ./balancers/ -run TestSticky -v`
Expected: PASS。

- [ ] **Step 5: 全量 balancers 测试回归**

Run: `cd /root/work/llmio && go test ./balancers/... -v`
Expected: 全绿。

- [ ] **Step 6: 提交**

```bash
cd /root/work/llmio
git add balancers/sticky.go balancers/sticky_test.go
git commit -m "feat: 新增 StickyBalancer IP 亲和性调度"
```

---

### Task 6: service 层接线

**Files:**
- Modify: `service/chat.go:24-49`、`service/chat.go:286-381`

**Interfaces:**
- Consumes: `consts.BalancerPriority`；`balancers.NewPriority(items, priorities)`；`balancers.NewSticky(inner, modelKey, clientIP, ttl)`；`models.ModelWithProvider.Priority`、`models.Model.Sticky`、`models.Model.StickyTTL`。
- Produces: `ProvidersWithMeta` 增 `PriorityItems map[uint]int`、`Sticky bool`、`StickyTTL int`。

- [ ] **Step 1: 扩展 `ProvidersWithMeta` 结构体**

`service/chat.go:286-294` 改为：

```go
type ProvidersWithMeta struct {
	ModelWithProviderMap map[uint]models.ModelWithProvider
	WeightItems          map[uint]int
	PriorityItems        map[uint]int
	ProviderMap          map[uint]models.Provider
	MaxRetry             int
	TimeOut              int
	Strategy             string
	Breaker              bool
	Sticky               bool
	StickyTTL            int
}
```

- [ ] **Step 2: 在 `ProvidersWithMetaBymodelsName` 构建 priority map 与 sticky 配置**

`service/chat.go:364-381`（weightItems 构建与 return 处）改为：

```go
	weightItems := make(map[uint]int)
	priorityItems := make(map[uint]int)
	for _, mp := range modelWithProviders {
		if _, ok := providerMap[mp.ProviderID]; !ok {
			continue
		}
		weightItems[mp.ID] = mp.Weight
		priorityItems[mp.ID] = mp.Priority
	}

	return &ProvidersWithMeta{
		ModelWithProviderMap: modelWithProviderMap,
		WeightItems:          weightItems,
		PriorityItems:        priorityItems,
		ProviderMap:           providerMap,
		MaxRetry:              model.MaxRetry,
		TimeOut:               model.TimeOut,
		Strategy:              model.Strategy,
		Breaker:               lo.FromPtrOr(model.Breaker, false),
		Sticky:                lo.FromPtrOr(model.Sticky, false),
		StickyTTL:             model.StickyTTL,
	}, nil
```

- [ ] **Step 3: 在 `BalanceChat` switch 增加 priority case 并加 sticky 包装**

`service/chat.go:35-49` 改为：

```go
	// 选择负载均衡策略
	var balancer balancers.Balancer
	switch providersWithMeta.Strategy {
	case consts.BalancerLottery:
		balancer = balancers.NewLottery(providersWithMeta.WeightItems)
	case consts.BalancerRotor:
		balancer = balancers.NewRotor(providersWithMeta.WeightItems)
	case consts.BalancerPriority:
		balancer = balancers.NewPriority(providersWithMeta.WeightItems, providersWithMeta.PriorityItems)
	default:
		balancer = balancers.NewLottery(providersWithMeta.WeightItems)
	}

	// 是否开启熔断
	if providersWithMeta.Breaker {
		balancer = balancers.BalancerWrapperBreaker(balancer)
	}

	// 是否开启 IP 亲和性（最外层，breaker 之外）
	if providersWithMeta.Sticky && reqMeta.RemoteIP != "" {
		ttl := time.Duration(providersWithMeta.StickyTTL) * time.Second
		balancer = balancers.NewSticky(balancer, before.Model, reqMeta.RemoteIP, ttl)
	}
```

- [ ] **Step 4: 编译确认**

Run: `cd /root/work/llmio && go build ./...`
Expected: 编译通过。

- [ ] **Step 5: 运行 service 包测试（若有）+ 全量编译**

Run: `cd /root/work/llmio && go test ./... 2>&1 | tail -30`
Expected: 无新增失败（service 包当前可能无测试，编译通过即可）。

- [ ] **Step 6: 提交**

```bash
cd /root/work/llmio
git add service/chat.go
git commit -m "feat: BalanceChat 接入 priority 策略与 sticky 亲和性"
```

---

### Task 7: handler/API 层字段与校验

**Files:**
- Modify: `handler/api.go:32-61`、`handler/api.go:259-267`、Create/Update 赋值处

**Interfaces:**
- Consumes: `consts.BalancerPriority`；`models.Model.Sticky`/`StickyTTL`；`models.ModelWithProvider.Priority`。
- Produces: `ModelRequest.Sticky`/`StickyTTL`；`ModelWithProviderRequest.Priority`；`GetModels` 接受 `priority` 策略过滤。

- [ ] **Step 1: `ModelRequest` 增字段**

`handler/api.go:32-39` 改为：

```go
type ModelRequest struct {
	Name      string `json:"name"`
	Remark    string `json:"remark"`
	MaxRetry  int    `json:"max_retry"`
	TimeOut   int    `json:"time_out"`
	Strategy  string `json:"strategy"`
	Breaker   bool   `json:"breaker"`
	Sticky    bool   `json:"sticky"`
	StickyTTL int    `json:"sticky_ttl"`
}
```

- [ ] **Step 2: `ModelWithProviderRequest` 增 Priority**

`handler/api.go:46-61`，在 `Weight` 行之后增 `Priority int`：

```go
	Weight           int               `json:"weight"`
	Priority         int               `json:"priority"`
	InputPrice       float64           `json:"input_price"`
```

- [ ] **Step 3: `CreateModel` 赋值 Sticky**

`handler/api.go:322-330`（model 构造处）增加 `Sticky`、`StickyTTL`：

```go
	model := models.Model{
		Name:         req.Name,
		Remark:       req.Remark,
		MaxRetry:     req.MaxRetry,
		TimeOut:      req.TimeOut,
		Strategy:     strategy,
		Breaker:      &req.Breaker,
		DisplayOrder: maxDisplayOrder + 1,
		Sticky:       &req.Sticky,
		StickyTTL:    req.StickyTTL,
	}
```

- [ ] **Step 4: `UpdateModel` 赋值 Sticky**

`handler/api.go:371-379`（updates 构造处）增加：

```go
	updates := models.Model{
		Name:      req.Name,
		Remark:    req.Remark,
		MaxRetry:  req.MaxRetry,
		TimeOut:   req.TimeOut,
		Strategy:  strategy,
		Breaker:   &req.Breaker,
		Sticky:    &req.Sticky,
		StickyTTL: req.StickyTTL,
	}
```

- [ ] **Step 5: `CreateModelProvider` 赋值 Priority**

`handler/api.go:604-619`（modelProvider 构造处）增加：

```go
	modelProvider := models.ModelWithProvider{
		ModelID:          req.ModelID,
		ProviderModel:    req.ProviderModel,
		ProviderID:       req.ProviderID,
		ToolCall:         &req.ToolCall,
		StructuredOutput: &req.StructuredOutput,
		Image:            &req.Image,
		WithHeader:       &req.WithHeader,
		CustomerHeaders:  customerHeaders,
		ExtraBody:        extraBody,
		Weight:           req.Weight,
		Priority:         req.Priority,
		InputPrice:       &req.InputPrice,
		CacheReadPrice:   &req.CacheReadPrice,
		OutputPrice:      &req.OutputPrice,
		Currency:         req.Currency,
	}
```

- [ ] **Step 6: `UpdateModelProvider` 赋值 Priority**

`handler/api.go:670-685`（updates 构造处）增加 `Priority: req.Priority,`（紧随 `Weight: req.Weight,`）。

- [ ] **Step 7: `GetModels` 策略校验增 priority**

`handler/api.go:259-267` 改为：

```go
	if strategy := strings.TrimSpace(c.Query("strategy")); strategy != "" {
		switch strategy {
		case consts.BalancerLottery, consts.BalancerRotor, consts.BalancerPriority:
			query = query.Where("strategy = ?", strategy)
		default:
			common.BadRequest(c, "invalid strategy filter")
			return
		}
	}
```

- [ ] **Step 8: 编译确认**

Run: `cd /root/work/llmio && go build ./...`
Expected: 编译通过。

- [ ] **Step 9: 提交**

```bash
cd /root/work/llmio
git add handler/api.go
git commit -m "feat: API 层支持 Priority/Sticky 字段与 priority 策略校验"
```

---

### Task 8: 前端 api.ts 类型与请求体

**Files:**
- Modify: `webui/src/lib/api.ts:26-43`、createModel/updateModel/createModelProvider/updateModelProvider 请求体

**Interfaces:**
- Produces: `ModelWithProvider.Priority`；`Model.Sticky`/`StickyTTL`；请求体含 `sticky`/`sticky_ttl`/`priority`。

- [ ] **Step 1: `ModelWithProvider` 加 Priority**

`webui/src/lib/api.ts:26-43`，在 `Weight` 之后增：

```ts
  Weight: number;
  Priority: number;
  InputPrice: number;
```

- [ ] **Step 2: 找到 `Model` 接口增 Sticky/StickyTTL**

Run: `cd /root/work/llmio/webui && grep -n "interface Model " src/lib/api.ts`
查看 `Model` 接口定义位置，在其中（`Strategy`/`Breaker` 附近）增：

```ts
  Sticky: boolean | null;
  StickyTTL: number;
```

（若 `Model` 接口未显式列出 `Strategy`/`Breaker`，则在其 `DisplayOrder` 或 `Breaker` 字段后追加。）

- [ ] **Step 3: createModel/updateModel 请求体增 sticky/sticky_ttl**

Run: `cd /root/work/llmio/webui && grep -n "breaker:" src/lib/api.ts`
在对应 `createModel`/`updateModel` 请求体里，`breaker:` 之后增：

```ts
    sticky: model.Sticky ?? false,
    sticky_ttl: model.StickyTTL ?? 0,
```

（若函数直接传 `values` 对象，则在调用处 schema 中已含；以实际代码为准，确保请求体包含这两个字段。）

- [ ] **Step 4: createModelProvider/updateModelProvider 请求体增 priority**

Run: `cd /root/work/llmio/webui && grep -n "weight:" src/lib/api.ts`
在对应 modelProvider 请求体里 `weight:` 之后增 `priority:`。

- [ ] **Step 5: 前端类型检查**

Run: `cd /root/work/llmio/webui && pnpm run lint 2>&1 | tail -20`
Expected: 无类型错误（已有 EPIPE patch，lint 仅做类型检查应稳定）。

- [ ] **Step 6: 提交**

```bash
cd /root/work/llmio
git add webui/src/lib/api.ts
git commit -m "feat(webui): api 类型与请求体支持 Priority/Sticky"
```

---

### Task 9: models 表单增 priority 策略与 sticky 开关

**Files:**
- Modify: `webui/src/routes/models.tsx:70-225`、`webui/src/routes/models.tsx:539-597`
- Modify: `webui/src/i18n/locales/{en,zh-CN,zh-TW}/models.json`

**Interfaces:**
- Consumes: `Model.Sticky`/`StickyTTL`、`Strategy`。
- Produces: 表单能提交 `strategy=priority`、`sticky`、`sticky_ttl`；表格 renderStrategy 显示 Priority。

- [ ] **Step 1: Zod schema 增 priority 与 sticky/sticky_ttl**

`webui/src/routes/models.tsx:81-82` 改为：

```ts
  strategy: z.enum(["lottery", "rotor", "priority"]),
  breaker: z.boolean(),
  sticky: z.boolean().default(false),
  sticky_ttl: z.number().int().min(0).default(0),
```

- [ ] **Step 2: 默认值与 reset 增 sticky/sticky_ttl**

`webui/src/routes/models.tsx:108-109` 及所有 `form.reset({...})` 处（约 164、186、225 行）的默认对象增：

```ts
      strategy: "lottery",
      breaker: false,
      sticky: false,
      sticky_ttl: 0,
```

- [ ] **Step 3: 编辑回填增 sticky/sticky_ttl**

`webui/src/routes/models.tsx:217-218` 附近改为：

```ts
      strategy: model.Strategy === "rotor" ? "rotor" : (model.Strategy === "priority" ? "priority" : "lottery"),
      breaker: model.Breaker ?? false,
      sticky: model.Sticky ?? false,
      sticky_ttl: model.StickyTTL ?? 0,
```

- [ ] **Step 4: 提交请求体增 sticky/sticky_ttl**

`webui/src/routes/models.tsx:159-160` 及 180-181 附近 `strategy: values.strategy, breaker: values.breaker,` 之后增：

```ts
        sticky: values.sticky,
        sticky_ttl: values.sticky_ttl,
```

- [ ] **Step 5: renderStrategy 支持 priority**

`webui/src/routes/models.tsx:70-71` 改为：

```tsx
const renderStrategy = (strategy?: string) =>
  strategy === "rotor" ? "Rotor" : (strategy === "priority" ? "Priority" : "Lottery");
```

- [ ] **Step 6: 策略过滤选项增 priority（若 UI 有下拉）**

`webui/src/routes/models.tsx:73` 改为 `type StrategyFilter = "all" | "lottery" | "rotor" | "priority";`，并在对应 `<Select>`（约 269 行）增 `<SelectItem value="priority">Priority</SelectItem>`。

- [ ] **Step 7: 策略表单选项增 priority 卡片**

`webui/src/routes/models.tsx:563-573` 的选项数组增：

```tsx
                      {
                        value: "priority",
                        title: "Priority",
                        desc: "按优先级分层, 高优先级全部失败后降到下一层.",
                      },
```

- [ ] **Step 8: 新增 sticky 开关与 TTL 输入（紧跟 breaker 表单之后、strategy 之前或之后均可）**

在 `webui/src/routes/models.tsx` breaker 的 `</FormField>` 之后插入：

```tsx
              <FormField
                control={form.control}
                name="sticky"
                render={({ field }) => (
                  <FormItem className="flex flex-row items-center justify-between rounded-lg border p-4">
                    <div className="space-y-0.5">
                      <FormLabel className="text-base">IP 亲和性</FormLabel>
                      <p className="text-[13px] text-muted-foreground">同客户端 IP 在 TTL 内尽量粘性到同一 provider.</p>
                    </div>
                    <FormControl>
                      <Checkbox checked={field.value} onCheckedChange={field.onChange} />
                    </FormControl>
                  </FormItem>
                )}
              />

              {form.watch("sticky") && (
                <FormField
                  control={form.control}
                  name="sticky_ttl"
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>亲和缓存 TTL(秒, 0=默认600)</FormLabel>
                      <FormControl>
                        <Input
                          type="number"
                          {...field}
                          onChange={e => field.onChange(+e.target.value)}
                        />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
              )}
```

- [ ] **Step 9: i18n 文案（en/zh-CN/zh-TW models.json）**

在各 `models.json` 的合适位置（与 strategy 同级）增加 key（en 示例）：

```json
  "strategy_priority": "Priority (Active-Standby)",
  "sticky": "IP Affinity",
  "sticky_desc": "Pin same client IP to one provider within TTL.",
  "sticky_ttl": "Sticky TTL (s, 0=default)",
```

zh-CN 用中文，zh-TW 用繁体。若 models.tsx 当前未引用这些 i18n key（表单用中文硬编码），可跳过 i18n 仅留硬编码文案——**为保持与现有 models.tsx 风格一致（它用中文硬编码而非 i18n），本步 i18n 为可选，跳过即可**。但 `renderStrategy` 与表格列若用 i18n 则需补。检查：

Run: `cd /root/work/llmio/webui && grep -n "renderStrategy\|loadStrategy\|model_table.strategy" src/routes/models.tsx src/i18n/locales/en/models.json`
按命中情况决定是否补 i18n。若 models.tsx 表格用 `renderStrategy` 直接渲染字符串，则无需 i18n。

- [ ] **Step 10: 前端类型/lint 检查**

Run: `cd /root/work/llmio/webui && pnpm run lint 2>&1 | tail -20`
Expected: 无类型错误。

- [ ] **Step 11: 提交**

```bash
cd /root/work/llmio
git add webui/src/routes/models.tsx webui/src/i18n/locales
git commit -m "feat(webui): models 表单支持 priority 策略与 sticky 开关"
```

---

### Task 10: model-providers 关联表与表单增 Priority

**Files:**
- Modify: `webui/src/routes/model-providers.tsx:1064-1127`、表单部分、排序部分
- Modify: `webui/src/i18n/locales/{en,zh-CN,zh-TW}/providers.json`

**Interfaces:**
- Consumes: `association.Priority`（来自 api.ts）。

- [ ] **Step 1: 关联表表格增 Priority 列**

`webui/src/routes/model-providers.tsx:1075`（`weight` TableHead 之后）增：

```tsx
                      <TableHead>{t('association_table.priority')}</TableHead>
```

`webui/src/routes/model-providers.tsx:1127`（`{association.Weight}` TableCell 之后）增：

```tsx
                          <TableCell>{association.Priority}</TableCell>
```

- [ ] **Step 2: 移动端展示增 priority（若有 MobileInfoItem）**

`webui/src/routes/model-providers.tsx:1219`（mobile weight 行之后）增：

```tsx
                          <MobileInfoItem label={t('association_table.mobile.priority')} value={association.Priority} />
```

- [ ] **Step 3: 关联表单增 Priority 输入**

找到关联创建/编辑表单中 weight 输入的 `FormField`（Run: `cd /root/work/llmio/webui && grep -n "weight" src/routes/model-providers.tsx`），在其后增一个 Priority FormField：

```tsx
                <FormField
                  control={form.control}
                  name="priority"
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('association_form.priority')}</FormLabel>
                      <FormControl>
                        <Input
                          type="number"
                          {...field}
                          onChange={e => field.onChange(+e.target.value)}
                        />
                      </FormControl>
                      <p className="text-[11px] text-muted-foreground">{t('association_form.priority_hint')}</p>
                      <FormMessage />
                    </FormItem>
                  )}
                />
```

并在该表单的 Zod schema 与默认值中增 `priority: z.number().int().default(0)` 与 `priority: 0`，编辑回填处增 `priority: association.Priority ?? 0`。

- [ ] **Step 4: i18n providers.json 增 key（en/zh-CN/zh-TW）**

en `providers.json`：

```json
  "association_table": {
    ...
    "priority": "Priority",
    "mobile": { ..., "priority": "Priority" }
  },
  "association_form": {
    ...
    "priority": "Priority",
    "priority_hint": "Higher = preferred. Only effective under Priority strategy."
  }
```

zh-CN / zh-TW 用对应中文/繁体。

- [ ] **Step 5: 前端 lint 检查**

Run: `cd /root/work/llmio/webui && pnpm run lint 2>&1 | tail -20`
Expected: 无类型错误。

- [ ] **Step 6: 提交**

```bash
cd /root/work/llmio
git add webui/src/routes/model-providers.tsx webui/src/i18n/locales
git commit -m "feat(webui): 关联表与表单支持 Priority 字段"
```

---

### Task 11: 全量验证与构建

**Files:**
- 全仓库

- [ ] **Step 1: 后端全量测试**

Run: `cd /root/work/llmio && go test ./... 2>&1 | tail -30`
Expected: 全绿。

- [ ] **Step 2: 后端编译**

Run: `cd /root/work/llmio && go build ./...`
Expected: 通过。

- [ ] **Step 3: 前端构建**

Run: `cd /root/work/llmio/webui && pnpm run build 2>&1 | tail -30`
Expected: 构建成功（若遇 EPIPE，按 CLAUDE.md 说明重试或重打 patch）。

- [ ] **Step 4: 整体构建（前端嵌入后端）**

Run: `cd /root/work/llmio && scripts/build.sh 2>&1 | tail -20`
Expected: 产出 `./llmio`。

- [ ] **Step 5: 手动冒烟（可选，需 TOKEN）**

启动后在前端创建一个模型，策略选 Priority，关联两个 provider 分别 Priority=2 / Priority=0；开 Sticky。发请求验证：正常走 Priority=2；模拟 Priority=2 失败后降到 0；同 IP 连续请求粘性到同一 provider。

- [ ] **Step 6: 若有未提交改动则提交**

```bash
cd /root/work/llmio
git status
git add -A && git commit -m "chore: 全量验证通过"
```
（仅当有未提交改动时执行。）

---

## Self-Review

**1. Spec coverage:**
- 数据模型 `Priority`/`Sticky`/`StickyTTL` → Task 1 ✓
- `BalancerPriority` 常量 → Task 1 ✓
- `PriorityBalancer` + 分桶 + `Has` → Task 4 ✓
- `StickyBalancer` + `sync.Map` + TTL + `Has` 校验 + Delete 失效 + Success 重绑 → Task 5 ✓
- `Lottery`/`Rotor`/`Breaker` 实现 `Has` → Task 2/3 ✓
- `hasChecker` 接口 → Task 3 ✓
- 包装顺序（base→breaker→sticky）→ Task 6 ✓
- `ProvidersWithMeta` 字段与构建 → Task 6 ✓
- handler 字段与 `GetModels` 校验 → Task 7 ✓
- 前端 api.ts → Task 8 ✓
- models 表单 → Task 9 ✓
- model-providers 表单 → Task 10 ✓
- 测试（priority/sticky）→ Task 4/5 ✓
- 全量验证 → Task 11 ✓

**2. Placeholder scan:** 无 TBD/TODO；Task 8-10 含「以实际代码为准」的探查指令（grep 定位），因前端文件未逐行读取，给出锚点行号 + grep 命令定位，属可执行指令而非占位。

**3. Type consistency:** `NewPriority(items, priorities)` 在 Task 4 定义、Task 6 调用一致；`NewSticky(inner, modelKey, clientIP, ttl)` 在 Task 5 定义、Task 6 调用一致；`hasChecker` 接口在 Task 3（放 `balancers.go`）定义，Task 5 sticky 使用一致；`PriorityItems`/`Sticky`/`StickyTTL` 在 Task 6 定义、Task 7 handler 与 Task 8 前端引用一致。

（注：Task 9 Step 9 标注 i18n 可选与 models.tsx 现有硬编码风格一致；Task 10 Step 3 表单 schema/默认值回填指令明确。）
