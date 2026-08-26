package terminal

import (
	"fmt"
	"io"
	"miniagent/agent/internal/markdown/stream"
	"strings"
	"unicode/utf8"
)

const (
	reset     = "\x1b[0m"
	bold      = "\x1b[1m"
	italic    = "\x1b[3m"
	strike    = "\x1b[9m"
	underline = "\x1b[4m"

	monokaiForeground = "\x1b[38;2;248;248;242m"
	monokaiComment    = "\x1b[38;2;117;113;94m"
	monokaiRed        = "\x1b[38;2;249;38;114m"
	monokaiOrange     = "\x1b[38;2;253;151;31m"
	monokaiYellow     = "\x1b[38;2;230;219;116m"
	monokaiGreen      = "\x1b[38;2;166;226;46m"
	monokaiBlue       = "\x1b[38;2;102;217;239m"
	monokaiPurple     = "\x1b[38;2;174;129;255m"
)

// Renderer writes terminal output for stream parser events.
type Renderer struct {
	w               io.Writer
	highlighter     CodeHighlighter
	codeBlock       CodeBlockStyle
	wrapWidth       int
	inCode          bool
	inIndented      bool
	codeLang        string
	inTable         bool
	table           tableBuffer
	listTight       []bool
	lineStart       bool
	lineWidth       int
	quoteDepth      int
	listDepth       int
	listItem        string
	spaced          bool
	pending         bool
	style           styler
	theme           Theme
	width           WidthFunc
	tableLayout     TableLayout
	inlineRenderers map[string]InlineRenderFunc
	parserOptions   []stream.ParserOption
}

// RendererOption configures a terminal renderer.
type RendererOption func(*Renderer)

// WidthFunc returns the terminal cell width of text without ANSI escapes.
type WidthFunc func(string) int

// InlineRenderResult is the rendered terminal form of a custom inline atom.
type InlineRenderResult struct {
	Text  string
	Width int
}

// InlineRenderFunc renders a custom inline atom. The bool return reports
// whether the renderer handled the event.
type InlineRenderFunc func(event stream.Event) (InlineRenderResult, bool)

// TableMode controls how terminal tables are laid out.
type TableMode int

const (
	// TableModeBuffered buffers a table until its end, computes final column
	// widths, and renders it once. This is the default and produces aligned
	// append-only output for arbitrary Markdown tables. If wrapping is enabled,
	// columns are shrunk and overflowing cells are ellipsized to fit the wrap
	// width.
	TableModeBuffered TableMode = iota

	// TableModeFixedWidth renders each row as soon as it closes using configured
	// column widths. This streams tables in append-only output, but overflowing
	// cell content is clipped or ellipsized.
	TableModeFixedWidth

	// TableModeAutoWidth renders each row as soon as it closes using widths
	// derived from MaxWidth, renderer wrap width, or an 80-column fallback.
	TableModeAutoWidth
)

// TableOverflow controls fixed-width table cell overflow behavior.
type TableOverflow int

const (
	TableOverflowClip TableOverflow = iota
	TableOverflowEllipsis
)

// TableLayout configures terminal table rendering.
type TableLayout struct {
	Mode TableMode

	// ColumnWidths are terminal display cell widths, excluding padding and
	// borders. In fixed-width mode, columns beyond this slice use the last width.
	ColumnWidths []int

	// MaxWidth is the total table width for auto-width mode, including borders
	// and padding. Zero means use the renderer wrap width, then an 80-column
	// fallback if no wrap width is configured.
	MaxWidth int

	Overflow TableOverflow
}

// StreamRenderer wires a parser to a terminal renderer and implements
// io.Writer for streaming Markdown inputs.
type StreamRenderer struct {
	parser  stream.Parser
	render  *Renderer
	flushed bool
}

// CodeBlockStyle controls terminal rendering for fenced-code blocks.
type CodeBlockStyle struct {
	Indent      int
	Border      bool
	BorderText  string
	BorderColor string
	Padding     int
}

type tableBuffer struct {
	align       []stream.TableAlign
	rows        []tableRow
	currentRow  *tableRow
	currentCell *tableCell
	fixedWidths []int
}

type tableRow struct {
	cells []tableCell
}

type tableCell struct {
	events []stream.Event
}

type renderedCell struct {
	text  string
	width int
}

