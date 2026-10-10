// behavior.go forges the behavior payload expected by the upstream login.
//
// 生成一组仿真人输入的行为采样数据（键鼠事件、提交方式、环境特征），
// 与 collectBehavior() 的输出结构对齐。服务端校验通过后放行登录。
package bailu

import (
	"math"
	"time"
)

// ForgeBehavior synthesizes a human-looking behavior payload for the given
// credentials (typing implies the keydown/input counts).
func ForgeBehavior(email, password string) map[string]any {
	now := time.Now().UnixMilli()

	typed := len(email) + len(password)
	if typed < 6 {
		typed = 6
	}

	// keydown 采样：8 个点，间隔 230-420ms，普通字符统一记 'c'
	kd := make([]map[string]any, 0, 8)
	tk := now - int64(4200)
	for i := 0; i < 8; i++ {
		tk += int64(randRange(230, 420))
		kd = append(kd, map[string]any{"t": tk, "k": "c"})
	}
	// mousemove 采样：8 点贝塞尔式轨迹
	mm := make([]map[string]any, 0, 8)
	tm := tk + int64(randRange(300, 700))
	x0, y0 := float64(randRange(120, 380)), float64(randRange(430, 620))
	x1, y1 := 683+float64(randRange(-15, 15)), 420+float64(randRange(-10, 10))
	for i := 0; i < 8; i++ {
		f := float64(i) / 7
		ease := 0.5 - 0.5*math.Cos(f*math.Pi) // ease-in-out
		tm += int64(randRange(180, 320))
		mm = append(mm, map[string]any{
			"t": tm,
			"x": int(math.Round(x0 + (x1-x0)*ease + float64(randRange(-2, 2)))),
			"y": int(math.Round(y0 + (y1-y0)*ease + float64(randRange(-2, 2)))),
		})
	}
	cl := []map[string]any{{"t": tm + int64(randRange(40, 120)), "id": "submitBtn",
		"x": x1, "y": y1}}
	fo := []map[string]any{
		{"t": now - int64(4200+randRange(200, 400)), "id": "email"},
		{"t": tk + int64(randRange(800, 1400)), "id": "password"},
	}

	return map[string]any{
		"residentMs":     randRange(8000, 16000),
		"focusCount":     2,
		"inputCount":     typed,
		"pasteCount":     0,
		"submitDelayMs":  randRange(300, 1200),
		"submitMode":     "click",
		"submitButtonId": "submitBtn",
		"kc":             typed,
		"mc":             randRange(24, 68),
		"cc":             1,
		"fc":             2,
		"mstr":           randRange(2, 6),
		"mtot":           randRange(18, 34),
		"mcurv":          randRange(8, 55),
		"mspd":           randRange(1, 2500),
		"sample": map[string]any{
			"kd": kd, "mm": mm, "cl": cl, "fo": fo,
		},
		"botFlags": map[string]any{
			"wd": 0, "pw": 0, "pp": 0, "auto": 0, "sw": 0, "pc": 4, "pdf": 1,
		},
		"botScore": 0,
	}
}
