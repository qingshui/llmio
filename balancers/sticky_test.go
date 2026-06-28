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
