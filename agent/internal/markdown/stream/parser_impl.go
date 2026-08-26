package stream

import (
	"bytes"
	"html"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const releaseBufferCap = 64 * 1024

// NewParser creates an incremental Markdown parser.
func NewParser(opts ...ParserOption) Parser {
	cfg := defaultParserConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return &parser{config: cfg, line: 1, column: 1}
}

type parser struct {
	config ParserConfig

	started bool
	flushed bool

	offset int64
	line   int
	column int

	partial []byte

	paragraph paragraphState
	fence     fenceState
	refs      map[string]linkReference
	refDef    linkReferenceDefinitionState
	table     tableState

	// pendingBlocks buffers closed paragraph/heading events so that
	// link reference definitions appearing after the paragraph can be
	// collected before inline parsing runs. The pending blocks are
	// drained (inline-parsed and emitted) when a non-definition block
	// starts or at Flush.
	pendingBlocks []pendingBlock

	blockquoteDepth    int  // nesting depth of open blockquotes
	bqInsideListItem   bool // true if blockquote was opened inside a list item
	bqDepthBeforeList  int  // blockquote depth when the list was entered
	inList             bool
	listData           ListData
	listLoose          bool
	inListItem         bool
	listItemIndent     int         // content column: marker indent + marker width + padding
	listItemBlankLine  bool        // saw a blank line inside the current list item
	listItemHasContent bool        // true if the list item has had any content
	listStack          []savedList // stack for nested lists
	inIndented         bool
	indentedBlankLines []string // buffered whitespace-only lines (after indent strip)
	inHTMLBlock        bool
	htmlBlockType      int    // 1-7 per CommonMark spec
	htmlBlockEnd       string // end condition string for types 1-5

	// Reusable scratch slices for the inline pipeline, avoiding
	// repeated allocation across parseInline calls.
	inlineTokens []inlineToken
	emphOut      []inlineToken // scratch for resolveEmphasis output
	tableCells   []string      // scratch for splitTableRow
}

// savedList stores the state of an outer list when entering a sublist.
type savedList struct {
	inList             bool
	listData           ListData
	listLoose          bool
	inListItem         bool
	listItemIndent     int
	listItemBlankLine  bool
	listItemHasContent bool
}

// pendingBlock stores a closed paragraph or heading whose inline content
// has not yet been parsed. This allows forward link reference definitions
// to be collected before inline parsing resolves references.
type pendingBlock struct {
	text        string
	span        Span
	block       BlockKind // BlockParagraph or BlockHeading
	level       int       // heading level (1-6), 0 for paragraphs
	hasBrackets bool
}

type linkReference struct {
	dest  string
	title string
}

type linkReferenceDefinitionState struct {
	active       bool
	label        string
	dest         string
	hasDest      bool
	title        string
	lines        []lineInfo
	pendingTitle bool   // title opener found but not yet closed
	titleOpener  byte   // the opening quote character (' or ")
	titleBuf     string // accumulated title content so far
	pendingLabel bool   // label opener [ found but ] not yet found
	labelBuf     string // accumulated label content so far
}

type lineInfo struct {
	text       string
	start      Position
	end        Position
	nextOffset int64
}

type paragraphState struct {
	lines       []paragraphLine
	hasBrackets bool
}

type paragraphLine struct {
	text string
	span Span
}

type fenceState struct {
	open   bool
	marker byte
	length int
	indent int
	info   string
}

type tableState struct {
	active bool
	align  []TableAlign
	span   Span
}

func (p *parser) Write(chunk []byte) ([]Event, error) {
	if len(chunk) == 0 || p.flushed {
		return nil, nil
	}
	p.partial = append(p.partial, chunk...)

	// Count lines to pre-size events slice.
	newlines := bytes.Count(p.partial, []byte("\n"))
	if cr := bytes.Count(p.partial, []byte("\r")); cr > newlines {
		newlines = cr
	}
	var events []Event
	if newlines > 0 {
		events = make([]Event, 0, newlines*4)
	}
	hasCR := bytes.IndexByte(p.partial, '\r') >= 0
	for {
		// Find the next line ending.
		var i int
		if hasCR {
			i = bytes.IndexAny(p.partial, "\r\n")
		} else {
			i = bytes.IndexByte(p.partial, '\n')
		}
		if i < 0 {
			break
		}
		raw := p.partial[:i]
		advance := i + 1
		if hasCR && p.partial[i] == '\r' && i+1 < len(p.partial) && p.partial[i+1] == '\n' {
			advance = i + 2 // \r\n
		}
		info := p.nextLineInfo(string(raw), true)
		p.processLine(info, &events)
		p.partial = p.partial[advance:]
		if cap(p.partial) > releaseBufferCap && len(p.partial) == 0 {
			p.partial = nil
		}
	}
	return events, nil
}

func (p *parser) Flush() ([]Event, error) {
	if p.flushed {
		return nil, nil
	}
	events := make([]Event, 0, 32)
	if len(p.partial) > 0 {
		raw := p.partial
		if len(raw) > 0 && raw[len(raw)-1] == '\r' {
			raw = raw[:len(raw)-1]
		}
		info := p.nextLineInfo(string(raw), false)
		p.processLine(info, &events)
		p.partial = nil
	}
	p.closePendingLinkReferenceDefinition(&events)
	p.closeTable(&events)
	p.closeParagraph(&events)
	p.closeIndentedCode(&events)
	if p.fence.open {
		p.fence.open = false
		events = append(events, Event{Kind: EventExitBlock, Block: BlockFencedCode})
	}
	p.closeHTMLBlock(&events)
	p.drainPendingBlocks(&events)
	p.closeContainers(&events)
	if p.started {
		events = append(events, Event{Kind: EventExitBlock, Block: BlockDocument})
	}
	p.flushed = true
	return events, nil
}

func (p *parser) Reset() {
	*p = parser{config: p.config, line: 1, column: 1}
}

func (p *parser) nextLineInfo(text string, hadNewline bool) lineInfo {
	start := Position{Offset: p.offset, Line: p.line, Column: p.column}
	end := Position{Offset: p.offset + int64(len(text)), Line: p.line, Column: p.column + len(text)}
	nextOffset := end.Offset
	if hadNewline {
		nextOffset++
	}
	p.offset = nextOffset
	p.line++
	p.column = 1
	return lineInfo{text: text, start: start, end: end, nextOffset: nextOffset}
}

func (p *parser) processLine(line lineInfo, events *[]Event) {
	if p.table.active {
		if strings.TrimSpace(line.text) == "" {
			p.closeTable(events)
		} else if p.processActiveTableLine(line, events) {
			return
		} else {
			p.closeTable(events)
			p.processLine(line, events)
			return
		}
	}

	// HTML block continuation.
	if p.inHTMLBlock {
		// Inside a blockquote, strip the > prefix before processing.
		if p.blockquoteDepth > 0 {
			if content, ok := blockquoteContent(line.text); ok {
				inner := line
				inner.text = content
				p.processHTMLBlockLine(inner, events)
				return
			}
			// Non-> line closes the blockquote (and the HTML block inside it).
			p.closeHTMLBlock(events)
			p.closeBlockquote(line, events)
			// Fall through to process the line normally.
		} else if p.inListItem {
			// Inside a list item, check if the line starts a new list item
			// (for type 6/7 HTML blocks which end at blank lines or new containers).
			if p.htmlBlockType == 6 || p.htmlBlockType == 7 {
				if _, ok := listItem(line.text); ok {
					p.closeHTMLBlock(events)
					// Fall through to process the line normally.
				} else {
					p.processHTMLBlockLine(line, events)
					return
				}
			} else {
				p.processHTMLBlockLine(line, events)
				return
			}
		} else {
			p.processHTMLBlockLine(line, events)
			return
		}
	}

	// When inside a list item with an open fenced/indented code block,
	// route through list item continuation so that indentation is
	// interpreted relative to the item's content column.
	if p.inListItem && (p.fence.open || p.inIndented) {
		indent, _ := leadingIndent(line.text)
		if indent >= p.listItemIndent || strings.TrimSpace(line.text) == "" {
			inner := line
			if strings.TrimSpace(line.text) != "" {
				inner.text = stripIndent(line.text, p.listItemIndent)
			}
			p.processListItemContent(inner, events)
			return
		}
		// Not enough indent — close the code block and fall through.
		p.closeIndentedCode(events)
		p.closeFencedCode(events)
	}

	if p.fence.open {
		// Inside a blockquote, non-> lines close the blockquote
		// (and the fence inside it).
		if p.blockquoteDepth > 0 {
			if content, ok := blockquoteContent(line.text); !ok {
				p.closeFencedCode(events)
				p.closeBlockquote(line, events)
				// Fall through to process the line normally.
			} else {
				inner := line
				inner.text = content
				p.processFenceLine(inner, events)
				return
			}
		} else {
			p.processFenceLine(line, events)
			return
		}
	}

	if p.inIndented {
		// Inside a blockquote, non-> lines close the blockquote.
		if p.blockquoteDepth > 0 {
			if _, ok := blockquoteContent(line.text); !ok {
				p.closeIndentedCode(events)
				p.closeBlockquote(line, events)
				// Fall through to process normally.
			} else {
				if indentedCode(line.text) {
					p.emitIndentedCodeLine(line, events)
					return
				}
				p.closeIndentedCode(events)
			}
		} else {
			if indentedCode(line.text) {
				p.emitIndentedCodeLine(line, events)
				return
			}
			if strings.TrimSpace(line.text) == "" {
				p.indentedBlankLines = append(p.indentedBlankLines, stripIndentColumns(line.text, 4))
				return
			}
			p.closeIndentedCode(events)
		}
	}

	if p.refDef.active && p.blockquoteDepth == 0 {
		if p.continueLinkReferenceDefinition(line, events) {
			return
		}
	}

	if strings.TrimSpace(line.text) == "" {
		p.closeParagraph(events)
		p.closeIndentedCode(events)
		if p.inListItem {
			// Don't close the list item yet — the next non-blank line
			// may be a continuation if it's indented enough.
			p.listItemBlankLine = true
		} else {
			p.closeListItem(events)
			p.closeContainers(events)
		}
		// Drain pending blocks that don't contain bracket syntax,
		// since they can't benefit from deferred reference resolution.
		// Blocks with brackets are kept pending so that link reference
		// definitions following the blank line can be collected first.
		p.drainPendingBlocksEager(events)
		return
	}

	// Non-blank line: check for list item continuation after a blank line.
	if p.inListItem && p.listItemBlankLine {
		p.listItemBlankLine = false
		indent, _ := leadingIndent(line.text)
		// When inside a blockquote, strip the > prefixes first to
		// check the content's indent against the list item indent.
		checkText := line.text
		if p.blockquoteDepth > 0 {
			tmp := line.text
			for d := 0; d < p.blockquoteDepth; d++ {
				if c, ok := blockquoteContent(tmp); ok {
					tmp = c
				} else {
					break
				}
			}
			checkText = tmp
			indent, _ = leadingIndent(checkText)
		}
		if indent >= p.listItemIndent && p.listItemHasContent {
			// Check if the stripped content starts a blockquote inside
			// an existing blockquote context — if so, let the blockquote
			// handler deal with it rather than treating as list content.
			stripped := stripIndent(line.text, p.listItemIndent)
			isBQContinuation := false
			if p.blockquoteDepth > 0 {
				_, isBQContinuation = blockquoteContent(stripped)
			}
			if !isBQContinuation {
				// Continuation after blank line — the list becomes loose.
				p.listLoose = true
				inner := line
				inner.text = stripped
				p.processListItemContent(inner, events)
				return
			}
			// Blockquote content after blank line — fall through to
			// let the blockquote handler process it. Restore the
			// blank line flag so the BQ handler can close the item.
			p.listItemBlankLine = true
		} else {
			// Not enough indent for current (inner) item.
			// Close the inner item and unwind sublists.
			p.closeParagraph(events)
			p.drainPendingBlocks(events)
			p.inListItem = false
			*events = append(*events, Event{Kind: EventExitBlock, Block: BlockListItem})
			closeSublist := false
			if len(p.listStack) > 0 {
				outerIndent := p.listStack[len(p.listStack)-1].listItemIndent
				if indent < outerIndent {
					closeSublist = true
				} else {
					stripped := stripIndent(line.text, outerIndent)
					if sItem, ok := listItem(stripped); ok && p.listData.Ordered == sItem.data.Ordered && p.listData.Marker == sItem.data.Marker {
						_ = sItem
					} else {
						closeSublist = true
					}
				}
			} else if _, ok := listItem(line.text); !ok {
				data := p.listData
				data.Tight = !p.listLoose
				p.inList = false
				*events = append(*events, Event{Kind: EventExitBlock, Block: BlockList, List: &data})
				p.listData = ListData{}
				p.listLoose = false
			} else {
				p.listLoose = true
			}
			if closeSublist {
				data := p.listData
				data.Tight = !p.listLoose
				p.inList = false
				*events = append(*events, Event{Kind: EventExitBlock, Block: BlockList, List: &data})
				p.listData = ListData{}
				p.listLoose = false
				p.popList()
				p.listLoose = true
			}
			// After unwinding, check if the line continues an outer item.
			if p.inListItem {
				indent2, _ := leadingIndent(line.text)
				if indent2 >= p.listItemIndent {
					inner := line
					inner.text = stripIndent(line.text, p.listItemIndent)
					p.processListItemContent(inner, events)
					return
				}
			}
		}
	}

	if len(p.paragraph.lines) == 0 && !p.inList && !p.inListItem {
		if p.startLinkReferenceDefinition(line) {
			return
		}
	}

	// If we reach here, the line is non-blank and not a ref def continuation.
	// Drain pending blocks that can't benefit from forward references.
	// If the line starts a blockquote, it may contain link reference
	// definitions that pending blocks with brackets need, so only do
	// an eager drain in that case.
	if _, isBQ := blockquoteContent(line.text); isBQ && len(p.pendingBlocks) > 0 {
		p.drainPendingBlocksEager(events)
	} else {
		p.drainPendingBlocks(events)
	}

	// When inside a blockquote and the line doesn't continue it,
	// close the blockquote before checking list item continuation.
	// Lists opened inside the blockquote are closed with it, so
	// they must not be treated as continuation targets.
	if p.blockquoteDepth > 0 && !p.fence.open && !p.inIndented {
		if _, ok := blockquoteContent(line.text); !ok {
			// Lazy continuation: a paragraph open inside the blockquote
			// can continue without the > prefix.
			if len(p.paragraph.lines) > 0 && !thematicBreak(line.text) {
				if _, _, _, _, isFence := openingFence(line.text); !isFence {
					if _, ok2 := listItem(line.text); !ok2 {
						p.addParagraphLine(line)
						return
					}
				}
			}
			p.closeBlockquote(line, events)
		}
	}

	// When inside a list item, check if the line is indented enough
	// to be continuation content. This must come before blockquote
	// and other container checks so that indented `>` markers inside
	// list items are treated as list item content, not top-level
	// blockquotes.
	// Skip this when a blank line was seen and the content is a
	// blockquote line — let the blockquote handler close the item.
	if p.inListItem && !p.fence.open && !p.inIndented && !(p.listItemBlankLine && p.blockquoteDepth > 0) {
		indent, _ := leadingIndent(line.text)
		// Unwind sublists that don't match.
		for p.inListItem && indent < p.listItemIndent && len(p.listStack) > 0 {
			p.closeParagraph(events)
			p.drainPendingBlocks(events)
			p.closeIndentedCode(events)
			p.closeFencedCode(events)
			p.inListItem = false
			*events = append(*events, Event{Kind: EventExitBlock, Block: BlockListItem})
			// Check for sibling in the sublist. The line must have
			// enough indent to be at the sublist's level (i.e., at
			// least the outer item's content indent).
			outerItemIndent := p.listStack[len(p.listStack)-1].listItemIndent
			if indent >= outerItemIndent {
				if item, ok := listItem(line.text); ok {
					stripped := stripIndent(line.text, outerItemIndent)
					if sItem, ok2 := listItem(stripped); ok2 && p.listData.Ordered == sItem.data.Ordered && p.listData.Marker == sItem.data.Marker {
						_ = item
						p.inListItem = true
						p.listItemIndent = outerItemIndent + sItem.contentIndent
						p.listItemBlankLine = false
						data := sItem.data
						*events = append(*events, Event{Kind: EventEnterBlock, Block: BlockListItem, List: &data, Span: Span{Start: line.start, End: line.end}})
						if strings.TrimSpace(sItem.content) != "" {
							inner := line
							inner.text = sItem.content
							p.processListItemFirstLine(inner, events)
						}
						return
					}
				}
			}
			// Not a sibling — close the sublist.
			p.inList = false
			data := p.listData
			data.Tight = !p.listLoose
			*events = append(*events, Event{Kind: EventExitBlock, Block: BlockList, List: &data})
			p.listData = ListData{}
			p.listLoose = false
			p.popList()
		}
		if p.inListItem && indent >= p.listItemIndent {
			inner := line
			inner.text = stripIndent(line.text, p.listItemIndent)
			p.processListItemContent(inner, events)
			return
		}
		// Lazy continuation: if a paragraph is open inside the list
		// item, a non-blank line continues it even without enough indent.
		// But not if the line is a blockquote continuation — let the
		// blockquote handler strip the > prefix first.
		if p.inListItem && len(p.paragraph.lines) > 0 && !thematicBreak(line.text) {
			if _, ok := listItem(line.text); !ok {
				if _, isBQ := blockquoteContent(line.text); !isBQ || p.blockquoteDepth == 0 {
					p.addParagraphLine(line)
					return
				}
			}
		}
	}

	if content, ok := blockquoteContent(line.text); ok {
		p.ensureDocument(events)
		// Count total nesting depth of > prefixes on this line.
		newDepth := 1
		inner := content
		for {
			nested, ok := blockquoteContent(inner)
			if !ok {
				break
			}
			newDepth++
			inner = nested
		}
		content = inner
		if p.blockquoteDepth == 0 {
			p.closeParagraph(events)
			// If the blockquote content is a link reference definition,
			// collect it before draining pending blocks so that forward
			// references resolve correctly.
			if strings.TrimSpace(content) != "" {
				if label, text, rest, startOK := parseLinkReferenceDefinitionStart(content); startOK {
					dest, _, hasDest, _, _, tailOK := parseLinkReferenceDefinitionTail(text, rest)
					if tailOK && hasDest {
						if p.refs == nil {
							p.refs = make(map[string]linkReference)
						}
						if _, exists := p.refs[label]; !exists {
							p.refs[label] = linkReference{dest: dest}
						}
					}
				}
			}
			p.drainPendingBlocks(events)
			p.closeList(events)
			p.bqInsideListItem = p.inListItem
		}
		// Lazy continuation: if the line has fewer > prefixes than
		// the current depth but a paragraph is open, continue it.
		if newDepth < p.blockquoteDepth && len(p.paragraph.lines) > 0 {
			bqInner := line
			bqInner.text = content
			p.addParagraphLine(bqInner)
			return
		}
		// Open new blockquote levels as needed.
		for p.blockquoteDepth < newDepth {
			if p.blockquoteDepth > 0 {
				p.closeParagraph(events)
				p.closeList(events)
			}
			p.blockquoteDepth++
			*events = append(*events, Event{Kind: EventEnterBlock, Block: BlockBlockquote, Span: Span{Start: line.start, End: line.end}})
		}
		if strings.TrimSpace(content) == "" {
			if p.refDef.active {
				bqBlank := line
				bqBlank.text = ""
				p.continueLinkReferenceDefinition(bqBlank, events)
			}
			p.closeParagraph(events)
			p.closeIndentedCode(events)
			// Propagate blank line to list item inside blockquote.
			if p.inListItem {
				p.listItemBlankLine = true
			}
			return
		}
		bqInner := line
		bqInner.text = content
		// Route blockquote content through full block detection
		// so that list items, fenced code, headings, etc. work
		// inside blockquotes.
		if p.fence.open {
			if p.isClosingFence(bqInner.text) {
				p.fence.open = false
				*events = append(*events, Event{Kind: EventExitBlock, Block: BlockFencedCode, Span: Span{Start: bqInner.start, End: bqInner.end}})
				return
			}
			*events = append(*events,
				Event{Kind: EventText, Text: stripFenceIndent(bqInner.text, p.fence.indent), Span: Span{Start: bqInner.start, End: bqInner.end}},
				Event{Kind: EventLineBreak, Span: Span{Start: bqInner.end, End: bqInner.end}},
			)
			return
		}
		if p.inIndented {
			if indentedCode(bqInner.text) {
				p.emitIndentedCodeLine(bqInner, events)
				return
			}
			p.closeIndentedCode(events)
		}
		// Ref def continuation inside blockquote.
		if p.refDef.active {
			if p.continueLinkReferenceDefinition(bqInner, events) {
				return
			}
		}
		// List item continuation inside blockquote.
		if p.inListItem {
			bqIndent, _ := leadingIndent(bqInner.text)
			if p.listItemBlankLine {
				p.listItemBlankLine = false
				if bqIndent >= p.listItemIndent && p.listItemHasContent {
					p.listLoose = true
					ci := bqInner
					ci.text = stripIndent(bqInner.text, p.listItemIndent)
					p.processListItemContent(ci, events)
					return
				}
				// Not enough indent after blank line — close the list item.
				p.closeListItem(events)
				p.closeList(events)
			} else if bqIndent >= p.listItemIndent {
				ci := bqInner
				ci.text = stripIndent(bqInner.text, p.listItemIndent)
				p.processListItemContent(ci, events)
				return
			}
		}
		if item, ok := listItem(bqInner.text); ok {
			p.closeParagraph(events)
			if !p.inList || p.listData.Ordered != item.data.Ordered || p.listData.Marker != item.data.Marker {
				p.closeList(events)
				p.inList = true
				p.listData = listBlockData(item.data)
				p.listLoose = false
				data := p.listData
				*events = append(*events, Event{Kind: EventEnterBlock, Block: BlockList, List: &data, Span: Span{Start: bqInner.start, End: bqInner.end}})
			}
			p.closeListItem(events)
			p.inListItem = true
			p.listItemIndent = item.contentIndent
			p.listItemBlankLine = false
			p.listItemHasContent = strings.TrimSpace(item.content) != ""
			data := item.data
			*events = append(*events, Event{Kind: EventEnterBlock, Block: BlockListItem, List: &data, Span: Span{Start: bqInner.start, End: bqInner.end}})
			if p.listItemHasContent {
				ci := bqInner
				ci.text = item.content
				p.processListItemFirstLine(ci, events)
			}
			return
		}
		// Link reference definition inside blockquote.
		if len(p.paragraph.lines) == 0 && !p.inList && !p.inListItem {
			if p.startLinkReferenceDefinition(bqInner) {
				return
			}
		}
		p.processNonContainerLine(bqInner, events)
		return
	}

	if p.blockquoteDepth > 0 {
		if len(p.paragraph.lines) > 0 && !thematicBreak(line.text) {
			if _, _, _, _, isFence := openingFence(line.text); !isFence {
				if _, ok := listItem(line.text); !ok {
					p.addParagraphLine(line)
					return
				}
			}
		}
		p.closeBlockquote(line, events)
	}

	if level, ok := setextHeading(line.text); ok && len(p.paragraph.lines) > 0 && !p.inList && !p.inListItem {
		p.closeSetextHeading(level, Span{Start: line.start, End: line.end}, events)
		return
	}

	if thematicBreak(line.text) {
		p.ensureDocument(events)
		p.closeParagraph(events)
		p.closeListItem(events)
		p.closeList(events)
		p.emitThematicBreak(line, events)
		return
	}

	if item, ok := listItem(line.text); ok {
		p.ensureDocument(events)
		// An empty list item cannot interrupt a paragraph (CommonMark §5.3).
		// Also, an ordered list with start != 1 cannot interrupt a paragraph.
		if !p.inList && len(p.paragraph.lines) > 0 {
			if (item.data.Ordered && item.data.Start != 1) || strings.TrimSpace(item.content) == "" {
				p.addParagraphLine(line)
				return
			}
		}
		p.closeParagraph(events)
		if !p.inList || p.listData.Ordered != item.data.Ordered || p.listData.Marker != item.data.Marker {
			p.closeList(events)
			p.inList = true
			p.listData = listBlockData(item.data)
			p.listLoose = false
			data := p.listData
			p.emitBlockStart(events, Event{Kind: EventEnterBlock, Block: BlockList, List: &data, Span: Span{Start: line.start, End: line.end}})
		}
		p.closeListItem(events)
		p.inListItem = true
		p.listItemIndent = item.contentIndent
		p.listItemBlankLine = false
		p.listItemHasContent = strings.TrimSpace(item.content) != ""
		data := item.data
		p.emitBlockStart(events, Event{Kind: EventEnterBlock, Block: BlockListItem, List: &data, Span: Span{Start: line.start, End: line.end}})
		if p.listItemHasContent {
			inner := line
			inner.text = item.content
			p.processListItemFirstLine(inner, events)
		}
		return
	}

	if p.inListItem {
		// Not enough indent and not a list item — close list.
		p.closeParagraph(events)
		p.closeListItem(events)
		p.closeList(events)
	} else if p.inList {
		if p.tryStartTable(line, events) {
			return
		}
		p.closeParagraph(events)
		p.closeListItem(events)
		p.closeList(events)
	}

	p.processNonContainerLine(line, events)
}

func (p *parser) processNonContainerLine(line lineInfo, events *[]Event) {
	if p.tryStartTable(line, events) {
		return
	}
	if marker, n, indent, info, ok := openingFence(line.text); ok {
		p.ensureDocument(events)
		p.closeParagraph(events)
		p.closeIndentedCode(events)
		p.fence = fenceState{open: true, marker: marker, length: n, indent: indent, info: info}
		p.emitBlockStart(events, Event{Kind: EventEnterBlock, Block: BlockFencedCode, Info: info, Span: Span{Start: line.start, End: line.end}})
		return
	}

	// HTML block detection (types 1-6 can interrupt paragraphs,
	// type 7 cannot).
	if htmlType, htmlEnd := detectHTMLBlockStart(line.text); htmlType > 0 {
		if htmlType <= 6 || len(p.paragraph.lines) == 0 {
			p.ensureDocument(events)
			p.closeParagraph(events)
			p.closeIndentedCode(events)
			p.emitBlockStart(events, Event{Kind: EventEnterBlock, Block: BlockHTML, Span: Span{Start: line.start, End: line.end}})
			p.inHTMLBlock = true
			p.htmlBlockType = htmlType
			p.htmlBlockEnd = htmlEnd
			p.processHTMLBlockLine(line, events)
			return
		}
	}

	if level, text, ok := heading(line.text); ok {
		p.ensureDocument(events)
		p.closeParagraph(events)
		span := Span{Start: line.start, End: line.end}
		// Defer heading inline parsing so forward link reference
		// definitions can be collected first.
		if strings.ContainsAny(text, "[]") {
			p.pendingBlocks = append(p.pendingBlocks, pendingBlock{
				text:        text,
				span:        span,
				block:       BlockHeading,
				level:       level,
				hasBrackets: true,
			})
		} else {
			p.drainPendingBlocks(events)
			*events = append(*events, Event{Kind: EventEnterBlock, Block: BlockHeading, Level: level, Span: span})
			p.parseInline(text, span, events)
			*events = append(*events, Event{Kind: EventExitBlock, Block: BlockHeading, Level: level, Span: span})
		}
		return
	}

	if thematicBreak(line.text) {
		p.ensureDocument(events)
		p.closeParagraph(events)
		p.emitThematicBreak(line, events)
		return
	}

	if indentedCode(line.text) {
		if len(p.paragraph.lines) > 0 {
			inner := line
			inner.text = strings.TrimLeft(line.text, " \t")
			p.addParagraphLine(inner)
			return
		}
		p.ensureDocument(events)
		p.closeParagraph(events)
		if !p.inIndented {
			p.inIndented = true
			p.emitBlockStart(events, Event{Kind: EventEnterBlock, Block: BlockIndentedCode, Span: Span{Start: line.start, End: line.end}})
		}
		p.emitIndentedCodeLine(line, events)
		return
	}

	p.addParagraphLine(line)
}

func (p *parser) ensureDocument(events *[]Event) {
	if p.started {
		return
	}
	p.started = true
	*events = append(*events, Event{Kind: EventEnterBlock, Block: BlockDocument})
}

func (p *parser) addParagraphLine(line lineInfo) {
	text := line.text
	if len(p.paragraph.lines) == 0 {
		if indent, bytes := leadingIndent(text); indent <= 3 {
			text = text[bytes:]
		}
	} else {
		text = strings.TrimLeft(text, " \t")
	}
	if !p.paragraph.hasBrackets && strings.ContainsAny(text, "[]") {
		p.paragraph.hasBrackets = true
	}
	p.paragraph.lines = append(p.paragraph.lines, paragraphLine{
		text: text,
		span: Span{Start: line.start, End: line.end},
	})
}

func (p *parser) closeParagraph(events *[]Event) {
	if len(p.paragraph.lines) == 0 {
		return
	}
	p.ensureDocument(events)
	start := p.paragraph.lines[0].span.Start
	end := p.paragraph.lines[len(p.paragraph.lines)-1].span.End
	span := Span{Start: start, End: end}
	if !p.paragraph.hasBrackets && len(p.pendingBlocks) == 0 {
		*events = append(*events, Event{Kind: EventEnterBlock, Block: BlockParagraph, Span: span})
		if canEmitPlainParagraphLines(p.paragraph.lines, p.config.GFMAutolinks, p.config.InlineScanners) {
			emitPlainParagraphLines(p.paragraph.lines, span, events)
		} else {
			p.parseInline(paragraphText(p.paragraph.lines), span, events)
		}
		*events = append(*events, Event{Kind: EventExitBlock, Block: BlockParagraph, Span: span})
	} else {
		p.pendingBlocks = append(p.pendingBlocks, pendingBlock{
			text:        paragraphText(p.paragraph.lines),
			span:        span,
			block:       BlockParagraph,
			hasBrackets: p.paragraph.hasBrackets,
		})
	}
	p.clearParagraphLines()
}

func (p *parser) clearParagraphLines() {
	clear(p.paragraph.lines)
	p.paragraph.hasBrackets = false
	if cap(p.paragraph.lines) > 1024 {
		p.paragraph.lines = nil
	} else {
		p.paragraph.lines = p.paragraph.lines[:0]
	}
}

func paragraphText(lines []paragraphLine) string {
	if len(lines) == 1 {
		return lines[0].text
	}
	length := len(lines) - 1 // inserted newlines
	for _, line := range lines {
		length += len(line.text)
	}
	var text strings.Builder
	text.Grow(length)
	for i, line := range lines {
		if i > 0 {
			text.WriteByte('\n')
		}
		text.WriteString(line.text)
	}
	return text.String()
}

func canEmitPlainParagraphLines(lines []paragraphLine, gfmAutolinks bool, scanners []InlineScanner) bool {
	for i, line := range lines {
		if hasInlineSyntax(line.text, gfmAutolinks, scanners) {
			return false
		}
		if i < len(lines)-1 && hasHardBreakSuffix(line.text) {
			return false
		}
	}
	return true
}

func hasHardBreakSuffix(text string) bool {
	if strings.HasSuffix(text, "\\") {
		return true
	}
	spaces := 0
	for i := len(text) - 1; i >= 0 && text[i] == ' '; i-- {
		spaces++
		if spaces >= 2 {
			return true
		}
	}
	return false
}

func emitPlainParagraphLines(lines []paragraphLine, span Span, events *[]Event) {
	for i, line := range lines {
		if i > 0 {
			*events = append(*events, Event{Kind: EventSoftBreak, Span: span})
		}
		*events = append(*events, Event{Kind: EventText, Text: line.text, Span: span})
	}
}

// drainPendingBlocks inline-parses and emits all buffered paragraph/heading
// blocks. Called before emitting any non-definition block and at Flush, so
// that forward link reference definitions are available for resolution.
func (p *parser) drainPendingBlocks(events *[]Event) {
	for _, pb := range p.pendingBlocks {
		*events = append(*events, Event{Kind: EventEnterBlock, Block: pb.block, Level: pb.level, Span: pb.span})
		p.parseInline(pb.text, pb.span, events)
		*events = append(*events, Event{Kind: EventExitBlock, Block: pb.block, Level: pb.level, Span: pb.span})
	}
	p.pendingBlocks = p.pendingBlocks[:0]
}

// drainPendingBlocksEager emits pending blocks that don't contain bracket
// syntax (and thus can't benefit from deferred reference resolution).
// Blocks with brackets are kept pending for forward reference resolution.
func (p *parser) drainPendingBlocksEager(events *[]Event) {
	// In-place filter: kept shares the backing array with p.pendingBlocks.
	// This is safe because the kept write index never advances past the
	// read index — every iteration either keeps or emits the element.
	kept := p.pendingBlocks[:0]
	for _, pb := range p.pendingBlocks {
		if pb.hasBrackets {
			kept = append(kept, pb)
			continue
		}
		*events = append(*events, Event{Kind: EventEnterBlock, Block: pb.block, Level: pb.level, Span: pb.span})
		p.parseInline(pb.text, pb.span, events)
		*events = append(*events, Event{Kind: EventExitBlock, Block: pb.block, Level: pb.level, Span: pb.span})
	}
	p.pendingBlocks = kept
}

// emitBlockStart drains any pending paragraph/heading blocks before emitting
// a new structural block. This ensures forward link reference definitions
// collected after a pending paragraph are available for inline resolution.
func (p *parser) emitBlockStart(events *[]Event, ev Event) {
	p.drainPendingBlocks(events)
	*events = append(*events, ev)
}

func (p *parser) closeSetextHeading(level int, underline Span, events *[]Event) {
	if len(p.paragraph.lines) == 0 {
		return
	}
	p.ensureDocument(events)
	p.drainPendingBlocks(events)
	start := p.paragraph.lines[0].span.Start
	end := underline.End
	span := Span{Start: start, End: end}
	*events = append(*events, Event{Kind: EventEnterBlock, Block: BlockHeading, Level: level, Span: span})
	p.parseInline(paragraphText(p.paragraph.lines), span, events)
	*events = append(*events, Event{Kind: EventExitBlock, Block: BlockHeading, Level: level, Span: span})
	p.clearParagraphLines()
}

func (p *parser) tryStartTable(line lineInfo, events *[]Event) bool {
	if p.table.active || len(p.paragraph.lines) == 0 {
		return false
	}
	// The table header is the last paragraph line. Check it before parsing the
	// separator so ordinary paragraphs avoid separator split/allocation work.
	header := p.paragraph.lines[len(p.paragraph.lines)-1]
	var headerHasPipe bool
	p.tableCells, headerHasPipe = splitTableRowReuse(header.text, p.tableCells[:0])
	headerCells := p.tableCells
	if !headerHasPipe {
		return false
	}
	align, ok := parseTableSeparator(line.text)
	if !ok {
		return false
	}
	// Reject degenerate headers where all cells are empty (e.g. just "|").
	allEmpty := true
	for _, c := range headerCells {
		if c != "" {
			allEmpty = false
			break
		}
	}
	if allEmpty {
		return false
	}
	// GFM spec: delimiter column count must match header column count.
	if len(headerCells) != len(align) {
		return false
	}

	p.ensureDocument(events)
	p.closeIndentedCode(events)
	// If the paragraph had lines before the header, emit them as a paragraph.
	if len(p.paragraph.lines) > 1 {
		prevLines := p.paragraph.lines[:len(p.paragraph.lines)-1]
		p.paragraph.lines = prevLines
		p.closeParagraph(events)
	} else {
		p.clearParagraphLines()
	}

	tableAlign := append([]TableAlign(nil), align...)
	p.table = tableState{
		active: true,
		align:  tableAlign,
		span:   Span{Start: header.span.Start, End: line.end},
	}
	p.emitBlockStart(events, Event{
		Kind:  EventEnterBlock,
		Block: BlockTable,
		Table: &TableData{Align: append([]TableAlign(nil), tableAlign...)},
		Span:  p.table.span,
	})
	headerLine := lineInfo{text: header.text, start: header.span.Start, end: header.span.End}
	p.emitTableRow(headerLine.text, headerLine, events, true)
	return true
}

func (p *parser) processActiveTableLine(line lineInfo, events *[]Event) bool {
	if !p.table.active {
		return false
	}
	// Lines that start a new block-level construct end the table.
	if startsNewBlock(line.text) {
		return false
	}
	p.tableCells, _ = splitTableRowReuse(line.text, p.tableCells[:0])
	if len(p.tableCells) == 0 {
		return false
	}
	p.table.span.End = line.end
	p.emitTableRow(line.text, line, events, false)
	return true
}

// startsNewBlock returns true if the line would start a block-level
// construct that should interrupt a table.
func startsNewBlock(text string) bool {
	s := strings.TrimLeft(text, " \t")
	if s == "" {
		return false
	}
	switch s[0] {
	case '>':
		return true // blockquote
	case '#':
		// ATX heading: # followed by space or end of line.
		for i := 0; i < len(s) && i < 6; i++ {
			if s[i] != '#' {
				return i > 0 && (s[i] == ' ' || s[i] == '\t')
			}
		}
		return len(s) <= 6 || s[6] == ' ' || s[6] == '\t'
	case '-', '*', '_':
		// Thematic break: 3+ of the same char with optional spaces.
		if thematicBreak(s) {
			return true
		}
		// List item: marker followed by space.
		if (s[0] == '-' || s[0] == '*') && len(s) > 1 && (s[1] == ' ' || s[1] == '\t') {
			return true
		}
		return false
	case '+':
		return len(s) > 1 && (s[1] == ' ' || s[1] == '\t') // list item
	case '`', '~':
		// Fenced code: 3+ backticks or tildes.
		if len(s) >= 3 && (s[:3] == "```" || s[:3] == "~~~") {
			return true
		}
		return false
	case '<':
		// HTML block.
		return true
	case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		// Ordered list item: digits followed by . or ) and space.
		for i := 0; i < len(s) && i < 10; i++ {
			if s[i] >= '0' && s[i] <= '9' {
				continue
			}
			if (s[i] == '.' || s[i] == ')') && i > 0 && i+1 < len(s) && (s[i+1] == ' ' || s[i+1] == '\t') {
				return true
			}
			break
		}
		return false
	default:
		return false
	}
}

func (p *parser) emitTableRow(text string, line lineInfo, events *[]Event, header bool) {
	rowSpan := Span{Start: line.start, End: line.end}
	var rowData *TableRowData
	if header {
		rowData = &TableRowData{Header: true}
	}
	*events = append(*events, Event{Kind: EventEnterBlock, Block: BlockTableRow, Span: rowSpan, TableRow: rowData})
	p.tableCells, _ = splitTableRowReuse(text, p.tableCells[:0])
	for _, cell := range p.tableCells {
		cellSpan := Span{Start: line.start, End: line.end}
		*events = append(*events, Event{Kind: EventEnterBlock, Block: BlockTableCell, Span: cellSpan})
		p.parseInline(cell, cellSpan, events)
		*events = append(*events, Event{Kind: EventExitBlock, Block: BlockTableCell, Span: cellSpan})
	}
	*events = append(*events, Event{Kind: EventExitBlock, Block: BlockTableRow, Span: rowSpan})
}

func (p *parser) closeTable(events *[]Event) {
	if !p.table.active {
		return
	}
	span := p.table.span
	align := append([]TableAlign(nil), p.table.align...)
	p.table = tableState{}
	*events = append(*events, Event{Kind: EventExitBlock, Block: BlockTable, Table: &TableData{Align: align}, Span: span})
}

func parseTableSeparator(line string) ([]TableAlign, bool) {
	cells, hasPipe := splitTableRow(line)
	if !hasPipe || len(cells) == 0 {
		return nil, false
	}
	align := make([]TableAlign, 0, len(cells))
	for _, cell := range cells {
		a, ok := parseTableAlign(cell)
		if !ok {
			return nil, false
		}
		align = append(align, a)
	}
	return align, true
}

func parseTableAlign(cell string) (TableAlign, bool) {
	cell = strings.TrimSpace(cell)
	if len(cell) == 0 {
		return TableAlignNone, false
	}
	left := strings.HasPrefix(cell, ":")
	right := strings.HasSuffix(cell, ":")
	if left {
		cell = cell[1:]
	}
	if right && len(cell) > 0 {
		cell = cell[:len(cell)-1]
	}
	// GFM spec requires at least one dash in the delimiter cell.
	if len(cell) == 0 {
		return TableAlignNone, false
	}
	for i := 0; i < len(cell); i++ {
		if cell[i] != '-' {
			return TableAlignNone, false
		}
	}
	switch {
	case left && right:
		return TableAlignCenter, true
	case left:
		return TableAlignLeft, true
	case right:
		return TableAlignRight, true
	default:
		return TableAlignNone, true
	}
}

func splitTableRow(line string) ([]string, bool) {
	return splitTableRowReuse(line, nil)
}

func splitTableRowReuse(line string, cells []string) ([]string, bool) {
	s := strings.TrimSpace(line)
	if s == "" {
		return nil, false
	}
	hasPipe := false
	if s[0] == '|' {
		hasPipe = true
		s = s[1:]
	}
	if len(s) > 0 && s[len(s)-1] == '|' {
		hasPipe = true
		s = s[:len(s)-1]
	}
	start := 0
	escaped := false
	codeRun := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			continue
		}
		if c == '`' {
			run := countRun(s[i:], '`')
			if codeRun == 0 {
				codeRun = run
			} else if run == codeRun {
				codeRun = 0
			}
			i += run - 1
			continue
		}
		if c == '|' && codeRun == 0 {
			cells = append(cells, strings.TrimSpace(s[start:i]))
			start = i + 1
			hasPipe = true
		}
	}
	cells = append(cells, strings.TrimSpace(s[start:]))
	// Unescape \| → | in cell content (GFM table pipe escaping).
	for i, cell := range cells {
		if strings.Contains(cell, "\\|") {
			cells[i] = strings.ReplaceAll(cell, "\\|", "|")
		}
	}
	return cells, hasPipe
}

