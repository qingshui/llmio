package service

import "testing"

func TestBoostProviderPriority(t *testing.T) {
	const base = providerPriorityBoostBase

	tests := []struct {
		name               string
		providerID         uint
		originalPriority   int
		providerPriorities []uint
		want               int
	}{
		{"nil list keeps priority", 3, 7, nil, 7},
		{"empty list keeps priority", 3, 7, []uint{}, 7},
		{"empty list keeps zero priority", 3, 0, []uint{}, 0},
		{"empty list keeps negative priority", 3, -5, []uint{}, -5},
		{"first in list gets boost base", 3, 7, []uint{3, 1}, base},
		{"second in list decrements", 1, 7, []uint{3, 1}, base - 1},
		{"last in list decrements by index", 9, 7, []uint{3, 1, 9}, base - 2},
		{"outside list keeps small priority", 5, 5, []uint{3, 1}, 5},
		{"outside list clamps huge priority", 5, base, []uint{3, 1}, base - 3},
		{"outside list clamps near-base priority", 5, base - 1, []uint{3}, base - 2},
		{"zero provider id falls to clamp path", 0, 7, []uint{3, 1}, 7},
		{"duplicate id uses first position", 3, 7, []uint{3, 3, 1}, base},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := boostProviderPriority(tt.providerID, tt.originalPriority, tt.providerPriorities); got != tt.want {
				t.Errorf("boostProviderPriority(%d, %d, %v) = %d, want %d",
					tt.providerID, tt.originalPriority, tt.providerPriorities, got, tt.want)
			}
		})
	}
}

func TestBoostProviderPriority_OrderingInvariant(t *testing.T) {
	priorities := []uint{1, 2, 3}
	inList := func(i int) int { return boostProviderPriority(priorities[i], 5, priorities) }
	outOfList := boostProviderPriority(4, 1<<29, priorities)
	outOfListSmall := boostProviderPriority(5, 1, priorities)

	// 列表内按序递减
	if !(inList(0) > inList(1) && inList(1) > inList(2)) {
		t.Errorf("expected in-list priorities strictly decreasing, got %d > %d > %d", inList(0), inList(1), inList(2))
	}
	// 列表内最低层严格高于列表外 clamp 上界
	if inList(2) <= outOfList {
		t.Errorf("expected last in-list (%d) > out-of-list clamp (%d)", inList(2), outOfList)
	}
	// 列表外相互间保持原序
	if outOfList < outOfListSmall {
		t.Errorf("expected clamp (%d) >= small original (%d)", outOfList, outOfListSmall)
	}
}