// WithCodeBlockStyle configures fenced-code block layout.
func WithCodeBlockStyle(style CodeBlockStyle) RendererOption {
	return func(r *Renderer) {
		r.SetCodeBlockStyle(style)
	}
}

// WithCodeHighlighter configures fenced-code highlighting.
//
// Passing nil restores the dependency-free default highlighter.
func WithCodeHighlighter(highlighter CodeHighlighter) RendererOption {
	return func(r *Renderer) {
		if highlighter == nil {
			highlighter = newDefaultHighlighter(r.theme.Syntax)
		}
		r.highlighter = highlighter
	}
}

// WithWrapWidth configures the maximum visible width before the renderer
// inserts its own wrap. A non-positive value disables wrapping.
func WithWrapWidth(width int) RendererOption {
	return func(r *Renderer) {
		if width < 0 {
			width = 0
		}
		r.wrapWidth = width
	}
}

// WithWidthFunc configures terminal display-width calculation.
func WithWidthFunc(fn WidthFunc) RendererOption {
	return func(r *Renderer) {
		if fn != nil {
			r.width = fn
		}
	}
}

// WithInlineRenderer registers a renderer for custom inline atoms with typeName.
func WithInlineRenderer(typeName string, fn InlineRenderFunc) RendererOption {
	return func(r *Renderer) {
		if typeName == "" || fn == nil {
			return
		}
		if r.inlineRenderers == nil {
			r.inlineRenderers = make(map[string]InlineRenderFunc)
		}
		r.inlineRenderers[typeName] = fn
	}
}

// WithParserOptions configures the parser used by NewStreamRenderer.
func WithParserOptions(opts ...stream.ParserOption) RendererOption {
	return func(r *Renderer) {
		r.parserOptions = append(r.parserOptions, opts...)
	}
}

// WithTableLayout configures terminal table layout.
func WithTableLayout(layout TableLayout) RendererOption {
	return func(r *Renderer) {
		r.tableLayout = normalizeTableLayout(layout)
	}
}

// DefaultCodeBlockStyle returns the default fenced-code block layout.
func DefaultCodeBlockStyle() CodeBlockStyle {
	return defaultCodeBlockStyle()
}

// NewRenderer creates a terminal renderer that writes to w.
func NewRenderer(w io.Writer, opts ...RendererOption) *Renderer {
	var st styler
	var hl CodeHighlighter
	theme := DefaultTheme()
	if isTerminal(w) {
		st = ansiStyler{}
		hl = newHybridHighlighter(theme.Syntax)
	} else {
		st = plainStyler{}
		hl = NewPlainHighlighter()
	}
	r := &Renderer{
		w:           w,
		highlighter: hl,
		codeBlock:   defaultCodeBlockStyle(),
		theme:       theme,
		wrapWidth:   detectWrapWidth(w),
		lineStart:   true,
		style:       st,
		width:       visibleWidth,
		tableLayout: TableLayout{Mode: TableModeBuffered},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(r)
		}
	}
	return r
}

// NewStreamRenderer creates a convenience wrapper that parses and renders
// Markdown written to it.
func NewStreamRenderer(w io.Writer, opts ...RendererOption) *StreamRenderer {
	r := NewRenderer(w, opts...)
	return &StreamRenderer{
		parser: stream.NewParser(r.parserOptions...),
		render: r,
	}
}

// SetCodeBlockStyle changes fenced-code block layout.
func (r *Renderer) SetCodeBlockStyle(style CodeBlockStyle) {
	r.codeBlock = normalizeCodeBlockStyle(style)
}

// Write parses Markdown bytes and renders any newly emitted events.
func (r *StreamRenderer) Write(p []byte) (int, error) {
	if r.flushed {
		return 0, io.ErrClosedPipe
	}
	events, err := r.parser.Write(p)
	if err != nil {
		return len(p), err
	}
	if err := r.render.Render(events); err != nil {
		return len(p), err
	}
	return len(p), nil
}

// Flush finalizes the parser and renders any remaining events.
func (r *StreamRenderer) Flush() error {
	if r.flushed {
		return nil
	}
	events, err := r.parser.Flush()
	if err != nil {
		return err
	}
	if err := r.render.Render(events); err != nil {
		return err
	}
	r.flushed = true
	return nil
}

