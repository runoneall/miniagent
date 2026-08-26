package stream

type Parser interface {
	Write(chunk []byte) ([]Event, error)
	Flush() ([]Event, error)
	Reset()
}

type ParserOption func(*ParserConfig)

type ParserConfig struct {
	InlineMode     InlineMode
	GFMAutolinks   bool
	InlineScanners []InlineScanner
}

type InlineScanner interface {
	TriggerBytes() string
	ScanInline(input string, ctx InlineContext) (InlineScanResult, bool)
}

type InlineContext struct {
	Span Span
}

type InlineScanResult struct {
	Consume int
	Event   Event
}

type InlineMode int

const InlineParagraphBoundary InlineMode = iota

func defaultParserConfig() ParserConfig {
	return ParserConfig{InlineMode: InlineParagraphBoundary}
}

func WithGFMAutolinks() ParserOption {
	return func(c *ParserConfig) {
		c.GFMAutolinks = true
	}
}

func WithInlineScanner(scanner InlineScanner) ParserOption {
	return func(c *ParserConfig) {
		if scanner != nil {
			c.InlineScanners = append(c.InlineScanners, scanner)
		}
	}
}
