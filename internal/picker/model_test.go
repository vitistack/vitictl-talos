package picker

import "testing"

func items(labels ...string) []Item {
	out := make([]Item, 0, len(labels))
	for _, l := range labels {
		out = append(out, Item{Label: l, Columns: []string{l}, Value: l})
	}
	return out
}

func TestFilterNarrowsAndClampsCursor(t *testing.T) {
	m := newModel(items("alpha", "beta", "gamma"))
	m.cursor = 2

	for _, r := range "bet" {
		m.push(r)
	}
	if got := len(m.visible()); got != 1 {
		t.Fatalf("visible() = %d rows, want 1", got)
	}
	if m.cursor != 0 {
		t.Errorf("cursor = %d after the list shrank, want 0", m.cursor)
	}
	sel, ok := m.selected()
	if !ok || sel.Label != "beta" {
		t.Errorf("selected() = %q (%v), want beta", sel.Label, ok)
	}
}

func TestSelectedIsFalseWhenNothingMatches(t *testing.T) {
	m := newModel(items("alpha", "beta"))
	for _, r := range "zzz" {
		m.push(r)
	}
	if _, ok := m.selected(); ok {
		t.Error("selected() reported a row from an empty list")
	}
	// Enter must resolve to nothing rather than to a stale row.
	if got := m.confirmed(); len(got) != 0 {
		t.Errorf("confirmed() = %d rows on an empty list, want 0", len(got))
	}
}

func TestBackspaceAndClearRestoreTheFullList(t *testing.T) {
	m := newModel(items("alpha", "beta", "gamma"))
	m.push('b')
	if len(m.visible()) != 1 {
		t.Fatalf("filter did not narrow: %d rows", len(m.visible()))
	}
	m.backspace()
	if len(m.visible()) != 3 {
		t.Errorf("backspace left %d rows, want 3", len(m.visible()))
	}
	m.push('b')
	m.clear()
	if len(m.visible()) != 3 || m.query != "" {
		t.Errorf("clear left query %q and %d rows, want empty and 3", m.query, len(m.visible()))
	}
}

func TestCursorStaysInRange(t *testing.T) {
	m := newModel(items("a", "b", "c"))
	m.up()
	if m.cursor != 0 {
		t.Errorf("up() from the top moved the cursor to %d", m.cursor)
	}
	m.pageDown()
	if m.cursor != 2 {
		t.Errorf("pageDown() left the cursor at %d, want the last row (2)", m.cursor)
	}
	m.down()
	if m.cursor != 2 {
		t.Errorf("down() past the end moved the cursor to %d", m.cursor)
	}
}

// Enter with nothing marked is the single-item path: it must resolve to the
// row under the cursor so choosing one cluster costs no extra keystrokes.
func TestConfirmedFallsBackToCursorRow(t *testing.T) {
	m := newModel(items("a", "b", "c")).withMulti()
	m.down()
	got := m.confirmed()
	if len(got) != 1 || got[0].Label != "b" {
		t.Fatalf("confirmed() = %v, want [b]", labelsOf(got))
	}
}

func TestToggleMarksAndAdvances(t *testing.T) {
	m := newModel(items("a", "b", "c")).withMulti()
	m.toggle() // marks a, moves to b
	m.toggle() // marks b, moves to c
	if m.markedCount() != 2 {
		t.Fatalf("markedCount() = %d, want 2", m.markedCount())
	}
	got := m.confirmed()
	if len(got) != 2 || got[0].Label != "a" || got[1].Label != "b" {
		t.Errorf("confirmed() = %v, want [a b]", labelsOf(got))
	}
}

func TestToggleIsReversible(t *testing.T) {
	m := newModel(items("a", "b")).withMulti()
	m.toggle()
	m.cursor = 0
	m.toggle()
	if m.markedCount() != 0 {
		t.Errorf("markedCount() = %d after toggling the same row twice, want 0", m.markedCount())
	}
}

// The fleet path: filter to a subset, mark all of it, clear the filter. The
// marks must survive refiltering, and must cover only what was shown.
func TestToggleAllMarksOnlyVisibleRowsAndSurvivesRefilter(t *testing.T) {
	m := newModel(items("prod-a", "prod-b", "test-c")).withMulti()
	for _, r := range "prod" {
		m.push(r)
	}
	m.toggleAll()
	m.clear()

	got := m.confirmed()
	if len(got) != 2 {
		t.Fatalf("confirmed() = %v, want the two prod rows", labelsOf(got))
	}
	for _, it := range got {
		if it.Label == "test-c" {
			t.Errorf("confirmed() included %q, which the filter had hidden", it.Label)
		}
	}
}

func TestToggleAllClearsWhenEverythingShownIsMarked(t *testing.T) {
	m := newModel(items("a", "b")).withMulti()
	m.toggleAll()
	if m.markedCount() != 2 {
		t.Fatalf("markedCount() = %d after the first Ctrl-A, want 2", m.markedCount())
	}
	m.toggleAll()
	if m.markedCount() != 0 {
		t.Errorf("markedCount() = %d after the second Ctrl-A, want 0", m.markedCount())
	}
}

func TestMarkingIsInertInSingleSelectMode(t *testing.T) {
	m := newModel(items("a", "b"))
	m.toggle()
	m.toggleAll()
	if m.markedCount() != 0 {
		t.Errorf("markedCount() = %d in single-select mode, want 0", m.markedCount())
	}
}

func TestRowsRenderMarkerGutterAlignedWithHeader(t *testing.T) {
	m := newModel([]Item{
		{Label: "a", Columns: []string{"alpha", "1"}},
		{Label: "b", Columns: []string{"b", "2"}},
	}).withHeader(Item{Columns: []string{"NAME", "N"}}).withMulti()
	m.toggle()

	rows := m.rows()
	if len(rows) != 2 {
		t.Fatalf("rows() = %d, want 2", len(rows))
	}
	if rows[0][:4] != "[x] " {
		t.Errorf("marked row starts %q, want %q", rows[0][:4], "[x] ")
	}
	if rows[1][:4] != "[ ] " {
		t.Errorf("unmarked row starts %q, want %q", rows[1][:4], "[ ] ")
	}
	header := m.headerRow()
	if header[:markerWidth] != "    " {
		t.Errorf("header %q is not indented by the marker gutter", header)
	}
	// The columns themselves must still line up under the header.
	if len(rows[0]) != len(rows[1]) {
		t.Errorf("rows are not padded to equal width: %q vs %q", rows[0], rows[1])
	}
}

func TestRenderRowHasNoTrailingWhitespace(t *testing.T) {
	got := renderRow(Item{Columns: []string{"a", "bb"}}, []int{5, 5})
	if got != "a      bb" {
		t.Errorf("renderRow() = %q", got)
	}
}

func labelsOf(items []Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Label)
	}
	return out
}
