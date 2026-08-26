package terminal

type Theme struct {
	Text          string
	Muted         string
	Heading       string
	Code          string
	Link          string
	TableBorder   string
	BlockquoteBar string
	ListMarker    string
	ThematicBreak string
	CodeBorder    string
	Syntax        SyntaxTheme
}

type SyntaxTheme struct {
	Text     string
	Comment  string
	Keyword  string
	String   string
	Number   string
	Type     string
	Function string
	Operator string
	Inserted string
	Deleted  string
	Heading  string
}

func DefaultTheme() Theme {
	return MonokaiTheme()
}

func MonokaiTheme() Theme {
	return Theme{
		Text:          monokaiForeground,
		Muted:         monokaiComment,
		Heading:       monokaiGreen,
		Code:          monokaiYellow,
		Link:          monokaiBlue,
		TableBorder:   monokaiComment,
		BlockquoteBar: monokaiComment,
		ListMarker:    monokaiComment,
		ThematicBreak: monokaiComment,
		CodeBorder:    monokaiComment,
		Syntax: SyntaxTheme{
			Text:     monokaiForeground,
			Comment:  monokaiComment,
			Keyword:  monokaiRed,
			String:   monokaiYellow,
			Number:   monokaiPurple,
			Type:     monokaiBlue,
			Function: monokaiGreen,
			Operator: monokaiRed,
			Inserted: monokaiGreen,
			Deleted:  monokaiRed,
			Heading:  monokaiComment,
		},
	}
}

func NordTheme() Theme {
	return Theme{
		Text:          "\x1b[38;2;216;222;233m",
		Muted:         "\x1b[38;2;129;161;193m",
		Heading:       "\x1b[38;2;163;190;140m",
		Code:          "\x1b[38;2;235;203;139m",
		Link:          "\x1b[38;2;136;192;208m",
		TableBorder:   "\x1b[38;2;129;161;193m",
		BlockquoteBar: "\x1b[38;2;129;161;193m",
		ListMarker:    "\x1b[38;2;129;161;193m",
		ThematicBreak: "\x1b[38;2;129;161;193m",
		CodeBorder:    "\x1b[38;2;129;161;193m",
		Syntax: SyntaxTheme{
			Text:     "\x1b[38;2;216;222;233m",
			Comment:  "\x1b[38;2;97;115;138m",
			Keyword:  "\x1b[38;2;129;161;193m",
			String:   "\x1b[38;2;163;190;140m",
			Number:   "\x1b[38;2;180;142;173m",
			Type:     "\x1b[38;2;143;188;187m",
			Function: "\x1b[38;2;136;192;208m",
			Operator: "\x1b[38;2;129;161;193m",
			Inserted: "\x1b[38;2;163;190;140m",
			Deleted:  "\x1b[38;2;191;97;106m",
			Heading:  "\x1b[38;2;97;115;138m",
		},
	}
}

func NoColorTheme() Theme {
	return Theme{}
}

func WithTheme(theme Theme) RendererOption {
	return func(r *Renderer) {
		oldCodeBorder := r.theme.CodeBorder
		if oldCodeBorder == "" {
			oldCodeBorder = MonokaiTheme().CodeBorder
		}

		r.theme = normalizeTheme(theme)
		if highlighter, ok := r.highlighter.(interface{ setSyntaxTheme(SyntaxTheme) }); ok {
			highlighter.setSyntaxTheme(r.theme.Syntax)
		}

		if r.codeBlock.BorderColor == "" || r.codeBlock.BorderColor == oldCodeBorder {
			r.codeBlock.BorderColor = r.theme.CodeBorder
		}
	}
}

func normalizeTheme(theme Theme) Theme {
	if theme.Syntax.Inserted == "" {
		theme.Syntax.Inserted = theme.Syntax.Function
	}

	if theme.Syntax.Deleted == "" {
		theme.Syntax.Deleted = theme.Syntax.Keyword
	}

	if theme.Syntax.Heading == "" {
		theme.Syntax.Heading = theme.Syntax.Comment
	}

	return theme
}