// Render writes terminal output for events.
func (r *Renderer) Render(events []stream.Event) error {
	for _, event := range events {
		if err := r.render(event); err != nil {
			return err
		}
	}
	return nil
}

func (r *Renderer) render(event stream.Event) error {
	if r.inTable {
		switch event.Kind {
		case stream.EventEnterBlock:
			switch event.Block {
			case stream.BlockTableRow:
				row := tableRow{}
				r.table.currentRow = &row
				return nil
			case stream.BlockTableCell:
				cell := tableCell{}
				r.table.currentCell = &cell
				return nil
			default:
				return nil
			}
		case stream.EventExitBlock:
			switch event.Block {
			case stream.BlockTableCell:
				if r.table.currentRow != nil && r.table.currentCell != nil {
					r.table.currentRow.cells = append(r.table.currentRow.cells, *r.table.currentCell)
				}
				r.table.currentCell = nil
				return nil
			case stream.BlockTableRow:
				if r.table.currentRow != nil {
					if r.streamingTableMode() {
						if err := r.writeStreamingTableRow(*r.table.currentRow); err != nil {
							return err
						}
					} else {
						r.table.rows = append(r.table.rows, *r.table.currentRow)
					}
				}
				r.table.currentRow = nil
				return nil
			case stream.BlockTable:
				return r.exitBlock(event)
			default:
				return nil
			}
		case stream.EventText, stream.EventInline, stream.EventSoftBreak, stream.EventLineBreak:
			if r.table.currentCell != nil {
				r.table.currentCell.events = append(r.table.currentCell.events, event)
			}
			return nil
		}
	}

	switch event.Kind {
	case stream.EventEnterBlock:
		return r.enterBlock(event)
	case stream.EventExitBlock:
		return r.exitBlock(event)
	case stream.EventText:
		return r.text(event)
	case stream.EventInline:
		return r.inline(event)
	case stream.EventSoftBreak:
		_, err := fmt.Fprint(r.w, " ")
		if err == nil {
			r.lineWidth++
		}
		return err
	case stream.EventLineBreak:
		_, err := fmt.Fprint(r.w, "\n")
		r.lineStart = true
		r.lineWidth = 0
		return err
	default:
		return nil
	}
}

func (r *Renderer) enterBlock(event stream.Event) error {
	switch event.Block {
	case stream.BlockDocument:
		return nil
	case stream.BlockHeading, stream.BlockParagraph, stream.BlockFencedCode, stream.BlockIndentedCode, stream.BlockThematicBreak, stream.BlockBlockquote, stream.BlockList, stream.BlockTable:
		if r.pending && !r.spaced && !r.tightListActive() {
			if _, err := fmt.Fprint(r.w, "\n"); err != nil {
				return err
			}
		}
		r.spaced = false
	}
	if event.Block == stream.BlockTable {
		r.inTable = true
		align := []stream.TableAlign(nil)
		if event.Table != nil {
			align = append([]stream.TableAlign(nil), event.Table.Align...)
		}
		r.table = tableBuffer{
			align: align,
		}
		return nil
	}
	if event.Block == stream.BlockBlockquote {
		r.quoteDepth++
		return nil
	}
	if event.Block == stream.BlockList {
		r.listDepth++
		tight := true
		if event.List != nil {
			tight = event.List.Tight
		}
		r.listTight = append(r.listTight, tight)
		return nil
	}
	if event.Block == stream.BlockListItem {
		r.listItem = listMarker(event)
		r.lineStart = true
		return nil
	}
	if event.Block == stream.BlockFencedCode {
		r.inCode = true
		r.codeLang = language(event.Info)
		r.highlighter.Start(r.codeLang, event.Info)
		return nil
	}
	if event.Block == stream.BlockIndentedCode {
		r.inIndented = true
		return nil
	}
	if event.Block == stream.BlockHeading {
		_, err := fmt.Fprint(r.w, r.style.code(bold), r.style.code(r.theme.Heading))
		return err
	}
	if event.Block == stream.BlockParagraph {
		_, err := fmt.Fprint(r.w, r.style.code(r.theme.Text))
		return err
	}
	if event.Block == stream.BlockThematicBreak {
		_, err := fmt.Fprint(r.w, r.style.code(r.theme.ThematicBreak), strings.Repeat("─", 24), r.style.reset(), "\n")
		r.pending = true
		r.lineStart = true
		r.lineWidth = 0
		return err
	}
	return nil
}