func (p *parser) startLinkReferenceDefinition(line lineInfo) bool {
	// Try multiline label: [\nfoo\n]: /url
	label, text, rest, ok := parseLinkReferenceDefinitionStart(line.text)
	if !ok {
		// Check for multiline label start: line starts with [ but no ] on this line.
		if p.tryStartMultilineLabel(line) {
			return true
		}
		return false
	}
	state := linkReferenceDefinitionState{
		active: true,
		label:  label,
		lines:  []lineInfo{line},
	}
	dest, title, hasDest, hasTitle, pending, ok := parseLinkReferenceDefinitionTail(text, rest)
	if !ok {
		// Check for multiline title: dest found but title opener without closer.
		if state.label != "" {
			// Re-parse to find dest and check for pending title.
			i := skipMarkdownSpace(text, rest)
			if i < len(text) {
				d, next, dok := parseInlineLinkDestination(text, i)
				if dok && next > i {
					spaceAfterDest := skipMarkdownSpace(text, next)
					if spaceAfterDest > next {
						if opener, content, pok := detectPendingTitle(text, spaceAfterDest); pok {
							state.dest = d
							state.hasDest = true
							state.pendingTitle = true
							state.titleOpener = opener
							state.titleBuf = content
							p.refDef = state
							return true
						}
					}
				}
			}
		}
		return false
	}
	state.dest = dest
	state.hasDest = hasDest
	state.title = title
	p.refDef = state
	if hasTitle {
		p.finishLinkReferenceDefinition()
		return true
	}
	if pending {
		return true
	}
	p.finishLinkReferenceDefinition()
	return true
}

