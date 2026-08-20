// Package picker presents interactive fuzzy-filtered lists in the terminal.
//
// The state machine lives in model, separate from any terminal I/O, so the
// interesting behaviour — fuzzy filtering, cursor movement, which rows are
// marked, and which item a selection resolves to — is testable without a TTY.
// Select and SelectMulti add the termui rendering on top.
package picker

import (
	"strings"
	"unicode/utf8"

	"github.com/sahilm/fuzzy"
)

// Item is one selectable row.
type Item struct {
	// Label is what the query is fuzzy-matched against. Include everything
	// worth searching on, not only what is displayed.
	Label string
	// Columns are the cell values rendered for this row, padded into
	// alignment with the other rows.
	Columns []string
	// Value is the caller's own payload for the row.
	Value any
}

// defaultViewport is the assumed page size before a terminal height is known.
const defaultViewport = 10

// markerWidth is the width of the "[x] " gutter multi-select rows carry, which
// the header row is indented by to stay aligned with them.
const markerWidth = 4

// model holds the picker's state: the full item set, the current query, the
// filtered view, where the cursor sits within it, and — in multi mode — which
// rows are marked.
//
// filtered holds indices into all rather than copies, so a row's identity
// survives refiltering. That is what lets a mark made under one query still be
// a mark under the next one.
type model struct {
	all      []Item
	filtered []int
	query    string
	cursor   int
	viewport int
	// multi enables marking. Kept on the model rather than only in the
	// renderer so the "Enter with nothing marked takes the cursor row" rule
	// is testable.
	multi bool
	// marked holds the indices into all that the user has toggled on.
	marked map[int]struct{}
	// header is measured alongside the data when sizing columns, so the
	// title row lines up with the rows beneath it.
	header Item
}

func newModel(all []Item) *model {
	m := &model{all: all, viewport: defaultViewport, marked: map[int]struct{}{}}
	m.applyFilter()
	return m
}

// withHeader returns the model set up to reserve width for a header row.
func (m *model) withHeader(header Item) *model {
	m.header = header
	return m
}

// withMulti turns on marking.
func (m *model) withMulti() *model {
	m.multi = true
	return m
}

// headerRow renders the column titles at the same widths as the data rows.
func (m *model) headerRow() string {
	row := renderRow(m.header, m.columnWidths())
	if m.multi {
		return strings.Repeat(" ", markerWidth) + row
	}
	return row
}

// visible returns the rows currently matching the query.
func (m *model) visible() []Item {
	out := make([]Item, 0, len(m.filtered))
	for _, i := range m.filtered {
		out = append(out, m.all[i])
	}
	return out
}

// selected returns the item under the cursor. It reports false when the query
// matches nothing, so callers never select from an empty list.
func (m *model) selected() (Item, bool) {
	i, ok := m.cursorIndex()
	if !ok {
		return Item{}, false
	}
	return m.all[i], true
}

// cursorIndex resolves the cursor to a position in all.
func (m *model) cursorIndex() (int, bool) {
	if m.cursor < 0 || m.cursor >= len(m.filtered) {
		return 0, false
	}
	return m.filtered[m.cursor], true
}

// confirmed returns what Enter resolves to in multi mode: every marked row in
// the original item order, or — when nothing is marked — the row under the
// cursor. Falling back to the cursor row means the common "just this one" case
// needs no marking step at all.
func (m *model) confirmed() []Item {
	if len(m.marked) == 0 {
		if sel, ok := m.selected(); ok {
			return []Item{sel}
		}
		return nil
	}
	out := make([]Item, 0, len(m.marked))
	for i := range m.all {
		if _, ok := m.marked[i]; ok {
			out = append(out, m.all[i])
		}
	}
	return out
}