func (r *Renderer) exitBlock(event stream.Event) error {
	switch event.Block {
	case stream.BlockHeading:
		_, err := fmt.Fprint(r.w, r.style.reset(), "\n")
		r.pending = true
		r.lineStart = true
		r.lineWidth = 0
		return err
	case stream.BlockParagraph:
		_, err := fmt.Fprint(r.w, r.style.reset(), "\n")
		r.lineStart = true
		r.lineWidth = 0
		r.pending = true
		return err
	case stream.BlockFencedCode:
		r.inCode = false
		r.codeLang = ""
		r.highlighter.End()
		r.pending = true
		return nil
	case stream.BlockIndentedCode:
		r.inIndented = false
		r.pending = true
		return nil
	case stream.BlockBlockquote:
		if r.quoteDepth > 0 {
			r.quoteDepth--
		}
		r.pending = true
		return nil
	case stream.BlockList:
		if r.listDepth > 0 {
			r.listDepth--
		}
		if n := len(r.listTight); n > 0 {
			r.listTight = r.listTight[:n-1]
		}
		r.pending = true
		return nil
	case stream.BlockListItem:
		r.listItem = ""
		r.lineStart = true
		return nil
	case stream.BlockTable:
		if err := r.flushTable(); err != nil {
			return err
		}
		r.inTable = false
		r.pending = true
		r.lineStart = true
		return nil
	case stream.BlockTableRow, stream.BlockTableCell:
		return nil
	default:
		return nil
	}
}

func (r *Renderer) text(event stream.Event) error {
	if r.inTable {
		if r.table.currentCell != nil {
			r.table.currentCell.events = append(r.table.currentCell.events, event)
		}
		return nil
	}
	if r.inCode || r.inIndented {
		text := event.Text
		if r.inCode {
			text = r.highlighter.HighlightLine(event.Text)
		}
		_, err := fmt.Fprint(r.w, r.codePrefix(), text)
		r.lineStart = false
		return err
	}
	return r.writeWrappedText(event)
}

func (r *Renderer) writeLinePrefix() error {
	if !r.lineStart {
		return nil
	}
	r.lineWidth = 0
	if r.quoteDepth > 0 {
		for range r.quoteDepth {
			if _, err := fmt.Fprint(r.w, r.style.code(r.theme.BlockquoteBar), "│ ", r.style.reset()); err != nil {
				return err
			}
			r.lineWidth += 2
		}
	}
	if r.listItem != "" {
		if _, err := fmt.Fprint(r.w, strings.Repeat("  ", max(0, r.listDepth-1)), r.style.code(r.theme.ListMarker), r.listItem, r.style.reset()); err != nil {
			return err
		}
		r.lineWidth += 2*max(0, r.listDepth-1) + r.width(r.listItem)
		r.listItem = strings.Repeat(" ", visibleListMarkerWidth(r.listItem))
	}
	r.lineStart = false
	return nil
}

func (r *Renderer) tightListActive() bool {
	return len(r.listTight) > 0 && r.listTight[len(r.listTight)-1]
}

func normalizeTableLayout(layout TableLayout) TableLayout {
	layout.Overflow = normalizeTableOverflow(layout.Overflow)
	switch layout.Mode {
	case TableModeFixedWidth:
		if len(layout.ColumnWidths) == 0 {
			return TableLayout{Mode: TableModeBuffered}
		}
		widths := append([]int(nil), layout.ColumnWidths...)
		for i := range widths {
			if widths[i] < 1 {
				widths[i] = 1
			}
		}
		layout.ColumnWidths = widths
		return layout
	case TableModeAutoWidth:
		if layout.MaxWidth < 0 {
			layout.MaxWidth = 0
		}
		layout.ColumnWidths = nil
		return layout
	default:
		return TableLayout{Mode: TableModeBuffered}
	}
}

func normalizeTableOverflow(overflow TableOverflow) TableOverflow {
	switch overflow {
	case TableOverflowClip, TableOverflowEllipsis:
		return overflow
	default:
		return TableOverflowEllipsis
	}
}

