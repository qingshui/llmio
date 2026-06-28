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