// tryStartMultilineLabel checks if a line starts a link reference definition
// with a label that spans multiple lines, e.g. "[\nfoo\n]: /url".
func (p *parser) tryStartMultilineLabel(line lineInfo) bool {
	indent, indentBytes := leadingIndent(line.text)
	if indent > 3 {
		return false
	}
	text := line.text[indentBytes:]
	if len(text) == 0 || text[0] != '[' {
		return false
	}
	// Check that there's no ] on this line (otherwise parseLinkReferenceDefinitionStart
	// would have handled it).
	for i := 1; i < len(text); i++ {
		if text[i] == '\\' && i+1 < len(text) {
			i++ // skip escaped char
			continue
		}
		if text[i] == '[' {
			return false // nested [ not allowed in labels
		}
		if text[i] == ']' {
			return false // ] found on same line
		}
	}
	p.refDef = linkReferenceDefinitionState{
		active:       true,
		pendingLabel: true,
		labelBuf:     text[1:], // content after [
		lines:        []lineInfo{line},
	}
	return true
}

func (p *parser) continueLinkReferenceDefinition(line lineInfo, events *[]Event) bool {
	// Handle pending multiline label: accumulate until ] found.
	if p.refDef.pendingLabel {
		return p.continueMultilineLabel(line, events)
	}

	// Handle pending multiline title: accumulate until closer found.
	if p.refDef.pendingTitle {
		return p.continueMultilineTitle(line, events)
	}

	if strings.TrimSpace(line.text) == "" {
		if !p.refDef.hasDest {
			p.failLinkReferenceDefinition()
		} else {
			p.finishLinkReferenceDefinition()
		}
		return false
	}

	if !p.refDef.hasDest {
		dest, title, hasDest, hasTitle, pending, ok := parseLinkReferenceDefinitionTail(line.text, 0)
		if !ok || !hasDest {
			p.failLinkReferenceDefinition()
			return false
		}
		p.refDef.lines = append(p.refDef.lines, line)
		p.refDef.dest = dest
		p.refDef.hasDest = hasDest
		p.refDef.title = title
		if hasTitle {
			p.finishLinkReferenceDefinition()
			return true
		}
		if pending {
			return true
		}
		p.finishLinkReferenceDefinition()
		return true
	}

	title, next, ok := parseInlineLinkTitle(line.text, skipMarkdownSpace(line.text, 0))
	if !ok {
		p.finishLinkReferenceDefinition()
		return false
	}
	if skipMarkdownSpace(line.text, next) != len(line.text) {
		p.finishLinkReferenceDefinition()
		return false
	}
	p.refDef.lines = append(p.refDef.lines, line)
	p.refDef.title = title
	p.finishLinkReferenceDefinition()
	return true
}

// continueMultilineLabel handles continuation lines for a label that
// started with [ on a previous line.
func (p *parser) continueMultilineLabel(line lineInfo, _ *[]Event) bool {
	if strings.TrimSpace(line.text) == "" {
		p.failLinkReferenceDefinition()
		return false
	}
	text := line.text
	// Look for ] in this line.
	for i := 0; i < len(text); i++ {
		if text[i] == '\\' && i+1 < len(text) {
			i++
			continue
		}
		if text[i] == '[' {
			// Unescaped [ inside label — fail.
			p.failLinkReferenceDefinition()
			return false
		}
		if text[i] == ']' {
			// Found closing ]. Must be followed by :
			if i+1 >= len(text) || text[i+1] != ':' {
				p.failLinkReferenceDefinition()
				return false
			}
			// Build the full label from accumulated content.
			fullLabel := p.refDef.labelBuf + "\n" + text[:i]
			label := normalizeReferenceLabel(fullLabel)
			if label == "" || len(fullLabel) > 999 {
				p.failLinkReferenceDefinition()
				return false
			}
			p.refDef.pendingLabel = false
			p.refDef.label = label
			p.refDef.lines = append(p.refDef.lines, line)
			// Now parse the tail (dest + optional title) from after the :
			tailStart := i + 2
			dest, title, hasDest, hasTitle, pending, ok := parseLinkReferenceDefinitionTail(text, tailStart)
			if !ok {
				// Check for pending multiline title.
				ii := skipMarkdownSpace(text, tailStart)
				if ii < len(text) {
					d, next, dok := parseInlineLinkDestination(text, ii)
					if dok && next > ii {
						spaceAfterDest := skipMarkdownSpace(text, next)
						if spaceAfterDest > next {
							if opener, content, pok := detectPendingTitle(text, spaceAfterDest); pok {
								p.refDef.dest = d
								p.refDef.hasDest = true
								p.refDef.pendingTitle = true
								p.refDef.titleOpener = opener
								p.refDef.titleBuf = content
								return true
							}
						}
					}
				}
				p.failLinkReferenceDefinition()
				return false
			}
			p.refDef.dest = dest
			p.refDef.hasDest = hasDest
			p.refDef.title = title
			if hasTitle {
				p.finishLinkReferenceDefinition()
				return true
			}
			if pending {
				return true
			}
			p.finishLinkReferenceDefinition()
			return true
		}
	}
	// No ] found — accumulate more label content.
	// CommonMark limits labels to 999 characters.
	p.refDef.labelBuf += "\n" + text
	if len(p.refDef.labelBuf) > 999 {
		p.failLinkReferenceDefinition()
		return false
	}
	p.refDef.lines = append(p.refDef.lines, line)
	return true
}

// continueMultilineTitle handles continuation lines for a title that
// started on a previous line but hasn't been closed yet.
func (p *parser) continueMultilineTitle(line lineInfo, _ *[]Event) bool {
	if strings.TrimSpace(line.text) == "" {
		// Blank line inside a title — the entire definition is invalid.
		p.refDef.pendingTitle = false
		p.refDef.titleBuf = ""
		p.failLinkReferenceDefinition()
		return false
	}
	closer := p.refDef.titleOpener
	if closer == '(' {
		closer = ')'
	}
	text := line.text
	escaped := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			continue
		}
		if c == closer {
			// Found the closing quote. Rest of line must be blank.
			rest := text[i+1:]
			if strings.TrimSpace(rest) != "" {
				// Non-blank after closer — fail the title but keep dest.
				p.refDef.pendingTitle = false
				p.refDef.titleBuf = ""
				p.finishLinkReferenceDefinition()
				return false
			}
			// Success — build the full title.
			fullTitle := p.refDef.titleBuf + "\n" + text[:i]
			p.refDef.title = decodeCharacterReferences(unescapeBackslashPunctuation(fullTitle))
			p.refDef.pendingTitle = false
			p.refDef.titleBuf = ""
			p.refDef.lines = append(p.refDef.lines, line)
			p.finishLinkReferenceDefinition()
			return true
		}
		if p.refDef.titleOpener == '(' && c == '(' {
			// Unescaped ( in paren title — fail.
			p.refDef.pendingTitle = false
			p.refDef.titleBuf = ""
			p.finishLinkReferenceDefinition()
			return false
		}
	}
	// No closer found — accumulate.
	p.refDef.titleBuf += "\n" + text
	p.refDef.lines = append(p.refDef.lines, line)
	return true
}

func (p *parser) closePendingLinkReferenceDefinition(events *[]Event) {
	if !p.refDef.active {
		return
	}
	if !p.refDef.hasDest {
		p.failLinkReferenceDefinition()
		p.closeParagraph(events)
		return
	}
	p.finishLinkReferenceDefinition()
}

func (p *parser) finishLinkReferenceDefinition() {
	if !p.refDef.active {
		return
	}
	if p.refDef.hasDest {
		if p.refs == nil {
			p.refs = make(map[string]linkReference)
		}
		if _, exists := p.refs[p.refDef.label]; !exists {
			p.refs[p.refDef.label] = linkReference{dest: p.refDef.dest, title: p.refDef.title}
		}
	}
	p.refDef = linkReferenceDefinitionState{}
}

func (p *parser) failLinkReferenceDefinition() {
	if !p.refDef.active {
		return
	}
	for _, line := range p.refDef.lines {
		p.addParagraphLine(line)
	}
	p.refDef = linkReferenceDefinitionState{}
}

func (p *parser) emitThematicBreak(line lineInfo, events *[]Event) {
	p.drainPendingBlocks(events)
	*events = append(*events,
		Event{Kind: EventEnterBlock, Block: BlockThematicBreak, Span: Span{Start: line.start, End: line.end}},
		Event{Kind: EventExitBlock, Block: BlockThematicBreak, Span: Span{Start: line.start, End: line.end}},
	)
}

func (p *parser) emitIndentedCodeLine(line lineInfo, events *[]Event) {
	for _, blankText := range p.indentedBlankLines {
		*events = append(*events,
			Event{Kind: EventText, Text: blankText},
			Event{Kind: EventLineBreak},
		)
	}
	p.indentedBlankLines = p.indentedBlankLines[:0]
	text := stripIndentColumns(line.text, 4)
	*events = append(*events,
		Event{Kind: EventText, Text: text, Span: Span{Start: line.start, End: line.end}},
		Event{Kind: EventLineBreak, Span: Span{Start: line.end, End: line.end}},
	)
}

func (p *parser) closeIndentedCode(events *[]Event) {
	if !p.inIndented {
		return
	}
	p.inIndented = false
	p.indentedBlankLines = p.indentedBlankLines[:0]
	*events = append(*events, Event{Kind: EventExitBlock, Block: BlockIndentedCode})
}

func (p *parser) closeContainers(events *[]Event) {
	p.closeTable(events)
	p.closeIndentedCode(events)
	// Close inner blockquotes (those opened inside list items) first.
	if p.blockquoteDepth > 0 && p.bqInsideListItem {
		p.closeBlockquote(lineInfo{}, events)
	}
	// Close lists.
	if p.inList {
		p.closeListItem(events)
		p.closeList(events)
	}
	// Close any remaining outer blockquotes.
	if p.blockquoteDepth > 0 {
		p.closeBlockquote(lineInfo{}, events)
	}
}

// processHTMLBlockLine handles a line inside an HTML block.
func (p *parser) processHTMLBlockLine(line lineInfo, events *[]Event) {
	// Types 6-7: blank line closes the block.
	if (p.htmlBlockType == 6 || p.htmlBlockType == 7) && strings.TrimSpace(line.text) == "" {
		p.closeHTMLBlock(events)
		return
	}

	// Emit the line as raw text.
	*events = append(*events,
		Event{Kind: EventText, Text: line.text, Span: Span{Start: line.start, End: line.end}},
		Event{Kind: EventLineBreak, Span: Span{Start: line.end, End: line.end}},
	)

	// Check end condition for types 1-5.
	if p.htmlBlockType >= 1 && p.htmlBlockType <= 5 {
		if containsFold(line.text, p.htmlBlockEnd) {
			p.closeHTMLBlock(events)
		}
	}
}

func (p *parser) closeHTMLBlock(events *[]Event) {
	if !p.inHTMLBlock {
		return
	}
	p.inHTMLBlock = false
	p.htmlBlockType = 0
	p.htmlBlockEnd = ""
	*events = append(*events, Event{Kind: EventExitBlock, Block: BlockHTML})
}

func (p *parser) closeBlockquote(line lineInfo, events *[]Event) {
	if p.blockquoteDepth == 0 {
		return
	}
	p.closeParagraph(events)
	p.drainPendingBlocks(events)
	p.closeIndentedCode(events)
	p.closeFencedCode(events)
	// Set inBlockquote = false BEFORE closing lists to prevent
	// mutual recursion: closeListItem -> closeBlockquote -> closeListItem.
	bqOpenedInsideList := p.bqInsideListItem
	depth := p.blockquoteDepth
	// When the blockquote was opened inside a list item, only close
	// the levels that were opened inside the list, not the outer ones.
	levelsToClose := depth
	if bqOpenedInsideList && p.bqDepthBeforeList < depth {
		levelsToClose = depth - p.bqDepthBeforeList
		p.blockquoteDepth = p.bqDepthBeforeList
	} else {
		p.blockquoteDepth = 0
	}
	// Close all lists that were opened inside this blockquote.
	// Don't close any lists if the blockquote was opened inside a list item
	// — those lists belong to the outer context.
	if !bqOpenedInsideList {
		for p.inList {
			if p.inListItem {
				p.closeParagraph(events)
				p.drainPendingBlocks(events)
				p.inListItem = false
				*events = append(*events, Event{Kind: EventExitBlock, Block: BlockListItem})
			}
			data := p.listData
			data.Tight = !p.listLoose
			p.inList = false
			*events = append(*events, Event{Kind: EventExitBlock, Block: BlockList, List: &data})
			p.listData = ListData{}
			p.listLoose = false
			p.popList()
		}
	}
	p.bqInsideListItem = false
	for i := 0; i < levelsToClose; i++ {
		*events = append(*events, Event{Kind: EventExitBlock, Block: BlockBlockquote, Span: Span{Start: line.start, End: line.start}})
	}
}

func (p *parser) closeListItem(events *[]Event) {
	if !p.inListItem {
		return
	}
	p.closeParagraph(events)
	p.drainPendingBlocks(events)
	p.closeIndentedCode(events)
	p.closeFencedCode(events)
	// Only close the blockquote if it was opened inside this list item.
	// When the list item is inside the blockquote (bqInsideListItem is
	// false), the blockquote is the outer container and must not be
	// closed here.
	if p.blockquoteDepth > 0 && p.bqInsideListItem {
		p.closeBlockquote(lineInfo{}, events)
	}
	// closeBlockquote may have already closed this list item
	// (when the list was inside the blockquote). Bail out to
	// avoid emitting a duplicate exit event.
	if !p.inListItem {
		return
	}
	// Close any open sublists inside this list item.
	// closeList closes the current list (and its open item), then
	// popList restores the outer list context. We must not close
	// the restored outer list item — only the original one.
	if len(p.listStack) > 0 {
		// The current list item will be closed by closeList as part
		// of closing the innermost list. After the loop, popList may
		// restore an outer list item — leave that alone.
		for len(p.listStack) > 0 {
			p.closeList(events)
		}
		// closeList already emitted exit list_item for the original
		// item. Don't emit another one.
		return
	}
	p.inListItem = false
	p.listItemBlankLine = false
	p.listItemIndent = 0
	*events = append(*events, Event{Kind: EventExitBlock, Block: BlockListItem})
}