func (r *Renderer) flushTable() error {
	if len(r.table.rows) == 0 {
		r.table = tableBuffer{}
		return nil
	}
	cols := len(r.table.align)
	for _, row := range r.table.rows {
		if len(row.cells) > cols {
			cols = len(row.cells)
		}
	}
	widths := make([]int, cols)
	rendered := make([][]renderedCell, len(r.table.rows))
	for i, row := range r.table.rows {
		rendered[i] = make([]renderedCell, cols)
		for c := 0; c < cols; c++ {
			var cell tableCell
			if c < len(row.cells) {
				cell = row.cells[c]
			}
			renderedCell := r.renderTableCell(cell.events)
			rendered[i][c] = renderedCell
			if renderedCell.width > widths[c] {
				widths[c] = renderedCell.width
			}
		}
	}
	widths = fitTableWidths(widths, r.wrapWidth)
	for i, row := range r.table.rows {
		for c := 0; c < cols; c++ {
			if rendered[i][c].width <= widths[c] {
				continue
			}
			var cell tableCell
			if c < len(row.cells) {
				cell = row.cells[c]
			}
			rendered[i][c] = r.renderEllipsizedTableCell(cell.events, widths[c])
		}
	}
	for _, row := range rendered {
		if err := r.writeTableRow(row, widths); err != nil {
			return err
		}
	}
	r.table = tableBuffer{}
	return nil
}
func (r *Renderer) streamingTableMode() bool {
	return r.tableLayout.Mode == TableModeFixedWidth || r.tableLayout.Mode == TableModeAutoWidth
}

func (r *Renderer) writeStreamingTableRow(row tableRow) error {
	cols := len(r.table.align)
	if len(row.cells) > cols {
		cols = len(row.cells)
	}
	if cols == 0 {
		return nil
	}
	cells := make([]renderedCell, cols)
	widths := make([]int, cols)
	for c := 0; c < cols; c++ {
		var cell tableCell
		if c < len(row.cells) {
			cell = row.cells[c]
		}
		width := r.streamingTableColumnWidth(c, cols)
		cells[c] = r.renderFixedWidthTableCell(cell.events, width)
		widths[c] = width
	}
	return r.writeTableRow(cells, widths)
}

func (r *Renderer) streamingTableColumnWidth(col, cols int) int {
	if len(r.table.fixedWidths) == 0 {
		switch r.tableLayout.Mode {
		case TableModeFixedWidth:
			r.table.fixedWidths = append([]int(nil), r.tableLayout.ColumnWidths...)
		case TableModeAutoWidth:
			r.table.fixedWidths = r.autoTableColumnWidths(cols)
		}
	}
	widths := r.table.fixedWidths
	if len(widths) == 0 {
		return 1
	}
	if col < len(widths) {
		return widths[col]
	}
	return widths[len(widths)-1]
}

func (r *Renderer) autoTableColumnWidths(cols int) []int {
	if cols <= 0 {
		cols = 1
	}
	total := r.tableLayout.MaxWidth
	if total <= 0 {
		total = r.wrapWidth
	}
	if total <= 0 {
		total = 80
	}
	overhead := 3*cols + 1 // borders plus one space of padding on each side.
	content := total - overhead
	if content < cols {
		content = cols
	}
	base := content / cols
	if base < 1 {
		base = 1
	}
	widths := make([]int, cols)
	for i := range widths {
		widths[i] = base
	}
	for extra := content - base*cols; extra > 0; extra-- {
		widths[(content-base*cols)-extra]++
	}
	return widths
}

func fitTableWidths(widths []int, maxTotal int) []int {
	if maxTotal <= 0 || len(widths) == 0 {
		return widths
	}
	total := 3*len(widths) + 1
	for _, width := range widths {
		total += width
	}
	if total <= maxTotal {
		return widths
	}
	fit := append([]int(nil), widths...)
	for total > maxTotal {
		largest := -1
		for i, width := range fit {
			if width <= 1 {
				continue
			}
			if largest < 0 || width > fit[largest] {
				largest = i
			}
		}
		if largest < 0 {
			break
		}
		fit[largest]--
		total--
	}
	return fit
}

