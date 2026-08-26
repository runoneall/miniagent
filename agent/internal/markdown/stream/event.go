package stream

type EventKind uint8

const (
	EventEnterBlock EventKind = iota + 1
	EventExitBlock
	EventText
	EventSoftBreak
	EventLineBreak
	EventInline
)

func (k EventKind) String() string {
	switch k {

	case EventEnterBlock:
		return "enter_block"

	case EventExitBlock:
		return "exit_block"

	case EventText:
		return "text"

	case EventSoftBreak:
		return "soft_break"

	case EventLineBreak:
		return "line_break"

	case EventInline:
		return "inline"

	default:
		return "unknown"

	}
}

type Position struct {
	Offset int64
	Line   int
	Column int
}

type Span struct {
	Start Position
	End   Position
}

type BlockKind uint8

const (
	BlockDocument BlockKind = iota + 1
	BlockParagraph
	BlockHeading
	BlockList
	BlockListItem
	BlockTable
	BlockTableRow
	BlockTableCell
	BlockBlockquote
	BlockFencedCode
	BlockIndentedCode
	BlockThematicBreak
	BlockHTML
)

func (k BlockKind) String() string {
	switch k {

	case BlockDocument:
		return "document"

	case BlockParagraph:
		return "paragraph"

	case BlockHeading:
		return "heading"

	case BlockList:
		return "list"

	case BlockListItem:
		return "list_item"

	case BlockTable:
		return "table"

	case BlockTableRow:
		return "table_row"

	case BlockTableCell:
		return "table_cell"

	case BlockBlockquote:
		return "blockquote"

	case BlockFencedCode:
		return "fenced_code"

	case BlockIndentedCode:
		return "indented_code"

	case BlockThematicBreak:
		return "thematic_break"

	case BlockHTML:
		return "html"

	default:
		return "unknown"

	}
}

type InlineStyle struct {
	Emphasis      bool
	Strong        bool
	Strike        bool
	Code          bool
	RawHTML       bool
	Image         bool
	EmphasisDepth int16
	StrongDepth   int16
	LinkData      *LinkData
}

type LinkData struct {
	Link           string
	LinkTitle      string
	HasLink        bool
	ImageLink      string
	ImageLinkTitle string
}

func (s InlineStyle) GetLink() string {
	if s.LinkData == nil {
		return ""
	}

	return s.LinkData.Link
}

func (s InlineStyle) GetLinkTitle() string {
	if s.LinkData == nil {
		return ""
	}

	return s.LinkData.LinkTitle
}

func (s InlineStyle) GetHasLink() bool {
	return s.LinkData != nil && s.LinkData.HasLink
}

func (s InlineStyle) GetImageLink() string {
	if s.LinkData == nil {
		return ""
	}

	return s.LinkData.ImageLink
}

func (s InlineStyle) GetImageLinkTitle() string {
	if s.LinkData == nil {
		return ""
	}

	return s.LinkData.ImageLinkTitle
}

func WithLink(link, title string) InlineStyle {
	return InlineStyle{LinkData: &LinkData{Link: link, LinkTitle: title, HasLink: true}}
}

type ListData struct {
	Ordered bool
	Start   int
	Marker  string
	Tight   bool
	Task    bool
	Checked bool
}

type TableData struct {
	Align []TableAlign
}

type TableRowData struct {
	Header bool
}

type TableAlign int

const (
	TableAlignNone TableAlign = iota
	TableAlignLeft
	TableAlignCenter
	TableAlignRight
)

type InlineData struct {
	Type         string
	Source       string
	Text         string
	DisplayWidth int
	Attrs        []Attribute
}

type Attribute struct {
	Key   string
	Value string
}

func (d *InlineData) Attr(key string) (string, bool) {
	if d == nil {
		return "", false
	}

	for _, attr := range d.Attrs {
		if attr.Key == key {
			return attr.Value, true
		}
	}

	return "", false
}

type Event struct {
	Kind     EventKind
	Block    BlockKind
	Text     string
	Style    InlineStyle
	Level    int
	Info     string
	Span     Span
	List     *ListData
	Table    *TableData
	TableRow *TableRowData
	Inline   *InlineData
}
