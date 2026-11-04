package main

import "testing"

func TestCoreBoxIgnoresFarOutliers(t *testing.T) {
	s := &Stats{X0: -5712, X1: -5602, Z0: -10210, Z1: -10048, Y0: 55, Y1: 94}
	for x := -5650; x < -5630; x++ {
		for z := -10140; z < -10120; z++ {
			s.Columns = append(s.Columns, Column{X: x, Z: z, N: 10, Y0: 60, Y1: 70})
		}
	}
	// 遠くに 1 列だけ (全体の 0.03% 以下)
	s.Columns = append(s.Columns, Column{X: -5712, Z: -10210, N: 1, Y0: 60, Y1: 60})
	b := CoreBox(s)
	if b[0] < -5655 || b[2] > -5627 || b[1] < -10145 || b[3] > -10117 {
		t.Fatalf("box は中心の塊に収まるはず: %v", b)
	}
	if b[0] >= b[2] || b[1] >= b[3] {
		t.Fatalf("box が逆: %v", b)
	}
}

func TestCoreBoxWithoutColumnsUsesRange(t *testing.T) {
	s := &Stats{X0: 1, X1: 9, Z0: 2, Z1: 8}
	if b := CoreBox(s); b != [4]int{1, 2, 10, 9} {
		t.Fatalf("box: %v", b)
	}
}

func TestAnnotForLimitsPillars(t *testing.T) {
	s := &Stats{X0: 0, X1: 100, Z0: 0, Z1: 100, Y0: 60, Y1: 70}
	for i := 0; i < maxPillars+500; i++ {
		s.Columns = append(s.Columns, Column{X: i % 100, Z: i / 100, N: 1 + i%7, Y0: 60, Y1: 62})
	}
	a := AnnotFor(s, "x", "y", true)
	if len(a.Pillars) != maxPillars {
		t.Fatalf("pillars=%d", len(a.Pillars))
	}
	if b := AnnotFor(s, "x", "y", false); len(b.Pillars) != 0 {
		t.Fatalf("復旧後の画面に赤い線は要らない: %d", len(b.Pillars))
	}
	arg := a.overlayArg()
	if arg["label"] != "x" || len(arg["pillars"].([]map[string]int)) != maxPillars {
		t.Fatalf("arg: %v", arg["label"])
	}
}