func (p *parser) processFenceLine(line lineInfo, events *[]Event) {
	if p.isClosingFence(line.text) {
		p.fence.open = false
		*events = append(*events, Event{Kind: EventExitBlock, Block: BlockFencedCode, Span: Span{Start: line.start, End: line.end}})
		return
	}
	p.ensureDocument(events)
	*events = append(*events,
		Event{Kind: EventText, Text: stripFenceIndent(line.text, p.fence.indent), Span: Span{Start: line.start, End: line.end}},
		Event{Kind: EventLineBreak, Span: Span{Start: line.end, End: line.end}},
	)
}

func (p *parser) closeFencedCode(events *[]Event) {
	if !p.fence.open {
		return
	}
	p.fence.open = false
	*events = append(*events, Event{Kind: EventExitBlock, Block: BlockFencedCode})
}

func (p *parser) closeList(events *[]Event) {
	if !p.inList {
		return
	}
	// Close any open item in this list first.
	if p.inListItem {
		p.closeParagraph(events)
		p.drainPendingBlocks(events)
		p.closeIndentedCode(events)
		p.closeFencedCode(events)
		// Only close the blockquote if it was opened inside this
		// list item. When the list is inside the blockquote, the
		// blockquote is the outer container.
		if p.blockquoteDepth > 0 && p.bqInsideListItem {
			p.closeBlockquote(lineInfo{}, events)
		}
		p.inListItem = false
		*events = append(*events, Event{Kind: EventExitBlock, Block: BlockListItem})
	}
	p.inList = false
	data := p.listData
	data.Tight = !p.listLoose
	*events = append(*events, Event{Kind: EventExitBlock, Block: BlockList, List: &data})
	p.listData = ListData{}
	p.listLoose = false
	// Restore outer list state if we were in a sublist.
	p.popList()
}

func (p *parser) pushList() {
	p.listStack = append(p.listStack, savedList{
		inList:             p.inList,
		listData:           p.listData,
		listLoose:          p.listLoose,
		inListItem:         p.inListItem,
		listItemIndent:     p.listItemIndent,
		listItemBlankLine:  p.listItemBlankLine,
		listItemHasContent: p.listItemHasContent,
	})
}

func (p *parser) popList() {
	if len(p.listStack) == 0 {
		return
	}
	saved := p.listStack[len(p.listStack)-1]
	p.listStack = p.listStack[:len(p.listStack)-1]
	p.inList = saved.inList
	p.listData = saved.listData
	p.listLoose = saved.listLoose
	p.inListItem = saved.inListItem
	p.listItemIndent = saved.listItemIndent
	p.listItemBlankLine = saved.listItemBlankLine
	p.listItemHasContent = saved.listItemHasContent
}

// processListItemFirstLine handles the first content line of a list item.
// Unlike continuation lines, this is the content after the marker on the
// same line. It checks for block-level constructs before falling back to
// paragraph text.
func (p *parser) processListItemFirstLine(line lineInfo, events *[]Event) {
	// HTML block inside list item.
	if htmlType, htmlEnd := detectHTMLBlockStart(line.text); htmlType > 0 {
		p.inHTMLBlock = true
		p.htmlBlockType = htmlType
		p.htmlBlockEnd = htmlEnd
		*events = append(*events, Event{Kind: EventEnterBlock, Block: BlockHTML, Span: Span{Start: line.start, End: line.end}})
		p.processHTMLBlockLine(line, events)
		return
	}
	// Indented code block (4+ spaces of content indent).
	if indentedCode(line.text) {
		p.inIndented = true
		*events = append(*events, Event{Kind: EventEnterBlock, Block: BlockIndentedCode, Span: Span{Start: line.start, End: line.end}})
		p.emitIndentedCodeLine(line, events)
		return
	}
	// Fenced code block.
	if marker, n, indent, info, ok := openingFence(line.text); ok {
		p.fence = fenceState{open: true, marker: marker, length: n, indent: indent, info: info}
		*events = append(*events, Event{Kind: EventEnterBlock, Block: BlockFencedCode, Info: info, Span: Span{Start: line.start, End: line.end}})
		return
	}
	// ATX heading.
	if level, text, ok := heading(line.text); ok {
		span := Span{Start: line.start, End: line.end}
		*events = append(*events, Event{Kind: EventEnterBlock, Block: BlockHeading, Level: level, Span: span})
		p.parseInline(text, span, events)
		*events = append(*events, Event{Kind: EventExitBlock, Block: BlockHeading, Span: span})
		return
	}
	// Thematic break inside list item.
	if thematicBreak(line.text) {
		span := Span{Start: line.start, End: line.end}
		*events = append(*events,
			Event{Kind: EventEnterBlock, Block: BlockThematicBreak, Span: span},
			Event{Kind: EventExitBlock, Block: BlockThematicBreak, Span: span},
		)
		return
	}
	// Blockquote inside list item (e.g., "> 1. > Blockquote").
	if content, ok := blockquoteContent(line.text); ok {
		p.bqDepthBeforeList = p.blockquoteDepth
		p.blockquoteDepth++
		p.bqInsideListItem = true
		*events = append(*events, Event{Kind: EventEnterBlock, Block: BlockBlockquote, Span: Span{Start: line.start, End: line.end}})
		if strings.TrimSpace(content) != "" {
			inner := line
			inner.text = content
			p.addParagraphLine(inner)
		}
		return
	}
	// Sublist on same line as outer marker (e.g., "- - foo").
	if item, ok := listItem(line.text); ok {
		p.pushList()
		p.inList = true
		p.listData = listBlockData(item.data)
		p.listLoose = false
		data := p.listData
		*events = append(*events, Event{Kind: EventEnterBlock, Block: BlockList, List: &data, Span: Span{Start: line.start, End: line.end}})
		p.inListItem = true
		p.listItemIndent = item.contentIndent
		p.listItemBlankLine = false
		idata := item.data
		*events = append(*events, Event{Kind: EventEnterBlock, Block: BlockListItem, List: &idata, Span: Span{Start: line.start, End: line.end}})
		if strings.TrimSpace(item.content) != "" {
			inner := line
			inner.text = item.content
			p.processListItemFirstLine(inner, events)
		}
		return
	}
	// Default: paragraph text.
	p.addParagraphLine(line)
}

// processListItemContent processes a line of content inside a list item.
// The line has already been stripped of the list item's content indent.
// It handles sublists, blockquotes, fenced code, headings, and other
// block-level constructs that can appear inside list items.
func (p *parser) processListItemContent(line lineInfo, events *[]Event) {
	// Handle fenced code continuation inside list items.
	if p.fence.open {
		if p.isClosingFence(line.text) {
			p.fence.open = false
			*events = append(*events, Event{Kind: EventExitBlock, Block: BlockFencedCode, Span: Span{Start: line.start, End: line.end}})
			return
		}
		*events = append(*events,
			Event{Kind: EventText, Text: stripFenceIndent(line.text, p.fence.indent), Span: Span{Start: line.start, End: line.end}},
			Event{Kind: EventLineBreak, Span: Span{Start: line.end, End: line.end}},
		)
		return
	}

	// Handle indented code continuation inside list items.
	if p.inIndented {
		if indentedCode(line.text) {
			p.emitIndentedCodeLine(line, events)
			return
		}
		if strings.TrimSpace(line.text) == "" {
			p.indentedBlankLines = append(p.indentedBlankLines, stripIndentColumns(line.text, 4))
			return
		}
		p.closeIndentedCode(events)
	}

	// Blank line inside continuation.
	if strings.TrimSpace(line.text) == "" {
		p.closeParagraph(events)
		p.closeIndentedCode(events)
		return
	}

	// Setext heading inside list item.
	if level, ok := setextHeading(line.text); ok && len(p.paragraph.lines) > 0 {
		p.closeSetextHeading(level, Span{Start: line.start, End: line.end}, events)
		return
	}

	// Thematic break inside list item continuation.
	if thematicBreak(line.text) {
		p.closeParagraph(events)
		p.drainPendingBlocks(events)
		span := Span{Start: line.start, End: line.end}
		*events = append(*events,
			Event{Kind: EventEnterBlock, Block: BlockThematicBreak, Span: span},
			Event{Kind: EventExitBlock, Block: BlockThematicBreak, Span: span},
		)
		return
	}

	// Blockquote inside list item.
	if content, ok := blockquoteContent(line.text); ok {
		p.closeParagraph(events)
		p.drainPendingBlocks(events)
		if p.blockquoteDepth == 0 {
			p.bqDepthBeforeList = 0
			p.blockquoteDepth++
			p.bqInsideListItem = p.inListItem
			*events = append(*events, Event{Kind: EventEnterBlock, Block: BlockBlockquote, Span: Span{Start: line.start, End: line.end}})
		}
		if strings.TrimSpace(content) == "" {
			p.closeParagraph(events)
			return
		}
		inner := line
		inner.text = content
		p.processNonContainerLine(inner, events)
		return
	}
	if p.blockquoteDepth > 0 && p.bqInsideListItem {
		if len(p.paragraph.lines) > 0 && !thematicBreak(line.text) {
			if _, _, _, _, isFence := openingFence(line.text); !isFence {
				if _, ok := listItem(line.text); !ok {
					p.addParagraphLine(line)
					return
				}
			}
		}
		p.closeBlockquote(line, events)
	}

	// HTML block inside list item continuation.
	if htmlType, htmlEnd := detectHTMLBlockStart(line.text); htmlType > 0 {
		if htmlType <= 6 || len(p.paragraph.lines) == 0 {
			p.closeParagraph(events)
			p.drainPendingBlocks(events)
			p.inHTMLBlock = true
			p.htmlBlockType = htmlType
			p.htmlBlockEnd = htmlEnd
			*events = append(*events, Event{Kind: EventEnterBlock, Block: BlockHTML, Span: Span{Start: line.start, End: line.end}})
			p.processHTMLBlockLine(line, events)
			return
		}
	}

	// Sublist inside list item.
	if item, ok := listItem(line.text); ok {
		p.closeParagraph(events)
		p.drainPendingBlocks(events)
		// A list item found inside the current item's content always
		// starts a new sublist (deeper nesting). Sibling detection
		// happens at the processLine level when unwinding indent.
		p.pushList()
		p.inList = true
		p.listData = listBlockData(item.data)
		p.listLoose = false
		data := p.listData
		*events = append(*events, Event{Kind: EventEnterBlock, Block: BlockList, List: &data, Span: Span{Start: line.start, End: line.end}})
		p.inListItem = true
		// Content indent is relative to the stripped text. Convert to
		// absolute by adding the outer item's indent.
		outerIndent := p.listStack[len(p.listStack)-1].listItemIndent
		p.listItemIndent = outerIndent + item.contentIndent
		p.listItemBlankLine = false
		idata := item.data
		*events = append(*events, Event{Kind: EventEnterBlock, Block: BlockListItem, List: &idata, Span: Span{Start: line.start, End: line.end}})
		if strings.TrimSpace(item.content) != "" {
			inner := line
			inner.text = item.content
			p.processListItemFirstLine(inner, events)
		}
		return
	}

	// Link reference definition inside list item.
	if len(p.paragraph.lines) == 0 {
		if p.startLinkReferenceDefinition(line) {
			return
		}
	}

	p.processNonContainerLine(line, events)
}

// stripIndent removes up to n columns of leading indentation from a line.
// When a tab partially overlaps the strip boundary, the remainder is
// expanded to spaces and subsequent tabs are also expanded so that their
// column alignment is preserved.
func stripIndent(line string, n int) string {
	col := 0
	for i := 0; i < len(line); i++ {
		if col >= n {
			return line[i:]
		}
		switch line[i] {
		case ' ':
			col++
		case '\t':
			newCol := col + 4 - col%4
			if newCol > n {
				// Tab extends past the strip boundary. Expand the
				// remainder and all subsequent leading tabs to spaces
				// so that column alignment is preserved.
				var b strings.Builder
				absCol := newCol
				b.WriteString(strings.Repeat(" ", absCol-n))
				for j := i + 1; j < len(line); j++ {
					switch line[j] {
					case '\t':
						w := 4 - absCol%4
						b.WriteString(strings.Repeat(" ", w))
						absCol += w
					case ' ':
						b.WriteByte(' ')
						absCol++
					default:
						b.WriteString(line[j:])
						return b.String()
					}
				}
				return b.String()
			}
			col = newCol
		default:
			return line[i:]
		}
	}
	return ""
}

func (p *parser) isClosingFence(line string) bool {
	indent, indentBytes := leadingIndent(line)
	if indent > 3 {
		return false
	}
	trimmed := line[indentBytes:]
	// Count consecutive fence marker characters.
	n := 0
	for n < len(trimmed) && trimmed[n] == p.fence.marker {
		n++
	}
	// Closing fence must have at least as many markers as the opening.
	if n < p.fence.length {
		return false
	}
	// Rest of line must be blank (only whitespace after the fence).
	return strings.TrimSpace(trimmed[n:]) == ""
}

