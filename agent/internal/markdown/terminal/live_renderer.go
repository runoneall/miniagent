package terminal

import (
	"fmt"
	"io"
	"strings"

	"miniagent/agent/internal/markdown/stream"
)

type LiveRenderer struct {
	parser    stream.Parser
	render    *Renderer
	flushed   bool
	liveLines int
}

func NewLiveRenderer(w io.Writer, opts ...RendererOption) *LiveRenderer {
	r := NewRenderer(w, opts...)
	r.tableLayout = TableLayout{Mode: TableModeBuffered}

	return &LiveRenderer{
		parser: stream.NewParser(r.parserOptions...),
		render: r,
	}
}

func (r *LiveRenderer) Write(p []byte) (int, error) {
	if r.flushed {
		return 0, io.ErrClosedPipe
	}

	events, err := r.parser.Write(p)
	if err != nil {
		return len(p), err
	}

	if err := r.Render(events); err != nil {
		return len(p), err
	}

	return len(p), nil
}

func (r *LiveRenderer) Flush() error {
	if r.flushed {
		return nil
	}

	events, err := r.parser.Flush()
	if err != nil {
		return err
	}

	if err := r.Render(events); err != nil {
		return err
	}

	r.flushed = true
	return nil
}

func (r *LiveRenderer) Render(events []stream.Event) error {
	for _, event := range events {
		if err := r.renderEvent(event); err != nil {
			return err
		}
	}

	return nil
}

func (r *LiveRenderer) renderEvent(event stream.Event) error {
	if r.render.inTable && event.Kind == stream.EventExitBlock && event.Block == stream.BlockTable {
		r.render.inTable = false
		r.render.table = tableBuffer{}
		r.render.pending = true
		r.render.lineStart = true
		r.liveLines = 0

		return nil
	}

	if err := r.render.render(event); err != nil {
		return err
	}

	if event.Kind == stream.EventEnterBlock && event.Block == stream.BlockTable {
		r.liveLines = 0
		return nil
	}

	if r.render.inTable && event.Kind == stream.EventExitBlock && event.Block == stream.BlockTableRow {
		return r.redrawTable()
	}

	return nil
}

func (r *LiveRenderer) redrawTable() error {
	if len(r.render.table.rows) == 0 {
		return nil
	}

	var out strings.Builder
	if r.liveLines > 0 {
		fmt.Fprintf(&out, "\x1b[%dA", r.liveLines)
		out.WriteString("\x1b[J")
	}

	var tableOut strings.Builder
	tmp := *r.render
	tmp.w = &tableOut
	tmp.table = tableBuffer{
		align: append([]stream.TableAlign(nil), r.render.table.align...),
		rows:  append([]tableRow(nil), r.render.table.rows...),
	}

	tmp.lineStart = true
	if err := tmp.flushTable(); err != nil {
		return err
	}

	rendered := tableOut.String()
	out.WriteString(rendered)
	if _, err := io.WriteString(r.render.w, out.String()); err != nil {
		return err
	}

	r.liveLines = countRenderedLines(rendered)
	r.render.lineStart = true
	r.render.lineWidth = 0

	return nil
}

func countRenderedLines(s string) int {
	if s == "" {
		return 0
	}

	lines := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		lines++
	}

	return lines
}
