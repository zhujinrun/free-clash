package probe

import "testing"

// makeResults 生成按延迟升序排列的可用节点，用于验证配额挑选。
func makeResults(sources map[string]int) []*Result {
	// 固定遍历顺序，避免 map 顺序导致用例不稳定。
	order := []string{"a.example", "b.example", "c.example"}
	out := make([]*Result, 0, 64)
	lat := int64(100)
	for _, src := range order {
		for i := 0; i < sources[src]; i++ {
			out = append(out, &Result{
				Name:       src,
				Source:     src,
				Success:    3,
				AvgLatency: lat,
			})
			lat += 10
		}
	}
	return out
}

func TestSelectCapsDominantSource(t *testing.T) {
	// 复现线上情况：一个来源 19 个可用，其余两个来源各 10 / 6 个。
	ranked := makeResults(map[string]int{"a.example": 19, "b.example": 10, "c.example": 6})
	got := Select(ranked, 30, 0.5)
	if len(got) != 30 {
		t.Fatalf("输出数量 = %d，期望 30", len(got))
	}
	counts := map[string]int{}
	for _, r := range got {
		counts[r.Source]++
	}
	if counts["a.example"] > 15 {
		t.Errorf("单一来源占了 %d 个，超过上限 15", counts["a.example"])
	}
	if counts["c.example"] < 4 {
		t.Errorf("c 来源只有 %d 个入选，配额过小", counts["c.example"])
	}
	if counts["b.example"] < 8 {
		t.Errorf("b 来源只有 %d 个入选，配额过小", counts["b.example"])
	}
}

func TestSelectFillsWhenCapped(t *testing.T) {
	// 只有一个来源时配额会限制数量，应放开限制补齐到 topN。
	ranked := makeResults(map[string]int{"a.example": 40})
	got := Select(ranked, 30, 0.5)
	if len(got) != 30 {
		t.Fatalf("输出数量 = %d，期望补齐到 30", len(got))
	}
}

func TestSelectSpreadsEvenSources(t *testing.T) {
	// 四个来源可用量相同，任何一个都不该独占。
	ranked := makeResults(map[string]int{"a.example": 15, "b.example": 15, "c.example": 15})
	got := Select(ranked, 30, 0.5)
	counts := map[string]int{}
	for _, r := range got {
		counts[r.Source]++
	}
	if len(got) != 30 {
		t.Fatalf("输出数量 = %d，期望 30", len(got))
	}
	for src, n := range counts {
		if n < 7 {
			t.Errorf("%s 只入选 %d 个，名额分配不均: %v", src, n, counts)
		}
	}
}

func TestSelectKeepsOrderAndAll(t *testing.T) {
	ranked := makeResults(map[string]int{"a.example": 5, "b.example": 5, "c.example": 5})
	got := Select(ranked, 30, 0.5)
	if len(got) != len(ranked) {
		t.Fatalf("可用节点少于上限时应全部输出，得到 %d/%d", len(got), len(ranked))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].AvgLatency > got[i].AvgLatency {
			t.Errorf("第 %d 个节点延迟 %d 大于后一个 %d，排序被破坏",
				i, got[i-1].AvgLatency, got[i].AvgLatency)
		}
	}
}

func TestSelectInvalidCapRatio(t *testing.T) {
	// 两个来源都有充足候选时，非法 capRatio 应回落到 0.5 生效。
	ranked := makeResults(map[string]int{"a.example": 40, "b.example": 20})
	// 0 与 1 都视为未配置，回落到 0.5。
	for _, ratio := range []float64{0, 1, 1.5} {
		got := Select(ranked, 20, ratio)
		if len(got) == 0 {
			t.Fatalf("capRatio=%v 时没有输出", ratio)
		}
		counts := map[string]int{}
		for _, r := range got {
			counts[r.Source]++
		}
		if counts["a.example"] > 10 {
			t.Errorf("capRatio=%v 时 a 来源占了 %d 个，超过上限 10", ratio, counts["a.example"])
		}
	}
}

func TestSourceKeyFallback(t *testing.T) {
	if k := sourceKey(Result{}); k != "unknown" {
		t.Errorf("空来源应归入 unknown，得到 %q", k)
	}
	if k := sourceKey(Result{Source: "x"}); k != "x" {
		t.Errorf("应返回原来源，得到 %q", k)
	}
}
