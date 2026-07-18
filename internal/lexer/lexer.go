package lexer

import (
	"strings"
	"unicode"
)

type Lexer struct {
	input string
	pos   int
}

func New(input string) *Lexer {
	return &Lexer{input: input}
}

func (l *Lexer) Tokenize() []Token {
	var tokens []Token
	for l.pos < len(l.input) {
		l.skipWhitespace()
		if l.pos >= len(l.input) {
			break
		}
		ch := l.input[l.pos]
		switch {
		case ch == '*':
			tokens = append(tokens, Token{ASTERISK, "*"})
			l.pos++
		case ch == ',':
			tokens = append(tokens, Token{COMMA, ","})
			l.pos++
		case ch == ';':
			tokens = append(tokens, Token{SEMICOLON, ";"})
			l.pos++
		case ch == '(':
			tokens = append(tokens, Token{LPAREN, "("})
			l.pos++
		case ch == ')':
			tokens = append(tokens, Token{RPAREN, ")"})
			l.pos++
		case ch == '=' :
			tokens = append(tokens, Token{EQ, "="})
			l.pos++
		case ch == '!' && l.peek() == '=':
			tokens = append(tokens, Token{NEQ, "!="})
			l.pos += 2
		case ch == '<' && l.peek() == '=':
			tokens = append(tokens, Token{LTE, "<="})
			l.pos += 2
		case ch == '>' && l.peek() == '=':
			tokens = append(tokens, Token{GTE, ">="})
			l.pos += 2
		case ch == '<':
			tokens = append(tokens, Token{LT, "<"})
			l.pos++
		case ch == '>':
			tokens = append(tokens, Token{GT, ">"})
			l.pos++
		case ch == '\'':
			tokens = append(tokens, l.readString())
		case unicode.IsDigit(rune(ch)):
			tokens = append(tokens, l.readNumber())
		case unicode.IsLetter(rune(ch)) || ch == '_':
			tokens = append(tokens, l.readIdent())
		default:
			l.pos++
		}
	}
	tokens = append(tokens, Token{EOF, ""})
	return tokens
}

func (l *Lexer) peek() byte {
	if l.pos+1 >= len(l.input) {
		return 0
	}
	return l.input[l.pos+1]
}

func (l *Lexer) skipWhitespace() {
	for l.pos < len(l.input) && unicode.IsSpace(rune(l.input[l.pos])) {
		l.pos++
	}
}

func (l *Lexer) readString() Token {
	l.pos++ // skip opening '
	start := l.pos
	for l.pos < len(l.input) && l.input[l.pos] != '\'' {
		l.pos++
	}
	val := l.input[start:l.pos]
	l.pos++ // skip closing '
	return Token{STRING_LIT, val}
}

func (l *Lexer) readNumber() Token {
	start := l.pos
	isFloat := false
	for l.pos < len(l.input) && (unicode.IsDigit(rune(l.input[l.pos])) || l.input[l.pos] == '.') {
		if l.input[l.pos] == '.' {
			isFloat = true
		}
		l.pos++
	}
	if isFloat {
		return Token{FLOAT_LIT, l.input[start:l.pos]}
	}
	return Token{INT_LIT, l.input[start:l.pos]}
}

func (l *Lexer) readIdent() Token {
	start := l.pos
	for l.pos < len(l.input) && (unicode.IsLetter(rune(l.input[l.pos])) || l.input[l.pos] == '_' || unicode.IsDigit(rune(l.input[l.pos]))) {
		l.pos++
	}
	word := l.input[start:l.pos]
	upper := strings.ToUpper(word)
	if tt, ok := keywords[upper]; ok {
		return Token{tt, upper}
	}
	return Token{IDENT, word}
}
