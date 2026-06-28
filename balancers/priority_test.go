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