// toggle marks or unmarks the row under the cursor and steps down, so holding
// Tab walks a run of rows.
func (m *model) toggle() {
	if !m.multi {
		return
	}
	idx, ok := m.cursorIndex()
	if !ok {
		return
	}
	if _, on := m.marked[idx]; on {
		delete(m.marked, idx)
	} else {
		m.marked[idx] = struct{}{}
	}
	m.down()
}

// toggleAll marks every currently visible row, or clears them when they are
// already all marked. Filter first, then toggle all: that is how a fleet-wide
// selection is made — type "prod", press Ctrl-A.
func (m *model) toggleAll() {
	if !m.multi || len(m.filtered) == 0 {
		return
	}
	allOn := true
	for _, idx := range m.filtered {
		if _, on := m.marked[idx]; !on {
			allOn = false
			break
		}
	}
	for _, idx := range m.filtered {
		if allOn {
			delete(m.marked, idx)
			continue
		}
		m.marked[idx] = struct{}{}
	}
}

// markedCount reports how many rows are marked, for the status line.
func (m *model) markedCount() int { return len(m.marked) }

// push appends a rune to the query and refilters.
func (m *model) push(r rune) {
	m.query += string(r)
	m.applyFilter()
}

// backspace removes the last rune of the query, if any.
func (m *model) backspace() {
	if n := utf8.RuneCountInString(m.query); n > 0 {
		runes := []rune(m.query)
		m.query = string(runes[:n-1])
		m.applyFilter()
	}
}

// clear empties the query.
func (m *model) clear() {
	m.query = ""
	m.applyFilter()
}

// applyFilter recomputes the visible rows and keeps the cursor in range —
// a shorter list must never leave it pointing past the end.
func (m *model) applyFilter() {
	if m.query == "" {
		m.filtered = make([]int, len(m.all))
		for i := range m.all {
			m.filtered[i] = i
		}
	} else {
		labels := make([]string, len(m.all))
		for i, it := range m.all {
			labels[i] = it.Label
		}
		matches := fuzzy.Find(m.query, labels)
		out := make([]int, 0, len(matches))
		for _, match := range matches {
			out = append(out, match.Index)
		}
		m.filtered = out
	}
	m.clampCursor()
}

func (m *model) clampCursor() {
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func (m *model) up()   { m.moveBy(-1) }
func (m *model) down() { m.moveBy(1) }

func (m *model) pageUp()   { m.moveBy(-m.viewport) }
func (m *model) pageDown() { m.moveBy(m.viewport) }

func (m *model) moveBy(delta int) {
	m.cursor += delta
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

// rows renders the visible items as column-aligned strings, so the picker
// reads like the tables the rest of the CLI prints.
func (m *model) rows() []string {
	widths := m.columnWidths()
	out := make([]string, 0, len(m.filtered))
	for _, idx := range m.filtered {
		row := renderRow(m.all[idx], widths)
		if m.multi {
			marker := "[ ] "
			if _, on := m.marked[idx]; on {
				marker = "[x] "
			}
			row = marker + row
		}
		out = append(out, row)
	}
	return out
}

// columnWidths measures each column across the visible rows and the header,
// so both render at the same widths.
func (m *model) columnWidths() []int {
	var widths []int
	measure := func(it Item) {
		for i, cell := range it.Columns {
			w := len([]rune(cell))
			if i >= len(widths) {
				widths = append(widths, w)
				continue
			}
			if w > widths[i] {
				widths[i] = w
			}
		}
	}
	measure(m.header)
	for _, idx := range m.filtered {
		measure(m.all[idx])
	}
	return widths
}

// renderRow pads an item's cells to the given column widths. The trailing
// column is not padded, so rows carry no trailing whitespace.
func renderRow(it Item, widths []int) string {
	var b strings.Builder
	for i, cell := range it.Columns {
		b.WriteString(cell)
		if i < len(it.Columns)-1 && i < len(widths) {
			b.WriteString(strings.Repeat(" ", widths[i]-len([]rune(cell))+2))
		}
	}
	return strings.TrimRight(b.String(), " ")
}