func openingFence(line string) (byte, int, int, string, bool) {
	indent, indentBytes := leadingIndent(line)
	if indent > 3 {
		return 0, 0, 0, "", false
	}
	trimmed := line[indentBytes:]
	if len(trimmed) < 3 {
		return 0, 0, 0, "", false
	}
	marker := trimmed[0]
	if marker != '`' && marker != '~' {
		return 0, 0, 0, "", false
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == marker {
		n++
	}
	if n < 3 {
		return 0, 0, 0, "", false
	}
	info := decodeCharacterReferences(unescapeBackslashPunctuation(strings.TrimSpace(trimmed[n:])))
	if marker == '`' && strings.Contains(info, "`") {
		return 0, 0, 0, "", false
	}
	return marker, n, indent, info, true
}

func stripFenceIndent(line string, indent int) string {
	n := 0
	for n < len(line) && n < indent && line[n] == ' ' {
		n++
	}
	return line[n:]
}

func heading(line string) (int, string, bool) {
	indent, indentBytes := leadingIndent(line)
	if indent > 3 {
		return 0, "", false
	}
	trimmed := line[indentBytes:]
	level := 0
	for level < len(trimmed) && trimmed[level] == '#' {
		level++
	}
	if level == 0 || level > 6 {
		return 0, "", false
	}
	if level < len(trimmed) && trimmed[level] != ' ' && trimmed[level] != '\t' {
		return 0, "", false
	}
	text := strings.TrimSpace(trimmed[level:])
	if i := closingATXSequence(text); i >= 0 {
		text = strings.TrimSpace(text[:i])
	}
	return level, text, true
}

func closingATXSequence(text string) int {
	i := len(text) - 1
	for i >= 0 && text[i] == '#' {
		i--
	}
	if i == len(text)-1 {
		return -1
	}
	if i < 0 || text[i] == ' ' || text[i] == '\t' {
		return i + 1
	}
	return -1
}

func thematicBreak(line string) bool {
	if indent, _ := leadingIndent(line); indent > 3 {
		return false
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	var marker byte
	count := 0
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		if c == ' ' || c == '\t' {
			continue
		}
		if marker == 0 {
			if c != '-' && c != '*' && c != '_' {
				return false
			}
			marker = c
		}
		if c != marker {
			return false
		}
		count++
	}
	return count >= 3
}

func setextHeading(line string) (int, bool) {
	indent, indentBytes := leadingIndent(line)
	if indent > 3 {
		return 0, false
	}
	trimmed := strings.TrimSpace(line[indentBytes:])
	if trimmed == "" {
		return 0, false
	}
	marker := trimmed[0]
	if marker != '=' && marker != '-' {
		return 0, false
	}
	for i := 1; i < len(trimmed); i++ {
		if trimmed[i] != marker {
			return 0, false
		}
	}
	if marker == '=' {
		return 1, true
	}
	return 2, true
}

func indentedCode(line string) bool {
	indent, _ := leadingIndent(line)
	return indent >= 4 && strings.TrimSpace(line) != ""
}

func blockquoteContent(line string) (string, bool) {
	indent, indentBytes := leadingIndent(line)
	if indent > 3 || indentBytes >= len(line) || line[indentBytes] != '>' {
		return "", false
	}
	content := line[indentBytes+1:]
	if len(content) > 0 && content[0] == ' ' {
		content = content[1:]
	} else if len(content) > 0 && content[0] == '\t' {
		// A tab after >: the > is at column `indent`, so the tab starts
		// at column indent+1. Consuming 1 space of the tab leaves the
		// remaining expansion as spaces. Expand all leading tabs to
		// preserve absolute column alignment.
		absCol := indent + 1
		// Skip 1 column (the optional space).
		firstTabWidth := 4 - absCol%4
		absCol += firstTabWidth
		var b strings.Builder
		if firstTabWidth > 1 {
			b.WriteString(strings.Repeat(" ", firstTabWidth-1))
		}
		for i := 1; i < len(content); i++ {
			switch content[i] {
			case '\t':
				w := 4 - absCol%4
				b.WriteString(strings.Repeat(" ", w))
				absCol += w
			case ' ':
				b.WriteByte(' ')
				absCol++
			default:
				b.WriteString(content[i:])
				return b.String(), true
			}
		}
		return b.String(), true
	}
	return content, true
}

type listItemData struct {
	data          ListData
	content       string
	contentIndent int // column where content starts
}

func listItem(line string) (listItemData, bool) {
	indent, indentBytes := leadingIndent(line)
	if indent > 3 {
		return listItemData{}, false
	}
	trimmed := line[indentBytes:]

	// Bullet list marker: -, +, or *
	if len(trimmed) >= 1 && (trimmed[0] == '-' || trimmed[0] == '+' || trimmed[0] == '*') {
		markerWidth := 1 // the bullet character
		if len(trimmed) == 1 {
			// Marker at end of line — content starts at marker + 1 space.
			return listItemData{
				data:          ListData{Ordered: false, Marker: string(trimmed[0]), Tight: true},
				contentIndent: indent + markerWidth + 1,
			}, true
		}
		if trimmed[1] != ' ' && trimmed[1] != '\t' {
			if len(trimmed) < 2 {
				return listItemData{}, false
			}
			// Not a list item — no space after marker.
			goto tryOrdered
		}
		// Count spaces after marker (1-4, per spec).
		padCols, _, content := countListPadding(trimmed[markerWidth:], indent+markerWidth)
		data := ListData{Ordered: false, Marker: string(trimmed[0]), Tight: true}
		data.Task, data.Checked, content = parseTaskListItem(content)
		return listItemData{
			data:          data,
			content:       content,
			contentIndent: indent + markerWidth + padCols,
		}, true
	}

tryOrdered:
	// Ordered list marker: 1-9 digits followed by . or )
	i := 0
	for i < len(trimmed) && trimmed[i] >= '0' && trimmed[i] <= '9' {
		i++
	}
	if i == 0 || i > 9 || i >= len(trimmed) {
		return listItemData{}, false
	}
	marker := trimmed[i]
	if marker != '.' && marker != ')' {
		return listItemData{}, false
	}
	markerWidth := i + 1 // digits + delimiter
	start, err := strconv.Atoi(trimmed[:i])
	if err != nil {
		return listItemData{}, false
	}
	if markerWidth >= len(trimmed) {
		// Marker at end of line.
		return listItemData{
			data:          ListData{Ordered: true, Start: start, Marker: string(marker), Tight: true},
			contentIndent: indent + markerWidth + 1,
		}, true
	}
	if trimmed[markerWidth] != ' ' && trimmed[markerWidth] != '\t' {
		return listItemData{}, false
	}
	padCols, _, content := countListPadding(trimmed[markerWidth:], indent+markerWidth)
	data := ListData{Ordered: true, Start: start, Marker: string(marker), Tight: true}
	data.Task, data.Checked, content = parseTaskListItem(content)
	return listItemData{
		data:          data,
		content:       content,
		contentIndent: indent + markerWidth + padCols,
	}, true
}

// countListPadding counts the spaces/tabs after a list marker.
// Per CommonMark, 1-4 spaces are consumed as padding. If the content
// is blank (only spaces), exactly 1 space of padding is used.
// startCol is the absolute column of the first character after the marker.
// Returns (columns consumed as padding, bytes consumed, remaining content
// with partial tab expansion).
func countListPadding(afterMarker string, startCol int) (int, int, string) {
	col := startCol
	bytes := 0
	for bytes < len(afterMarker) {
		switch afterMarker[bytes] {
		case ' ':
			col++
			bytes++
		case '\t':
			col += 4 - col%4
			bytes++
		default:
			padCols := col - startCol
			if padCols > 4 {
				// Content starts with indented code; use 1 space padding.
				// The first whitespace character contributes 1 column of
				// padding; any remaining expansion is content.
				content := expandTabsFromSkipCols(afterMarker, startCol, 1)
				return 1, 0, content
			}
			// Normal padding — expand remaining leading tabs.
			content := expandTabsFromSkipCols(afterMarker, startCol, padCols)
			return padCols, 0, content
		}
	}
	// All spaces — blank content, use 1 space padding.
	return 1, 0, ""
}

// expandTabsFromSkipCols expands leading tabs in s to spaces, starting from
// absolute column startCol, skipping the first skipCols columns.
// Any partial tab remainder after skipping is emitted as spaces.
// Non-whitespace characters and everything after them are kept as-is.
func expandTabsFromSkipCols(s string, startCol int, skipCols int) string {
	absCol := startCol
	skipped := 0
	byteStart := 0
	// Skip skipCols columns.
OuterLoop:
	for byteStart < len(s) && skipped < skipCols {
		switch s[byteStart] {
		case '\t':
			w := 4 - absCol%4
			if skipped+w > skipCols {
				// Tab partially consumed by skip; remainder becomes spaces.
				remainder := skipped + w - skipCols
				absCol += w
				byteStart++
				// Expand remaining leading tabs.
				var b strings.Builder
				b.WriteString(strings.Repeat(" ", remainder))
				for i := byteStart; i < len(s); i++ {
					switch s[i] {
					case '\t':
						tw := 4 - absCol%4
						b.WriteString(strings.Repeat(" ", tw))
						absCol += tw
					case ' ':
						b.WriteByte(' ')
						absCol++
					default:
						b.WriteString(s[i:])
						return b.String()
					}
				}
				return b.String()
			}
			skipped += w
			absCol += w
		case ' ':
			skipped++
			absCol++
		default:
			break OuterLoop
		}
		byteStart++
	}
	// Check if remaining has tabs in leading whitespace.
	hasTabs := false
	for i := byteStart; i < len(s); i++ {
		if s[i] == '\t' {
			hasTabs = true
			break
		}
		if s[i] != ' ' {
			break
		}
	}
	if !hasTabs {
		return s[byteStart:]
	}
	var b strings.Builder
	for i := byteStart; i < len(s); i++ {
		switch s[i] {
		case '\t':
			w := 4 - absCol%4
			b.WriteString(strings.Repeat(" ", w))
			absCol += w
		case ' ':
			b.WriteByte(' ')
			absCol++
		default:
			b.WriteString(s[i:])
			return b.String()
		}
	}
	return b.String()
}

func listBlockData(item ListData) ListData {
	item.Task = false
	item.Checked = false
	return item
}

func parseTaskListItem(content string) (task bool, checked bool, rest string) {
	if len(content) < 3 || content[0] != '[' || content[2] != ']' {
		return false, false, content
	}
	switch content[1] {
	case ' ':
		checked = false
	case 'x', 'X':
		checked = true
	default:
		return false, false, content
	}
	if len(content) == 3 {
		return true, checked, ""
	}
	if content[3] != ' ' && content[3] != '\t' {
		return false, false, content
	}
	return true, checked, strings.TrimLeft(content[4:], " \t")
}

func leadingIndent(line string) (int, int) {
	columns := 0
	bytes := 0
	for bytes < len(line) {
		switch line[bytes] {
		case ' ':
			columns++
			bytes++
		case '\t':
			columns += 4 - columns%4
			bytes++
		default:
			return columns, bytes
		}
	}
	return columns, bytes
}

func stripIndentColumns(line string, columns int) string {
	current := 0
	for i := 0; i < len(line); i++ {
		if current >= columns {
			return line[i:]
		}
		switch line[i] {
		case ' ':
			current++
		case '\t':
			newCol := current + 4 - current%4
			if newCol > columns {
				// Tab crosses the strip boundary; replace excess with spaces.
				return strings.Repeat(" ", newCol-columns) + line[i+1:]
			}
			current = newCol
		default:
			return line[i:]
		}
	}
	return ""
}

func (p *parser) parseInline(text string, span Span, events *[]Event) {
	if text == "" {
		*events = append(*events, Event{Kind: EventText, Text: "", Span: span})
		return
	}
	if !hasInlineSyntax(text, p.config.GFMAutolinks, p.config.InlineScanners) {
		*events = append(*events, Event{Kind: EventText, Text: text, Span: span})
		return
	}
	p.inlineTokens = tokenizeInlineReuse(text, span, p.refs, p.config.GFMAutolinks, p.config.InlineScanners, p.inlineTokens[:0])
	p.inlineTokens, p.emphOut = resolveEmphasisReuse(p.inlineTokens, p.emphOut[:0])
	coalesceInlineTokensInto(p.inlineTokens, span, events)
}

// parseInlineInto is the non-method variant used by tokenizeLinkContent
// (which may be called recursively and cannot share the parser's scratch slice).
func parseInlineInto(text string, span Span, refs map[string]linkReference, gfmAutolinks bool, events *[]Event) {
	if text == "" {
		*events = append(*events, Event{Kind: EventText, Text: "", Span: span})
		return
	}
	if !hasInlineSyntax(text, gfmAutolinks, nil) {
		*events = append(*events, Event{Kind: EventText, Text: text, Span: span})
		return
	}
	tokens := tokenizeInline(text, span, refs, gfmAutolinks, nil)
	tokens = resolveEmphasis(tokens)
	coalesceInlineTokensInto(tokens, span, events)
}

func hasInlineSyntax(text string, gfmAutolinks bool, scanners []InlineScanner) bool {
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\n', '\\', '`', '!', '[', '<', '&', '*', '_', '~':
			return true
		case '@':
			if gfmAutolinks {
				return true
			}
		case 'h', 'H', 'f', 'F', 'w', 'W':
			if gfmAutolinks {
				return true
			}
		}
		for _, scanner := range scanners {
			if scanner != nil && strings.IndexByte(scanner.TriggerBytes(), text[i]) >= 0 {
				return true
			}
		}
	}
	return false
}

type inlineTokenKind int

const (
	inlineTokenText inlineTokenKind = iota
	inlineTokenSoftBreak
	inlineTokenLineBreak
	inlineTokenDelimiter
	inlineTokenLinkOpen  // marks the start of a link's content
	inlineTokenLinkClose // marks the end of a link's content
	inlineTokenEvent     // custom inline scanner event
)

type inlineToken struct {
	kind    inlineTokenKind
	text    string
	style   InlineStyle // for linkOpen/linkClose: the link style
	delim   byte
	run     int
	origRun int // original run length, for rule-of-three checks
	open    bool
	close   bool
	event   Event
}

func tokenizeInline(text string, span Span, refs map[string]linkReference, gfmAutolinks bool, scanners []InlineScanner) []inlineToken {
	return tokenizeInlineReuse(text, span, refs, gfmAutolinks, scanners, nil)
}

func tokenizeInlineReuse(text string, span Span, refs map[string]linkReference, gfmAutolinks bool, scanners []InlineScanner, tokens []inlineToken) []inlineToken {
	var prevSource string
	linkPossible := strings.Contains(text, "](")
	imagePossible := linkPossible
	autolinkPossible := strings.Contains(text, ">")
outer:
	for len(text) > 0 {
		if text[0] == '\n' {
			if trimHardBreakMarkerTokens(&tokens) {
				tokens = append(tokens, inlineToken{kind: inlineTokenLineBreak})
			} else {
				tokens = append(tokens, inlineToken{kind: inlineTokenSoftBreak})
			}
			prevSource = "\n"
			text = text[1:]
			continue
		}
		if text[0] == '\\' && len(text) > 1 && isEscapablePunctuation(text[1]) {
			tokens = append(tokens, inlineToken{kind: inlineTokenText, text: text[1:2]})
			prevSource = text[:2]
			text = text[2:]
			continue
		}
		if text[0] == '\\' {
			tokens = append(tokens, inlineToken{kind: inlineTokenText, text: text[:1]})
			prevSource = text[:1]
			text = text[1:]
			continue
		}
		if text[0] == '`' {
			if ev, rest, ok := parseCodeSpan(text, span); ok {
				tokens = append(tokens, inlineToken{kind: inlineTokenText, text: ev.Text, style: ev.Style})
				prevSource = text[:len(text)-len(rest)]
				text = rest
				continue
			}
			n := countRun(text, '`')
			tokens = append(tokens, inlineToken{kind: inlineTokenText, text: text[:n]})
			prevSource = text[:n]
			text = text[n:]
			continue
		}
		if text[0] == '!' {
			if imagePossible {
				if ev, rest, ok := parseInlineImage(text, span); ok {
					tokens = append(tokens, inlineToken{kind: inlineTokenText, text: ev.Text, style: ev.Style})
					prevSource = text[:len(text)-len(rest)]
					text = rest
					continue
				}
				imagePossible = strings.Contains(text[1:], "](")
			}
			if len(refs) > 0 {
				if ev, rest, ok := parseReferenceImage(text, span, refs); ok {
					tokens = append(tokens, inlineToken{kind: inlineTokenText, text: ev.Text, style: ev.Style})
					prevSource = text[:len(text)-len(rest)]
					text = rest
					continue
				}
			}
		}
		// Footnote references [^...] — not supported, emit as literal text.
		if text[0] == '[' && len(text) > 1 && text[1] == '^' {
			close := strings.IndexByte(text[2:], ']')
			if close >= 0 {
				end := 2 + close + 1
				tokens = append(tokens, inlineToken{kind: inlineTokenText, text: text[:end]})
				prevSource = text[:end]
				text = text[end:]
				continue
			}
		}
		if text[0] == '[' && linkPossible {
			if ev, rest, labelRaw, ok := parseInlineLink(text, span); ok {
				linkStyle := InlineStyle{LinkData: ev.Style.LinkData}
				// Resolve emphasis within the label, then wrap in link boundaries.
				// This lets outer emphasis wrap the link while keeping inner
				// delimiters scoped to the label.
				tokens = append(tokens, inlineToken{kind: inlineTokenLinkOpen, style: linkStyle})
				tokens = append(tokens, tokenizeLinkContent(labelRaw, InlineStyle{}, span, refs, gfmAutolinks, scanners)...)
				tokens = append(tokens, inlineToken{kind: inlineTokenLinkClose, style: linkStyle})
				prevSource = text[:len(text)-len(rest)]
				text = rest
				continue
			}
			linkPossible = strings.Contains(text[1:], "](")
		}
		if text[0] == '[' && len(refs) > 0 {
			if ev, rest, labelRaw, ok := parseReferenceLink(text, span, refs); ok {
				_ = ev
				linkStyle := InlineStyle{LinkData: ev.Style.LinkData}
				tokens = append(tokens, inlineToken{kind: inlineTokenLinkOpen, style: linkStyle})
				tokens = append(tokens, tokenizeLinkContent(labelRaw, InlineStyle{}, span, refs, gfmAutolinks, scanners)...)
				tokens = append(tokens, inlineToken{kind: inlineTokenLinkClose, style: linkStyle})
				prevSource = text[:len(text)-len(rest)]
				text = rest
				continue
			}
		}
		// If [ failed as a link opener, emit just the [ as text
		// so the next character gets a chance to be a link opener.
		if text[0] == '[' {
			tokens = append(tokens, inlineToken{kind: inlineTokenText, text: "["})
			prevSource = text[:1]
			text = text[1:]
			continue
		}
		if text[0] == '<' && autolinkPossible {
			if ev, rest, ok := parseAutolink(text, span); ok {
				tokens = append(tokens, inlineToken{kind: inlineTokenText, text: ev.Text, style: ev.Style})
				prevSource = text[:len(text)-len(rest)]
				text = rest
				continue
			}
			// Raw HTML tag (same precedence as autolinks and code spans).
			if tag, ok := parseRawHTMLTag(text); ok {
				tokens = append(tokens, inlineToken{kind: inlineTokenText, text: tag, style: InlineStyle{RawHTML: true}})
				prevSource = tag
				text = text[len(tag):]
				continue
			}
			autolinkPossible = strings.Contains(text[1:], ">")
		}
		if gfmAutolinks {
			if ev, rest, ok := parseAutolinkLiteral(text, span, prevSource); ok {
				tokens = append(tokens, inlineToken{kind: inlineTokenText, text: ev.Text, style: ev.Style})
				prevSource = text[:len(text)-len(rest)]
				text = rest
				continue
			}
		}
		if text[0] == '&' {
			if decoded, rest, ok := parseCharacterReference(text); ok {
				tokens = append(tokens, inlineToken{kind: inlineTokenText, text: decoded})
				prevSource = text[:len(text)-len(rest)]
				text = rest
				continue
			}
		}
		for _, scanner := range scanners {
			if scanner == nil || strings.IndexByte(scanner.TriggerBytes(), text[0]) < 0 {
				continue
			}
			if match, ok := scanner.ScanInline(text, InlineContext{Span: span}); ok && match.Consume > 0 && match.Consume <= len(text) {
				ev := match.Event
				if ev.Kind == 0 {
					ev.Kind = EventInline
				}
				if ev.Span == (Span{}) {
					ev.Span = span
				}
				tokens = append(tokens, inlineToken{kind: inlineTokenEvent, event: ev})
				prevSource = text[:match.Consume]
				text = text[match.Consume:]
				continue outer
			}
		}
		if isInlineDelimiterByte(text[0]) {
			n := countRun(text, text[0])
			// Tilde runs of 3+ are not strikethrough delimiters.
			if text[0] == '~' && n > 2 {
				tokens = append(tokens, inlineToken{kind: inlineTokenText, text: text[:n]})
				prevSource = text[:n]
				text = text[n:]
				continue
			}
			open, close := emphasisDelimRun(prevSource, text[n:], text[0], n)
			tokens = append(tokens, inlineToken{kind: inlineTokenDelimiter, text: text[:n], delim: text[0], run: n, origRun: n, open: open, close: close})
			prevSource = text[:n]
			text = text[n:]
			continue
		}
		next := nextInlineDelimiter(text)
		if start := nextAutolinkLiteralStart(text, prevSource); start >= 0 && start < next {
			next = start
		}
		if start := nextInlineScannerStart(text, scanners); start >= 0 && start < next {
			next = start
		}
		if next <= 0 {
			next = 1
		}
		tokens = append(tokens, inlineToken{kind: inlineTokenText, text: text[:next]})
		prevSource = text[:next]
		text = text[next:]
	}
	return tokens
}

func nextInlineScannerStart(text string, scanners []InlineScanner) int {
	best := -1
	for _, scanner := range scanners {
		if scanner == nil {
			continue
		}
		triggers := scanner.TriggerBytes()
		for i := 0; i < len(triggers); i++ {
			idx := strings.IndexByte(text, triggers[i])
			if idx >= 0 && (best < 0 || idx < best) {
				best = idx
			}
		}
	}
	return best
}

// resolveEmphasis implements the CommonMark "process emphasis" algorithm.
//
// Phase 1: match openers to closers, recording (opener, closer, use) triples.
// Phase 2: walk tokens and use the recorded matches to build nested styled output.
func resolveEmphasis(tokens []inlineToken) []inlineToken {
	result, _ := resolveEmphasisReuse(tokens, nil)
	return result
}

