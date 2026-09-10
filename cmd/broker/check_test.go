package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSizingClass пришивает вывод проверки к таблице классов из требований к
// размещению: 200, 1000 и 4000 фиксаций в секунду — границы классов S, M и L.
func TestSizingClass(t *testing.T) {
	cases := []struct {
		rate float64
		want string
	}{
		{199, "недостаточно"},
		{200, "класса S"},
		{999, "класса S"},
		{1000, "класса M"},
		{3999, "класса M"},
		{4000, "класса L"},
		{50000, "класса L"},
	}
	for _, c := range cases {
		got := sizingClass(c.rate)
		if !strings.Contains(got, c.want) {
			t.Errorf("sizingClass(%.0f) = %q, ожидалось упоминание %q", c.rate, got, c.want)
		}
	}
}

// TestStatusLabelsSameWidth: метки печатаются в столбец, и разная ширина его
// разъезжает.
func TestStatusLabelsSameWidth(t *testing.T) {
	width := utf8.RuneCountInString(statusOK.label())
	for _, s := range []checkStatus{statusOK, statusWarn, statusFail} {
		if got := utf8.RuneCountInString(s.label()); got != width {
			t.Errorf("метка %q шириной %d символов, а у первой %d", s.label(), got, width)
		}
	}
}