func (r *Renderer) renderEllipsizedTableCell(events []stream.Event, width int) renderedCell {
	if width <= 0 {
		return renderedCell{}
	}
	full := r.renderTableCell(events)
	if full.width <= width {
		return full
	}
	suffix := tableOverflowSuffix(width)
	suffixWidth := r.width(suffix)
	if suffixWidth >= width {
		return renderedCell{text: suffix, width: suffixWidth}
	}
	cell := r.renderTableCellLimited(events, width-suffixWidth)
	cell.text += suffix
	cell.width += suffixWidth
	return cell
}

func (r *Renderer) renderFixedWidthTableCell(events []stream.Event, width int) renderedCell {
	if width <= 0 {
		return renderedCell{}
	}
	full := r.renderTableCell(events)
	if full.width <= width {
		return full
	}
	if r.tableLayout.Overflow == TableOverflowClip {
		return r.renderTableCellLimited(events, width)
	}
	suffix := tableOverflowSuffix(width)
	suffixWidth := r.width(suffix)
	if suffixWidth >= width {
		return renderedCell{text: suffix, width: suffixWidth}
	}
	cell := r.renderTableCellLimited(events, width-suffixWidth)
	cell.text += suffix
	cell.width += suffixWidth
	return cell
}

func tableOverflowSuffix(width int) string {
	switch {
	case width >= 3:
		return "..."
	case width == 2:
		return ".."
	case width == 1:
		return "."
	default:
		return ""
	}
}

func (r *Renderer) renderTableCellLimited(events []stream.Event, limit int) renderedCell {
	if limit <= 0 {
		return renderedCell{}
	}
	var out strings.Builder
	width := 0
	for _, ev := range events {
		remaining := limit - width
		if remaining <= 0 {
			break
		}
		switch ev.Kind {
		case stream.EventText:
			part, partWidth := r.takeWidth(ev.Text, remaining)
			out.WriteString(styledText(r.style, r.theme, ev.Style, part))
			width += partWidth
		case stream.EventInline:
			rendered := r.renderInline(ev)
			if rendered.Width <= remaining {
				out.WriteString(rendered.Text)
				width += rendered.Width
			}
		case stream.EventSoftBreak, stream.EventLineBreak:
			out.WriteByte(' ')
			width++
		}
	}
	return renderedCell{text: out.String(), width: width}
}

func (r *Renderer) takeWidth(text string, maxWidth int) (string, int) {
	if maxWidth <= 0 || text == "" {
		return "", 0
	}
	width := 0
	end := 0
	for i, rn := range text {
		runeText := string(rn)
		runeWidth := r.width(runeText)
		if runeWidth < 0 {
			runeWidth = 0
		}
		if width+runeWidth > maxWidth {
			break
		}
		width += runeWidth
		end = i + utf8.RuneLen(rn)
	}
	return text[:end], width
}

func (r *Renderer) renderTableCell(events []stream.Event) renderedCell {
	var out strings.Builder
	width := 0
	for _, ev := range events {
		switch ev.Kind {
		case stream.EventText:
			out.WriteString(r.styleText(ev))
			width += r.width(ev.Text)
		case stream.EventInline:
			rendered := r.renderInline(ev)
			out.WriteString(rendered.Text)
			width += rendered.Width
		case stream.EventSoftBreak, stream.EventLineBreak:
			out.WriteByte(' ')
			width++
		}
	}
	return renderedCell{text: out.String(), width: width}
}

func (r *Renderer) writeTableRow(cells []renderedCell, widths []int) error {
	if err := r.writeLinePrefix(); err != nil {
		return err
	}
	var out strings.Builder
	out.WriteString(r.style.code(r.theme.TableBorder))
	out.WriteString("│")
	out.WriteString(r.style.reset())
	for i := 0; i < len(widths); i++ {
		out.WriteByte(' ')
		cell := renderedCell{}
		if i < len(cells) {
			cell = cells[i]
		}
		width := cell.width
		align := stream.TableAlignNone
		if i < len(r.table.align) {
			align = r.table.align[i]
		}
		out.WriteString(padTableCell(cell.text, widths[i], width, align))
		out.WriteByte(' ')
		out.WriteString(r.style.code(r.theme.TableBorder))
		out.WriteString("│")
		out.WriteString(r.style.reset())
	}
	out.WriteByte('\n')
	if _, err := io.WriteString(r.w, out.String()); err != nil {
		return err
	}
	r.lineStart = true
	return nil
}