// resolveEmphasisReuse is like resolveEmphasis but accepts a reusable output
// slice to avoid allocation. It returns (result, scratch) where scratch is the
// backing array of the output for the caller to retain.
func resolveEmphasisReuse(tokens []inlineToken, out []inlineToken) ([]inlineToken, []inlineToken) {
	if len(tokens) == 0 {
		return tokens, out
	}

	// Collect delimiter indices.
	var dstack []int // indices into tokens, acts as the delimiter stack
	for i, tok := range tokens {
		if tok.kind == inlineTokenDelimiter {
			dstack = append(dstack, i)
		}
	}
	if len(dstack) == 0 {
		return tokens, out
	}

	// emphPair records a matched opener-closer pair.
	type emphPair struct {
		openerIdx int
		closerIdx int
		use       int
		delim     byte
	}
	var pairs []emphPair

	// removed[di] = true means dstack[di] has been removed from the stack.
	removed := make([]bool, len(dstack))

	// openersBottom keyed by (delim, closerCanOpen, closerOrigRun%3).
	openersBottom := map[int]int{} // value = dstack index (exclusive lower bound)
	obKey := func(delim byte, canOpen bool, origRun int) int {
		k := int(delim) * 6
		if canOpen {
			k += 3
		}
		k += origRun % 3
		return k
	}

	for ci := 0; ci < len(dstack); ci++ {
		if removed[ci] {
			continue
		}
		closerTokIdx := dstack[ci]
		closer := &tokens[closerTokIdx]
		if !closer.close || (closer.delim != '*' && closer.delim != '_' && closer.delim != '~') {
			continue
		}

		bk := obKey(closer.delim, closer.open, closer.origRun)
		bottom := openersBottom[bk] // dstack index; search only above this

		found := false
		for oi := ci - 1; oi >= bottom; oi-- {
			if removed[oi] {
				continue
			}
			openerTokIdx := dstack[oi]
			opener := &tokens[openerTokIdx]
			if opener.delim != closer.delim || !opener.open {
				continue
			}

			// Rule of three (spec rules 9-10).
			if closer.delim != '~' {
				if (opener.open && opener.close) || (closer.open && closer.close) {
					if (opener.origRun+closer.origRun)%3 == 0 &&
						opener.origRun%3 != 0 && closer.origRun%3 != 0 {
						continue
					}
				}
			}

			var use int
			if closer.delim == '~' {
				if opener.run >= 2 && closer.run >= 2 {
					use = 2
				} else if opener.run == 1 && closer.run == 1 {
					use = 1
				} else {
					continue
				}
			} else if opener.run >= 2 && closer.run >= 2 {
				use = 2
			} else {
				use = 1
			}

			pairs = append(pairs, emphPair{
				openerIdx: openerTokIdx,
				closerIdx: closerTokIdx,
				use:       use,
				delim:     closer.delim,
			})

			// Consume delimiters.
			opener.run -= use
			opener.text = opener.text[:opener.run]
			closer.run -= use
			closer.text = closer.text[use:]

			// Remove delimiters between opener and closer from the stack.
			for ri := oi + 1; ri < ci; ri++ {
				removed[ri] = true
			}

			if opener.run == 0 {
				removed[oi] = true
			}
			if closer.run == 0 {
				removed[ci] = true
			} else {
				ci-- // re-process closer
			}

			found = true
			break
		}

		if !found {
			openersBottom[bk] = ci
			if !closer.open {
				removed[ci] = true
			}
		}
	}

	if len(pairs) == 0 {
		// No matches — emit all delimiters as literal text.
		for _, tok := range tokens {
			switch tok.kind {
			case inlineTokenDelimiter:
				if tok.run > 0 {
					out = append(out, inlineToken{kind: inlineTokenText, text: tok.text})
				}
			default:
				out = append(out, tok)
			}
		}
		return out, out
	}

	// Phase 2: emit output.
	//
	// Pairs are recorded in match order (outermost first for nested cases
	// like ***foo***). We need to sort them by opener position and handle
	// nesting. A pair (A, B) is nested inside (C, D) if C < A and B < D.
	//
	// We build a stack of active styles. For each token position, we check
	// if any pair opens or closes here.

	// Build open/close events sorted by token index. A flat slice avoids the
	// per-paragraph map allocation from the original implementation.
	type styleEvent struct {
		idx   int
		use   int
		delim byte
		open  bool // true = open, false = close
		seq   int  // ordering for multiple events at same position
	}
	events := make([]styleEvent, 0, len(pairs)*2)
	for i, p := range pairs {
		// Open event goes right after the opener token.
		// Close event goes right before the closer token.
		events = append(events, styleEvent{idx: p.openerIdx, use: p.use, delim: p.delim, open: true, seq: i})
		events = append(events, styleEvent{idx: p.closerIdx, use: p.use, delim: p.delim, open: false, seq: i})
	}

	// Sort events by token index, then by event order at that position:
	// opens before closes, opens by seq descending, closes by seq ascending.
	// Insertion sort keeps the common small-pair case allocation-free.
	for i := 1; i < len(events); i++ {
		for j := i; j > 0; j-- {
			a, b := events[j], events[j-1]
			swap := false
			if a.idx != b.idx {
				swap = a.idx < b.idx
			} else if a.open != b.open {
				swap = a.open // opens before closes
			} else if a.open {
				swap = a.seq > b.seq // opens: outer first
			} else {
				swap = a.seq < b.seq // closes: inner first
			}
			if !swap {
				break
			}
			events[j], events[j-1] = events[j-1], events[j]
		}
	}

	// Track emphasis/strong depth so nested emphasis produces
	// separate text events at boundaries.
	type depthStyle struct {
		emDepth     int
		strongDepth int
		strike      bool
		base        InlineStyle // non-emphasis styles (link, code, etc.)
	}

	toInlineStyle := func(ds depthStyle) InlineStyle {
		s := ds.base
		s.Emphasis = ds.emDepth > 0
		s.Strong = ds.strongDepth > 0
		s.Strike = ds.strike
		s.EmphasisDepth = int16(ds.emDepth)
		s.StrongDepth = int16(ds.strongDepth)
		return s
	}

	var dsStack []depthStyle
	current := depthStyle{}

	eventPos := 0
	for i, tok := range tokens {
		eventStart := eventPos
		for eventPos < len(events) && events[eventPos].idx == i {
			eventPos++
		}
		if eventStart < eventPos {
			evs := events[eventStart:eventPos]
			// For openers, remaining delimiter text is a prefix
			// (emitted before the open, as literal text).
			// For closers, remaining text is a suffix (emitted
			// after the close, as literal text).
			hasOpen := false
			hasClose := false
			for _, ev := range evs {
				if ev.open {
					hasOpen = true
				} else {
					hasClose = true
				}
			}
			// Emit opener's remaining text before the open event.
			if hasOpen && !hasClose && tok.kind == inlineTokenDelimiter && tok.run > 0 {
				s := toInlineStyle(current)
				out = append(out, inlineToken{kind: inlineTokenText, text: tok.text, style: s})
			}
			for _, ev := range evs {
				if ev.open {
					dsStack = append(dsStack, current)
					if ev.delim == '~' {
						current.strike = true
					} else {
						for c := ev.use; c >= 2; c -= 2 {
							current.strongDepth++
						}
						if ev.use%2 == 1 {
							current.emDepth++
						}
					}
				} else {
					if len(dsStack) > 0 {
						current = dsStack[len(dsStack)-1]
						dsStack = dsStack[:len(dsStack)-1]
					}
				}
				// Emit zero-width boundary after each open event so the
				// renderer sees intermediate depth states and can determine
				// nesting order (e.g. strong-then-em vs em-then-strong).
				if ev.open {
					out = append(out, inlineToken{kind: inlineTokenText, text: "", style: toInlineStyle(current)})
				}
			}
			// Emit closer's remaining text after the close event.
			if hasClose && tok.kind == inlineTokenDelimiter && tok.run > 0 {
				s := toInlineStyle(current)
				out = append(out, inlineToken{kind: inlineTokenText, text: tok.text, style: s})
			}
			continue
		}

		s := toInlineStyle(current)
		switch tok.kind {
		case inlineTokenDelimiter:
			if tok.run > 0 {
				out = append(out, inlineToken{kind: inlineTokenText, text: tok.text, style: s})
			}
		case inlineTokenText:
			out = append(out, inlineToken{kind: inlineTokenText, text: tok.text, style: mergeInlineStyles(s, tok.style)})
		case inlineTokenEvent:
			tok.event.Style = mergeInlineStyles(s, tok.event.Style)
			out = append(out, tok)
		case inlineTokenSoftBreak:
			out = append(out, inlineToken{kind: inlineTokenSoftBreak})
		case inlineTokenLineBreak:
			out = append(out, inlineToken{kind: inlineTokenLineBreak})
		case inlineTokenLinkOpen, inlineTokenLinkClose:
			out = append(out, tok)
		}
	}
	return out, out
}

func applyDelimiterStyle(style InlineStyle, count int, marker byte) InlineStyle {
	out := style
	if marker == '~' {
		out.Strike = true
		return out
	}
	for count >= 2 {
		out.Strong = true
		count -= 2
	}
	if count == 1 {
		out.Emphasis = true
	}
	return out
}

// tokenizeLinkContent tokenizes and resolves emphasis in link label
// text, then applies the link style to every resulting token.
// tokenizeLinkLabel tokenizes a link label's raw text into inline tokens
// WITHOUT applying the link style. The caller wraps these in linkOpen/linkClose.
func tokenizeLinkLabel(labelRaw string, span Span, refs map[string]linkReference, gfmAutolinks bool, scanners []InlineScanner) []inlineToken {
	label := decodeCharacterReferences(unescapeBackslashPunctuation(labelRaw))
	if label == "" {
		return []inlineToken{{kind: inlineTokenText, text: ""}}
	}
	return tokenizeInline(label, span, refs, gfmAutolinks, scanners)
}

func tokenizeLinkContent(labelRaw string, linkStyle InlineStyle, span Span, refs map[string]linkReference, gfmAutolinks bool, scanners []InlineScanner) []inlineToken {
	label := decodeCharacterReferences(unescapeBackslashPunctuation(labelRaw))
	if label == "" {
		return []inlineToken{{kind: inlineTokenText, text: "", style: linkStyle}}
	}
	inner := tokenizeInline(label, span, refs, gfmAutolinks, scanners)
	inner = resolveEmphasis(inner)
	for i := range inner {
		if inner[i].style.Image && inner[i].style.GetHasLink() && linkStyle.GetHasLink() {
			// Image inside a link: keep the image's own Link (src),
			// store the outer link in ImageLink for the renderer.
			if inner[i].style.LinkData == nil {
				inner[i].style.LinkData = &LinkData{}
			}
			inner[i].style.LinkData.ImageLink = linkStyle.LinkData.Link
			inner[i].style.LinkData.ImageLinkTitle = linkStyle.LinkData.LinkTitle
		} else {
			inner[i].style = mergeInlineStyles(inner[i].style, linkStyle)
		}
	}
	return inner
}

func mergeInlineStyles(base, add InlineStyle) InlineStyle {
	out := base
	out.Emphasis = out.Emphasis || add.Emphasis
	out.Strong = out.Strong || add.Strong
	out.Strike = out.Strike || add.Strike
	out.Code = out.Code || add.Code
	out.RawHTML = out.RawHTML || add.RawHTML
	out.Image = out.Image || add.Image
	// Depths are additive: outer emphasis + inner emphasis = nested.
	out.EmphasisDepth += add.EmphasisDepth
	out.StrongDepth += add.StrongDepth
	if out.EmphasisDepth > 0 {
		out.Emphasis = true
	}
	if out.StrongDepth > 0 {
		out.Strong = true
	}
	if add.LinkData != nil && add.LinkData.HasLink {
		out.LinkData = add.LinkData
	}
	return out
}

func coalesceInlineTokensInto(tokens []inlineToken, span Span, events *[]Event) {
	if len(tokens) == 0 {
		return
	}
	start := len(*events)
	var linkStack []InlineStyle
	currentLink := func() InlineStyle {
		if len(linkStack) > 0 {
			return linkStack[len(linkStack)-1]
		}
		return InlineStyle{}
	}
	for _, tok := range tokens {
		switch tok.kind {
		case inlineTokenLinkOpen:
			linkStack = append(linkStack, tok.style)
		case inlineTokenLinkClose:
			if len(linkStack) > 0 {
				linkStack = linkStack[:len(linkStack)-1]
			}
		case inlineTokenText:
			s := tok.style
			if ls := currentLink(); ls.GetHasLink() {
				if s.Image && s.GetHasLink() {
					// Image inside link: keep image's own Link, set ImageLink.
					if s.LinkData == nil {
						s.LinkData = &LinkData{}
					}
					s.LinkData.ImageLink = ls.LinkData.Link
					s.LinkData.ImageLinkTitle = ls.LinkData.LinkTitle
				} else {
					s = mergeInlineStyles(s, ls)
				}
			}
			*events = append(*events, Event{Kind: EventText, Text: tok.text, Style: s, Span: span})
		case inlineTokenEvent:
			ev := tok.event
			if ls := currentLink(); ls.GetHasLink() {
				ev.Style = mergeInlineStyles(ev.Style, ls)
			}
			*events = append(*events, ev)
		case inlineTokenSoftBreak:
			*events = append(*events, Event{Kind: EventSoftBreak, Span: span})
		case inlineTokenLineBreak:
			*events = append(*events, Event{Kind: EventLineBreak, Span: span})
		}
	}
	coalesceTextInPlace(events, start)
}

func parseInlineImage(text string, span Span) (Event, string, bool) {
	parsed, rest, ok := parseInlineImageAsLink(text, span)
	if !ok {
		return Event{}, text, false
	}
	parsed.Style.Image = true
	parsed.Text = plainTextFromInline(parsed.Text, nil)
	return parsed, rest, true
}

func parseReferenceImage(text string, span Span, refs map[string]linkReference) (Event, string, bool) {
	if len(refs) == 0 || !strings.HasPrefix(text, "![") {
		return Event{}, text, false
	}
	ev, rest, _, ok := parseReferenceLink(text[1:], span, refs)
	if !ok {
		return Event{}, text, false
	}
	ev.Style.Image = true
	ev.Text = plainTextFromInline(ev.Text, refs)
	return ev, text[len(text)-len(rest):], true
}

func parseInlineImageAsLink(text string, span Span) (Event, string, bool) {
	if !strings.HasPrefix(text, "![") {
		return Event{}, text, false
	}
	// Images may contain links in their alt text, so skip the
	// nested-link rejection.
	ev, rest, _, ok := parseInlineLinkInner(text[1:], span, false)
	if !ok {
		return Event{}, text, false
	}
	rest = text[len(text)-len(rest):]
	return ev, rest, true
}

// plainTextFromInline resolves inline markup in text and returns only
// the text content. Used for image alt text per CommonMark §6.4:
// "The text content is the text with inline markup resolved and stripped."
func plainTextFromInline(text string, refs map[string]linkReference) string {
	if !hasInlineSyntax(text, false, nil) {
		return text
	}
	tokens := tokenizeInline(text, Span{}, refs, false, nil)
	tokens = resolveEmphasis(tokens)
	var b strings.Builder
	for _, tok := range tokens {
		switch tok.kind {
		case inlineTokenText:
			// For images/links inside alt text, extract their text recursively.
			if tok.style.Image {
				// Already stripped by parseInlineImage.
				b.WriteString(tok.text)
			} else if tok.style.GetHasLink() {
				b.WriteString(tok.text)
			} else {
				b.WriteString(tok.text)
			}
		case inlineTokenSoftBreak:
			b.WriteByte('\n')
		case inlineTokenLineBreak:
			b.WriteByte('\n')
		case inlineTokenDelimiter:
			// Unmatched delimiters become literal text.
			if tok.run > 0 {
				b.WriteString(tok.text)
			}
		}
	}
	return b.String()
}

func parseInlineLink(text string, span Span) (Event, string, string, bool) {
	return parseInlineLinkInner(text, span, true)
}

func parseInlineLinkInner(text string, span Span, rejectNestedLinks bool) (Event, string, string, bool) {
	closeText := matchingLinkLabelEnd(text)
	if closeText < 0 || closeText+1 >= len(text) || text[closeText+1] != '(' {
		return Event{}, text, "", false
	}
	labelRaw := text[1:closeText]
	// Links cannot nest (CommonMark spec §6.5). If the label text
	// itself contains an inline link, this outer link is invalid.
	// Images are allowed to contain links, so this check is skipped
	// when called from the image path.
	if rejectNestedLinks && containsInlineLink(labelRaw) {
		return Event{}, text, "", false
	}
	label := decodeCharacterReferences(unescapeBackslashPunctuation(labelRaw))
	dest, title, end, ok := parseInlineLinkTail(text[closeText+2:])
	if !ok {
		return Event{}, text, "", false
	}
	return Event{Kind: EventText, Text: label, Style: InlineStyle{LinkData: &LinkData{Link: dest, LinkTitle: title, HasLink: true}}, Span: span}, text[closeText+2+end:], labelRaw, true
}

// containsInlineLinkOrRef reports whether text contains a valid inline link
// [...](...) or reference link [...][...] / [...]. Used to enforce the
// no-nested-links rule. Images are not counted as nested links.
func containsInlineLinkOrRef(text string, refs map[string]linkReference) bool {
	if containsInlineLink(text) {
		return true
	}
	if len(refs) > 0 {
		for i := 0; i < len(text); i++ {
			if text[i] == '\\' && i+1 < len(text) {
				i++
				continue
			}
			if text[i] == '!' && i+1 < len(text) && text[i+1] == '[' {
				i++ // skip image markers
				continue
			}
			if text[i] != '[' {
				continue
			}
			if _, _, _, ok := parseReferenceLink(text[i:], Span{}, refs); ok {
				return true
			}
		}
	}
	return false
}

// containsInlineLink reports whether text contains a valid inline link
// [...](...). Used to enforce the no-nested-links rule.
// Images (![...](...)) are not counted as nested links.
func containsInlineLink(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] == '\\' && i+1 < len(text) {
			i++ // skip escaped char
			continue
		}
		if text[i] == '`' {
			n := countRun(text[i:], '`')
			close := findClosingBackticks(text[i+n:], n)
			if close >= 0 {
				i += n + close + n - 1
				continue
			}
			i += n - 1
			continue
		}
		// Skip images: ![...](...) is allowed inside links.
		if text[i] == '!' && i+1 < len(text) && text[i+1] == '[' {
			sub := text[i+1:]
			close := matchingLinkLabelEnd(sub)
			if close >= 0 && close+1 < len(sub) && sub[close+1] == '(' {
				if _, _, end, ok := parseInlineLinkTail(sub[close+2:]); ok {
					i += 1 + close + 2 + end - 1
					continue
				}
			}
			i++ // skip the '!', '[' will be processed next iteration
			continue
		}
		if text[i] != '[' {
			continue
		}
		// Try to parse an inline link starting at text[i:].
		sub := text[i:]
		close := matchingLinkLabelEnd(sub)
		if close < 0 || close+1 >= len(sub) || sub[close+1] != '(' {
			continue
		}
		if _, _, _, ok := parseInlineLinkTail(sub[close+2:]); ok {
			return true
		}
	}
	return false
}

func parseReferenceLink(text string, span Span, refs map[string]linkReference) (Event, string, string, bool) {
	closeLabel := matchingLinkLabelEnd(text)
	if closeLabel <= 0 {
		return Event{}, text, "", false
	}
	labelRaw := text[1:closeLabel]
	// Links cannot nest (CommonMark spec §6.5).
	if containsInlineLinkOrRef(labelRaw, refs) {
		return Event{}, text, "", false
	}
	labelText := decodeCharacterReferences(unescapeBackslashPunctuation(labelRaw))
	end := closeLabel + 1

	// Try full reference [text][label] or collapsed [text][].
	// The spec forbids spaces between the two bracket pairs.
	// The second label must not contain unescaped brackets.
	if end < len(text) && text[end] == '[' {
		closeRef := matchingStrictLinkLabelEnd(text[end:])
		if closeRef >= 0 {
			if closeRef > 1 {
				// Full reference: [text][label]
				// Normalize the raw label — no unescaping (spec §6.3).
				refLabelRaw := text[end+1 : end+closeRef]
				ref, ok := refs[normalizeReferenceLabel(refLabelRaw)]
				if ok {
					return Event{Kind: EventText, Text: labelText, Style: InlineStyle{LinkData: &LinkData{Link: ref.dest, LinkTitle: ref.title, HasLink: true}}, Span: span}, text[end+closeRef+1:], labelRaw, true
				}
				// Full reference label not found — don't fall through to shortcut.
				return Event{}, text, "", false
			}
			// Collapsed reference: [text][]
			// Use raw first label for lookup.
			ref, ok := refs[normalizeReferenceLabel(labelRaw)]
			if ok {
				return Event{Kind: EventText, Text: labelText, Style: InlineStyle{LinkData: &LinkData{Link: ref.dest, LinkTitle: ref.title, HasLink: true}}, Span: span}, text[end+closeRef+1:], labelRaw, true
			}
			return Event{}, text, "", false
		}
		// No matching ] for the second [ — fall through to shortcut.
	}

	// Shortcut reference: [text]
	// Use raw first label for lookup.
	ref, ok := refs[normalizeReferenceLabel(labelRaw)]
	if !ok {
		return Event{}, text, "", false
	}
	return Event{Kind: EventText, Text: labelText, Style: InlineStyle{LinkData: &LinkData{Link: ref.dest, LinkTitle: ref.title, HasLink: true}}, Span: span}, text[end:], labelRaw, true
}

