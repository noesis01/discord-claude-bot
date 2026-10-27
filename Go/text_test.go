package main

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// golden.json 은 Python 원본(bot.py)의 결과 (testdata/gen_golden.py 로 생성)
type golden struct {
	Split map[string]struct {
		Input  string   `json:"input"`
		Output []string `json:"output"`
	} `json:"split"`
	Tools []struct {
		Name   string         `json:"name"`
		Input  map[string]any `json:"input"`
		Output string         `json:"output"`
	} `json:"tools"`
}

func loadGolden(t *testing.T) golden {
	b, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestSplitMessageMatchesPython(t *testing.T) {
	for name, c := range loadGolden(t).Split {
		got := splitMessage(c.Input, msgLimit)
		if !reflect.DeepEqual(got, c.Output) {
			t.Errorf("%s: %d chunks, Python %d", name, len(got), len(c.Output))
			for i := range got {
				if i < len(c.Output) && got[i] != c.Output[i] {
					t.Errorf("%s: chunk %d differs\n go: %q\n py: %q", name, i, runeCut(got[i], 120), runeCut(c.Output[i], 120))
					break
				}
			}
		}
		for i, ch := range got {
			if n := runeLen(ch); n > 2000 {
				t.Errorf("%s: chunk %d is %d chars (> Discord 2000)", name, i, n)
			}
		}
	}
}

func TestDescribeToolMatchesPython(t *testing.T) {
	for _, c := range loadGolden(t).Tools {
		if got := describeTool(c.Name, c.Input); got != c.Output {
			t.Errorf("%s %v:\n go: %q\n py: %q", c.Name, c.Input, got, c.Output)
		}
	}
}

func TestSplitLinesKeepEnds(t *testing.T) {
	got := splitLinesKeepEnds("a\r\nb\rc\n\nd")
	want := []string{"a\r\n", "b\r", "c\n", "\n", "d"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}