func padTableCell(cell string, targetWidth, visibleWidth int, align stream.TableAlign) string {
	if targetWidth <= visibleWidth {
		return cell
	}
	pad := targetWidth - visibleWidth
	switch align {
	case stream.TableAlignRight:
		return strings.Repeat(" ", pad) + cell
	case stream.TableAlignCenter:
		left := pad / 2
		right := pad - left
		return strings.Repeat(" ", left) + cell + strings.Repeat(" ", right)
	default:
		return cell + strings.Repeat(" ", pad)
	}
}

func (r *Renderer) inline(event stream.Event) error {
	if err := r.writeLinePrefix(); err != nil {
		return err
	}
	rendered := r.renderInline(event)
	_, err := fmt.Fprint(r.w, rendered.Text)
	if err == nil {
		r.lineWidth += rendered.Width
	}
	return err
}

func (r *Renderer) renderInline(event stream.Event) InlineRenderResult {
	if event.Inline != nil {
		if fn := r.inlineRenderers[event.Inline.Type]; fn != nil {
			if rendered, ok := fn(event); ok {
				return rendered
			}
		}
	}
	text := ""
	width := 0
	if event.Inline != nil {
		text = event.Inline.Text
		width = event.Inline.DisplayWidth
	}
	if width < 0 {
		width = r.width(text)
	}
	return InlineRenderResult{Text: styledText(r.style, r.theme, event.Style, text), Width: width}
}

func (r *Renderer) styleText(event stream.Event) string {
	return styledText(r.style, r.theme, event.Style, event.Text)
}

func styledText(st styler, theme Theme, style stream.InlineStyle, text string) string {
	if text == "" {
		return ""
	}
	if style.GetLink() != "" {
		return styledLink(st, theme, style.GetLink(), text)
	}
	var out strings.Builder
	if style.Code {
		out.WriteString(st.code(theme.Code))
	}
	if style.Emphasis {
		out.WriteString(st.code(italic))
	}
	if style.Strong {
		out.WriteString(st.code(bold))
	}
	if style.Strike {
		out.WriteString(st.code(strike))
	}
	out.WriteString(text)
	out.WriteString(st.reset())
	if style.Code || style.Emphasis || style.Strong || style.Strike {
		out.WriteString(st.code(theme.Text))
	}
	return out.String()
}

func styledLink(st styler, theme Theme, url, text string) string {
	if _, ok := st.(plainStyler); ok {
		return text
	}
	return st.code(underline) + st.code(theme.Link) + osc8Open(url) + text + osc8Close() + st.reset() + st.code(theme.Text)
}