func parseLinkReferenceDefinition(line string) (string, linkReference, bool) {
	label, text, rest, ok := parseLinkReferenceDefinitionStart(line)
	if !ok {
		return "", linkReference{}, false
	}
	dest, title, hasDest, hasTitle, pending, ok := parseLinkReferenceDefinitionTail(text, rest)
	if !ok || pending || !hasDest {
		return "", linkReference{}, false
	}
	if !hasTitle {
		title = ""
	}
	return label, linkReference{dest: dest, title: title}, true
}

func parseLinkReferenceDefinitionStart(line string) (string, string, int, bool) {
	indent, indentBytes := leadingIndent(line)
	if indent > 3 {
		return "", "", 0, false
	}
	text := line[indentBytes:]
	// Link labels in definitions must not contain unescaped brackets.
	closeLabel := matchingStrictLinkLabelEnd(text)
	if closeLabel <= 0 || closeLabel+1 >= len(text) || text[closeLabel+1] != ':' {
		return "", "", 0, false
	}
	// Normalize the raw label text — the spec does not unescape
	// or decode character references during normalization.
	label := normalizeReferenceLabel(text[1:closeLabel])
	if label == "" {
		return "", "", 0, false
	}
	return label, text, closeLabel + 2, true
}

func parseLinkReferenceDefinitionTail(text string, start int) (dest string, title string, hasDest bool, hasTitle bool, pending bool, ok bool) {
	i := skipMarkdownSpace(text, start)
	if i == len(text) {
		return "", "", false, false, true, true
	}
	dest, next, ok := parseInlineLinkDestination(text, i)
	if !ok || next == i {
		return "", "", false, false, false, false
	}
	spaceAfterDest := skipMarkdownSpace(text, next)
	i = spaceAfterDest
	if i < len(text) {
		// Title must be separated from destination by whitespace.
		if spaceAfterDest == next {
			return "", "", false, false, false, false
		}
		parsedTitle, next, ok := parseInlineLinkTitle(text, i)
		if !ok {
			return "", "", false, false, false, false
		}
		title = parsedTitle
		i = skipMarkdownSpace(text, next)
	}
	if i != len(text) {
		return "", "", false, false, false, false
	}
	return dest, title, true, title != "", title == "", true
}

// detectPendingTitle checks if text[start:] begins a title that doesn't
// close on this line. Returns the opener byte and the content after the
// opener, or ok=false if no pending title is detected.
func detectPendingTitle(text string, start int) (opener byte, content string, ok bool) {
	if start >= len(text) {
		return 0, "", false
	}
	c := text[start]
	if c != '"' && c != '\'' && c != '(' {
		return 0, "", false
	}
	closer := c
	if c == '(' {
		closer = ')'
	}
	// Check that the closer is NOT on this line (otherwise parseInlineLinkTitle
	// would have succeeded).
	escaped := false
	for i := start + 1; i < len(text); i++ {
		if escaped {
			escaped = false
			continue
		}
		if text[i] == '\\' {
			escaped = true
			continue
		}
		if text[i] == closer {
			return 0, "", false // closer found on same line
		}
		if c == '(' && text[i] == '(' {
			return 0, "", false // unescaped ( in paren title
		}
	}
	return c, text[start+1:], true
}

func normalizeReferenceLabel(label string) string {
	// Fast path: if the label has no leading/trailing whitespace,
	// no consecutive whitespace, and is already lowercase ASCII,
	// we can return it without allocation.
	if isNormalized(label) {
		return label
	}
	fields := strings.Fields(label)
	if len(fields) == 0 {
		return ""
	}
	return unicodeCaseFold(strings.Join(fields, " "))
}

// isNormalized reports whether label is already in normalized form:
// trimmed, single-spaced, and lowercase.
func isNormalized(s string) bool {
	if len(s) == 0 {
		return false
	}
	if s[0] == ' ' || s[0] == '\t' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t' {
		return false
	}
	prevSpace := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			if prevSpace {
				return false
			}
			prevSpace = true
			continue
		}
		prevSpace = false
		if c >= 'A' && c <= 'Z' {
			return false
		}
		if c >= 0x80 {
			// Non-ASCII: might need case folding, bail to slow path.
			return false
		}
	}
	return true
}

