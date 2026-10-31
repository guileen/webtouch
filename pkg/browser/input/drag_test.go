package input

import "testing"

func TestParseDragCoords(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
		x1y1 [2]float64
		x2y2 [2]float64
	}{
		{"标准四坐标", "100,200,300,200", true, [2]float64{100, 200}, [2]float64{300, 200}},
		{"空白容忍", " 10 , 20 , 30 , 40 ", true, [2]float64{10, 20}, [2]float64{30, 40}},
		{"小数坐标", "1.5,2.5,3.5,4.5", true, [2]float64{1.5, 2.5}, [2]float64{3.5, 4.5}},
		{"空串", "", false, [2]float64{}, [2]float64{}},
		{"缺坐标", "1,2,3", false, [2]float64{}, [2]float64{}},
		{"多余坐标", "1,2,3,4,5", false, [2]float64{}, [2]float64{}},
		{"非数字", "a,b,c,d", false, [2]float64{}, [2]float64{}},
		{"部分非法", "1,2,x,4", false, [2]float64{}, [2]float64{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			x1, y1, x2, y2, err := ParseDragCoords(c.in)
			if c.ok {
				if err != nil {
					t.Fatalf("应解析成功: %v", err)
				}
				if x1 != c.x1y1[0] || y1 != c.x1y1[1] || x2 != c.x2y2[0] || y2 != c.x2y2[1] {
					t.Fatalf("坐标不匹配: got (%v,%v,%v,%v)", x1, y1, x2, y2)
				}
				return
			}
			if err == nil {
				t.Fatalf("非法输入应报错: %q", c.in)
			}
		})
	}
}