func (r *Renderer) writeWrappedText(event stream.Event) error {
	if err := r.writeLinePrefix(); err != nil {
		return err
	}
	if r.wrapWidth <= 0 {
		_, err := fmt.Fprint(r.w, styledText(r.style, r.theme, event.Style, event.Text))
		r.lineWidth += r.width(event.Text)
		return err
	}
	text := event.Text
	for len(text) > 0 {
		remaining := r.wrapWidth - r.lineWidth
		// If the line prefix already consumed all available width,
		// emit the rest on one line to avoid degenerate one-rune-
		// per-line output in deeply nested containers.
		if remaining <= 0 {
			if _, err := fmt.Fprint(r.w, styledText(r.style, r.theme, event.Style, text)); err != nil {
				return err
			}
			r.lineWidth += r.width(text)
			return nil
		}
		segment, rest := splitWrappedText(text, remaining)
		if segment == "" {
			segment, rest = text[:firstRuneIndex(text)], text[firstRuneIndex(text):]
		}
		if _, err := fmt.Fprint(r.w, styledText(r.style, r.theme, event.Style, segment)); err != nil {
			return err
		}
		r.lineWidth += r.width(segment)
		text = rest
		if len(text) > 0 {
			if err := r.newline(); err != nil {
				return err
			}
			if err := r.writeLinePrefix(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Renderer) newline() error {
	if _, err := fmt.Fprint(r.w, "\n"); err != nil {
		return err
	}
	r.lineStart = true
	r.lineWidth = 0
	return nil
}

func visibleWidth(text string) int {
	return utf8.RuneCountInString(stripANSI(text))
}

func firstRuneIndex(text string) int {
	for i := range text {
		if i > 0 {
			return i
		}
	}
	return len(text)
}

func splitWrappedText(text string, max int) (string, string) {
	if max <= 0 || len(text) == 0 {
		return "", text
	}
	width := 0
	lastBreak := -1
	for i, r := range text {
		if width >= max {
			break
		}
		width++
		if r == ' ' || r == '\t' {
			lastBreak = i + utf8.RuneLen(r)
		}
		if width == max {
			if lastBreak > 0 && lastBreak < len(text) {
				return strings.TrimRight(text[:lastBreak], " \t"), strings.TrimLeft(text[lastBreak:], " \t")
			}
			next := i + utf8.RuneLen(r)
			if next < len(text) {
				return text[:next], text[next:]
			}
			return text, ""
		}
	}
	return text, ""
}

func (r *Renderer) codePrefix() string {
	style := normalizeCodeBlockStyle(r.codeBlock)
	var out strings.Builder
	if style.Indent > 0 {
		out.WriteString(strings.Repeat(" ", style.Indent))
	}
	if style.Border {
		if style.BorderColor != "" {
			out.WriteString(r.style.code(style.BorderColor))
		}
		out.WriteString(style.BorderText)
		if style.BorderColor != "" {
			out.WriteString(r.style.reset())
		}
	}
	if style.Padding > 0 {
		out.WriteString(strings.Repeat(" ", style.Padding))
	}
	return out.String()
}

func defaultCodeBlockStyle() CodeBlockStyle {
	return CodeBlockStyle{
		Indent:      4,
		Border:      true,
		BorderText:  "│",
		BorderColor: monokaiComment,
		Padding:     1,
	}
}

func normalizeCodeBlockStyle(style CodeBlockStyle) CodeBlockStyle {
	if style.Indent < 0 {
		style.Indent = 0
	}
	if style.Padding < 0 {
		style.Padding = 0
	}
	if style.Border && style.BorderText == "" {
		style.BorderText = "│"
	}
	return style
}

func language(info string) string {
	lang, _, _ := strings.Cut(strings.TrimSpace(info), " ")
	return strings.ToLower(lang)
}

func listMarker(event stream.Event) string {
	if event.List != nil && event.List.Ordered {
		start := event.List.Start
		if start <= 0 {
			start = 1
		}
		marker := event.List.Marker
		if marker == "" {
			marker = "."
		}
		prefix := fmt.Sprintf("%d%s ", start, marker)
		if event.List.Task {
			prefix += taskCheckbox(event.List.Checked)
		}
		return prefix
	}
	prefix := "- "
	if event.List != nil && event.List.Task {
		prefix += taskCheckbox(event.List.Checked)
	}
	return prefix
}

func visibleListMarkerWidth(marker string) int {
	return len(marker)
}

func taskCheckbox(checked bool) string {
	if checked {
		return "[x] "
	}
	return "[ ] "
}

func stripANSI(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\x1b' || i+1 >= len(s) {
			out.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '[':
			i += 2
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		case ']':
			i += 2
			for i < len(s) {
				if s[i] == '\x07' {
					break
				}
				if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '\\' {
					i++
					break
				}
				i++
			}
			continue
		}
	}
	return out.String()
}

func osc8Open(target string) string {
	// Use BEL as the OSC string terminator for broad terminal compatibility.
	return "\x1b]8;;" + target + "\a"
}

func osc8Close() string {
	return "\x1b]8;;\a"
}

// AnsiMode controls ANSI escape sequence emission.
type AnsiMode int

const (
	AnsiAuto AnsiMode = iota // auto-detect from writer (default)
	AnsiOn                   // force ANSI colour regardless of TTY
	AnsiOff                  // force plain text regardless of TTY
)

// WithAnsi sets the ANSI mode for the renderer.
// AnsiAuto (default) detects from the writer; AnsiOn forces colour;
// AnsiOff forces plain. Use AnsiOn in tests that assert escape sequences.
func WithAnsi(mode AnsiMode) RendererOption {
	return func(r *Renderer) {
		switch mode {
		case AnsiOn:
			r.style = ansiStyler{}
			r.highlighter = newHybridHighlighter(r.theme.Syntax)
		case AnsiOff:
			r.style = plainStyler{}
			r.highlighter = NewPlainHighlighter()
			// AnsiAuto: no-op, NewRenderer already set the correct pair
		}
	}
}