// unicodeCaseFold performs a simple Unicode case fold. It handles
// the standard ToLower mapping plus the special case of
// U+1E9E LATIN CAPITAL LETTER SHARP S (ẞ) which folds to "ss".
func unicodeCaseFold(s string) string {
	if !strings.ContainsRune(s, '\u1e9e') {
		return strings.ToLower(s)
	}
	var b strings.Builder
	for _, r := range s {
		if r == '\u1e9e' {
			b.WriteString("ss")
		} else {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// indexFold returns the index of the first case-insensitive match of
// substr (assumed lowercase ASCII) in s, or -1. Zero allocations.
func indexFold(s, substr string) int {
	if len(substr) == 0 {
		return 0
	}
	if len(substr) > len(s) {
		return -1
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if equalFoldASCII(s[i:i+len(substr)], substr) {
			return i
		}
	}
	return -1
}

// containsFold reports whether substr (assumed lowercase ASCII) is
// contained in s, using case-insensitive comparison. Zero allocations.
func containsFold(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	if len(substr) > len(s) {
		return false
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if equalFoldASCII(s[i:i+len(substr)], substr) {
			return true
		}
	}
	return false
}

// equalFoldASCII reports whether s equals lowerASCII under ASCII-only case
// folding. lowerASCII must already be lowercase ASCII. This intentionally
// avoids strings.EqualFold's Unicode handling for CommonMark's ASCII-only
// HTML tag and URI scheme matching hot paths.
func equalFoldASCII(s, lowerASCII string) bool {
	if len(s) != len(lowerASCII) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := asciiLower(s[i])
		if c != lowerASCII[i] {
			return false
		}
	}
	return true
}

func asciiLower(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// parseRawHTMLTag tries to parse a raw HTML tag at the start of text.
// Returns the full tag string and true if successful.
// Handles open tags, closing tags, comments, processing instructions,
// declarations, and CDATA sections per CommonMark spec §6.6.
func parseRawHTMLTag(text string) (string, bool) {
	if len(text) < 2 || text[0] != '<' {
		return "", false
	}
	// Closing tag: </tagname>
	if len(text) >= 3 && text[1] == '/' {
		if !isASCIILetter(text[2]) {
			return "", false
		}
		i := 3
		for i < len(text) && (isASCIILetter(text[i]) || isASCIIDigit(text[i]) || text[i] == '-') {
			i++
		}
		for i < len(text) && (text[i] == ' ' || text[i] == '\t' || text[i] == '\n') {
			i++
		}
		if i < len(text) && text[i] == '>' {
			return text[:i+1], true
		}
		return "", false
	}
	// Comment: <!-- ... -->
	// Per HTML spec, <!--> and <!---> are valid (empty) comments.
	if strings.HasPrefix(text, "<!--") {
		// Degenerate case: <!-->
		if len(text) > 4 && text[4] == '>' {
			return text[:5], true
		}
		// Degenerate case: --->
		if len(text) > 5 && text[4] == '-' && text[5] == '>' {
			return text[:6], true
		}
		end := strings.Index(text[4:], "-->")
		if end >= 0 {
			return text[:4+end+3], true
		}
		return "", false
	}
	// Processing instruction: <? ... ?>
	if strings.HasPrefix(text, "<?") {
		end := strings.Index(text[2:], "?>")
		if end >= 0 {
			return text[:2+end+2], true
		}
		return "", false
	}
	// Declaration: <! LETTER ... >
	if len(text) >= 3 && text[1] == '!' && isASCIILetter(text[2]) {
		for i := 3; i < len(text); i++ {
			if text[i] == '>' {
				return text[:i+1], true
			}
		}
		return "", false
	}
	// CDATA: <![CDATA[ ... ]]>
	if strings.HasPrefix(text, "<![CDATA[") {
		end := strings.Index(text[9:], "]]>")
		if end >= 0 {
			return text[:9+end+3], true
		}
		return "", false
	}
	// Open tag: <tagname attributes? /? >
	if !isASCIILetter(text[1]) {
		return "", false
	}
	i := 2
	for i < len(text) && (isASCIILetter(text[i]) || isASCIIDigit(text[i]) || text[i] == '-') {
		i++
	}
	// Skip attributes.
	for i < len(text) {
		// Skip whitespace.
		j := i
		for i < len(text) && (text[i] == ' ' || text[i] == '\t' || text[i] == '\n') {
			i++
		}
		if i >= len(text) {
			return "", false
		}
		if text[i] == '>' {
			return text[:i+1], true
		}
		if text[i] == '/' {
			if i+1 < len(text) && text[i+1] == '>' {
				return text[:i+2], true
			}
			return "", false
		}
		// Must have whitespace before attribute.
		if i == j {
			return "", false
		}
		// Attribute name: [a-zA-Z_:][a-zA-Z0-9_.:-]*
		if !isASCIILetter(text[i]) && text[i] != '_' && text[i] != ':' {
			return "", false
		}
		i++
		for i < len(text) && (isASCIILetter(text[i]) || isASCIIDigit(text[i]) || text[i] == '_' || text[i] == '.' || text[i] == ':' || text[i] == '-') {
			i++
		}
		// Optional value specification.
		j = i
		for i < len(text) && (text[i] == ' ' || text[i] == '\t' || text[i] == '\n') {
			i++
		}
		if i < len(text) && text[i] == '=' {
			i++
			for i < len(text) && (text[i] == ' ' || text[i] == '\t' || text[i] == '\n') {
				i++
			}
			if i >= len(text) {
				return "", false
			}
			if text[i] == '\'' || text[i] == '"' {
				quote := text[i]
				i++
				for i < len(text) && text[i] != quote {
					i++
				}
				if i >= len(text) {
					return "", false
				}
				i++ // skip closing quote
			} else {
				// Unquoted value.
				if text[i] == ' ' || text[i] == '\t' || text[i] == '\n' || text[i] == '"' || text[i] == '\'' || text[i] == '=' || text[i] == '<' || text[i] == '>' || text[i] == '`' {
					return "", false
				}
				for i < len(text) && text[i] != ' ' && text[i] != '\t' && text[i] != '\n' && text[i] != '"' && text[i] != '\'' && text[i] != '=' && text[i] != '<' && text[i] != '>' && text[i] != '`' {
					i++
				}
			}
		} else {
			i = j // no value, restore position
		}
	}
	return "", false
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isASCIIDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

// htmlBlockType6Tags are the block-level HTML element names for type 6.
var htmlBlockType6Tags = map[string]bool{
	"address": true, "article": true, "aside": true, "base": true,
	"basefont": true, "blockquote": true, "body": true, "caption": true,
	"center": true, "col": true, "colgroup": true, "dd": true,
	"details": true, "dialog": true, "dir": true, "div": true,
	"dl": true, "dt": true, "fieldset": true, "figcaption": true,
	"figure": true, "footer": true, "form": true, "frame": true,
	"frameset": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "head": true,
	"header": true, "hr": true, "html": true, "iframe": true,
	"legend": true, "li": true, "link": true, "main": true,
	"menu": true, "menuitem": true, "nav": true, "noframes": true,
	"ol": true, "optgroup": true, "option": true, "p": true,
	"param": true, "search": true, "section": true, "summary": true,
	"table": true, "tbody": true, "td": true, "tfoot": true,
	"th": true, "thead": true, "title": true, "tr": true,
	"track": true, "ul": true,
}

// detectHTMLBlockStart checks if a line starts an HTML block.
// Returns the block type (1-7) or 0 if not an HTML block.
func detectHTMLBlockStart(line string) (int, string) {
	indent, indentBytes := leadingIndent(line)
	if indent > 3 {
		return 0, ""
	}
	s := line[indentBytes:]
	if len(s) < 2 || s[0] != '<' {
		return 0, ""
	}

	// Type 1: <pre, <script, <style, <textarea (case-insensitive)
	for _, tag := range []string{"pre", "script", "style", "textarea"} {
		if matchHTMLBlockTag(s[1:], tag) {
			return 1, "</" + tag + ">"
		}
	}

	// Type 2: <!--
	if strings.HasPrefix(s, "<!--") {
		return 2, "-->"
	}

	// Type 3: <?
	if strings.HasPrefix(s, "<?") {
		return 3, "?>"
	}

	// Type 4: <! + ASCII letter
	if len(s) >= 3 && s[1] == '!' && isASCIILetter(s[2]) {
		return 4, ">"
	}

	// Type 5: <![CDATA[
	if strings.HasPrefix(s, "<![CDATA[") {
		return 5, "]]>"
	}

	// Type 6: block-level tag
	closing := s[1] == '/'
	tagStart := 1
	if closing {
		tagStart = 2
	}
	if tagStart < len(s) && isASCIILetter(s[tagStart]) {
		i := tagStart + 1
		for i < len(s) && (isASCIILetter(s[i]) || isASCIIDigit(s[i])) {
			i++
		}
		tagName := strings.ToLower(s[tagStart:i])
		if htmlBlockType6Tags[tagName] {
			if i >= len(s) || s[i] == ' ' || s[i] == '\t' || s[i] == '>' || (s[i] == '/' && i+1 < len(s) && s[i+1] == '>') || s[i] == '\n' {
				return 6, ""
			}
		}
	}

	// Type 7: complete open or closing tag on its own line
	if tag, ok := parseRawHTMLTag(s); ok {
		rest := strings.TrimSpace(s[len(tag):])
		if rest == "" {
			tagName := extractTagName(s)
			tn := strings.ToLower(tagName)
			// Opening tags for type-1 elements are already handled above.
			// Closing tags (</script>, </style>, </pre>) are allowed as type 7.
			isClosing := len(s) >= 2 && s[1] == '/'
			if !isClosing {
				switch tn {
				case "pre", "script", "style", "textarea":
					// Opening tag — already handled as type 1
					return 0, ""
				}
			}
			return 7, ""
		}
	}

	return 0, ""
}

// matchHTMLBlockTag checks if text starts with a tag name (case-insensitive)
// followed by space, tab, >, newline, or end of string.
func matchHTMLBlockTag(text, tag string) bool {
	if len(text) < len(tag) {
		return false
	}
	if !strings.EqualFold(text[:len(tag)], tag) {
		return false
	}
	if len(text) == len(tag) {
		return true
	}
	c := text[len(tag)]
	return c == ' ' || c == '\t' || c == '>' || c == '\n'
}

// extractTagName extracts the tag name from an HTML tag string like "<div ...>".
func extractTagName(s string) string {
	if len(s) < 2 || s[0] != '<' {
		return ""
	}
	i := 1
	if i < len(s) && s[i] == '/' {
		i++
	}
	start := i
	for i < len(s) && (isASCIILetter(s[i]) || isASCIIDigit(s[i]) || s[i] == '-') {
		i++
	}
	return s[start:i]
}

func matchingLinkLabelEnd(text string) int {
	return matchingBracketEnd(text, false)
}

// matchingStrictLinkLabelEnd finds the closing ] for a link label,
// rejecting any unescaped [ inside the label (CommonMark spec §6.3).
func matchingStrictLinkLabelEnd(text string) int {
	return matchingBracketEnd(text, true)
}

func matchingBracketEnd(text string, strict bool) int {
	if text == "" || text[0] != '[' {
		return -1
	}
	depth := 0
	escaped := false
	for i := 1; i < len(text); i++ {
		c := text[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			continue
		}
		// Code spans take precedence over link structure (CommonMark
		// spec §6.3). Skip over any code span so that brackets inside
		// it are not counted.
		if c == '`' {
			n := countRun(text[i:], '`')
			close := findClosingBackticks(text[i+n:], n)
			if close < 0 {
				// No matching close — the backticks are literal.
				i += n - 1
				continue
			}
			i += n + close + n - 1
			continue
		}
		// Autolinks and raw HTML tags take precedence over link structure.
		// Skip over them so that brackets inside are not counted.
		if c == '<' {
			// Try autolink first.
			if end := strings.IndexByte(text[i+1:], '>'); end >= 0 {
				target := text[i+1 : i+1+end]
				if isURIAutolink(target) || isEmailAutolink(target) {
					i += 1 + end // skip to '>'
					continue
				}
			}
			if tag, ok := parseRawHTMLTag(text[i:]); ok {
				i += len(tag) - 1
				continue
			}
		}
		switch c {
		case '[':
			if strict {
				return -1
			}
			depth++
		case ']':
			if depth == 0 {
				return i
			}
			depth--
		}
	}
	return -1
}

// findClosingBackticks finds the position of a closing backtick sequence
// of exactly n backticks in text. Returns the byte offset of the start of
// the closing sequence, or -1 if not found.
func findClosingBackticks(text string, n int) int {
	for i := 0; i < len(text); {
		if text[i] != '`' {
			i++
			continue
		}
		run := countRun(text[i:], '`')
		if run == n {
			return i
		}
		i += run
	}
	return -1
}

func parseInlineLinkTail(text string) (string, string, int, bool) {
	i := skipMarkdownSpace(text, 0)
	dest, next, ok := parseInlineLinkDestination(text, i)
	if !ok {
		return "", "", 0, false
	}
	i = skipMarkdownSpace(text, next)
	if i < len(text) && text[i] == ')' {
		return dest, "", i + 1, true
	}
	title, next, ok := parseInlineLinkTitle(text, i)
	if !ok {
		return "", "", 0, false
	}
	i = skipMarkdownSpace(text, next)
	if i >= len(text) || text[i] != ')' {
		return "", "", 0, false
	}
	return dest, title, i + 1, true
}

func parseInlineLinkDestination(text string, start int) (string, int, bool) {
	if start >= len(text) {
		return "", start, false
	}
	if text[start] == '<' {
		escaped := false
		for i := start + 1; i < len(text); i++ {
			c := text[i]
			if c == '\n' {
				return "", start, false
			}
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '<' {
				return "", start, false
			}
			if c == '>' {
				dest := decodeCharacterReferences(unescapeBackslashPunctuation(text[start+1 : i]))
				return dest, i + 1, true
			}
		}
		return "", start, false
	}
	if text[start] == ')' || isMarkdownSpace(text[start]) {
		return "", start, true
	}
	escaped := false
	depth := 0
	for i := start; i < len(text); i++ {
		c := text[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && i+1 < len(text) && isEscapablePunctuation(text[i+1]) {
			escaped = true
			continue
		}
		if isMarkdownSpace(c) {
			if depth == 0 {
				return decodeCharacterReferences(unescapeBackslashPunctuation(text[start:i])), i, true
			}
			return "", start, false
		}
		switch c {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return decodeCharacterReferences(unescapeBackslashPunctuation(text[start:i])), i, true
			}
			depth--
		case '<':
			return "", start, false
		}
	}
	if depth == 0 {
		return decodeCharacterReferences(unescapeBackslashPunctuation(text[start:])), len(text), true
	}
	return "", start, false
}

func parseInlineLinkTitle(text string, start int) (string, int, bool) {
	if start >= len(text) {
		return "", start, false
	}
	open := text[start]
	close := open
	if open == '(' {
		close = ')'
	} else if open != '"' && open != '\'' {
		return "", start, false
	}
	escaped := false
	for i := start + 1; i < len(text); i++ {
		c := text[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			continue
		}
		if c == close {
			title := decodeCharacterReferences(unescapeBackslashPunctuation(text[start+1 : i]))
			return title, i + 1, true
		}
		if open == '(' && c == '(' {
			return "", start, false
		}
	}
	return "", start, false
}

func skipMarkdownSpace(text string, start int) int {
	for start < len(text) && isMarkdownSpace(text[start]) {
		start++
	}
	return start
}

func isMarkdownSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func parseCharacterReference(text string) (string, string, bool) {
	end := strings.IndexByte(text, ';')
	if end < 0 || end > 32 {
		return "", text, false
	}
	candidate := text[:end+1]
	decoded, ok := decodeCharacterReference(candidate)
	if !ok {
		return "", text, false
	}
	return decoded, text[end+1:], true
}

func decodeCharacterReferences(text string) string {
	if !strings.Contains(text, "&") {
		return text
	}
	var out strings.Builder
	out.Grow(len(text))
	for len(text) > 0 {
		i := strings.IndexByte(text, '&')
		if i < 0 {
			out.WriteString(text)
			break
		}
		out.WriteString(text[:i])
		decoded, rest, ok := parseCharacterReference(text[i:])
		if !ok {
			out.WriteByte('&')
			text = text[i+1:]
			continue
		}
		out.WriteString(decoded)
		text = rest
	}
	return out.String()
}

func decodeCharacterReference(candidate string) (string, bool) {
	if strings.HasPrefix(candidate, "&#x") || strings.HasPrefix(candidate, "&#X") {
		return decodeNumericCharacterReference(candidate[3:len(candidate)-1], 16)
	}
	if strings.HasPrefix(candidate, "&#") {
		return decodeNumericCharacterReference(candidate[2:len(candidate)-1], 10)
	}
	if !validNamedCharacterReference(candidate) {
		return "", false
	}
	decoded := html.UnescapeString(candidate)
	if decoded == candidate {
		return "", false
	}
	return decoded, true
}

func validNamedCharacterReference(candidate string) bool {
	if len(candidate) < 3 || candidate[0] != '&' || candidate[len(candidate)-1] != ';' {
		return false
	}
	if !isASCIIAlpha(candidate[1]) {
		return false
	}
	for i := 2; i < len(candidate)-1; i++ {
		if !isASCIIAlphaNumeric(candidate[i]) {
			return false
		}
	}
	return true
}

func decodeNumericCharacterReference(digits string, base int) (string, bool) {
	if digits == "" {
		return "", false
	}
	value, err := strconv.ParseInt(digits, base, 32)
	if err != nil || value > utf8.MaxRune {
		return "", false
	}
	r := rune(value)
	if r == 0 || !utf8.ValidRune(r) {
		return "\uFFFD", true
	}
	return string(r), true
}

func findInlineCloser(text string, marker string) int {
	escaped := false
	for i := 0; i <= len(text)-len(marker); i++ {
		if text[i] == '\n' {
			return -1
		}
		if escaped {
			escaped = false
			continue
		}
		if text[i] == '\\' {
			escaped = true
			continue
		}
		if strings.HasPrefix(text[i:], marker) {
			return i
		}
	}
	return -1
}

func parseCodeSpan(text string, span Span) (Event, string, bool) {
	n := countRun(text, '`')
	for i := n; i < len(text); {
		if text[i] != '`' {
			i++
			continue
		}
		closing := countRun(text[i:], '`')
		if closing == n {
			content := normalizeCodeSpan(text[n:i])
			return Event{Kind: EventText, Text: content, Style: InlineStyle{Code: true}, Span: span}, text[i+closing:], true
		}
		i += closing
	}
	return Event{}, text, false
}

func countRun(text string, marker byte) int {
	n := 0
	for n < len(text) && text[n] == marker {
		n++
	}
	return n
}

func normalizeCodeSpan(text string) string {
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, "\r", " ")
	if len(text) >= 2 && text[0] == ' ' && text[len(text)-1] == ' ' && strings.Trim(text, " ") != "" {
		text = text[1 : len(text)-1]
	}
	return text
}

func trimHardBreakMarkerTokens(tokens *[]inlineToken) bool {
	if len(*tokens) == 0 {
		return false
	}
	last := &(*tokens)[len(*tokens)-1]
	if last.kind != inlineTokenText || last.style != (InlineStyle{}) {
		return false
	}
	if strings.HasSuffix(last.text, "\\") {
		last.text = strings.TrimSuffix(last.text, "\\")
		return true
	}
	trimmed := strings.TrimRight(last.text, " ")
	if len(last.text)-len(trimmed) >= 2 {
		last.text = trimmed
		return true
	}
	return false
}

func emphasisDelimRun(prevSource, nextSource string, marker byte, _ int) (bool, bool) {
	prevRune, _ := lastRune(prevSource)
	nextRune, _ := firstRune(nextSource)
	left := isLeftFlanking(prevRune, nextRune)
	right := isRightFlanking(prevRune, nextRune)
	if marker == '*' || marker == '~' {
		return left, right
	}
	if marker == '_' {
		open := left && (!right || isPunctuationRune(prevRune))
		close := right && (!left || isPunctuationRune(nextRune))
		return open, close
	}
	return false, false
}

func firstRune(text string) (rune, bool) {
	if text == "" {
		return 0, false
	}
	r, _ := utf8.DecodeRuneInString(text)
	return r, true
}

func lastRune(text string) (rune, bool) {
	if text == "" {
		return 0, false
	}
	r, _ := utf8.DecodeLastRuneInString(text)
	return r, true
}

func isLeftFlanking(prev, next rune) bool {
	if next == 0 || isSpaceOrControlRune(next) {
		return false
	}
	if isPunctuationRune(next) && !isSpaceOrControlRune(prev) && !isPunctuationRune(prev) {
		return false
	}
	return true
}

func isRightFlanking(prev, next rune) bool {
	if prev == 0 || isSpaceOrControlRune(prev) {
		return false
	}
	if isPunctuationRune(prev) && !isSpaceOrControlRune(next) && !isPunctuationRune(next) {
		return false
	}
	return true
}

func isSpaceOrControlRune(r rune) bool {
	return r == 0 || unicode.IsSpace(r) || unicode.IsControl(r)
}

func isPunctuationRune(r rune) bool {
	return unicode.IsPunct(r) || unicode.IsSymbol(r)
}

func trimHardBreakMarker(events []Event) bool {
	if len(events) == 0 {
		return false
	}
	last := &events[len(events)-1]
	if last.Kind != EventText || last.Style != (InlineStyle{}) {
		return false
	}
	if strings.HasSuffix(last.Text, "\\") {
		last.Text = strings.TrimSuffix(last.Text, "\\")
		return true
	}
	trimmed := strings.TrimRight(last.Text, " ")
	if len(last.Text)-len(trimmed) >= 2 {
		last.Text = trimmed
		return true
	}
	return false
}

func parseAutolink(text string, span Span) (Event, string, bool) {
	end := strings.IndexByte(text, '>')
	if end <= 1 {
		return Event{}, text, false
	}
	target := text[1:end]
	if isURIAutolink(target) {
		return Event{Kind: EventText, Text: target, Style: InlineStyle{LinkData: &LinkData{HasLink: true, Link: target}}, Span: span}, text[end+1:], true
	}
	if isEmailAutolink(target) {
		return Event{Kind: EventText, Text: target, Style: InlineStyle{LinkData: &LinkData{HasLink: true, Link: "mailto:" + target}}, Span: span}, text[end+1:], true
	}
	return Event{}, text, false
}

func parseAutolinkLiteral(text string, span Span, prevSource string) (Event, string, bool) {
	if !autolinkLiteralBoundary(prevSource) {
		return Event{}, text, false
	}
	candidate, _ := scanAutolinkLiteralCandidate(text)
	if candidate == "" {
		return Event{}, text, false
	}
	untrimmed := candidate
	candidate = trimAutolinkLiteralSuffix(candidate)
	if candidate == "" {
		return Event{}, text, false
	}
	switch {
	case len(candidate) > 7 && strings.EqualFold(candidate[:7], "http://"):
		if isURIAutolink(candidate) {
			return Event{Kind: EventText, Text: candidate, Style: InlineStyle{LinkData: &LinkData{HasLink: true, Link: candidate}}, Span: span}, text[len(candidate):], true
		}
	case len(candidate) > 8 && strings.EqualFold(candidate[:8], "https://"):
		if isURIAutolink(candidate) {
			return Event{Kind: EventText, Text: candidate, Style: InlineStyle{LinkData: &LinkData{HasLink: true, Link: candidate}}, Span: span}, text[len(candidate):], true
		}
	case len(candidate) > 6 && strings.EqualFold(candidate[:6], "ftp://"):
		if isURIAutolink(candidate) {
			return Event{Kind: EventText, Text: candidate, Style: InlineStyle{LinkData: &LinkData{HasLink: true, Link: candidate}}, Span: span}, text[len(candidate):], true
		}
	case len(candidate) > 4 && strings.EqualFold(candidate[:4], "www."):
		if isWWWAutolink(candidate) {
			return Event{Kind: EventText, Text: candidate, Style: InlineStyle{LinkData: &LinkData{HasLink: true, Link: "http://" + candidate}}, Span: span}, text[len(candidate):], true
		}
	}
	// For email autolinks: if the untrimmed candidate looks like an
	// email (has @) and the domain ends with - or _, the GFM spec says
	// it's not an autolink — don't try trimmed version either.
	if at := strings.LastIndexByte(untrimmed, '@'); at > 0 {
		domain := untrimmed[at+1:]
		if len(domain) > 0 {
			last := domain[len(domain)-1]
			if last == '-' || last == '_' {
				// Domain ends with disallowed char — not an email autolink.
			} else if isEmailAutolink(candidate) {
				return Event{Kind: EventText, Text: candidate, Style: InlineStyle{LinkData: &LinkData{HasLink: true, Link: "mailto:" + candidate}}, Span: span}, text[len(candidate):], true
			}
		}
	} else if isEmailAutolink(candidate) {
		return Event{Kind: EventText, Text: candidate, Style: InlineStyle{LinkData: &LinkData{HasLink: true, Link: "mailto:" + candidate}}, Span: span}, text[len(candidate):], true
	}
	return Event{}, text, false
}

func isURIAutolink(target string) bool {
	colon := strings.IndexByte(target, ':')
	if colon < 2 || colon > 32 || !isASCIIAlpha(target[0]) {
		return false
	}
	for i := 1; i < colon; i++ {
		c := target[i]
		if !isASCIIAlphaNumeric(c) && c != '.' && c != '+' && c != '-' {
			return false
		}
	}
	for i := colon + 1; i < len(target); i++ {
		c := target[i]
		if c <= ' ' || c == '<' || c == '>' {
			return false
		}
	}
	return colon+1 < len(target)
}

func autolinkLiteralBoundary(prevSource string) bool {
	if prevSource == "" {
		return true
	}
	prev, ok := lastRune(prevSource)
	if !ok {
		return true
	}
	return !isAlphaNumeric(prev)
}

func scanAutolinkLiteralCandidate(text string) (string, string) {
	end := 0
	for end < len(text) {
		c := text[end]
		if c <= ' ' || c == '<' || c == '>' || c == '"' || c == '\'' {
			break
		}
		end++
	}
	if end == 0 {
		return "", text
	}
	return text[:end], text[end:]
}

func trimAutolinkLiteralSuffix(candidate string) string {
	for len(candidate) > 0 {
		switch candidate[len(candidate)-1] {
		case '.', ',', ':', '!', '?', '*', '_', '~':
			candidate = candidate[:len(candidate)-1]
			continue
		case ';':
			// Check for entity-like pattern: &alphanumeric+;
			amp := strings.LastIndexByte(candidate, '&')
			if amp >= 0 {
				entity := candidate[amp+1 : len(candidate)-1]
				allAlnum := len(entity) > 0
				for _, c := range entity {
					if !isAlphaNumeric(c) {
						allAlnum = false
						break
					}
				}
				if allAlnum {
					candidate = candidate[:amp]
					continue
				}
			}
			candidate = candidate[:len(candidate)-1]
			continue
		case ')':
			if strings.Count(candidate, ")") > strings.Count(candidate, "(") {
				candidate = candidate[:len(candidate)-1]
				continue
			}
		case ']':
			if strings.Count(candidate, "]") > strings.Count(candidate, "[") {
				candidate = candidate[:len(candidate)-1]
				continue
			}
		}
		return candidate
	}
	return candidate
}

func nextAutolinkLiteralStart(text string, prevSource string) int {
	for i := 0; i < len(text); i++ {
		switch asciiLower(text[i]) {
		case 'h':
			if i+7 <= len(text) && equalFoldASCII(text[i:i+7], "http://") && autolinkLiteralBoundaryAt(text, i, prevSource) {
				return i
			}
			if i+8 <= len(text) && equalFoldASCII(text[i:i+8], "https://") && autolinkLiteralBoundaryAt(text, i, prevSource) {
				return i
			}
		case 'f':
			if i+6 <= len(text) && equalFoldASCII(text[i:i+6], "ftp://") && autolinkLiteralBoundaryAt(text, i, prevSource) {
				return i
			}
		case 'w':
			if i+4 <= len(text) && equalFoldASCII(text[i:i+4], "www.") && autolinkLiteralBoundaryAt(text, i, prevSource) {
				return i
			}
		case '@':
			start := i
			for start > 0 && isEmailLocalAutolinkByte(text[start-1]) {
				start--
			}
			if start == i || !autolinkLiteralBoundaryAt(text, start, prevSource) {
				continue
			}
			candidate, _ := scanAutolinkLiteralCandidate(text[start:])
			candidate = trimAutolinkLiteralSuffix(candidate)
			if isEmailAutolink(candidate) {
				return start
			}
		}
	}
	return -1
}

func autolinkLiteralBoundaryAt(text string, start int, prevSource string) bool {
	if start < 0 || start > len(text) {
		return false
	}
	if start == 0 {
		return autolinkLiteralBoundary(prevSource)
	}
	prev, _ := lastRune(text[:start])
	return !isAlphaNumeric(prev)
}

func isEmailLocalAutolinkByte(c byte) bool {
	if isASCIIAlphaNumeric(c) {
		return true
	}
	switch c {
	case '.', '!', '#', '$', '%', '&', '\'', '*', '+', '/', '=', '?', '^', '_', '`', '{', '|', '}', '~', '-':
		return true
	}
	return false
}

func isWWWAutolink(target string) bool {
	if len(target) < 5 || !strings.EqualFold(target[:4], "www.") {
		return false
	}
	hostPort := target[4:]
	if hostPort == "" {
		return false
	}
	end := len(hostPort)
	for i := 0; i < len(hostPort); i++ {
		switch hostPort[i] {
		case '/', '?', '#':
			end = i
			i = len(hostPort)
		}
	}
	hostPort = hostPort[:end]
	if hostPort == "" {
		return false
	}
	host := hostPort
	if colon := strings.LastIndexByte(hostPort, ':'); colon >= 0 {
		port := hostPort[colon+1:]
		if port == "" {
			return false
		}
		for i := 0; i < len(port); i++ {
			if port[i] < '0' || port[i] > '9' {
				return false
			}
		}
		host = hostPort[:colon]
	}
	if !isDomainAutolink(host) {
		return false
	}
	// GFM: no underscores in the last two segments of the domain.
	labels := strings.Split(host, ".")
	for i := len(labels) - 1; i >= 0 && i >= len(labels)-2; i-- {
		if strings.ContainsRune(labels[i], '_') {
			return false
		}
	}
	return true
}

func isEmailAutolink(target string) bool {
	at := strings.IndexByte(target, '@')
	if at <= 0 || at != strings.LastIndexByte(target, '@') || at == len(target)-1 {
		return false
	}
	local, domain := target[:at], target[at+1:]
	if strings.Contains(domain, ".") == false {
		return false
	}
	for i := 0; i < len(local); i++ {
		if !isEmailLocalAutolinkByte(local[i]) {
			return false
		}
	}
	return isDomainAutolink(domain)
}

func isDomainAutolink(domain string) bool {
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !isASCIIAlphaNumeric(c) && c != '-' && c != '_' {
				return false
			}
		}
	}
	// GFM: last character of domain must not be - or _.
	last := domain[len(domain)-1]
	if last == '-' || last == '_' {
		return false
	}
	return true
}

func nextInlineDelimiter(text string) int {
	if i := strings.IndexAny(text, "\n\\*~_`![<&"); i >= 0 {
		return i
	}
	return len(text)
}

func isInlineDelimiterByte(c byte) bool {
	return c == '\n' || c == '\\' || c == '*' || c == '~' || c == '_' || c == '`' || c == '!' || c == '[' || c == '<' || c == '&'
}

// coalesceTextInPlace merges adjacent text events with the same style
// in (*events)[start:]. It compacts the tail in-place and trims the slice.
func coalesceTextInPlace(events *[]Event, start int) {
	tail := (*events)[start:]
	if len(tail) < 2 {
		return
	}
	write := start
	current := tail[0]
	var builder strings.Builder
	merging := false
	flush := func() {
		if merging {
			current.Text = builder.String()
			builder.Reset()
			merging = false
		}
		(*events)[write] = current
		write++
	}
	for _, ev := range tail[1:] {
		if current.Kind == EventText && ev.Kind == EventText && sameCoalesceStyle(current.Style, ev.Style) {
			if !merging {
				builder.WriteString(current.Text)
				merging = true
			}
			builder.WriteString(ev.Text)
			current.Span.End = ev.Span.End
			continue
		}
		flush()
		current = ev
	}
	flush()
	*events = (*events)[:write]
}

func sameStyle(a, b InlineStyle) bool {
	return a.Emphasis == b.Emphasis && a.Strong == b.Strong && a.Strike == b.Strike && a.Code == b.Code && a.GetHasLink() == b.GetHasLink() && a.GetLink() == b.GetLink() && a.GetLinkTitle() == b.GetLinkTitle() && a.Image == b.Image && a.GetImageLink() == b.GetImageLink() && a.GetImageLinkTitle() == b.GetImageLinkTitle() && a.RawHTML == b.RawHTML
}

// sameCoalesceStyle is like sameStyle but also compares emphasis/strong
// depth to prevent coalescing across nesting boundaries.
func sameCoalesceStyle(a, b InlineStyle) bool {
	return sameStyle(a, b) && a.EmphasisDepth == b.EmphasisDepth && a.StrongDepth == b.StrongDepth
}

func isEscapablePunctuation(c byte) bool {
	switch c {
	case '!', '"', '#', '$', '%', '&', '\'', '(', ')', '*', '+', ',', '-', '.', '/', ':', ';', '<', '=', '>', '?', '@', '[', '\\', ']', '^', '_', '`', '{', '|', '}', '~':
		return true
	}
	return false
}

func unescapeBackslashPunctuation(text string) string {
	if !strings.Contains(text, "\\") {
		return text
	}
	var out strings.Builder
	out.Grow(len(text))
	for i := 0; i < len(text); i++ {
		if text[i] == '\\' && i+1 < len(text) && isEscapablePunctuation(text[i+1]) {
			out.WriteByte(text[i+1])
			i++
			continue
		}
		out.WriteByte(text[i])
	}
	return out.String()
}

func isASCIIAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isASCIIAlphaNumeric(c byte) bool {
	return isASCIIAlpha(c) || (c >= '0' && c <= '9')
}

func isAlphaNumeric(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

var _ = isAlphaNumeric
